// 外部测试包：只允许依赖 internal/plugins 的公开边界（注册/目录公共边界）。
package plugins_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

func TestRegisterDescriptor(t *testing.T) {
	registry := plugins.NewRegistry()
	descriptor := plugins.Descriptor{
		ID:          "prometheus",
		Version:     "1",
		DisplayName: "Prometheus",
		Description: "metrics source connection",
	}
	if err := registry.RegisterDescriptor(descriptor); err != nil {
		t.Fatalf("RegisterDescriptor(prometheus) = %v, want nil", err)
	}
	got, ok := registry.Descriptor("prometheus")
	if !ok {
		t.Fatal("Descriptor(prometheus) not found after registration")
	}
	if !reflect.DeepEqual(got, descriptor) {
		t.Fatalf("Descriptor(prometheus) = %+v, want %+v", got, descriptor)
	}
	if _, ok := registry.Descriptor("thanos"); ok {
		t.Fatal("Descriptor(thanos) found before registration")
	}
}

func TestDescriptorsReturnsStableIDOrder(t *testing.T) {
	registry := plugins.NewRegistry()
	for _, id := range []string{"thanos", "alertmanager", "kubernetes", "prometheus"} {
		descriptor := plugins.Descriptor{ID: id, Version: "1", DisplayName: id, Description: "test descriptor"}
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			t.Fatalf("RegisterDescriptor(%s) = %v, want nil", id, err)
		}
	}
	list := registry.Descriptors()
	if len(list) != 4 {
		t.Fatalf("len(Descriptors()) = %d, want 4", len(list))
	}
	for i, want := range []string{"alertmanager", "kubernetes", "prometheus", "thanos"} {
		if got := list[i].ID; got != want {
			t.Fatalf("Descriptors()[%d].ID = %q, want %q", i, got, want)
		}
	}
}

// registrationError runs want on every rejected registration.
func registrationError(t *testing.T, descriptor plugins.Descriptor, want error) {
	t.Helper()
	registry := plugins.NewRegistry()
	err := registry.RegisterDescriptor(descriptor)
	if err == nil {
		t.Fatalf("RegisterDescriptor(%s) accepted invalid descriptor", descriptor.ID)
	}
	if !errors.Is(err, want) {
		t.Fatalf("RegisterDescriptor(%s) = %v, want %v", descriptor.ID, err, want)
	}
}

