package config

// CFG-VALIDATION-001 (JSON Schema fixtures through the real draft 2020-12
// validator) and CFG-VALIDATION-003 (official PromQL AST rules) plus the
// cron/timezone/uniqueness semantics of CFG-YAML-003 and the embedded
// Journey Catalog static validation.

import (
	"strings"
	"testing"
)

const validSystemYAML = `system_key: payments
display_name: 支付系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="payments", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily-check
    display_name: Daily Check
    cron: "30 8 * * *"
    checks:
      - key: up-ratio
        display_name: Up Ratio
        analysis_question: 当前可用比例是否正常？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="payments"}'
      - key: latency-window
        display_name: Latency Window
        analysis_question: 时延趋势如何？
        kind: promql
        query:
          mode: range
          expression: 'rate(http_requests_total{business_system="payments"}[5m])'
          range_seconds: 3600
          step_seconds: 60
`

func parseValidSystem(t *testing.T, body string) BusinessSystemDocument {
	t.Helper()
	value, failures := ParseStrictYAML([]byte(body), Limits{}, "document")
	if len(failures) != 0 {
		t.Fatalf("strict parse failed: %v", failures)
	}
	if fields := ValidateSchema(value, SchemaBusinessSystemConfig); len(fields) != 0 {
		t.Fatalf("schema validation failed: %v", fields)
	}
	document, err := ExtractBusinessSystem(value)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	return document
}

func TestSchemaAcceptsValidFixture(t *testing.T) {
	document := parseValidSystem(t, validSystemYAML)
	if document.SystemKey != "payments" || document.Enabled || document.Timezone != "Asia/Shanghai" {
		t.Fatalf("extraction wrong: %#v", document)
	}
	if len(document.Discoveries) != 1 || len(document.Plans) != 1 || len(document.Plans[0].Checks) != 2 {
		t.Fatalf("projection wrong: %#v", document)
	}
	if document.Plans[0].Checks[1].QueryMode != "range" || document.Plans[0].Checks[1].RangeSeconds != 3600 {
		t.Fatalf("range check wrong: %#v", document.Plans[0].Checks[1])
	}
}

func TestSchemaRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		path string
	}{
		{"unknown root field", validSystemYAML + "extra_field: 1\n", "extra_field"},
		{"unknown discovery field", strings.Replace(validSystemYAML, "identity_labels: [job, instance]", "identity_labels: [job, instance]\n    bogus: 1", 1), "bogus"},
		{"cross-kind promql field on browser check", strings.Replace(validSystemYAML, "        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system=\"payments\"}'", "        kind: browser\n        journey_id: j1\n        query:\n          mode: instant\n          expression: 'up{business_system=\"payments\"}'", 1), "query"},
		{"instant with range fields", strings.Replace(validSystemYAML, "          mode: instant\n          expression: 'up{business_system=\"payments\"}'", "          mode: instant\n          range_seconds: 60\n          step_seconds: 30\n          expression: 'up{business_system=\"payments\"}'", 1), "range_seconds"},
		{"range missing step", strings.Replace(validSystemYAML, "          range_seconds: 3600\n          step_seconds: 60", "          range_seconds: 3600", 1), "step_seconds"},
		{"missing root field", strings.Replace(validSystemYAML, "enabled: false\n", "", 1), "missing"},
		{"invalid label name", strings.Replace(validSystemYAML, "identity_labels: [job, instance]", "identity_labels: [\"1bad\", instance]", 1), "identity_labels"},
		{"negative refresh interval", strings.Replace(validSystemYAML, "resource_refresh_interval_seconds: 300", "resource_refresh_interval_seconds: 0", 1), "resource_refresh_interval_seconds"},
		// Deep-equal duplicate array entries (same identity label twice) are
		// rejected by uniqueItems; same-key-but-different-entry is the
		// semantic layer's job (CFG-YAML-003) and covered below.
		{"duplicate identity label entries", strings.Replace(validSystemYAML, "identity_labels: [job, instance]", "identity_labels: [job, job]", 1), "identity_labels"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			value, failures := ParseStrictYAML([]byte(item.body), Limits{}, "document")
			if len(failures) != 0 {
				t.Fatalf("strict parse failed: %v", failures)
			}
			fields := ValidateSchema(value, SchemaBusinessSystemConfig)
			if len(fields) == 0 {
				t.Fatal("schema must reject")
			}
			found := false
			for _, field := range fields {
				if strings.Contains(field.Path, item.path) || strings.Contains(field.Reason, item.path) {
					found = true
				}
			}
			if !found {
				t.Fatalf("no error pointing at %s: %v", item.path, fields)
			}
		})
	}
}

func TestSemanticChecksPassOnValidFixture(t *testing.T) {
	document := parseValidSystem(t, validSystemYAML)
	if fields := SemanticChecks(document, "business_system"); len(fields) != 0 {
		t.Fatalf("valid fixture must pass semantics: %v", fields)
	}
}

