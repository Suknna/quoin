package attempt

// Registry-driven frozen catalog assembly tests (ADR-0004): enablement
// selects plugin tools, platform tools are always present, retired plugins
// never advertise, and the frozen document renders byte-stable provider
// schemas without the registry.

import (
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

func buildTestCatalogs(t *testing.T, configured []string) (*plugins.Registry, *Catalogs) {
	t.Helper()
	registry := builtin.Registry()
	enabled, err := registry.ResolveEnabled(configured)
	if err != nil {
		t.Fatal(err)
	}
	catalogs, err := BuildCatalogs(registry, Implementations(), enabled)
	if err != nil {
		t.Fatal(err)
	}
	return registry, catalogs
}

func catalogToolNames(t *testing.T, catalogs *Catalogs, agentVersion string) map[string]bool {
	t.Helper()
	catalog, err := catalogs.CatalogFor(agentVersion)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range catalog.Tools {
		names[tool.Name] = true
	}
	return names
}

// The default mainline offers the metrics observation tool but never the
// retired browser or kubernetes tools (受控退役): they must not enter any
// newly frozen catalog, no matter the agent generation, while their
// compiled implementations stay resolvable for frozen historical attempts.
func TestDefaultCatalogsExcludeRetiredTools(t *testing.T) {
	registry, catalogs := buildTestCatalogs(t, nil)
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		names := catalogToolNames(t, catalogs, agentVersion)
		if names["quoin_browser"] || names["kubernetes_read"] {
			t.Fatalf("agent %s offers a retired tool: %v", agentVersion, names)
		}
		if !names["thanos_query"] || !names["bash"] || !names["artifact_read"] {
			t.Fatalf("agent %s lost platform or enabled-plugin tools: %v", agentVersion, names)
		}
	}
	// The compiled implementations stay resolvable through the SAME
	// assembly — retired declarations are authorities, not dead entries.
	for _, name := range []string{"quoin_browser", "kubernetes_read"} {
		if _, ok := catalogs.Implementation(name); !ok {
			t.Fatalf("retired implementation %s left the assembled table", name)
		}
	}
	if _, err := registry.ResolveEnabled([]string{plugins.BrowserID, plugins.KubernetesID}); err == nil {
		t.Fatal("retired plugin ids must fail enablement resolution like unknown ids")
	}
}

// Disabling a plugin removes its tools from newly frozen catalogs without
// touching the platform tool set.
func TestEnablementSelectsPluginToolsPerGeneration(t *testing.T) {
	// PromQL query is a SHARED contract: disabling the thanos plugin does not
	// remove the query tool while prometheus (same contract) stays enabled.
	_, catalogs := buildTestCatalogs(t, []string{"prometheus", "alertmanager"})
	investigation := catalogToolNames(t, catalogs, "investigation-v1")
	if !investigation["thanos_query"] {
		t.Fatal("prometheus provider of the shared query tool missing")
	}
	if !investigation["bash"] {
		t.Fatal("platform workspace tools must survive plugin disablement")
	}
	// Disabling BOTH metrics providers removes the query tool.
	_, catalogs = buildTestCatalogs(t, []string{"alertmanager"})
	investigation = catalogToolNames(t, catalogs, "investigation-v1")
	if investigation["thanos_query"] {
		t.Fatal("no enabled metrics provider, yet the query tool is offered")
	}
}

// An explicitly EMPTY whitelist disables every plugin tool while platform
// tools survive; it is distinct from the omitted field's default mainline.
func TestEmptyWhitelistDisablesPluginToolsOnly(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, []string{})
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		names := catalogToolNames(t, catalogs, agentVersion)
		if names["thanos_query"] || names["quoin_browser"] || names["kubernetes_read"] {
			t.Fatalf("agent %s still offers plugin tools under an empty whitelist: %v", agentVersion, names)
		}
		if !names["bash"] || !names["artifact_grep"] {
			t.Fatalf("agent %s lost platform tools: %v", agentVersion, names)
		}
	}
}

