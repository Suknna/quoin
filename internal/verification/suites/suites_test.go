package suites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dedicated acceptance scenarios retain their legacy Compose helpers while the
// Kubernetes backend enters the native Stack through the common verify surface.
func TestResolvePhaseRoutesDedicatedKubernetesAcceptanceSuites(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{
			command: "quoin-deploy <compose|kubernetes> restore --verify-isolation --phase action",
			want:    "quoin-deploy kubernetes verify --suite restore-isolation --phase action",
		},
		{
			command: "quoin-deploy <compose|kubernetes> recover-lintel --phase assert",
			want:    "quoin-deploy kubernetes verify --suite lintel-recovery --phase assert",
		},
	}
	for _, test := range tests {
		got, err := ResolvePhase(test.command, BackendKubernetes)
		if err != nil {
			t.Fatalf("ResolvePhase(%q): %v", test.command, err)
		}
		if got != test.want {
			t.Errorf("ResolvePhase(%q) = %q, want %q", test.command, got, test.want)
		}
	}
}

func TestResolvePhasePreservesDedicatedComposeCommands(t *testing.T) {
	command := "quoin-deploy <compose|kubernetes> restore --verify-isolation --phase action"
	got, err := ResolvePhase(command, BackendCompose)
	if err != nil {
		t.Fatalf("ResolvePhase: %v", err)
	}
	want := "quoin-deploy compose restore --verify-isolation --phase action"
	if got != want {
		t.Fatalf("ResolvePhase = %q, want %q", got, want)
	}
}

// The unimplemented native recovery seam must be a hard non-success and must
// not write a facts document which the coordinator could interpret as proof.
func TestLintelRecoveryNativeBoundaryNeverEmitsGenericFacts(t *testing.T) {
	facts := filepath.Join(t.TempDir(), "facts.json")
	request := DeploymentRequest{Backend: BackendKubernetes, Suite: SuiteLintelRecovery, Phase: PhaseAction, FactsPath: facts}
	err := RunLintelRecoveryPhase(request)
	if err == nil || !strings.Contains(err.Error(), "NOT_RUN") {
		t.Fatalf("RunLintelRecoveryPhase() error = %v, want explicit NOT_RUN", err)
	}
	if _, statErr := os.Stat(facts); !os.IsNotExist(statErr) {
		t.Fatalf("RunLintelRecoveryPhase() wrote facts (%v); an unobserved recovery must not pass assertions", statErr)
	}
}
