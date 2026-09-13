package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

const DefaultDiscoveryRefresh = 5 * time.Minute

// BusinessSystem is the canonical user-facing quoin/v1 declaration. It keeps
// connection and alert references stable by name; later orchestration resolves
// those names without making this parser depend on storage.
type BusinessSystem struct {
	APIVersion string
	Kind       string
	Metadata   BusinessSystemMetadata
	Spec       BusinessSystemSpec
}

type BusinessSystemMetadata struct{ Name, DisplayName, Description string }
type BusinessSystemSpec struct {
	Enabled     bool
	Metrics     MetricsScope
	Alerts      AlertScope
	Inspections []Inspection
	Discovery   DiscoverySettings
}
type MetricsScope struct {
	ConnectionRef string
	MatchLabels   map[string]string
	Resources     []ResourceScope
}
type ResourceScope struct {
	Name, DisplayName, DiscoveryMetric string
	MatchLabels                        map[string]string
	IdentityLabels, AllowedMetrics     []string
}
type AlertScope struct {
	SourceRefs  []string
	MatchLabels map[string]string
}
type Inspection struct {
	Name, DisplayName, Schedule, Timezone string
	Checks                                []InspectionCheck
}
type InspectionCheck struct {
	Name, ResourceRef, Expression, Question, QueryMode string
	RangeSeconds, StepSeconds                          int64
}
type DiscoverySettings struct{ Refresh time.Duration }

// CompiledResourceScope is the safe execution interface for a declared
// resource. Callers receive selectors only through this type, which injects
// every mandatory selector into every vector metric AST node.
type CompiledResourceScope struct {
	Name, DisplayName, ConnectionRef, DiscoveryMetric string
	IdentityLabels                                    []string
	AllowedMetrics                                    []string
	selectors                                         map[string]string
}

// ParseBusinessSystem is the sole user-facing BusinessSystem parser. Legacy
// documents intentionally require an explicit migration path and never enter
// this function.
func ParseBusinessSystem(body []byte, limits Limits) (BusinessSystem, []FieldError) {
	value, fields := ParseStrictYAML(body, limits, "document")
	if len(fields) != 0 {
		return BusinessSystem{}, fields
	}
	if fields = ValidateSchema(value, SchemaBusinessSystem); len(fields) != 0 {
		return BusinessSystem{}, fields
	}
	document, err := extractBusinessSystemV1(value)
	if err != nil {
		return BusinessSystem{}, []FieldError{{Path: "document", Reason: err.Error()}}
	}
	return document, ValidateBusinessSystem(document)
}

func (document BusinessSystem) DiscoveryRefresh() time.Duration {
	if document.Spec.Discovery.Refresh == 0 {
		return DefaultDiscoveryRefresh
	}
	return document.Spec.Discovery.Refresh
}

