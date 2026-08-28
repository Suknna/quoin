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
	"time"
	_ "time/tzdata" // the runtime image is scratch-based; the binary embeds the IANA tz database (CFG-YAML-003)

	"github.com/robfig/cron/v3"
)

// BusinessSystemDocument is the parse-once typed projection persisted as
// immutable columns (DATA-CONFIG-003); runtime never re-parses the YAML.
type BusinessSystemDocument struct {
	SystemKey                      string
	DisplayName                    string
	Enabled                        bool
	Timezone                       string
	ResourceRefreshIntervalSeconds int64
	Discoveries                    []DiscoveryProjection
	Plans                          []PlanProjection
}

// DiscoveryProjection mirrors one resource_discoveries entry.
type DiscoveryProjection struct {
	Key            string
	DisplayName    string
	Selector       string
	IdentityLabels []string
}

// PlanProjection mirrors one inspection_plans entry with its checks.
type PlanProjection struct {
	Key         string
	DisplayName string
	Cron        *string
	Checks      []CheckProjection
}

// CheckProjection is the closed promql|browser discrimination. For promql,
// QueryMode is instant|range and RangeSeconds/StepSeconds are set only in
// range mode (both zero for instant). For browser, JourneyParams is the
// normalized (possibly empty) object.
type CheckProjection struct {
	Key              string
	DisplayName      string
	AnalysisQuestion string
	Kind             string // promql | browser
	QueryMode        string // instant | range (promql only)
	Expression       string // promql only
	RangeSeconds     int64  // range mode only
	StepSeconds      int64  // range mode only
	JourneyID        string // browser only
	JourneyParams    map[string]any
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
				"analysis_question": check.AnalysisQuestion, "kind": check.Kind,
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
		planValue := map[string]any{"key": plan.Key, "display_name": plan.DisplayName, "checks": checks}
		if plan.Cron != nil {
			planValue["cron"] = *plan.Cron
		}
		plans = append(plans, planValue)
	}
	return map[string]any{
		"system_key": document.SystemKey, "display_name": document.DisplayName,
		"enabled": document.Enabled, "timezone": document.Timezone,
		"resource_refresh_interval_seconds": document.ResourceRefreshIntervalSeconds,
		"resource_discoveries":              discoveries, "inspection_plans": plans,
	}
}

// LabelContractDocument is the parse-once typed projection of a Label
// Contract YAML.
type LabelContractDocument struct {
	BusinessSystemLabel string
}

func (document LabelContractDocument) canonicalValue() map[string]any {
	return map[string]any{
		"label_contract": map[string]any{
			"business_system_label": document.BusinessSystemLabel,
		},
	}
}

// Digest returns the SHA-256 over the canonical JSON encoding of the parsed
// Label Contract document.
func (document LabelContractDocument) Digest() string {
	return digestOfParsed(document.canonicalValue())
}

