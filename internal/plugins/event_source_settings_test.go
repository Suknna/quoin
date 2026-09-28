package plugins_test

// EventSource instance-settings contract (ADR-0014 story 2): the closed
// EventSourceConfigSchema is a separate authority from ConfigSchema, is
// validated at registration (closure + secret-material ban) and is enforced
// against instances through ValidateEventSourceConfig.

import (
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

func sourceSettingsRegistry(t *testing.T, schema map[string]any) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	plugin := plugins.Plugin{
		ID: "settings-plugin", Version: "1", DefaultEnabled: true,
		EventSource: stubSource{kind: "settings-source"},
		EventTypes:  []string{"alerts.batch"},
	}
	if schema != nil {
		plugin.EventSourceConfigSchema = schema
	}
	if err := registry.Register(plugin); err != nil {
		t.Fatalf("register: %v", err)
	}
	return registry
}

func TestEventSourceConfigRequiresEventSource(t *testing.T) {
	registry := plugins.NewRegistry()
	err := registry.Register(plugins.Plugin{
		ID: "orphan", Version: "1",
		EventSourceConfigSchema: map[string]any{"type": "object", "additionalProperties": false},
	})
	if err == nil || !strings.Contains(err.Error(), "without an event source") {
		t.Fatalf("schema without event source accepted: %v", err)
	}
	_ = registry
}

func TestEventSourceConfigSchemaMustBeClosed(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"not an object":            {"type": "string"},
		"missing closure":          {"type": "object"},
		"closure not false":        {"type": "object", "additionalProperties": true},
		"nested secret property":   {"type": "object", "additionalProperties": false, "properties": map[string]any{"retries": map[string]any{"type": "integer"}, "nested": map[string]any{"type": "object", "properties": map[string]any{"password": map[string]any{"type": "string"}}}}},
		"top-level token property": {"type": "object", "additionalProperties": false, "properties": map[string]any{"token": map[string]any{"type": "string"}}},
		"api key variant":          {"type": "object", "additionalProperties": false, "properties": map[string]any{"api_key": map[string]any{"type": "string"}}},
	} {
		registry := plugins.NewRegistry()
		err := registry.Register(plugins.Plugin{
			ID: "broken", Version: "1",
			EventSource:             stubSource{kind: "broken-source"},
			EventTypes:              []string{"alerts.batch"},
			EventSourceConfigSchema: schema,
		})
		if err == nil {
			t.Fatalf("%s: unclosed or secret-carrying schema accepted", name)
		}
	}
}

func TestEventSourceConfigAllowsCompoundNames(t *testing.T) {
	registry := sourceSettingsRegistry(t, map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"pageToken":    map[string]any{"type": "string"},
			"passwordTTL":  map[string]any{"type": "integer"},
			"site":         map[string]any{"type": "string"},
			"ignoredNames": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	})
	if _, _, ok := registry.EventSourceSettings("settings-source"); !ok {
		t.Fatal("source kind did not resolve its settings schema")
	}
}

func TestValidateEventSourceConfig(t *testing.T) {
	registry := sourceSettingsRegistry(t, map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"site":  map[string]any{"type": "string", "maxLength": 100},
			"limit": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []any{"site"},
	})
	if err := registry.ValidateEventSourceConfig("settings-plugin", []byte(`{"site":"eu","limit":3}`)); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	for name, settings := range map[string]string{
		"unknown field":  `{"site":"eu","extra":1}`,
		"missing field":  `{"limit":1}`,
		"wrong type":     `{"site":7}`,
		"not an object":  `["site"]`,
		"not valid json": `{`,
	} {
		if err := registry.ValidateEventSourceConfig("settings-plugin", []byte(settings)); err == nil {
			t.Fatalf("%s: invalid settings accepted", name)
		}
	}
}

func TestValidateEventSourceConfigSchemalessPluginOnlyAcceptsEmpty(t *testing.T) {
	registry := sourceSettingsRegistry(t, nil)
	for _, empty := range []string{"", "null", "{}"} {
		raw := []byte(empty)
		if err := registry.ValidateEventSourceConfig("settings-plugin", raw); err != nil {
			t.Fatalf("empty settings %q rejected: %v", empty, err)
		}
	}
	if err := registry.ValidateEventSourceConfig("settings-plugin", nil); err != nil {
		t.Fatalf("absent settings rejected: %v", err)
	}
	if err := registry.ValidateEventSourceConfig("settings-plugin", []byte(`{"site":"eu"}`)); err == nil {
		t.Fatal("settings accepted for a schema-less source")
	}
	if err := registry.ValidateEventSourceConfig("unknown-plugin", []byte(`{}`)); err == nil {
		t.Fatal("unknown plugin accepted")
	}
}

func TestValidateEventSourceConfigBoundSize(t *testing.T) {
	registry := sourceSettingsRegistry(t, map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"blob": map[string]any{"type": "string"}},
	})
	oversize := `{"blob":"` + strings.Repeat("x", plugins.MaxEventSourceSettingsBytes) + `"}`
	if err := registry.ValidateEventSourceConfig("settings-plugin", []byte(oversize)); err == nil {
		t.Fatal("oversize settings accepted")
	}
}

func TestEventSourceSchemaJoinsInboundManifestFingerprint(t *testing.T) {
	plain := sourceSettingsRegistry(t, nil).InboundManifestFingerprint()
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"site": map[string]any{"type": "string"}},
	}
	withSchema := sourceSettingsRegistry(t, schema).InboundManifestFingerprint()
	// Canonical JSON key ordering must make the digest deterministic across
	// equal declarations.
	sameSchema := sourceSettingsRegistry(t, map[string]any{
		"additionalProperties": false, "type": "object",
		"properties": map[string]any{"site": map[string]any{"type": "string"}},
	}).InboundManifestFingerprint()
	if plain == withSchema {
		t.Fatal("schema drift did not change the manifest fingerprint")
	}
	if withSchema != sameSchema {
		t.Fatal("equal declarations with different key order produced different fingerprints")
	}
}
