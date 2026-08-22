package config

// CFG-VALIDATION-002: the strict single-document YAML lexical rules verified
// against real yaml.v3 Node behavior — every rejection class plus the legal
// single document.

import (
	"strings"
	"testing"
)

func strictParse(t *testing.T, body string) (any, []FieldError) {
	t.Helper()
	return ParseStrictYAML([]byte(body), Limits{}, "document")
}

func TestStrictYAMLAcceptsLegalSingleDocument(t *testing.T) {
	value, failures := strictParse(t, "system_key: payments\nenabled: true\ncount: 300\nratio: 1.5\nitems:\n  - one\n  - two\n")
	if len(failures) != 0 {
		t.Fatalf("legal document rejected: %v", failures)
	}
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("root must be a map, got %T", value)
	}
	if root["system_key"] != "payments" || root["enabled"] != true || root["count"] != int64(300) || root["ratio"] != 1.5 {
		t.Fatalf("canonical conversion wrong: %#v", root)
	}
	if items, ok := root["items"].([]any); !ok || len(items) != 2 || items[1] != "two" {
		t.Fatalf("sequence conversion wrong: %#v", root["items"])
	}
}

func TestStrictYAMLRejections(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{"second document", "a: 1\n---\nb: 2\n", "第二个"},
		{"trailing content after document", "a: 1\n---\n@@@\n", ""},
		{"duplicate key", "a: 1\nb: 2\na: 3\n", "重复"},
		{"nested duplicate key", "root:\n  x: 1\n  x: 2\n", "重复"},
		{"anchor definition", "base: &b\n  x: 1\n", "锚点"},
		{"anchor and alias use", "base: &b\n  x: 1\ncopy: *b\n", "锚点"},
		{"merge key without anchor", "item:\n  <<:\n    x: 1\n  y: 2\n", "merge"},
		{"custom tag", "value: !custom thing\n", "tag"},
		{"non-string key int", "1: one\n", "字符串"},
		{"non-string key bool", "true: yes\n", "字符串"},
		{"non-string key null", "~: value\n", "字符串"},
		{"empty document", "", "为空"},
		{"comment-only document", "# nothing here\n", "为空"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, failures := strictParse(t, item.body)
			if len(failures) == 0 {
				t.Fatalf("must be rejected")
			}
			// A trailing-garbage body surfaces as either the explicit
			// trailing-content rejection or the first-decode syntax failure;
			// both are deterministic rejections of the same input class.
			if item.reason != "" && !strings.Contains(failures[0].Reason, item.reason) {
				t.Fatalf("reason %q should contain %q", failures[0].Reason, item.reason)
			}
		})
	}
}

func TestStrictYAMLQuotedNumericKeyIsString(t *testing.T) {
	value, failures := strictParse(t, "\"1\": one\n")
	if len(failures) != 0 {
		t.Fatalf("quoted numeric key is a string and must pass: %v", failures)
	}
	if _, ok := value.(map[string]any)["1"]; !ok {
		t.Fatalf("quoted key missing: %#v", value)
	}
}

func TestStrictYAMLLimits(t *testing.T) {
	oversize := strings.Repeat("a", 64)
	_, failures := ParseStrictYAML([]byte("k: "+oversize), Limits{MaxDocumentBytes: 8}, "document")
	if len(failures) == 0 || !strings.Contains(failures[0].Reason, "大小上限") {
		t.Fatalf("byte limit must reject: %v", failures)
	}
	deep := &strings.Builder{}
	for i := 0; i < 8; i++ {
		deep.WriteString(strings.Repeat("  ", i) + "nested:\n")
	}
	deep.WriteString(strings.Repeat("  ", 8) + "leaf: 1\n")
	_, failures = ParseStrictYAML([]byte(deep.String()), Limits{MaxDepth: 4}, "document")
	if len(failures) == 0 || !strings.Contains(failures[0].Reason, "嵌套深度") {
		t.Fatalf("depth limit must reject: %v", failures)
	}
	// Node limit: a wide document with a tiny node budget.
	var wide strings.Builder
	wide.WriteString("root:\n")
	for i := 0; i < 40; i++ {
		wide.WriteString("  - item\n")
	}
	_, failures = ParseStrictYAML([]byte(wide.String()), Limits{MaxNodes: 5}, "document")
	if len(failures) == 0 || !strings.Contains(failures[0].Reason, "节点数") {
		t.Fatalf("node limit must reject: %v", failures)
	}
}

func TestStrictYAMLInvalidUTF8(t *testing.T) {
	_, failures := ParseStrictYAML([]byte{0x6b, 0x3a, 0x20, 0xff, 0xfe, 0x0a}, Limits{}, "document")
	if len(failures) == 0 {
		t.Fatal("invalid UTF-8 must be rejected")
	}
}

func TestStrictYAMLBooleanAndIntForms(t *testing.T) {
	value, failures := strictParse(t, "yes: true\nbig: 0x10\nunderscored: 1_000\n")
	if len(failures) != 0 {
		t.Fatalf("parsable forms rejected: %v", failures)
	}
	root := value.(map[string]any)
	if root["yes"] != true || root["big"] != int64(16) || root["underscored"] != int64(1000) {
		t.Fatalf("value conversion wrong: %#v", root)
	}
}