func TestRegisterDescriptorRejectsInvalidDescriptors(t *testing.T) {
	valid := plugins.Descriptor{ID: "prometheus", Version: "1", DisplayName: "Prometheus", Description: "metrics source"}
	cases := map[string]struct {
		mutate func(*plugins.Descriptor)
		want   error
	}{
		"empty-id":          {func(d *plugins.Descriptor) { d.ID = "" }, plugins.ErrInvalidDescriptor},
		"upper-id":          {func(d *plugins.Descriptor) { d.ID = "Prometheus" }, plugins.ErrInvalidDescriptor},
		"empty-version":     {func(d *plugins.Descriptor) { d.Version = "" }, plugins.ErrInvalidDescriptor},
		"empty-display":     {func(d *plugins.Descriptor) { d.DisplayName = "" }, plugins.ErrInvalidDescriptor},
		"empty-description": {func(d *plugins.Descriptor) { d.Description = "" }, plugins.ErrInvalidDescriptor},
		"unknown-capability": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{"magic"}
		}, plugins.ErrInvalidDescriptor},
		"duplicate-capability": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityProbe, plugins.CapabilityProbe}
		}, plugins.ErrInvalidDescriptor},
		"tools-capability-without-tools": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityTools}
		}, plugins.ErrInvalidDescriptor},
		"tools-without-capability": {func(d *plugins.Descriptor) {
			d.Tools = []plugins.Tool{{Name: "p_query", Version: "1", ExecutionLocation: plugins.LocationPlinthSupervisor, FailureMode: "return_to_model", Description: "query"}}
		}, plugins.ErrInvalidDescriptor},
		"templates-capability-without-templates": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityInspectionTemplates}
		}, plugins.ErrInvalidDescriptor},
		"execute-tool-without-tools": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityExecuteTool}
		}, plugins.ErrInvalidDescriptor},
		"collect-without-templates": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityCollect}
		}, plugins.ErrInvalidDescriptor},
		"schema-not-object": {func(d *plugins.Descriptor) {
			d.ConfigSchema = map[string]any{"type": "string"}
		}, plugins.ErrInvalidDescriptor},
		"schema-open-world": {func(d *plugins.Descriptor) {
			d.ConfigSchema = map[string]any{"type": "object"}
		}, plugins.ErrInvalidDescriptor},
		"schema-wrong-draft": {func(d *plugins.Descriptor) {
			d.ConfigSchema = map[string]any{"type": "object", "additionalProperties": false, "$schema": "http://json-schema.org/draft-07/schema#"}
		}, plugins.ErrInvalidDescriptor},
		"tool-unknown-location": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityTools}
			d.Tools = []plugins.Tool{{Name: "p_query", Version: "1", ExecutionLocation: "browser_local", FailureMode: "return_to_model", Description: "query"}}
		}, plugins.ErrInvalidDescriptor},
		"tool-unknown-failure-mode": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityTools}
			d.Tools = []plugins.Tool{{Name: "p_query", Version: "1", ExecutionLocation: plugins.LocationQuoin, FailureMode: "crash", Description: "query"}}
		}, plugins.ErrInvalidDescriptor},
		"tool-bad-name": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityTools}
			d.Tools = []plugins.Tool{{Name: "P-Query", Version: "1", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "query"}}
		}, plugins.ErrInvalidDescriptor},
		"duplicate-tool-in-descriptor": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityTools}
			d.Tools = []plugins.Tool{
				{Name: "p_query", Version: "1", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "query"},
				{Name: "p_query", Version: "2", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "query"},
			}
		}, plugins.ErrInvalidDescriptor},
		"duplicate-template": {func(d *plugins.Descriptor) {
			d.Capabilities = []plugins.Capability{plugins.CapabilityInspectionTemplates}
			d.InspectionTemplates = []plugins.InspectionTemplate{
				{ID: "tls", Version: "1", Title: "TLS", Description: "checks"},
				{ID: "tls", Version: "2", Title: "TLS", Description: "checks"},
			}
		}, plugins.ErrInvalidDescriptor},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			descriptor := valid
			testCase.mutate(&descriptor)
			registrationError(t, descriptor, testCase.want)
		})
	}
}

func TestRegisterDescriptorAcceptsConsistentToolAndTemplateDeclarations(t *testing.T) {
	registry := plugins.NewRegistry()
	descriptor := plugins.Descriptor{
		ID:             "thanos",
		Version:        "1",
		DisplayName:    "Thanos",
		Description:    "global query",
		Capabilities:   []plugins.Capability{plugins.CapabilityProbe, plugins.CapabilityDiscover, plugins.CapabilityTools, plugins.CapabilityExecuteTool, plugins.CapabilityInspectionTemplates, plugins.CapabilityCollect},
		ConnectionKind: "thanos",
		ConfigSchema:   map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"region": map[string]any{"type": "string"}}, "required": []any{"region"}},
		DiscoverObjects: []plugins.DiscoverObject{
			{ObjectType: "target", IdentityLabels: []string{"job", "instance"}, Query: "up", Limit: 500},
		},
		Tools: []plugins.Tool{
			{Name: "p_query", Version: "1", ExecutionLocation: plugins.LocationPlinthSupervisor, FailureMode: "return_to_model", Description: "query"},
		},
		InspectionTemplates: []plugins.InspectionTemplate{
			{ID: "tls-expiry", Version: "1", Title: "TLS expiry", Description: "deterministic checks"},
		},
	}
	if err := registry.RegisterDescriptor(descriptor); err != nil {
		t.Fatalf("RegisterDescriptor = %v, want nil", err)
	}
	owner, ok := registry.ToolOwner("p_query")
	if !ok || owner != "thanos" {
		t.Fatalf("ToolOwner(p_query) = (%s, %t), want (thanos, true)", owner, ok)
	}
}

