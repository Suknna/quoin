package config

// Semantic validation beyond the JSON Schema (CFG-YAML-003) plus the typed
// projection of a validated Business System document (DATA-CONFIG-003): the
// timezone must resolve through the running IANA database, cron expressions
// must be standard five-field entries accepted by the locked parser (no
// descriptors, no seconds field, no embedded TZ), and stable keys must be
// unique within their real parent scope. The typed structures below only
// carry fields the frozen schema has already validated — they never
// re-declare the field inventory (CFG-YAML-001).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
)

// BusinessSystemDocument is the parse-once typed projection persisted as
// immutable columns (DATA-CONFIG-003); runtime never re-parses the YAML.
type BusinessSystemDocument struct {
	SystemKey   string `json:"systemKey"`
	DisplayName string `json:"displayName"`
	// The declaration fields are the JSON-serializable compiled representation
	// for the quoin/v1 user-facing document. Legacy columns remain below until
	// orchestration persists declaration_json during the migration.
	Description                     string               `json:"description,omitempty"`
	MetricsConnectionRef            string               `json:"metricsConnectionRef,omitempty"`
	AlertSourceRefs                 []string             `json:"alertSourceRefs,omitempty"`
	Resources                       []ResourceProjection `json:"resources,omitempty"`
	DiscoveryRefreshIntervalSeconds int64                `json:"discoveryRefreshIntervalSeconds,omitempty"`

	// MetricsConnectionID is the required, versioned metrics route. It is a
	// locator rather than a display name so a renamed connection cannot redirect
	// an already reviewed declaration.
	MetricsConnectionID int64                 `json:"metricsConnectionID,omitempty"`
	Enabled             bool                  `json:"enabled,omitempty"`
	Timezone            string                `json:"timezone,omitempty"`
	AlertSourceIDs      []int64               `json:"alertSourceIDs,omitempty"`
	AlertSourceLabels   map[string]string     `json:"alertSourceLabels,omitempty"`
	Discoveries         []DiscoveryProjection `json:"discoveries,omitempty"`
	Plans               []PlanProjection      `json:"plans,omitempty"`
}

// DiscoveryProjection mirrors one resource_discoveries entry.
type DiscoveryProjection struct {
	Key            string
	DisplayName    string
	Selector       string
	IdentityLabels []string
}

// ResourceProjection is the normalized declared metrics scope stored with a
// configuration version. It is name-based deliberately: resolving stable
// connection and alert references belongs to orchestration, not parsing.
type ResourceProjection struct {
	Name            string            `json:"name"`
	DisplayName     string            `json:"displayName"`
	MatchLabels     map[string]string `json:"matchLabels"`
	DiscoveryMetric string            `json:"discoveryMetric"`
	IdentityLabels  []string          `json:"identityLabels"`
	AllowedMetrics  []string          `json:"allowedMetrics"`
}

// PlanProjection mirrors one inspection_plans entry with its checks.
type PlanProjection struct {
	Key         string            `json:"key"`
	DisplayName string            `json:"displayName"`
	Cron        *string           `json:"cron,omitempty"`
	Timezone    string            `json:"timezone,omitempty"`
	Checks      []CheckProjection `json:"checks"`
}

// CheckProjection is the closed promql|browser discrimination. For promql,
// QueryMode is instant|range and RangeSeconds/StepSeconds are set only in
// range mode (both zero for instant). For browser, JourneyParams is the
// normalized (possibly empty) object.
type CheckProjection struct {
	Key              string `json:"key"`
	DisplayName      string `json:"displayName"`
	AnalysisQuestion string `json:"question"`
	// ResourceRef links a frozen inspection check to the compiled resource
	// policy that bounds every PromQL selector it executes.
	ResourceRef   string         `json:"resourceRef,omitempty"`
	Kind          string         `json:"kind"`                   // promql | browser
	QueryMode     string         `json:"queryMode,omitempty"`    // instant | range (promql only)
	Expression    string         `json:"expression,omitempty"`   // promql only
	RangeSeconds  int64          `json:"rangeSeconds,omitempty"` // range mode only
	StepSeconds   int64          `json:"stepSeconds,omitempty"`  // range mode only
	JourneyID     string         `json:"journeyID,omitempty"`    // browser only
	JourneyParams map[string]any `json:"journeyParams,omitempty"`
}

// Digest returns the SHA-256 over the canonical JSON encoding of the parsed
// document — all semantic content (DATA-CONFIG-003).
func (document BusinessSystemDocument) Digest() string {
	return digestOfParsed(document.canonicalValue())
}

