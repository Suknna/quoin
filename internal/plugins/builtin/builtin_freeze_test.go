package builtin_test

import (
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	_ "github.com/Suknna/quoin/internal/plugins/builtin"
)

// TestBuiltinRegistryFreezes exercises the real builtin assembly: freeze must
// succeed and the shared tool contract must dedup onto one entry with both
// metrics plugins as provenance.
func TestBuiltinRegistryFreezes(t *testing.T) {
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
	if !ok || len(owners) != 2 {
		t.Fatalf("thanos_query owners = %v, want prometheus+thanos", owners)
	}
	if _, _, ok := plugins.Default().EventSource("alertmanager"); !ok {
		t.Fatal("alertmanager event source missing")
	}
}
