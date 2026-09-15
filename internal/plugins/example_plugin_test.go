package plugins_test

// ExamplePlugin is the compile-able reference plugin for ADR-0004 (the
// runnable companion of docs/plugin-development.md). It demonstrates the
// complete contract surface — descriptor with closed config schema, model
// tool, inspection template, ToolExecutor and Collector bindings — and the
// registry rules a plugin must survive: descriptive capabilities are
// rejected inside an execution bundle, executable capabilities must exactly
// match the non-nil interfaces, and enablement resolution distinguishes an
// omitted whitelist from an explicit one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

// exampleExecutor implements both ToolExecutor and Collector: the collector
// reuses the tool operation, it is not a second execution engine.
type exampleExecutor struct{}

func (exampleExecutor) ExecuteTool(ctx context.Context, call *plugins.Call, request plugins.ToolRequest) (*plugins.ToolResult, error) {
	if request.Name != "example_rooms" {
		return nil, fmt.Errorf("executor bound to example_rooms, got %q", request.Name)
	}
	var args struct {
		Building string `json:"building"`
	}
	if err := json.Unmarshal(request.ArgumentsJSON, &args); err != nil || args.Building == "" {
		return &plugins.ToolResult{Success: false, ErrorCode: "invalid_arguments", ErrorDetail: "building 必须是非空字符串"}, nil
	}
	payload, _ := json.Marshal(map[string]any{"building": args.Building, "rooms": []string{"1a", "1b"}})
	return &plugins.ToolResult{Success: true, Payload: payload}, nil
}

func (exampleExecutor) Collect(ctx context.Context, call *plugins.Call, request plugins.CollectRequest) (*plugins.CollectResult, error) {
	checks := make([]plugins.CheckObservation, 0, len(request.Targets))
	for _, target := range request.Targets {
		evidence, _ := json.Marshal(map[string]any{
			"template": request.TemplateID, "templateVersion": request.TemplateVersion,
			"evidenceAt": request.EvidenceAt, "target": target.CanonicalIdentity,
		})
		checks = append(checks, plugins.CheckObservation{
			CheckID: "rooms_present", Succeeded: target.CanonicalIdentity != "", Detail: "observed", EvidenceJSON: evidence,
		})
	}
	return &plugins.CollectResult{Checks: checks}, nil
}

// exampleDescriptor assembles the full descriptor. The tool declaration and
// the inspection template are the frozen contracts the registry and the
// attempt catalog verify against the compiled implementation.
func exampleDescriptor() plugins.Descriptor {
	return plugins.Descriptor{
		ID:          "example",
		Version:     "1",
		DisplayName: "示例插件",
		Description: "演示完整契约面的参考插件：模型工具、巡检模板与采集绑定。",
		Capabilities: []plugins.Capability{
			plugins.CapabilityTools, plugins.CapabilityExecuteTool,
			plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect,
		},
		DefaultEnabled: true,
		ConnectionKind: "example",
		ConfigSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"site": map[string]any{"type": "string"},
			},
			"required": []any{"site"},
		},
		Tools: []plugins.Tool{{
			Name: "example_rooms", Version: "1",
			ExecutionLocation: plugins.LocationPlinthSupervisor, FailureMode: "return_to_model",
			Description: "列出一栋建筑的会议室。",
			Parameters: map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"building": map[string]any{"type": "string"}},
				"required":   []any{"building"},
			},
		}},
		InspectionTemplates: []plugins.InspectionTemplate{
			{ID: "rooms_snapshot", Version: "1", Title: "会议室快照", Description: "对每个目标执行一次会议室列表观察"},
		},
	}
}