// CompileBusinessSystemDocument produces both the canonical declaration JSON
// projection and compatibility projections consumed by the existing service.
// It is deliberately fallible: callers must never persist unscoped PromQL or a
// partially merged policy. MetricsConnectionID remains zero until the service
// resolves the stable connection name in its writer transaction.
func CompileBusinessSystemDocument(declaration BusinessSystem) (BusinessSystemDocument, error) {
	if fields := ValidateBusinessSystem(declaration); len(fields) != 0 {
		return BusinessSystemDocument{}, &ValidationError{Errors: fields}
	}
	resources := make([]ResourceProjection, 0, len(declaration.Spec.Metrics.Resources))
	discoveries := make([]DiscoveryProjection, 0, len(declaration.Spec.Metrics.Resources))
	for _, resource := range declaration.Spec.Metrics.Resources {
		// Persist the fully merged scope, so runtime never rebuilds an authority
		// boundary from independent root and group fragments.
		resourceLabels, err := mergeLabels(declaration.Spec.Metrics.MatchLabels, resource.MatchLabels)
		if err != nil {
			return BusinessSystemDocument{}, err
		}
		projection := ResourceProjection{Name: resource.Name, DisplayName: resource.DisplayName, MatchLabels: resourceLabels, DiscoveryMetric: resource.DiscoveryMetric, IdentityLabels: append([]string(nil), resource.IdentityLabels...), AllowedMetrics: append([]string(nil), resource.AllowedMetrics...)}
		resources = append(resources, projection)
		scope, err := NewCompiledResourceScope(declaration.Spec.Metrics.ConnectionRef, projection)
		if err != nil {
			return BusinessSystemDocument{}, err
		}
		selector, err := scope.ScopeExpression(resource.DiscoveryMetric)
		if err != nil {
			return BusinessSystemDocument{}, fmt.Errorf("compile discovery %q: %w", resource.Name, err)
		}
		discoveries = append(discoveries, DiscoveryProjection{Key: resource.Name, DisplayName: resource.DisplayName, Selector: selector, IdentityLabels: append([]string(nil), resource.IdentityLabels...)})
	}
	plans := make([]PlanProjection, 0, len(declaration.Spec.Inspections))
	for _, inspection := range declaration.Spec.Inspections {
		cron := inspection.Schedule
		plan := PlanProjection{Key: inspection.Name, DisplayName: inspection.DisplayName, Cron: &cron, Timezone: inspection.Timezone, Checks: make([]CheckProjection, 0, len(inspection.Checks))}
		for _, check := range inspection.Checks {
			queryMode := check.QueryMode
			if queryMode == "" {
				queryMode = "instant"
			}
			scope, err := findCompiledResourceScope(declaration.Spec.Metrics.ConnectionRef, resources, check.ResourceRef)
			if err != nil {
				return BusinessSystemDocument{}, err
			}
			expression, err := scope.ScopeExpression(check.Expression)
			if err != nil {
				return BusinessSystemDocument{}, fmt.Errorf("compile inspection %q check %q: %w", inspection.Name, check.Name, err)
			}
			plan.Checks = append(plan.Checks, CheckProjection{Key: check.Name, DisplayName: check.Name, AnalysisQuestion: check.Question, ResourceRef: check.ResourceRef, Kind: "promql", QueryMode: queryMode, Expression: expression, RangeSeconds: check.RangeSeconds, StepSeconds: check.StepSeconds})
		}
		plans = append(plans, plan)
	}
	alertLabels := cloneStringMap(declaration.Spec.Alerts.MatchLabels)
	if len(alertLabels) == 0 {
		alertLabels = cloneStringMap(declaration.Spec.Metrics.MatchLabels)
	}
	return BusinessSystemDocument{SystemKey: declaration.Metadata.Name, DisplayName: declaration.Metadata.DisplayName, Description: declaration.Metadata.Description, MetricsConnectionRef: declaration.Spec.Metrics.ConnectionRef, AlertSourceRefs: append([]string(nil), declaration.Spec.Alerts.SourceRefs...), AlertSourceLabels: alertLabels, Resources: resources, Discoveries: discoveries, Plans: plans, Enabled: declaration.Spec.Enabled, Timezone: "UTC", DiscoveryRefreshIntervalSeconds: int64(declaration.DiscoveryRefresh() / time.Second)}, nil
}

func findCompiledResourceScope(connectionRef string, resources []ResourceProjection, resourceRef string) (CompiledResourceScope, error) {
	for _, resource := range resources {
		if resource.Name == resourceRef {
			return NewCompiledResourceScope(connectionRef, resource)
		}
	}
	return CompiledResourceScope{}, fmt.Errorf("unknown resourceRef %q", resourceRef)
}

func (document BusinessSystem) CompileResourceScope(resourceRef string) (CompiledResourceScope, error) {
	for _, resource := range document.Spec.Metrics.Resources {
		if resource.Name == resourceRef {
			selectors, err := mergeLabels(document.Spec.Metrics.MatchLabels, resource.MatchLabels)
			if err != nil {
				return CompiledResourceScope{}, err
			}
			return NewCompiledResourceScope(document.Spec.Metrics.ConnectionRef, ResourceProjection{Name: resource.Name, DisplayName: resource.DisplayName, MatchLabels: selectors, DiscoveryMetric: resource.DiscoveryMetric, IdentityLabels: resource.IdentityLabels, AllowedMetrics: resource.AllowedMetrics})
		}
	}
	return CompiledResourceScope{}, fmt.Errorf("unknown resourceRef %q", resourceRef)
}

