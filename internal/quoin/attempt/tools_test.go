package attempt

import (
	"strings"
	"testing"
)

// Kubernetes must not be present in any provider-facing catalog while its
// connection and observation capability are explicitly gated as development-only.
func TestKubernetesToolsAreExcludedFromEveryModelCatalog(t *testing.T) {
	for _, agentVersion := range []string{AgentVersion, "investigation-v1"} {
		t.Run(agentVersion, func(t *testing.T) {
			if _, ok := LookupToolForAgentVersion(agentVersion, "kubernetes_read"); ok {
				t.Fatal("kubernetes_read is model-callable")
			}
			body, err := CanonicalToolsJSON(agentVersion)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "kubernetes") {
				t.Fatalf("provider schema exposes Kubernetes: %s", body)
			}
		})
	}
}

// The capability gate must not remove the independent metrics observation tool.
func TestNonKubernetesObservationRemainsModelCallable(t *testing.T) {
	tool, ok := LookupToolForAgentVersion("investigation-v1", "thanos_query")
	if !ok || !tool.ProducesEvidence {
		t.Fatalf("thanos_query=%+v, registered=%t", tool, ok)
	}
}
