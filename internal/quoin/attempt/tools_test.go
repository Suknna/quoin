package attempt

import (
	"strings"
	"testing"
)

// The Kubernetes observation capability shipped its real binding: the
// kubernetes_read tool is supervisor-executed, grant-routed and evidence-
// projecting. The only remaining hard gate is the browser tool, which is
// deployment-selected, and no undeclared tool may leak into any catalog.
func TestCatalogExcludesUnboundAndDeploymentGatedTools(t *testing.T) {
	_, catalogs := DefaultCatalogs(), DefaultCatalogs()
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		catalog, err := catalogs.CatalogFor(agentVersion)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := catalog.Lookup("quoin_browser"); ok {
			t.Fatalf("agent %s offers quoin_browser with the browser plugin disabled", agentVersion)
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

// The capability gate must not remove the independent metrics observation tool.
func TestNonKubernetesObservationRemainsModelCallable(t *testing.T) {
	tool, ok := LookupToolForAgentVersion("investigation-v1", "thanos_query")
	if !ok || !tool.ProducesEvidence {
		t.Fatalf("thanos_query=%+v, registered=%t", tool, ok)
	}
	kubernetesTool, ok := LookupToolForAgentVersion("investigation-v1", "kubernetes_read")
	if !ok || !kubernetesTool.ProducesEvidence {
		t.Fatalf("kubernetes_read=%+v, registered=%t", kubernetesTool, ok)
	}
}