func TestRegisterDescriptorRejectsDuplicateIDAndToolName(t *testing.T) {
	registry := plugins.NewRegistry()
	first := plugins.Descriptor{ID: "a", Version: "1", DisplayName: "A", Description: "a",
		Capabilities: []plugins.Capability{plugins.CapabilityTools},
		Tools:        []plugins.Tool{{Name: "shared_tool", Version: "1", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "x"}},
	}
	if err := registry.RegisterDescriptor(first); err != nil {
		t.Fatal(err)
	}
	duplicateID := plugins.Descriptor{ID: "a", Version: "2", DisplayName: "A2", Description: "a2"}
	if err := registry.RegisterDescriptor(duplicateID); !errors.Is(err, plugins.ErrDuplicateDescriptor) {
		t.Fatalf("duplicate id = %v, want ErrDuplicateDescriptor", err)
	}
	// The SAME contract re-declared by another provider plugin is accepted:
	// shared tools (e.g. PromQL query over prometheus/thanos) keep one name,
	// one canonical declaration; authorization resolves the actual source.
	second := plugins.Descriptor{ID: "b", Version: "1", DisplayName: "B", Description: "b",
		Capabilities: []plugins.Capability{plugins.CapabilityTools},
		Tools:        []plugins.Tool{{Name: "shared_tool", Version: "1", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "x"}},
	}
	if err := registry.RegisterDescriptor(second); err != nil {
		t.Fatalf("identical re-declaration = %v, want nil", err)
	}
	owner, ok := registry.ToolOwner("shared_tool")
	if !ok || owner != "a" {
		t.Fatalf("ToolOwner(shared_tool) = (%s, %t), want canonical owner a", owner, ok)
	}
	// A DIVERGENT re-declaration is still rejected.
	divergent := plugins.Descriptor{ID: "c", Version: "1", DisplayName: "C", Description: "c",
		Capabilities: []plugins.Capability{plugins.CapabilityTools},
		Tools:        []plugins.Tool{{Name: "shared_tool", Version: "2", ExecutionLocation: plugins.LocationQuoin, FailureMode: "return_to_model", Description: "x"}},
	}
	if err := registry.RegisterDescriptor(divergent); !errors.Is(err, plugins.ErrDuplicateToolName) {
		t.Fatalf("divergent re-declaration = %v, want ErrDuplicateToolName", err)
	}
}

func TestRegisterBundleEnforcesDeclarationAgreement(t *testing.T) {
	descriptor := plugins.Descriptor{
		ID: "p", Version: "1", DisplayName: "P", Description: "p",
		Capabilities:    []plugins.Capability{plugins.CapabilityProbe, plugins.CapabilityDiscover},
		ConnectionKind:  "prometheus",
		DiscoverObjects: []plugins.DiscoverObject{{ObjectType: "target", IdentityLabels: []string{"job"}, Query: "up", Limit: 10}},
	}
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	// Bundle without a registered descriptor is rejected.
	err := registry.RegisterBundle(plugins.ExecutionBundle{PluginID: "ghost", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityProbe}, Prober: fakeProber{}})
	if !errors.Is(err, plugins.ErrUndeclaredBundle) {
		t.Fatalf("ghost bundle = %v, want ErrUndeclaredBundle", err)
	}
	// Capability listed without its implementation is rejected.
	err = registry.RegisterBundle(plugins.ExecutionBundle{PluginID: "p", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityProbe, plugins.CapabilityDiscover}})
	if !errors.Is(err, plugins.ErrInvalidBundle) {
		t.Fatalf("unbacked capability = %v, want ErrInvalidBundle", err)
	}
	// Implemented but unlisted capability is rejected.
	err = registry.RegisterBundle(plugins.ExecutionBundle{PluginID: "p", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityProbe}, Prober: fakeProber{}, Discoverer: fakeDiscoverer{}})
	if !errors.Is(err, plugins.ErrInvalidBundle) {
		t.Fatalf("unlisted implementation = %v, want ErrInvalidBundle", err)
	}
	// Capability not declared by the descriptor is rejected.
	err = registry.RegisterBundle(plugins.ExecutionBundle{PluginID: "p", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool}, ToolExecutor: fakeExecutor{}})
	if !errors.Is(err, plugins.ErrUndeclaredBundle) {
		t.Fatalf("undeclared capability = %v, want ErrUndeclaredBundle", err)
	}
	bundle := plugins.ExecutionBundle{PluginID: "p", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityProbe, plugins.CapabilityDiscover}, Prober: fakeProber{}, Discoverer: fakeDiscoverer{}}
	if err := registry.RegisterBundle(bundle); err != nil {
		t.Fatalf("RegisterBundle = %v, want nil", err)
	}
	// A second bundle for the same plugin is rejected.
	err = registry.RegisterBundle(bundle)
	if !errors.Is(err, plugins.ErrDuplicateBundle) {
		t.Fatalf("duplicate bundle = %v, want ErrDuplicateBundle", err)
	}
	got, ok := registry.Bundle("p")
	if !ok || !reflect.DeepEqual(got, bundle) {
		t.Fatalf("Bundle(p) = (%+v, %t), want the registered bundle", got, ok)
	}
	if _, ok := registry.Bundle("thanos"); ok {
		t.Fatal("Bundle(thanos) found without registration")
	}
}