func TestExamplePluginRegistersAndBinds(t *testing.T) {
	registry := plugins.NewRegistry()
	descriptor := exampleDescriptor()
	if err := registry.RegisterDescriptor(descriptor); err != nil {
		t.Fatalf("descriptor rejected: %v", err)
	}
	// The execution bundle lists ONLY the executable capabilities backed by
	// non-nil interfaces; the descriptive `tools`/`inspection_templates`
	// capabilities live on the descriptor and are rejected in a bundle.
	bundle := plugins.ExecutionBundle{
		PluginID:     descriptor.ID,
		Location:     plugins.LocationPlinthSupervisor,
		Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool, plugins.CapabilityCollect},
		ToolExecutor: exampleExecutor{},
		Collector:    exampleExecutor{},
	}
	if err := registry.RegisterBundle(bundle); err != nil {
		t.Fatalf("bundle rejected: %v", err)
	}

	// A descriptive capability inside a bundle is a deterministic wiring
	// failure — this is the rule the guide warns about.
	descriptive := bundle
	descriptive.Capabilities = []plugins.Capability{plugins.CapabilityTools, plugins.CapabilityExecuteTool, plugins.CapabilityCollect}
	if err := registry.RegisterBundle(descriptive); !errors.Is(err, plugins.ErrInvalidBundle) && !errors.Is(err, plugins.ErrDuplicateBundle) {
		t.Fatalf("descriptive capability bundle = %v, want a registration error", err)
	}

	// Enablement: omitted whitelist selects the default-enabled plugin; an
	// explicit empty whitelist disables everything.
	defaults, err := registry.ResolveEnabled(nil)
	if err != nil || len(defaults) != 1 || defaults[0] != "example" {
		t.Fatalf("ResolveEnabled(nil) = %v, want [example]", defaults)
	}
	if enabled, _ := registry.ResolveEnabled([]string{}); len(enabled) != 0 {
		t.Fatalf("ResolveEnabled([]) = %v, want no plugins", enabled)
	}

	// The executor runs against the frozen Call: settings travel as the
	// validated settings document, secret slots as opaque references the
	// host resolves per call.
	call := &plugins.Call{
		PluginID:   "example",
		Settings:   json.RawMessage(`{"site":"campus-a"}`),
		SecretRefs: map[string]string{"roomToken": "credential-generation-17"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := bundle.ToolExecutor.ExecuteTool(ctx, call, plugins.ToolRequest{
		Name: "example_rooms", ArgumentsJSON: json.RawMessage(`{"building":"b1"}`),
	})
	if err != nil || !result.Success {
		t.Fatalf("ExecuteTool = (%+v, %v), want success", result, err)
	}
}

func TestExamplePluginCollectPassIsDeterministic(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(exampleDescriptor()); err != nil {
		t.Fatal(err)
	}
	bundle := plugins.ExecutionBundle{
		PluginID: "example", Location: plugins.LocationPlinthSupervisor,
		Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool, plugins.CapabilityCollect},
		ToolExecutor: exampleExecutor{}, Collector: exampleExecutor{},
	}
	if err := registry.RegisterBundle(bundle); err != nil {
		t.Fatal(err)
	}
	result, err := bundle.Collector.Collect(context.Background(), &plugins.Call{PluginID: "example"}, plugins.CollectRequest{
		TemplateID: "rooms_snapshot", TemplateVersion: "1",
		Params:     json.RawMessage(`{"expression":"rooms","windowSeconds":0}`),
		EvidenceAt: "2026-09-13T00:00:00Z",
		Targets:    []plugins.CollectTarget{{ObjectType: "room", CanonicalIdentity: "b1/1a"}},
	})
	if err != nil || result.Incomplete || len(result.Checks) != 1 || !result.Checks[0].Succeeded {
		t.Fatalf("Collect = (%+v, %v), want one succeeded check", result, err)
	}
}

// Instance-settings validation: unknown fields, missing required fields and
// wrong types are rejected by the declared ConfigSchema; a valid builtin-
// shaped document passes; a schema-less plugin rejects non-empty settings.
func TestValidateConfigEnforcesDeclaredSchema(t *testing.T) {
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(exampleDescriptor()); err != nil {
		t.Fatal(err)
	}
	schemaless := plugins.Descriptor{ID: "bare", Version: "1", DisplayName: "Bare", Description: "no configuration"}
	if err := registry.RegisterDescriptor(schemaless); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		pluginID  string
		settings  string
		wantError string
	}{
		{"valid settings", "example", `{"site":"campus-a"}`, ""},
		{"unknown field rejected", "example", `{"site":"a","extra":1}`, "invalid plugin instance settings"},
		{"missing required rejected", "example", `{}`, "invalid plugin instance settings"},
		{"wrong type rejected", "example", `{"site":17}`, "invalid plugin instance settings"},
		{"schema-less accepts empty", "bare", `{}`, ""},
		{"schema-less rejects settings", "bare", `{"site":"x"}`, "invalid plugin instance settings"},
		{"unknown plugin rejected", "ghost", `{}`, "unknown plugin id"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := registry.ValidateConfig(testCase.pluginID, json.RawMessage(testCase.settings))
			switch {
			case testCase.wantError == "" && err != nil:
				t.Fatalf("ValidateConfig = %v, want nil", err)
			case testCase.wantError != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantError)):
				t.Fatalf("ValidateConfig = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

// The builtin metrics settings shape (type/baseUrl and the optional typed
// connection fields) passes its declared schema — the production
// newMetricsCall path validates exactly this document.
func TestValidateConfigAcceptsBuiltinMetricsSettings(t *testing.T) {
	registry := plugins.NewRegistry()
	for _, descriptor := range builtin.Descriptors() {
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	valid := `{"type":"prometheus","baseUrl":"https://prom.example.com","authType":"basic","username":"metrics"}`
	if err := registry.ValidateConfig("prometheus", json.RawMessage(valid)); err != nil {
		t.Fatalf("valid metrics settings rejected: %v", err)
	}
	wrongProvider := `{"type":"thanos","baseUrl":"https://prom.example.com"}`
	if err := registry.ValidateConfig("prometheus", json.RawMessage(wrongProvider)); err == nil {
		t.Fatal("prometheus accepted a thanos-typed settings document")
	}
}