// CompileResourceScope lets runtime use a BusinessSystemDocument decoded from
// declaration_json directly. Its Resources already contain the root and group
// label selectors merged by CompileBusinessSystemDocument.
func (document BusinessSystemDocument) CompileResourceScope(resourceRef string) (CompiledResourceScope, error) {
	for _, resource := range document.Resources {
		if resource.Name == resourceRef {
			return NewCompiledResourceScope(document.MetricsConnectionRef, resource)
		}
	}
	return CompiledResourceScope{}, fmt.Errorf("unknown resourceRef %q", resourceRef)
}

// NewCompiledResourceScope establishes the only runtime-facing scope wrapper
// from a persisted resource policy. Callers should use ScopeExpression rather
// than manually adding labels to untrusted PromQL.
func NewCompiledResourceScope(connectionRef string, resource ResourceProjection) (CompiledResourceScope, error) {
	if resource.Name == "" {
		return CompiledResourceScope{}, fmt.Errorf("resource policy name is required")
	}
	if len(resource.MatchLabels) == 0 {
		return CompiledResourceScope{}, fmt.Errorf("resource policy %q has no mandatory selectors", resource.Name)
	}
	selectors := cloneStringMap(resource.MatchLabels)
	return CompiledResourceScope{Name: resource.Name, DisplayName: resource.DisplayName, ConnectionRef: connectionRef, DiscoveryMetric: resource.DiscoveryMetric, IdentityLabels: append([]string(nil), resource.IdentityLabels...), AllowedMetrics: append([]string(nil), resource.AllowedMetrics...), selectors: selectors}, nil
}

// MandatorySelectors returns an immutable copy of every exact scope selector.
func (scope CompiledResourceScope) MandatorySelectors() map[string]string {
	copy := make(map[string]string, len(scope.selectors))
	for k, v := range scope.selectors {
		copy[k] = v
	}
	return copy
}

// ScopeExpression validates the complete vector-metric whitelist then injects
// required exact selectors AST-first. It does not permit regex-based scoping.
func (scope CompiledResourceScope) ScopeExpression(expression string) (string, error) {
	expr, err := promQLParser.ParseExpr(expression)
	if err != nil {
		return "", fmt.Errorf("parse PromQL: %w", err)
	}
	var scopeErr error
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vector, ok := node.(*parser.VectorSelector)
		if !ok || scopeErr != nil {
			return nil
		}
		if !metricAllowed(vector.Name, scope.AllowedMetrics) {
			scopeErr = fmt.Errorf("metric %q is not allowed for resource %q", vector.Name, scope.Name)
			return nil
		}
		for name, value := range scope.selectors {
			found := false
			for _, matcher := range vector.LabelMatchers {
				if matcher.Name != name {
					continue
				}
				if matcher.Type != labels.MatchEqual || matcher.Value != value {
					scopeErr = fmt.Errorf("selector %s must exactly equal %q", name, value)
					return nil
				}
				found = true
			}
			if !found {
				vector.LabelMatchers = append(vector.LabelMatchers, &labels.Matcher{Type: labels.MatchEqual, Name: name, Value: value})
			}
		}
		return nil
	})
	if scopeErr != nil {
		return "", scopeErr
	}
	return expr.String(), nil
}

