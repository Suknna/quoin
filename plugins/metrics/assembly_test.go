package metrics_test

import (
	"reflect"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/plugins/alertmanager"
	_ "github.com/Suknna/quoin/plugins/metrics"
)

// TestBuiltinRegistryFreezes exercises the compiled plugin assembly: freeze must
// succeed and the shared tool contract must dedup onto one entry with both
// metrics plugins as provenance.
func TestBuiltinRegistryFreezes(t *testing.T) {
	var ids []string
	for _, plugin := range plugins.Default().Plugins() {
		ids = append(ids, plugin.ID)
	}
	if want := []string{plugins.AlertmanagerID, plugins.PrometheusID, plugins.ThanosID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("compiled plugins = %v, want %v", ids, want)
	}
	entries := plugins.Default().ToolEntries()
	byName := map[string]plugins.ToolEntry{}
	for _, entry := range entries {
		byName[entry.Definition.Name] = entry
	}
	for _, name := range []string{"thanos_query", "metrics_probe", "metrics_discover", "metrics_collect"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("tool %s missing from builtin assembly", name)
		}
	}
	_, owners, ok := plugins.Default().ToolEntryByName("thanos_query")
	if !ok || !reflect.DeepEqual(owners, []string{plugins.PrometheusID, plugins.ThanosID}) {
		t.Fatalf("thanos_query owners = %v, want prometheus+thanos", owners)
	}
	if _, _, ok := plugins.Default().EventSource("alertmanager"); !ok {
		t.Fatal("alertmanager event source missing")
	}
}
