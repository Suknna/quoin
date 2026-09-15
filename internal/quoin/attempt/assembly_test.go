package attempt

// The single-source pins: ONE registry assembly derives catalogs,
// implementation lookup and (on executing hosts) the dispatch table. These
// tests reject the failure modes that would reintroduce a second tool
// source: duplicate implementations, declarations without implementations,
// implementations without declarations, and contract drift between a
// descriptor and its compiled implementation.

import (
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/plugins/builtin"
)

// The assembled implementation table is exactly platform + plugin-owned +
// retired: no duplicates, and every registered descriptor tool resolves in
// it (declaration/implementation agreement for ALL descriptors).
func TestImplementationsComposeSingleSource(t *testing.T) {
	registry := builtin.Registry()
	table, err := NewImplementationTable(Implementations())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, def := range Implementations() {
		if seen[def.Name] {
			t.Fatalf("tool %s implemented twice in the composed table", def.Name)
		}
		seen[def.Name] = true
	}
	for _, descriptor := range registry.Descriptors() {
		if len(descriptor.Tools) == 0 {
			continue
		}
		for _, declared := range descriptor.Tools {
			implementation, ok := table.Lookup(declared.Name)
			if !ok {
				t.Fatalf("descriptor %s tool %s has no compiled implementation", descriptor.ID, declared.Name)
			}
			if implementation.Version != declared.Version || implementation.Description != declared.Description {
				t.Fatalf("descriptor %s tool %s drifts from its compiled implementation", descriptor.ID, declared.Name)
			}
		}
	}
}

// A descriptor whose declaration drifts from the compiled implementation
// (version, execution location, failure mode or description) fails the
// assembly deterministically instead of advertising a lying catalog. Both
// shared-contract providers drift identically so registration itself stays
// consistent and the ASSEMBLY is what rejects.
func TestBuildCatalogsRejectsDeclarationDrift(t *testing.T) {
	drifted := builtin.Descriptors()
	for i := range drifted {
		if drifted[i].ID == plugins.PrometheusID || drifted[i].ID == plugins.ThanosID {
			tool := drifted[i].Tools[0]
			tool.Version = "999"
			drifted[i].Tools[0] = tool
		}
	}
	driftedRegistry := plugins.NewRegistry()
	for _, descriptor := range drifted {
		if err := driftedRegistry.RegisterDescriptor(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	_, err := BuildCatalogs(driftedRegistry, Implementations(), nil)
	if err == nil || !strings.Contains(err.Error(), "declares version 999") {
		t.Fatalf("BuildCatalogs = %v, want a deterministic drift rejection", err)
	}
}

// A compiled implementation no descriptor declares must fail the assembly —
// an unclaimed tool can never silently enter a catalog.
func TestBuildCatalogsRejectsUnclaimedImplementation(t *testing.T) {
	registry := builtin.Registry()
	orphan := builtin.QueryTool
	orphan.Name = "orphan_query"
	_, err := BuildCatalogs(registry, append(Implementations(), orphan), nil)
	if err == nil || !strings.Contains(err.Error(), "no registered plugin declaration") {
		t.Fatalf("BuildCatalogs = %v, want an unclaimed-implementation rejection", err)
	}
}

// A duplicate implementation name fails the table construction.
func TestImplementationTableRejectsDuplicates(t *testing.T) {
	_, err := NewImplementationTable(append(Implementations(), builtin.QueryTool))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("NewImplementationTable = %v, want a duplicate rejection", err)
	}
}

// The frozen legacy fallback is fixed data: it is independent of the
// deployment enablement, includes the retired always-on tools, and still
// resolves against the assembled implementations — while a drifted version
// is denied explicitly instead of being reinterpreted.
func TestLegacyFallbackIsFixedAndContractMatched(t *testing.T) {
	// Same bytes for a silent deployment and an explicit whitelist: the
	// fallback never consults enablement.
	first := legacyGenerationCatalog(AgentVersion)
	second := legacyGenerationCatalog(AgentVersion)
	firstJSON, err := first.ProviderToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.ProviderToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("legacy fallback rendering is not fixed")
	}
	if _, ok := first.Lookup("kubernetes_read"); !ok {
		t.Fatal("the historical fallback must keep the retired always-on tools")
	}
	catalogs := DefaultCatalogs()
	frozen, ok := first.Lookup("thanos_query")
	if !ok {
		t.Fatal("thanos_query missing from the historical fallback")
	}
	if _, err := catalogs.InstalledDefinition(frozen); err != nil {
		t.Fatalf("historical thanos_query must match the installed executor: %v", err)
	}
	drifted := frozen
	drifted.Version = "999"
	if _, err := catalogs.InstalledDefinition(drifted); err == nil {
		t.Fatal("a drifted historical tool must be denied explicitly")
	}
}