func TestRegisterBundleToolLocationMustMatch(t *testing.T) {
	descriptor := plugins.Descriptor{
		ID: "p", Version: "1", DisplayName: "P", Description: "p",
		Capabilities: []plugins.Capability{plugins.CapabilityTools, plugins.CapabilityExecuteTool},
		Tools:        []plugins.Tool{{Name: "p_tool", Version: "1", ExecutionLocation: plugins.LocationWorkerLocal, FailureMode: "return_to_model", Description: "t"}},
	}
	registry := plugins.NewRegistry()
	if err := registry.RegisterDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	err := registry.RegisterBundle(plugins.ExecutionBundle{PluginID: "p", Location: plugins.LocationQuoin, Capabilities: []plugins.Capability{plugins.CapabilityExecuteTool}, ToolExecutor: fakeExecutor{}})
	if !errors.Is(err, plugins.ErrInvalidBundle) {
		t.Fatalf("mismatched tool location = %v, want ErrInvalidBundle", err)
	}
}

func TestResolveEnabled(t *testing.T) {
	registry := plugins.NewRegistry()
	for _, descriptor := range builtin.Descriptors() {
		if err := registry.RegisterDescriptor(descriptor); err != nil {
			t.Fatalf("RegisterDescriptor(%s) = %v, want nil", descriptor.ID, err)
		}
	}
	// Silent deployment config selects every DefaultEnabled descriptor. The
	// browser and kubernetes descriptors are retired and no longer register.
	defaults, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alertmanager", "prometheus", "thanos"}
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("ResolveEnabled(nil) = %v, want %v", defaults, want)
	}
	// An explicit list is a whitelist: unknown ids fail deterministically —
	// including the retired browser/kubernetes ids.
	if _, err := registry.ResolveEnabled([]string{"prometheus", "ghost"}); !errors.Is(err, plugins.ErrUnknownPlugin) {
		t.Fatalf("ResolveEnabled(ghost) = %v, want ErrUnknownPlugin", err)
	}
	if _, err := registry.ResolveEnabled([]string{"kubernetes"}); !errors.Is(err, plugins.ErrUnknownPlugin) {
		t.Fatalf("ResolveEnabled(retired kubernetes) = %v, want ErrUnknownPlugin", err)
	}
	explicit, err := registry.ResolveEnabled([]string{"thanos", "prometheus", "thanos"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(explicit, []string{"prometheus", "thanos"}) {
		t.Fatalf("ResolveEnabled(explicit) = %v, want [prometheus thanos]", explicit)
	}
	if !plugins.IsEnabled(explicit, "prometheus") || plugins.IsEnabled(explicit, "alertmanager") {
		t.Fatal("IsEnabled disagrees with the resolved set")
	}
	// An explicit EMPTY whitelist disables every plugin: omission and empty
	// array are different deployment facts and must never be conflated.
	none, err := registry.ResolveEnabled([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("ResolveEnabled([]) = %v, want no plugins enabled", none)
	}
}

type fakeProber struct{}
type fakeDiscoverer struct{}
type fakeExecutor struct{}

func (fakeProber) Probe(context.Context, *plugins.Call, plugins.ProbeRequest) (*plugins.ProbeResult, error) {
	return &plugins.ProbeResult{}, nil
}

func (fakeDiscoverer) Discover(context.Context, *plugins.Call, plugins.DiscoverRequest) (*plugins.DiscoverResult, error) {
	return &plugins.DiscoverResult{}, nil
}

func (fakeExecutor) ExecuteTool(context.Context, *plugins.Call, plugins.ToolRequest) (*plugins.ToolResult, error) {
	return &plugins.ToolResult{}, nil
}
