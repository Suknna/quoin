package attempt

import (
	"strings"
	"testing"
)

// New catalogs serve exactly the enabled plugins' tools plus the platform
// set; retired tools never leak in and no undeclared tool may leak into any
// catalog. Provider schemas must stay model-facing.
func TestCatalogExcludesRetiredAndUndeclaredTools(t *testing.T) {
	catalogs := DefaultCatalogs()
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		catalog, err := catalogs.CatalogFor(agentVersion)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := catalog.Lookup("quoin_browser"); ok {
			t.Fatalf("agent %s offers quoin_browser", agentVersion)
		}
		if _, ok := catalog.Lookup("kubernetes_read"); ok {
			t.Fatalf("agent %s offers retired kubernetes_read", agentVersion)
		}
		body, err := catalog.ProviderToolsJSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "deployment_verification") {
			t.Fatalf("provider schema exposes non-model tooling: %s", body)
		}
	}
}

// The metrics observation tool remains model-callable in the default
// assembly, evidence-projecting and grant-routed.
func TestMetricsObservationRemainsModelCallable(t *testing.T) {
	catalogs := DefaultCatalogs()
	catalog, err := catalogs.CatalogFor("investigation-v1")
	if err != nil {
		t.Fatal(err)
	}
	frozen, ok := catalog.Lookup("thanos_query")
	if !ok || !frozen.ProducesEvidence {
		t.Fatalf("thanos_query=%+v, registered=%t", frozen, ok)
	}
	def, err := catalogs.InstalledDefinition(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if !def.RequiresConnectionGrant {
		t.Fatal("the metrics observation tool must freeze connection grants")
	}
}