// CanonicalJSON returns the deterministic (sorted-key) JSON encoding of the
// parsed projection persisted as contract_json.
func (document LabelContractDocument) CanonicalJSON() string {
	encoded, err := json.Marshal(document.canonicalValue())
	if err != nil {
		return "{}"
	}
	return string(encoded)
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

// ExtractBusinessSystem projects the schema-validated canonical value into
// the typed document. The schema guarantees the shape; defensive nil guards
// only prevent panics on impossible input.
func ExtractBusinessSystem(value any) (BusinessSystemDocument, error) {
	root, ok := value.(map[string]any)
	if !ok {
		return BusinessSystemDocument{}, fmt.Errorf("文档根必须是映射")
	}
	document := BusinessSystemDocument{
		SystemKey:                      rootString(root, "system_key"),
		DisplayName:                    rootString(root, "display_name"),
		Enabled:                        rootBool(root, "enabled"),
		Timezone:                       rootString(root, "timezone"),
		ResourceRefreshIntervalSeconds: int64(rootInt(root, "resource_refresh_interval_seconds")),
	}
	if rawDiscoveries, ok := root["resource_discoveries"].([]any); ok {
		for _, item := range rawDiscoveries {
			entry, _ := item.(map[string]any)
			if entry == nil {
				continue
			}
			discovery := DiscoveryProjection{
				Key:            rootString(entry, "key"),
				DisplayName:    rootString(entry, "display_name"),
				Selector:       rootString(entry, "selector"),
				IdentityLabels: stringSlice(entry["identity_labels"]),
			}
			document.Discoveries = append(document.Discoveries, discovery)
		}
	}
	if rawPlans, ok := root["inspection_plans"].([]any); ok {
		for _, item := range rawPlans {
			entry, _ := item.(map[string]any)
			if entry == nil {
				continue
			}
			plan := PlanProjection{Key: rootString(entry, "key"), DisplayName: rootString(entry, "display_name")}
			if rawCron, present := entry["cron"]; present {
				if text, isString := rawCron.(string); isString {
					expression := text
					plan.Cron = &expression
				}
			}
			if rawChecks, ok := entry["checks"].([]any); ok {
				for _, checkItem := range rawChecks {
					check, _ := checkItem.(map[string]any)
					if check == nil {
						continue
					}
					projection := CheckProjection{
						Key:              rootString(check, "key"),
						DisplayName:      rootString(check, "display_name"),
						AnalysisQuestion: rootString(check, "analysis_question"),
						Kind:             rootString(check, "kind"),
					}
					switch projection.Kind {
					case "promql":
						query, _ := check["query"].(map[string]any)
						projection.QueryMode = rootString(query, "mode")
						projection.Expression = rootString(query, "expression")
						projection.RangeSeconds = int64(rootInt(query, "range_seconds"))
						projection.StepSeconds = int64(rootInt(query, "step_seconds"))
					case "browser":
						projection.JourneyID = rootString(check, "journey_id")
						if params, ok := check["journey_params"].(map[string]any); ok {
							projection.JourneyParams = params
						} else {
							// Absent params normalize to the empty object
							// (frozen schema description, CFG-JOURNEY-003).
							projection.JourneyParams = map[string]any{}
						}
					}
					plan.Checks = append(plan.Checks, projection)
				}
			}
			document.Plans = append(document.Plans, plan)
		}
	}
	return document, nil
}

// ExtractLabelContract projects a schema-validated Label Contract value.
func ExtractLabelContract(value any) (LabelContractDocument, error) {
	root, ok := value.(map[string]any)
	if !ok {
		return LabelContractDocument{}, fmt.Errorf("文档根必须是映射")
	}
	contract, _ := root["label_contract"].(map[string]any)
	if contract == nil {
		return LabelContractDocument{}, fmt.Errorf("缺少 label_contract")
	}
	return LabelContractDocument{BusinessSystemLabel: rootString(contract, "business_system_label")}, nil
}

// SemanticChecks runs the schema-external validations on the canonical
// business-system value (CFG-YAML-003): timezone, cron and same-scope key
// uniqueness, plus the PromQL and Journey static checks. businessSystemLabel
// comes from the explicitly targeted Label Contract version
// (CFG-CONTRACT-003).
func SemanticChecks(document BusinessSystemDocument, businessSystemLabel string) []FieldError {
	var fields []FieldError
	if _, err := time.LoadLocation(document.Timezone); err != nil {
		fields = append(fields, FieldError{
			Path:        "timezone",
			Reason:      "时区必须是运行环境可解析的 IANA 名称（" + document.Timezone + " 无效）",
			Remediation: "使用如 Asia/Shanghai 的 IANA 时区名",
		})
	}
	discoveryKeys := map[string]bool{}
	for index, discovery := range document.Discoveries {
		path := fmt.Sprintf("resource_discoveries[%d]", index)
		if discoveryKeys[discovery.Key] {
			fields = append(fields, FieldError{Path: path + ".key", Reason: "discovery key 在同一配置内重复: " + discovery.Key, Remediation: "每个 discovery key 必须在本配置内唯一"})
		}
		discoveryKeys[discovery.Key] = true
		fields = append(fields, ValidateDiscoverySelector(discovery.Selector, businessSystemLabel, document.SystemKey, path+".selector")...)
		seenLabels := map[string]bool{}
		for labelIndex, label := range discovery.IdentityLabels {
			if seenLabels[label] {
				fields = append(fields, FieldError{Path: fmt.Sprintf("%s.identity_labels[%d]", path, labelIndex), Reason: "身份 label 重复: " + label, Remediation: "删除重复的身份 label"})
			}
			seenLabels[label] = true
		}
	}
	planKeys := map[string]bool{}
	for planIndex, plan := range document.Plans {
		path := fmt.Sprintf("inspection_plans[%d]", planIndex)
		if planKeys[plan.Key] {
			fields = append(fields, FieldError{Path: path + ".key", Reason: "plan key 在同一配置内重复: " + plan.Key, Remediation: "每个 plan key 必须在本配置内唯一"})
		}
		planKeys[plan.Key] = true
		if plan.Cron != nil {
			fields = append(fields, validateCron(*plan.Cron, path+".cron")...)
		}
		checkKeys := map[string]bool{}
		for checkIndex, check := range plan.Checks {
			checkPath := fmt.Sprintf("%s.checks[%d]", path, checkIndex)
			if checkKeys[check.Key] {
				fields = append(fields, FieldError{Path: checkPath + ".key", Reason: "check key 在所属 plan 内重复: " + check.Key, Remediation: "check key 只需在所属 plan 内唯一；重命名或删除重复项"})
			}
			checkKeys[check.Key] = true
			switch check.Kind {
			case "promql":
				fields = append(fields, ValidateCheckExpression(check.Expression, businessSystemLabel, document.SystemKey, checkPath+".query.expression")...)
			case "browser":
				fields = append(fields, ValidateJourneyReferenceVersion(check.JourneyID, 0, "journey", check.JourneyParams, checkPath)...)
			}
		}
	}
	return fields
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

func rootBool(root map[string]any, key string) bool {
	value, _ := root[key].(bool)
	return value
}

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