func ValidateBusinessSystem(document BusinessSystem) []FieldError {
	var fields []FieldError
	if refresh := document.DiscoveryRefresh(); refresh < time.Minute || refresh > 24*time.Hour {
		fields = append(fields, FieldError{Path: "spec.discovery.refresh", Reason: "discovery refresh 必须介于 1m 和 24h 之间", Remediation: "使用 1m 至 24h 的整秒、分或小时 duration"})
	}
	fields = append(fields, validateMatchLabelNames(document.Spec.Metrics.MatchLabels, "spec.metrics.matchLabels")...)
	resourceNames := map[string]bool{}
	for resourceIndex, resource := range document.Spec.Metrics.Resources {
		path := fmt.Sprintf("spec.metrics.resources[%d]", resourceIndex)
		if resourceNames[resource.Name] {
			fields = append(fields, FieldError{Path: path + ".name", Reason: "resource name 在同一配置内重复: " + resource.Name, Remediation: "每个 resource name 必须在本配置内唯一"})
		}
		resourceNames[resource.Name] = true
		fields = append(fields, validateMatchLabelNames(resource.MatchLabels, path+".matchLabels")...)
		if _, err := mergeLabels(document.Spec.Metrics.MatchLabels, resource.MatchLabels); err != nil {
			fields = append(fields, FieldError{Path: path + ".matchLabels", Reason: err.Error(), Remediation: "删除或改正与 metrics.matchLabels 冲突的值"})
		}
		for _, metric := range resource.AllowedMetrics {
			if strings.HasSuffix(metric, "*") && len(strings.TrimSuffix(metric, "*")) == 0 {
				fields = append(fields, FieldError{Path: path + ".allowedMetrics", Reason: "指标前缀通配符必须含非空前缀", Remediation: "使用如 http_requests_* 的前缀通配符"})
			}
		}
		if !metricAllowed(resource.DiscoveryMetric, resource.AllowedMetrics) {
			fields = append(fields, FieldError{Path: path + ".discoveryMetric", Reason: "discoveryMetric 必须在该资源的 allowedMetrics 白名单中", Remediation: "将 discoveryMetric 加入 allowedMetrics 或选择允许的指标"})
		}
	}
	fields = append(fields, validateMatchLabelNames(document.Spec.Alerts.MatchLabels, "spec.alerts.matchLabels")...)
	inspectionNames := map[string]bool{}
	for inspectionIndex, inspection := range document.Spec.Inspections {
		path := fmt.Sprintf("spec.inspections[%d]", inspectionIndex)
		if inspectionNames[inspection.Name] {
			fields = append(fields, FieldError{Path: path + ".name", Reason: "inspection name 在同一配置内重复: " + inspection.Name, Remediation: "每个 inspection name 必须在本配置内唯一"})
		}
		inspectionNames[inspection.Name] = true
		fields = append(fields, validateCron(inspection.Schedule, path+".schedule")...)
		if _, err := time.LoadLocation(inspection.Timezone); err != nil {
			fields = append(fields, FieldError{Path: path + ".timezone", Reason: "时区必须是可解析的 IANA 名称", Remediation: "使用如 Asia/Shanghai 的 IANA 时区名"})
		}
		checkNames := map[string]bool{}
		for checkIndex, check := range inspection.Checks {
			checkPath := fmt.Sprintf("%s.checks[%d]", path, checkIndex)
			if checkNames[check.Name] {
				fields = append(fields, FieldError{Path: checkPath + ".name", Reason: "check name 在所属 inspection 内重复: " + check.Name, Remediation: "check name 只需在所属 inspection 内唯一；重命名或删除重复项"})
			}
			checkNames[check.Name] = true
			scope, err := document.CompileResourceScope(check.ResourceRef)
			if err != nil {
				fields = append(fields, FieldError{Path: checkPath + ".resourceRef", Reason: err.Error(), Remediation: "引用 spec.metrics.resources 中存在的 name"})
				continue
			}
			if _, err := scope.ScopeExpression(check.Expression); err != nil {
				fields = append(fields, FieldError{Path: checkPath + ".expression", Reason: err.Error(), Remediation: "只查询该资源允许的指标，且不要放宽必需 labels"})
			}
		}
	}
	return fields
}

// validateMatchLabelNames closes the schema's open label maps with the same
// legacy syntax Prometheus accepts in unescaped PromQL selectors.
func validateMatchLabelNames(labels map[string]string, path string) []FieldError {
	var fields []FieldError
	for name := range labels {
		if !model.LegacyValidation.IsValidLabelName(name) {
			fields = append(fields, FieldError{Path: path + "." + name, Reason: "label 名称不是有效的 Prometheus label name: " + name, Remediation: "使用字母或下划线开头，后续只含字母、数字或下划线的 label 名称"})
		}
	}
	return fields
}

