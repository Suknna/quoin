package ops

import (
	"fmt"
	"sort"
	"strings"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"
)

// metricsCatalog is the parsed projection of contracts/metrics.yaml: the
// single machine authority for the four components' custom metric families
// (OPS-METRIC-001). Family shapes are read from the embedded catalog at
// startup; closed label values come from enumLabelValues below, which is a
// frozen projection of the referenced OpenAPI/Proto/SQL authorities and is
// proven equal to those machine sources by TestMetricsLabelProjection.
type metricsCatalog struct {
	Families []catalogFamily
	Buckets  map[string][]float64
}

type catalogFamily struct {
	Name   string   `yaml:"name"`
	Type   string   `yaml:"type"`
	Help   string   `yaml:"help"`
	Labels []string `yaml:"labels"`
	// Buckets references a histogram_buckets entry in the catalog.
	Buckets       string `yaml:"buckets"`
	Preinitialize bool   `yaml:"preinitialize"`
}

type catalogDocument struct {
	EnumSources map[string]struct {
		Values    []string `yaml:"values"`
		Authority string   `yaml:"authority"`
	} `yaml:"enum_sources"`
	HistogramBuckets map[string][]float64 `yaml:"histogram_buckets"`
	LabelSets        map[string]struct {
		Values     []string `yaml:"values"`
		ValuesFrom string   `yaml:"values_from"`
	} `yaml:"label_sets"`
	Families []catalogFamily `yaml:"families"`
}

// enumLabelValues is the closed label-value projection for every catalog
// label set declared with values_from. Sources (see metrics.yaml
// enum_sources): openapi.yaml tags and methods, runtime.proto enums and
// services, and sql/schema.sql CHECK constraints, each normalized by the
// catalog's transform rules. TestMetricsLabelProjection fails when any real
// machine source drifts from this table.
var enumLabelValues = map[string][]string{
	"openapi_route_group": {"admin", "adminops", "alerts", "auth", "config", "files", "inspections", "investigations", "knowledge", "maintenance", "realtime", "setup", "verification"},
	"openapi_method":      {"get", "patch", "post", "put"},
	"runtime_slot":        {"lintel", "plinth"},
	"attempt_type": {
		"browser_exploration", "connection_probe", "embedding", "initial_analysis",
		"inspection_analysis", "inspection_collection", "investigation", "knowledge_extraction",
	},
	"maintenance_reason": {"lintel_recovery", "restore", "root_key_rebind", "upgrade"},
	"attempt_termination_reason": {
		"artifact_body_expired", "artifact_commit_failed", "business_system_disabled", "cancelled",
		"connection_disabled", "context_too_large", "invalid_response", "lease_expired",
		"provider_unavailable", "rate_limited", "replaced", "revoked",
		"sandbox_unavailable", "timeout", "tool_error", "worker_protocol_error",
	},
	"model_operation":     {"chat", "embedding"},
	"model_call_status":   {"cancelled", "failed", "running", "succeeded"},
	"tool_execution_mode": {"quoin_browser", "supervisor_typed", "worker_local"},
	"tool_call_status":    {"cancelled", "failed", "pending", "running", "succeeded"},
	"rpc_group":           {"artifact_service", "browser_tunnel", "runtime_control", "stele_relay"},
	"delivery_status":     {"accepted", "rejected", "unavailable"},
}

// catalogLabelSets holds the catalog's own closed values (authority: metrics)
// after parsing, so label resolution never depends on repository files at
// component runtime.
var catalogLabelSets map[string][]string

func parseMetricsCatalog() (*metricsCatalog, error) {
	var document catalogDocument
	if err := yaml.Unmarshal(gen.MetricsYAML, &document); err != nil {
		return nil, fmt.Errorf("parse metrics catalog: %w", err)
	}
	catalogLabelSets = make(map[string][]string, len(document.LabelSets))
	for name, set := range document.LabelSets {
		if len(set.Values) == 0 && set.ValuesFrom == "" {
			return nil, fmt.Errorf("metrics catalog label set %q has no values", name)
		}
		if set.ValuesFrom != "" {
			if projected, ok := enumLabelValues[set.ValuesFrom]; ok && len(projected) > 0 {
				catalogLabelSets[name] = projected
				continue
			}
			// Catalog-owned enum sources carry their closed values inline
			// (authority: metrics) instead of referencing an external machine
			// source; resolve them directly so no repository read is needed.
			if source, ok := document.EnumSources[set.ValuesFrom]; ok && len(source.Values) > 0 {
				values := append([]string(nil), source.Values...)
				sort.Strings(values)
				catalogLabelSets[name] = values
				continue
			}
			return nil, fmt.Errorf("metrics catalog label set %q references unprojected enum source %q", name, set.ValuesFrom)
		}
		values := append([]string(nil), set.Values...)
		sort.Strings(values)
		catalogLabelSets[name] = values
	}
	for family := range document.Families {
		for _, label := range document.Families[family].Labels {
			if _, ok := catalogLabelSets[label]; !ok {
				return nil, fmt.Errorf("metrics family %q uses label %q outside the catalog label sets", document.Families[family].Name, label)
			}
		}
	}
	return &metricsCatalog{Families: document.Families, Buckets: document.HistogramBuckets}, nil
}

