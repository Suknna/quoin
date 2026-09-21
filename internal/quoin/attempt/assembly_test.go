package attempt

// The single-source pins (ADR-0004, reworked by ADR-0011): ONE registry
// assembly derives the implementation table, the handler dispatch table and
// the per-generation frozen catalogs. Declaration and implementation come
// from the same typed Tool definition by construction, so the remaining
// failure modes are structural: duplicate implementations under one name in
// the composed table, internal tools never rendering into model catalogs,
// and divergent shared-contract manifests under one tool name being rejected
// at registry freeze.

import (
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
)

// The assembled implementation table resolves every registered plugin tool,
// keeps the platform tools, and never duplicates a name.
func TestImplementationsComposeSingleSource(t *testing.T) {
	registry := plugins.Default()
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalogs, err := BuildCatalogs(registry, enabled)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, def := range catalogs.Implementations.Definitions() {
		if seen[def.Name] {
			t.Fatalf("tool %s implemented twice in the composed table", def.Name)
		}
		seen[def.Name] = true
	}
	for _, entry := range registry.ToolEntries() {
		if _, ok := catalogs.Implementations.Lookup(entry.Definition.Name); !ok {
			t.Fatalf("plugin tool %s has no compiled implementation", entry.Definition.Name)
		}
		if handler, ok := catalogs.Handlers[entry.Definition.Name]; !ok || handler.Invoke == nil {
			t.Fatalf("plugin tool %s has no dispatch handler", entry.Definition.Name)
		}
	}
	// ADR-0012 平台工具全集（7 个）：工作区/Artifact 工具 + alerts_recent。
	platformTools := []string{"bash", "read", "write", "grep", "artifact_read", "artifact_grep", "alerts_recent"}
	for _, name := range platformTools {
		if _, ok := catalogs.Implementations.Lookup(name); !ok {
			t.Fatalf("platform tool %s missing from the composed table", name)
		}
	}
	definition, _ := catalogs.Implementations.Lookup("alerts_recent")
	if definition.ExecutionMode != plugins.ModeQuoinRouted || definition.RequiresConnectionGrant || definition.ProducesEvidence || definition.ResultSchemaKind != "alerts_recent_result_v1" {
		t.Fatalf("alerts_recent contract drifted: %+v", definition)
	}
	if definition.Parameters == nil || definition.ValidateArguments == nil {
		t.Fatal("alerts_recent must carry its frozen parameter schema and ingress validator")
	}
}

// Internal tools (probe/discover/collect) register into the implementation
// and handler tables but never render into any model-facing catalog.
func TestInternalToolsNeverRenderIntoCatalogs(t *testing.T) {
	registry := plugins.Default()
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		t.Fatal(err)
	}
	catalogs, err := BuildCatalogs(registry, enabled)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"metrics_probe", "metrics_discover", "metrics_collect"} {
		if _, ok := catalogs.Handlers[name]; !ok {
			t.Fatalf("internal tool %s must stay dispatchable by the local executor", name)
		}
		for _, agentVersion := range []string{AgentVersion, "investigation-v3"} {
			catalog, err := catalogs.CatalogFor(agentVersion)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := catalog.Lookup(name); ok {
				t.Fatalf("internal tool %s leaked into the %s model catalog", name, agentVersion)
			}
		}
	}
}

// Two plugins contributing divergent manifests under one tool name fail the
// registry freeze (the shared-contract rule) instead of assembling a lying
// catalog.
func TestRegistryRejectsDivergentSharedContract(t *testing.T) {
	registry := plugins.NewRegistry()
	tool := plugins.Tool[struct{}, struct{}]{
		Name: "shared_query", Version: "1", FailureMode: plugins.FailureReturnToModel, ResultKind: "shared_query_result_v1",
		Description: "one",
		Handler:     func(*plugins.ToolContext, struct{}) (struct{}, error) { return struct{}{}, nil },
	}
	other := tool
	other.Description = "diverged"
	if err := registry.Register(plugins.Plugin{ID: "left", Version: "1", Tools: toolProvider{tool.Entry("")}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(plugins.Plugin{ID: "right", Version: "1", Tools: toolProvider{other.Entry("")}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registry freeze must reject divergent manifests under one tool name")
		}
	}()
	registry.ToolEntries()
}

type toolProvider struct{ entry plugins.ToolEntry }

func (p toolProvider) Tools() []plugins.ToolEntry { return []plugins.ToolEntry{p.entry} }

// A duplicate implementation name fails the table construction.
func TestImplementationTableRejectsDuplicates(t *testing.T) {
	duplicates := append(PlatformImplementations(), PlatformImplementations()[0])
	if _, err := NewImplementationTable(duplicates); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("NewImplementationTable = %v, want a duplicate rejection", err)
	}
}
