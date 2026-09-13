package config

// Legacy schema fixtures remain covered by their archived migration tests. These
// tests retain the reusable strict PromQL and Journey Catalog boundaries, which
// are independent of the retired BusinessSystemConfig and LabelContract parsers.

import (
	"strings"
	"testing"
)

func containsAny(fields []FieldError, text string) bool {
	for _, field := range fields {
		if strings.Contains(field.Reason, text) {
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
	for _, selector := range []string{
		`up{business_system="payments"}`,
		`node_cpu_seconds_total{business_system="payments", mode="idle"}`,
	} {
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

// The public declaration parser is intentionally the only normal YAML path.
// Retired names must not cause their archived schemas to be compiled on demand.
func TestValidateSchemaRejectsRetiredSchemaNames(t *testing.T) {
	for _, name := range []string{"business-system-config", "label-contract"} {
		fields := ValidateSchema(map[string]any{}, name)
		if len(fields) != 1 || !strings.Contains(fields[0].Reason, "未知配置 Schema") {
			t.Fatalf("retired schema %q was unexpectedly available: %#v", name, fields)
		}
	}
}