func mergeLabels(base, extra map[string]string) (map[string]string, error) {
	merged := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		if previous, exists := merged[k]; exists && previous != v {
			return nil, fmt.Errorf("matchLabels conflicts on %q: %q versus %q", k, previous, v)
		}
		merged[k] = v
	}
	return merged, nil
}
func metricAllowed(metric string, allowed []string) bool {
	for _, pattern := range allowed {
		if pattern == metric || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(metric, strings.TrimSuffix(pattern, "*"))) {
			return true
		}
	}
	return false
}

func extractBusinessSystemV1(value any) (BusinessSystem, error) {
	root, ok := value.(map[string]any)
	if !ok {
		return BusinessSystem{}, fmt.Errorf("文档根必须是映射")
	}
	metadata, _ := root["metadata"].(map[string]any)
	spec, _ := root["spec"].(map[string]any)
	metrics, _ := spec["metrics"].(map[string]any)
	alerts, _ := spec["alerts"].(map[string]any)
	// enabled defaults to true when absent; a bool cannot distinguish absent from
	// explicit false without looking at the canonical parsed mapping first.
	enabled := true
	if rawEnabled, present := spec["enabled"]; present {
		enabled, _ = rawEnabled.(bool)
	}
	document := BusinessSystem{APIVersion: rootString(root, "apiVersion"), Kind: rootString(root, "kind"), Metadata: BusinessSystemMetadata{Name: rootString(metadata, "name"), DisplayName: rootString(metadata, "displayName"), Description: rootString(metadata, "description")}, Spec: BusinessSystemSpec{Enabled: enabled, Metrics: MetricsScope{ConnectionRef: rootString(metrics, "connectionRef"), MatchLabels: stringMap(metrics["matchLabels"])}, Alerts: AlertScope{SourceRefs: stringSlice(alerts["sourceRefs"]), MatchLabels: stringMap(alerts["matchLabels"])}}}
	if discovery, ok := spec["discovery"].(map[string]any); ok {
		refresh, err := time.ParseDuration(rootString(discovery, "refresh"))
		if err != nil {
			return BusinessSystem{}, fmt.Errorf("discovery refresh 无法解析")
		}
		document.Spec.Discovery.Refresh = refresh
	}
	for _, item := range anySlice(metrics["resources"]) {
		resource, _ := item.(map[string]any)
		document.Spec.Metrics.Resources = append(document.Spec.Metrics.Resources, ResourceScope{Name: rootString(resource, "name"), DisplayName: rootString(resource, "displayName"), MatchLabels: stringMap(resource["matchLabels"]), DiscoveryMetric: rootString(resource, "discoveryMetric"), IdentityLabels: stringSlice(resource["identityLabels"]), AllowedMetrics: stringSlice(resource["allowedMetrics"])})
	}
	for _, item := range anySlice(spec["inspections"]) {
		inspection, _ := item.(map[string]any)
		result := Inspection{Name: rootString(inspection, "name"), DisplayName: rootString(inspection, "displayName"), Schedule: rootString(inspection, "schedule"), Timezone: rootString(inspection, "timezone")}
		for _, checkItem := range anySlice(inspection["checks"]) {
			check, _ := checkItem.(map[string]any)
			result.Checks = append(result.Checks, InspectionCheck{Name: rootString(check, "name"), ResourceRef: rootString(check, "resourceRef"), Expression: rootString(check, "expression"), Question: rootString(check, "question"), QueryMode: rootString(check, "queryMode"), RangeSeconds: int64(rootInt(check, "rangeSeconds")), StepSeconds: int64(rootInt(check, "stepSeconds"))})
		}
		document.Spec.Inspections = append(document.Spec.Inspections, result)
	}
	return document, nil
}
func anySlice(value any) []any { items, _ := value.([]any); return items }

func cloneStringMap(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for name, value := range source {
		copy[name] = value
	}
	return copy
}

// SortedMandatorySelectorNames lets downstream consumers produce stable audit
// records without relying on Go's random map iteration.
func (scope CompiledResourceScope) SortedMandatorySelectorNames() []string {
	names := make([]string, 0, len(scope.selectors))
	for name := range scope.selectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
