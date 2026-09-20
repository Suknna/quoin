package plugins_test

// Registry + generic-tool assembly tests (ADR-0004 reworked by ADR-0011):
// blank-import style registration, freeze-time invariants, shared-contract
// dedup, typed tool round-trips and reflection-derived schemas.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

type echoArgs struct {
	Query   string   `json:"query" doc:"查询表达式"`
	Tags    []string `json:"tags,omitempty"`
	Attempt int      `json:"attempt,omitempty"`
}

type echoResult struct {
	Echo string `json:"echo"`
}

func newTestRegistry(t *testing.T) *plugins.Registry {
	t.Helper()
	registry := plugins.NewRegistry()
	if err := registry.Register(plugins.Plugin{
		ID: "alpha", Version: "1", DefaultEnabled: true,
		EventSource: stubSource{kind: "alphaevents"},
		Tools: stubProvider{entries: []plugins.ToolEntry{
			plugins.Tool[echoArgs, echoResult]{
				Name: "alpha_query", Version: "3", FailureMode: plugins.FailureReturnToModel,
				ResultKind: "alpha_result_v1", Description: "alpha query tool",
				Handler: func(t *plugins.ToolContext, args echoArgs) (echoResult, error) {
					return echoResult{Echo: args.Query}, nil
				},
			}.Entry("alpha"),
		}},
	}); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	return registry
}

type stubSource struct{ kind string }

func (s stubSource) Kind() string { return s.kind }
func (s stubSource) VerifyAndParse(_ context.Context, _ plugins.InboundRequest) ([]plugins.Event, error) {
	return nil, nil
}

type stubProvider struct{ entries []plugins.ToolEntry }

func (p stubProvider) Tools() []plugins.ToolEntry { return p.entries }

func TestRegistryDuplicatePluginRejected(t *testing.T) {
	registry := newTestRegistry(t)
	if err := registry.Register(plugins.Plugin{ID: "alpha", Version: "2"}); err == nil {
		t.Fatal("duplicate plugin ID accepted")
	}
}

func TestRegistryEventSourceLookup(t *testing.T) {
	registry := newTestRegistry(t)
	source, pluginID, ok := registry.EventSource("alphaevents")
	if !ok || pluginID != "alpha" || source == nil {
		t.Fatalf("event source lookup failed: ok=%v plugin=%v", ok, pluginID)
	}
	if _, _, ok := registry.EventSource("missing"); ok {
		t.Fatal("unknown event source kind resolved")
	}
}

func TestRegistrySharedContractDedup(t *testing.T) {
	registry := plugins.NewRegistry()
	shared := func(owner string) plugins.ToolEntry {
		return plugins.Tool[echoArgs, echoResult]{
			Name: "shared_query", Version: "1", FailureMode: plugins.FailureReturnToModel,
			ResultKind: "shared_result_v1", Description: "shared contract",
			Handler: func(t *plugins.ToolContext, args echoArgs) (echoResult, error) { return echoResult{}, nil },
		}.Entry(owner)
	}
	_ = registry.Register(plugins.Plugin{ID: "one", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{shared("one")}}})
	_ = registry.Register(plugins.Plugin{ID: "two", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{shared("two")}}})
	// A divergent manifest under the same name must fail the freeze.
	divergent := shared("two")
	divergent.Definition.Description = "divergent"
	if err := registry.Register(plugins.Plugin{ID: "three", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{divergent}}}); err != nil {
		t.Fatalf("register three: %v", err)
	}
	assertPanic(t, func() { registry.ToolEntries() })
}

