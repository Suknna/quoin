package attempt

// Registry-driven frozen catalog assembly tests (ADR-0004): enablement
// selects plugin tools, platform tools are always present, retired plugins
// never advertise, and the frozen document renders byte-stable provider
// schemas without the registry.

import (
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
)

func buildTestCatalogs(t *testing.T, configured []string) (*plugins.Registry, *Catalogs) {
	t.Helper()
	registry := plugins.Default()
	enabled, err := registry.ResolveEnabled(configured)
	if err != nil {
		t.Fatal(err)
	}
	catalogs, err := BuildCatalogs(registry, enabled)
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

// The default mainline offers the metrics observation tool and the platform
// tools in every agent generation's newly frozen catalog.
func TestDefaultCatalogsOfferMainlineTools(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, nil)
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		names := catalogToolNames(t, catalogs, agentVersion)
		if !names["thanos_query"] || !names["bash"] || !names["artifact_read"] {
			t.Fatalf("agent %s lost platform or enabled-plugin tools: %v", agentVersion, names)
		}
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

// Keep 提示词迁入新增的执行代必须拥有自己的冻结目录：initial-analysis-v2 与
// investigation-v3 可供创建期冻结，且 investigation-v3 沿用 investigation 工具
// schema 标签，溯源不漂到 initial-analysis 工具代；knowledge 固定的原共享身份
// 照常可冻结；investigation 各代的 NULL-catalog 历史回退都命中 investigation
// 冻结文档。
func TestKeepGenerationCatalogsAssemble(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, nil)
	for _, agentVersion := range []string{AgentVersion, PreviousAgentVersion, KnowledgeAgentVersion} {
		if _, err := catalogs.CatalogFor(agentVersion); err != nil {
			t.Fatalf("initial-analysis generation %s has no frozen catalog: %v", agentVersion, err)
		}
	}
	for _, agentVersion := range []string{"investigation-v2", "investigation-v3"} {
		catalog, err := catalogs.CatalogFor(agentVersion)
		if err != nil {
			t.Fatalf("investigation generation %s has no frozen catalog: %v", agentVersion, err)
		}
		if catalog.SchemaVersion != "investigation-tools-v3" {
			t.Fatalf("investigation generation %s schema version = %q", agentVersion, catalog.SchemaVersion)
		}
	}
}