// A Prometheus-only deployment still offers a real metrics query tool: the
// PromQL tool is a shared contract declared by both provider plugins, and
// catalog provenance binds only the ENABLED contributing provider.
func TestSharedMetricsToolFollowsEnabledProvider(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, []string{"prometheus"})
	catalog, err := catalogs.CatalogFor(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Lookup("thanos_query"); !ok {
		t.Fatal("prometheus-only deployment lost the metrics query tool")
	}
	provenance := map[string]bool{}
	for _, plugin := range catalog.Plugins {
		provenance[plugin.ID] = true
	}
	if !provenance["prometheus"] || provenance["thanos"] {
		t.Fatalf("provenance = %v, want prometheus only", provenance)
	}
	_, catalogs = buildTestCatalogs(t, []string{"thanos"})
	catalog, err = catalogs.CatalogFor(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Lookup("thanos_query"); !ok {
		t.Fatal("thanos-only deployment lost the metrics query tool")
	}
	provenance = map[string]bool{}
	for _, plugin := range catalog.Plugins {
		provenance[plugin.ID] = true
	}
	if !provenance["thanos"] || provenance["prometheus"] {
		t.Fatalf("provenance = %v, want thanos only", provenance)
	}
	// Both enabled: one catalog entry, both providers in provenance.
	_, catalogs = buildTestCatalogs(t, []string{"prometheus", "thanos", "alertmanager"})
	catalog, err = catalogs.CatalogFor(AgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for _, tool := range catalog.Tools {
		if tool.Name == "thanos_query" {
			entries++
		}
	}
	if entries != 1 {
		t.Fatalf("shared tool entries = %d, want exactly one", entries)
	}
}

// The frozen document must be self-contained: rendering the provider schema
// of a stored document succeeds without the registry and equals the digest
// the creation-time catalog renders.
func TestFrozenCatalogRendersStableBytes(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, []string{"prometheus", "thanos", "alertmanager"})
	source, err := catalogs.CatalogFor("investigation-v1")
	if err != nil {
		t.Fatal(err)
	}
	want, err := source.Digest()
	if err != nil {
		t.Fatal(err)
	}
	document, catalog, err := FrozenCatalogJSONForCreation(catalogs, "investigation-v1")
	if err != nil {
		t.Fatal(err)
	}
	var restored FrozenCatalog
	if err := json.Unmarshal(document, &restored); err != nil {
		t.Fatal(err)
	}
	got, err := restored.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("stored document digest drifted: %s != %s", got, want)
	}
	if catalog.SchemaVersion != "investigation-tools-v3" {
		t.Fatalf("catalog schema version = %q", catalog.SchemaVersion)
	}
}

// TestFrozenBrowserToolV1RejectsExplicitly pins the breaking quoin_browser
// locator change (ADR-0004): a catalog frozen with v1 semantics (the retired
// businessSystemKey locator) must drift-reject against the installed v2
// implementation instead of being reinterpreted under the same tool name.
// The v2 implementation stays compiled under the retired declaration for
// ingress validation of frozen historical executions (受控浏览器退役) even
// though no catalog serves it.
func TestFrozenBrowserToolV1RejectsExplicitly(t *testing.T) {
	legacy := FrozenTool{
		Name: "quoin_browser", Version: "1", ExecutionMode: "quoin_browser",
		FailureMode: "return_to_model", ResultSchemaKind: "browser_tool_result_v1",
		Description: "在已授权的浏览器身份中执行一个封闭的探索动作。只接受 open、页面导航、元素交互、受限读取、截图和会话关闭；不接受 JavaScript、HTTP、CDP 或 Playwright 指令。",
		Parameters:  map[string]any{"type": "object", "required": []string{"action", "businessSystemKey"}},
	}
	table, err := NewImplementationTable(Implementations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.InstalledDefinition(legacy); err == nil {
		t.Fatal("frozen quoin_browser v1 must not resolve against the installed v2 implementation")
	}
	def, known := table.Lookup("quoin_browser")
	if !known {
		t.Fatal("installed quoin_browser implementation must stay compiled")
	}
	if def.Version != "2" {
		t.Fatalf("installed quoin_browser version = %q, want 2", def.Version)
	}
	if err := ValidateToolArguments(def, []byte(`{"action":"open","identityKey":"ops-console"}`)); err != nil {
		t.Fatalf("v2 open with identityKey must validate: %v", err)
	}
	if err := ValidateToolArguments(def, []byte(`{"action":"open","businessSystemKey":"payments"}`)); err == nil {
		t.Fatal("v2 open must explicitly reject the retired businessSystemKey locator")
	}
}