// canonicalValue rebuilds the parsed document shape from the typed
// projection; the JSON marshal of maps sorts keys, so equal documents hash
// equally regardless of YAML key order or formatting.
func (document BusinessSystemDocument) canonicalValue() map[string]any {
	discoveries := make([]any, 0, len(document.Discoveries))
	for _, discovery := range document.Discoveries {
		labels := make([]any, 0, len(discovery.IdentityLabels))
		for _, label := range discovery.IdentityLabels {
			labels = append(labels, label)
		}
		discoveries = append(discoveries, map[string]any{
			"key": discovery.Key, "display_name": discovery.DisplayName,
			"selector": discovery.Selector, "identity_labels": labels,
		})
	}
	plans := make([]any, 0, len(document.Plans))
	for _, plan := range document.Plans {
		checks := make([]any, 0, len(plan.Checks))
		for _, check := range plan.Checks {
			entry := map[string]any{
				"key": check.Key, "display_name": check.DisplayName,
				"analysis_question": check.AnalysisQuestion, "resource_ref": check.ResourceRef, "kind": check.Kind,
			}
			switch check.Kind {
			case "promql":
				query := map[string]any{"mode": check.QueryMode, "expression": check.Expression}
				if check.QueryMode == "range" {
					query["range_seconds"] = check.RangeSeconds
					query["step_seconds"] = check.StepSeconds
				}
				entry["query"] = query
			case "browser":
				entry["journey_id"] = check.JourneyID
				entry["journey_params"] = check.JourneyParams
			}
			checks = append(checks, entry)
		}
		planValue := map[string]any{"key": plan.Key, "display_name": plan.DisplayName, "timezone": plan.Timezone, "checks": checks}
		if plan.Cron != nil {
			planValue["cron"] = *plan.Cron
		}
		plans = append(plans, planValue)
	}
	resources := make([]any, 0, len(document.Resources))
	for _, resource := range document.Resources {
		selectors := make(map[string]any, len(resource.MatchLabels))
		for name, value := range resource.MatchLabels {
			selectors[name] = value
		}
		resources = append(resources, map[string]any{
			"name": resource.Name, "display_name": resource.DisplayName,
			"match_labels": selectors, "discovery_metric": resource.DiscoveryMetric,
			"identity_labels": resource.IdentityLabels, "allowed_metrics": resource.AllowedMetrics,
		})
	}
	alertSourceRefs := make([]any, 0, len(document.AlertSourceRefs))
	for _, ref := range document.AlertSourceRefs {
		alertSourceRefs = append(alertSourceRefs, ref)
	}
	alertSourceIDs := make([]any, 0, len(document.AlertSourceIDs))
	for _, id := range document.AlertSourceIDs {
		alertSourceIDs = append(alertSourceIDs, fmt.Sprint(id))
	}
	alertSourceLabels := make(map[string]any, len(document.AlertSourceLabels))
	for name, value := range document.AlertSourceLabels {
		alertSourceLabels[name] = value
	}
	return map[string]any{
		"system_key": document.SystemKey, "display_name": document.DisplayName,
		"description": document.Description, "metrics_connection_ref": document.MetricsConnectionRef,
		"metrics_connection_id": fmt.Sprint(document.MetricsConnectionID),
		"enabled":               document.Enabled, "timezone": document.Timezone,
		"alert_source_refs": alertSourceRefs, "alert_source_ids": alertSourceIDs,
		"alert_source_labels": alertSourceLabels, "resources": resources,
		"discovery_refresh_interval_seconds": document.DiscoveryRefreshIntervalSeconds,
		"resource_discoveries":               discoveries, "inspection_plans": plans,
	}
}

func digestOfParsed(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Canonical values from the parser are always marshalable; a failure
		// here is a build fault. Hash the error text so the row is never
		// written with an empty digest silently.
		encoded = []byte("marshal-error:" + err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// validateCron enforces the standard five-field form before delegating
// range/syntax checking to the locked parser (CFG-CRON-001): no descriptors,
// no seconds field, no embedded CRON_TZ/TZ.
func validateCron(expression, path string) []FieldError {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return []FieldError{{Path: path, Reason: "cron 不能是空白；缺省调度请省略该字段", Remediation: "删除 cron 字段表示仅人工运行"}}
	}
	if strings.HasPrefix(trimmed, "@") {
		return []FieldError{{Path: path, Reason: "不支持 @every/@daily 等 descriptor；必须是标准五字段 cron", Remediation: "改用五字段表达式，如 \"30 8 * * *\""}}
	}
	upper := strings.ToUpper(trimmed)
	if strings.Contains(upper, "CRON_TZ=") || strings.Contains(upper, "TZ=") {
		return []FieldError{{Path: path, Reason: "cron 表达式不允许内嵌 CRON_TZ/TZ；时区由配置根 timezone 统一提供", Remediation: "删除时区前缀，时区写在根节点 timezone 字段"}}
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return []FieldError{{Path: path, Reason: fmt.Sprintf("cron 必须恰好五个空白分隔字段（当前 %d 个）", len(fields)), Remediation: "使用 分 时 日 月 周 的五字段形式"}}
	}
	if _, err := cron.ParseStandard(trimmed); err != nil {
		return []FieldError{{Path: path, Reason: "cron 解析失败: " + firstLine(err.Error()), Remediation: "检查各字段取值范围"}}
	}
	return nil
}

func rootString(root map[string]any, key string) string {
	if value, ok := root[key].(string); ok {
		return value
	}
	return ""
}

func stringMap(value any) map[string]string {
	items, _ := value.(map[string]any)
	result := make(map[string]string, len(items))
	for key, value := range items {
		if text, ok := value.(string); ok {
			result[key] = text
		}
	}
	return result
}

// rootInt accepts the JSON decoder's integer representations used by the
// current declaration extractor. Schema validation has already ruled out
// fractional values before it reaches this projection.
func rootInt(root map[string]any, key string) int64 {
	if value, ok := root[key].(int64); ok {
		return value
	}
	if value, ok := root[key].(float64); ok && value == float64(int64(value)) {
		return int64(value)
	}
	return 0
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