func TestRegistrySharedContractSingleOwnerLookup(t *testing.T) {
	registry := plugins.NewRegistry()
	shared := func(owner string) plugins.ToolEntry {
		return plugins.Tool[echoArgs, echoResult]{
			Name: "shared_query", Version: "1", FailureMode: plugins.FailureReturnToModel,
			ResultKind: "shared_result_v1", Description: "shared contract",
			Handler: func(t *plugins.ToolContext, args echoArgs) (echoResult, error) { return echoResult{}, nil },
		}.Entry(owner)
	}
	_ = registry.Register(plugins.Plugin{ID: "one", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{shared("one")}}})
	_ = registry.Register(plugins.Plugin{ID: "two", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{shared("two")}}})
	entry, owners, ok := registry.ToolEntryByName("shared_query")
	if !ok || len(owners) != 2 || entry.Owner != "one" {
		t.Fatalf("shared contract dedup failed: ok=%v owners=%v owner=%v", ok, owners, entry.Owner)
	}
}

func assertPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected freeze to panic on divergent shared tool")
		}
	}()
	fn()
}

func TestGenericToolRoundTrip(t *testing.T) {
	registry := newTestRegistry(t)
	entry, _, ok := registry.ToolEntryByName("alpha_query")
	if !ok {
		t.Fatal("alpha_query missing")
	}
	def := entry.Definition
	if def.ExecutionMode != plugins.ModeQuoinRouted {
		t.Fatalf("execution mode %q", def.ExecutionMode)
	}
	parameters := def.ProviderParameters()
	properties, _ := parameters["properties"].(map[string]any)
	query, _ := properties["query"].(map[string]any)
	if query["type"] != "string" || query["description"] != "查询表达式" {
		t.Fatalf("derived query property: %#v", query)
	}
	required, _ := parameters["required"].([]string)
	if len(required) != 1 || required[0] != "query" {
		t.Fatalf("derived required: %#v", required)
	}
	// Typed round trip: arguments decode into the struct, the handler runs
	// typed, the result marshals back at the rim.
	arguments, _ := json.Marshal(map[string]any{"query": "up", "tags": []string{"a"}})
	payload, err := entry.Invoke(context.Background(), plugins.ToolExecution{
		Arguments: arguments,
		Platform:  stubCaller{},
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var result echoResult
	if err := json.Unmarshal(payload, &result); err != nil || result.Echo != "up" {
		t.Fatalf("round trip payload: %s err=%v", payload, err)
	}
	// Unknown fields are rejected (closed schema).
	if err := entry.Definition.ValidateArguments([]byte(`{"query":"x","rogue":1}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

func TestGenericToolStrictRequiredArguments(t *testing.T) {
	registry := newTestRegistry(t)
	entry, _, _ := registry.ToolEntryByName("alpha_query")
	if err := entry.Definition.ValidateArguments([]byte(`{"attempt":1}`)); err == nil {
		t.Fatal("missing required argument accepted")
	}
	if err := entry.Definition.ValidateArguments([]byte(`{"query":"up"}`)); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
}

type stubCaller struct{}

func (stubCaller) Call(_ context.Context, _ plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	return &plugins.PlatformResponse{StatusCode: 200, Body: []byte(`{"status":"success"}`)}, nil
}

func TestToolNameVocabulary(t *testing.T) {
	registry := plugins.NewRegistry()
	bad := plugins.Tool[echoArgs, echoResult]{
		Name: "Bad-Name", Version: "1", FailureMode: plugins.FailureReturnToModel, ResultKind: "k", Description: "d",
		Handler: func(t *plugins.ToolContext, args echoArgs) (echoResult, error) { return echoResult{}, nil },
	}
	if err := registry.Register(plugins.Plugin{ID: "p", Version: "1", Tools: stubProvider{[]plugins.ToolEntry{bad.Entry("p")}}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	caught := false
	func() {
		defer func() {
			if recover() != nil {
				caught = true
			}
		}()
		registry.ToolEntries()
	}()
	if !caught {
		t.Fatal("invalid tool name accepted at freeze")
	}
}

func TestEnabledResolution(t *testing.T) {
	registry := newTestRegistry(t)
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil || len(enabled) != 1 || enabled[0] != "alpha" {
		t.Fatalf("default enabled: %v err=%v", enabled, err)
	}
	if _, err := registry.ResolveEnabled([]string{"ghost"}); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown plugin whitelist accepted: %v", err)
	}
}