func TestSemanticRejections(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		path   string
		reason string
	}{
		{"bad timezone", strings.Replace(validSystemYAML, "timezone: Asia/Shanghai", "timezone: Not/AZone", 1), "timezone", "IANA"},
		{"cron six fields", strings.Replace(validSystemYAML, "cron: \"30 8 * * *\"", "cron: \"0 30 8 * * *\"", 1), "cron", "五个"},
		{"cron descriptor", strings.Replace(validSystemYAML, "cron: \"30 8 * * *\"", "cron: \"@daily\"", 1), "cron", "descriptor"},
		{"cron with tz", strings.Replace(validSystemYAML, "cron: \"30 8 * * *\"", "cron: \"CRON_TZ=Asia/Shanghai 30 8 * * *\"", 1), "cron", "CRON_TZ"},
		{"cron out of range", strings.Replace(validSystemYAML, "cron: \"30 8 * * *\"", "cron: \"99 8 * * *\"", 1), "cron", "解析失败"},
		{"duplicate discovery key", strings.Replace(validSystemYAML, "    identity_labels: [job, instance]", "    identity_labels: [job, instance]\n  - key: web-pods\n    display_name: Dup\n    selector: 'up{business_system=\"payments\"}'\n    identity_labels: [job]", 1), "resource_discoveries[1].key", "重复"},
		{"duplicate check key in plan", strings.Replace(validSystemYAML, "      - key: latency-window", "      - key: up-ratio", 1), "checks[1].key", "重复"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			value, failures := ParseStrictYAML([]byte(item.body), Limits{}, "document")
			if len(failures) != 0 {
				t.Fatalf("strict parse failed: %v", failures)
			}
			if fields := ValidateSchema(value, SchemaBusinessSystemConfig); len(fields) != 0 {
				t.Fatalf("schema must accept this shape (semantics under test): %v", fields)
			}
			document, err := ExtractBusinessSystem(value)
			if err != nil {
				t.Fatal(err)
			}
			fields := SemanticChecks(document, "business_system")
			if len(fields) == 0 {
				t.Fatal("semantics must reject")
			}
			if !strings.Contains(fields[0].Path, strings.Split(item.path, ".")[0]) || !containsAny(fields, item.reason) {
				t.Fatalf("expected %s/%s, got %v", item.path, item.reason, fields)
			}
		})
	}
}

func containsAny(fields []FieldError, reason string) bool {
	for _, field := range fields {
		if strings.Contains(field.Reason, reason) {
			return true
		}
	}
	return false
}

func TestPromQLOwnershipMatrix(t *testing.T) {
	label, key := "business_system", "payments"
	cases := []struct {
		name       string
		expression string
		valid      bool
	}{
		{"exact match", `up{business_system="payments"}`, true},
		{"other labels allowed", `up{business_system="payments", job="web"}`, true},
		{"offset allowed on checks", `up{business_system="payments"} offset 5m`, true},
		{"at modifier allowed on checks", `up{business_system="payments"} @ 100`, true},
		{"subquery allowed on checks", `rate(up{business_system="payments"}[5m:1m])`, true},
		{"aggregation allowed on checks", `sum(up{business_system="payments"})`, true},
		{"label_replace allowed on checks", `label_replace(up{business_system="payments"}, "x", "$1", "job", "(.*)")`, true},
		{"missing ownership", `up{job="web"}`, false},
		{"negated ownership", `up{business_system!="payments"}`, false},
		{"regex ownership", `up{business_system=~"pay.*"}`, false},
		{"wrong value", `up{business_system="billing"}`, false},
		{"second selector missing ownership", `up{business_system="payments"} or up{job="x"}`, false},
		{"syntax error", `up{business_system=`, false},
		{"no selector at all", `1 + 1`, true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			fields := ValidateCheckExpression(item.expression, label, key, "expr")
			if item.valid && len(fields) != 0 {
				t.Fatalf("must pass: %v", fields)
			}
			if !item.valid && len(fields) == 0 {
				t.Fatal("must fail")
			}
		})
	}
}