// familyOwner maps a catalog family to the component whose /metrics endpoint
// exports it. Every catalog family name starts with its component token.
func familyOwner(family catalogFamily) string {
	return strings.SplitN(family.Name, "_", 2)[0]
}

// registerCatalogMetrics creates and preinitializes every catalog family the
// component owns (OPS-METRIC-003): all foreseen series of closed label sets
// are exported from startup. It returns the collectors by family name so the
// shared server can keep driving the dynamic readiness gauges.
func registerCatalogMetrics(registry *prometheus.Registry, component string) (map[string]prometheus.Collector, error) {
	catalog, err := parseMetricsCatalog()
	if err != nil {
		return nil, err
	}
	collectors := make(map[string]prometheus.Collector)
	for _, family := range catalog.Families {
		if familyOwner(family) != component {
			continue
		}
		collector, err := newFamilyCollector(catalog, family)
		if err != nil {
			return nil, err
		}
		if err := registry.Register(collector); err != nil {
			return nil, fmt.Errorf("register metrics family %q: %w", family.Name, err)
		}
		collectors[family.Name] = collector
	}
	return collectors, nil
}

func newFamilyCollector(catalog *metricsCatalog, family catalogFamily) (prometheus.Collector, error) {
	if len(family.Labels) == 0 {
		switch family.Type {
		case "gauge":
			return prometheus.NewGauge(prometheus.GaugeOpts{Name: family.Name, Help: family.Help}), nil
		case "counter":
			return prometheus.NewCounter(prometheus.CounterOpts{Name: family.Name, Help: family.Help}), nil
		case "histogram":
			buckets, err := catalog.buckets(family)
			if err != nil {
				return nil, err
			}
			return prometheus.NewHistogram(prometheus.HistogramOpts{Name: family.Name, Help: family.Help, Buckets: buckets}), nil
		default:
			return nil, fmt.Errorf("metrics family %q has unsupported type %q", family.Name, family.Type)
		}
	}
	values, err := catalog.labelCartesian(family)
	if err != nil {
		return nil, err
	}
	switch family.Type {
	case "gauge":
		vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: family.Name, Help: family.Help}, family.Labels)
		for _, combination := range values {
			vec.WithLabelValues(combination...).Set(float64(initialFamilyValue(family)))
		}
		return vec, nil
	case "counter":
		vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: family.Name, Help: family.Help}, family.Labels)
		for _, combination := range values {
			vec.WithLabelValues(combination...)
		}
		return vec, nil
	case "histogram":
		buckets, err := catalog.buckets(family)
		if err != nil {
			return nil, err
		}
		vec := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: family.Name, Help: family.Help, Buckets: buckets}, family.Labels)
		for _, combination := range values {
			vec.WithLabelValues(combination...)
		}
		return vec, nil
	default:
		return nil, fmt.Errorf("metrics family %q has unsupported type %q", family.Name, family.Type)
	}
}

// initialFamilyValue keeps the shipped storage-probe semantics: the writable
// gauges assert the startup durability probe passed (1); every other family
// starts at the catalog default of zero.
func initialFamilyValue(family catalogFamily) int {
	if family.Name == "quoin_storage_writable" || family.Name == "plinth_storage_writable" || family.Name == "lintel_storage_writable" {
		return 1
	}
	return 0
}

func (catalog *metricsCatalog) buckets(family catalogFamily) ([]float64, error) {
	if family.Buckets == "" {
		return nil, fmt.Errorf("metrics histogram %q has no bucket set", family.Name)
	}
	buckets, ok := catalog.Buckets[family.Buckets]
	if !ok {
		return nil, fmt.Errorf("metrics histogram %q references unknown bucket set %q", family.Name, family.Buckets)
	}
	return buckets, nil
}

// labelCartesian expands the closed label values of a family into every
// foreseen combination, in stable label order.
func (catalog *metricsCatalog) labelCartesian(family catalogFamily) ([][]string, error) {
	combinations := [][]string{{}}
	for _, label := range family.Labels {
		values := catalogLabelSets[label]
		next := make([][]string, 0, len(combinations)*len(values))
		for _, combination := range combinations {
			for _, value := range values {
				expanded := append(append([]string(nil), combination...), value)
				next = append(next, expanded)
			}
		}
		combinations = next
	}
	return combinations, nil
}

// CatalogFamiliesFor returns the names of every catalog family a component
// exports, in catalog order. The deployment helper uses the same projection
// to judge real /metrics output against the catalog.
func CatalogFamiliesFor(component string) ([]map[string]any, error) {
	catalog, err := parseMetricsCatalog()
	if err != nil {
		return nil, err
	}
	projection := make([]map[string]any, 0, len(catalog.Families))
	for _, family := range catalog.Families {
		if familyOwner(family) != component {
			continue
		}
		entry := map[string]any{"name": family.Name, "type": family.Type, "help": family.Help, "labels": family.Labels}
		if len(family.Labels) > 0 {
			values := map[string][]string{}
			for _, label := range family.Labels {
				values[label] = catalogLabelSets[label]
			}
			entry["labelValues"] = values
		}
		if family.Buckets != "" {
			entry["buckets"] = family.Buckets
		}
		projection = append(projection, entry)
	}
	if len(projection) == 0 {
		return nil, fmt.Errorf("no metrics families for component %q", component)
	}
	return projection, nil
}
