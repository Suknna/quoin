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
	for _, agentVersion := range []string{AgentVersion, "investigation-v3"} {
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
	investigation := catalogToolNames(t, catalogs, "investigation-v3")
	if !investigation["thanos_query"] {
		t.Fatal("prometheus provider of the shared query tool missing")
	}
	if !investigation["bash"] {
		t.Fatal("platform workspace tools must survive plugin disablement")
	}
	// Disabling BOTH metrics providers removes the query tool.
	_, catalogs = buildTestCatalogs(t, []string{"alertmanager"})
	investigation = catalogToolNames(t, catalogs, "investigation-v3")
	if investigation["thanos_query"] {
		t.Fatal("no enabled metrics provider, yet the query tool is offered")
	}
}

// An explicitly EMPTY whitelist disables every plugin tool while platform
// tools survive; it is distinct from the omitted field's default mainline.
func TestEmptyWhitelistDisablesPluginToolsOnly(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, []string{})
	for _, agentVersion := range []string{AgentVersion, "investigation-v3"} {
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
	source, err := catalogs.CatalogFor("investigation-v3")
	if err != nil {
		t.Fatal(err)
	}
	want, err := source.Digest()
	if err != nil {
		t.Fatal(err)
	}
	document, catalog, err := FrozenCatalogJSONForCreation(catalogs, "investigation-v3")
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
	for _, agentVersion := range []string{AgentVersion, KnowledgeAgentVersion} {
		if _, err := catalogs.CatalogFor(agentVersion); err != nil {
			t.Fatalf("initial-analysis generation %s has no frozen catalog: %v", agentVersion, err)
		}
	}
	for _, agentVersion := range []string{"investigation-v3", "investigation-v4"} {
		if _, err := catalogs.CatalogFor(agentVersion); err != nil {
			t.Fatalf("investigation generation %s has no frozen catalog: %v", agentVersion, err)
		}
	}
	if catalog, err := catalogs.CatalogFor("investigation-v3"); err != nil || catalog.SchemaVersion != "investigation-tools-v3" {
		t.Fatalf("investigation v3 schema version = %q err=%v", catalog.SchemaVersion, err)
	}
	if catalog, err := catalogs.CatalogFor("investigation-v4"); err != nil || catalog.SchemaVersion != "investigation-tools-v4" {
		t.Fatalf("investigation v4 schema version = %q err=%v", catalog.SchemaVersion, err)
	}
}

// 知识接入代：分析/调查/巡检世代的基础目录携带知识检索平台工具；知识抽取
// 钉住的 initial-analysis-v1 世代（其唯一新 Attempt 消费者）不携带——抽取
// 候选必须只来自来源材料，不得检索既有知识库。
func TestKnowledgeToolsFollowAgentGenerations(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, nil)
	withKnowledge := []string{AgentVersion, "initial-analysis-v2", "investigation-v3", "investigation-v4", InspectionAgentVersion}
	for _, agentVersion := range withKnowledge {
		names := catalogToolNames(t, catalogs, agentVersion)
		if !names["knowledge_search"] || !names["knowledge_get"] {
			t.Fatalf("agent %s lost the knowledge retrieval tools: %v", agentVersion, names)
		}
	}
	excluded := catalogToolNames(t, catalogs, KnowledgeAgentVersion)
	if excluded["knowledge_search"] || excluded["knowledge_get"] {
		t.Fatalf("knowledge extraction generation must not carry knowledge retrieval tools: %v", excluded)
	}
	// v1 世代的基础平台工具面保持不变（目录字节语义不因知识接入漂移）。
	for _, required := range []string{"bash", "read", "write", "grep", "artifact_read", "artifact_grep", "alerts_recent"} {
		if !excluded[required] {
			t.Fatalf("knowledge extraction generation lost platform tool %s", required)
		}
	}
}

// 巡检分析世代首次拥有可冻结目录：inspection-analysis-v4 的目录可冻结、携带
// 既有平台工具（artifact_read/grep 是读取巡检证据的必经工具）与知识检索工具，
// 且拥有自己的 schema 版本标签。
func TestInspectionGenerationCatalogFreezes(t *testing.T) {
	_, catalogs := buildTestCatalogs(t, nil)
	names := catalogToolNames(t, catalogs, InspectionAgentVersion)
	for _, required := range []string{"artifact_read", "artifact_grep", "knowledge_search", "knowledge_get", "alerts_recent"} {
		if !names[required] {
			t.Fatalf("inspection generation catalog lost %s: %v", required, names)
		}
	}
	catalog, err := catalogs.CatalogFor(InspectionAgentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.SchemaVersion != "inspection-analysis-tools-v1" {
		t.Fatalf("inspection schema version = %q", catalog.SchemaVersion)
	}
	for _, tool := range catalog.Tools {
		if tool.Name == "knowledge_search" || tool.Name == "knowledge_get" {
			if tool.ExecutionMode != plugins.ModeQuoinRouted {
				t.Fatalf("knowledge tool %s execution mode = %q", tool.Name, tool.ExecutionMode)
			}
		}
	}
}