func TestDiscoverySelectorMatrix(t *testing.T) {
	label, key := "business_system", "payments"
	valid := []string{
		`up{business_system="payments"}`,
		`node_cpu_seconds_total{business_system="payments", mode="idle"}`,
	}
	for _, selector := range valid {
		if fields := ValidateDiscoverySelector(selector, label, key, "sel"); len(fields) != 0 {
			t.Fatalf("selector %q must pass: %v", selector, fields)
		}
	}
	invalid := []struct {
		selector string
		reason   string
	}{
		{`up{business_system="payments"} offset 5m`, "offset"},
		{`up{business_system="payments"} @ 100`, "@"},
		{`sum(up{business_system="payments"})`, "聚合"},
		{`label_replace(up{business_system="payments"}, "x", "$1", "job", "(.*)")`, "聚合"},
		{`up{business_system="payments"}[5m:1m]`, "聚合"},
		{`up{job="web"}`, "归属"},
	}
	for _, item := range invalid {
		fields := ValidateDiscoverySelector(item.selector, label, key, "sel")
		if len(fields) == 0 {
			t.Fatalf("selector %q must fail", item.selector)
		}
		if !strings.Contains(fields[0].Reason, item.reason) {
			t.Fatalf("selector %q reason %q should contain %q", item.selector, fields[0].Reason, item.reason)
		}
	}
}

func TestJourneyCatalogValidation(t *testing.T) {
	document, version, digest, err := JourneyCatalog()
	if err != nil || document == nil || version == "" || len(digest) != 64 {
		t.Fatalf("catalog unavailable: %v", err)
	}
	if _, exists := document["journeys"].(map[string]any)["anything"]; exists {
		t.Fatal("empty catalog must have no journeys")
	}
	fields := ValidateJourneyReference("login-journey", map[string]any{}, "checks[0]")
	if len(fields) == 0 || !strings.Contains(fields[0].Reason, "不在嵌入 Journey Catalog") {
		t.Fatalf("unknown journey must be rejected: %v", fields)
	}
	if fields := ValidateJourneyReference("login-journey", nil, "checks[0]"); len(fields) == 0 {
		t.Fatal("any journey reference in the empty catalog must be rejected")
	}
}

func TestBrowserCheckFailsAgainstEmptyCatalog(t *testing.T) {
	body := strings.Replace(validSystemYAML, "        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system=\"payments\"}'", "        kind: browser\n        journey_id: login-journey\n        journey_params:\n          username: op", 1)
	value, failures := ParseStrictYAML([]byte(body), Limits{}, "document")
	if len(failures) != 0 {
		t.Fatalf("strict parse failed: %v", failures)
	}
	if fields := ValidateSchema(value, SchemaBusinessSystemConfig); len(fields) != 0 {
		t.Fatalf("schema must accept a well-formed browser check: %v", fields)
	}
	document, _ := ExtractBusinessSystem(value)
	fields := SemanticChecks(document, "business_system")
	if len(fields) == 0 || !strings.Contains(fields[0].Reason, "Journey Catalog") {
		t.Fatalf("browser check must fail the embedded catalog lookup: %v", fields)
	}
}

func TestLabelContractSchema(t *testing.T) {
	value, failures := ParseStrictYAML([]byte("label_contract:\n  business_system_label: business_system\n"), Limits{}, "document")
	if len(failures) != 0 {
		t.Fatalf("strict parse failed: %v", failures)
	}
	if fields := ValidateSchema(value, SchemaLabelContract); len(fields) != 0 {
		t.Fatalf("valid contract rejected: %v", fields)
	}
	document, err := ExtractLabelContract(value)
	if err != nil || document.BusinessSystemLabel != "business_system" {
		t.Fatalf("extraction wrong: %v %v", document, err)
	}
	if document.Digest() != document.Digest() || len(document.Digest()) != 64 {
		t.Fatal("digest must be stable 64-hex")
	}

	value, failures = ParseStrictYAML([]byte("label_contract:\n  business_system_label: business_system\n  extra: 1\n"), Limits{}, "document")
	if len(failures) != 0 {
		t.Fatalf("strict parse failed: %v", failures)
	}
	if fields := ValidateSchema(value, SchemaLabelContract); len(fields) == 0 {
		t.Fatal("unknown contract field must be rejected")
	}
	value, failures = ParseStrictYAML([]byte("label_contract:\n  business_system_label: \"1bad\"\n"), Limits{}, "document")
	if len(failures) != 0 {
		t.Fatalf("strict parse failed: %v", failures)
	}
	if fields := ValidateSchema(value, SchemaLabelContract); len(fields) == 0 {
		t.Fatal("invalid label name must be rejected")
	}
}

func TestDocumentDigestCoversSemantics(t *testing.T) {
	base := parseValidSystem(t, validSystemYAML)
	// Same semantics, different formatting: digest unchanged.
	reformatted := strings.Replace(validSystemYAML, "display_name: 支付系统", `display_name: "支付系统"`, 1)
	same := parseValidSystem(t, reformatted)
	if base.Digest() != same.Digest() {
		t.Fatal("formatting-only change must not alter the digest")
	}
	changed := parseValidSystem(t, strings.Replace(validSystemYAML, "resource_refresh_interval_seconds: 300", "resource_refresh_interval_seconds: 301", 1))
	if base.Digest() == changed.Digest() {
		t.Fatal("semantic change must alter the digest")
	}
}
