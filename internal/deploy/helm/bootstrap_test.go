package helm

import (
	"strings"
	"testing"
)

func installInputFixture() installInput {
	input := installInput{PublicOrigin: "https://quoin.example.test", LintelBrowserSlots: 1, LintelShmSize: "1Gi"}
	input.Storage.QuoinData = pvcInput{Capacity: "1Gi", AccessMode: "ReadWriteOnce"}
	input.Storage.QuoinBackup = pvcInput{Capacity: "1Gi", AccessMode: "ReadWriteOnce"}
	input.Storage.PlinthState = pvcInput{Capacity: "1Gi", AccessMode: "ReadWriteOnce"}
	input.Storage.LintelState = pvcInput{Capacity: "1Gi", AccessMode: "ReadWriteOnce"}
	return input
}

func TestRenderRetainedPVCsMatchesFrozenTopology(t *testing.T) {
	manifest := renderRetainedPVCs("t31", installInputFixture())
	for _, required := range []string{
		"name: t31-quoin-data", "name: t31-quoin-backups", "name: t31-plinth-state", "name: t31-lintel-state",
		"accessModes: [ReadWriteOnce]", "storage: 1Gi",
	} {
		if !strings.Contains(manifest, required) {
			t.Errorf("retained manifest missing %q:\n%s", required, manifest)
		}
	}
	for _, forbidden := range []string{"storageClassName"} {
		if strings.Contains(manifest, forbidden) {
			t.Errorf("retained manifest must not default %q:\n%s", forbidden, manifest)
		}
	}
	if strings.Contains(manifest, "kind: Job") || strings.Contains(manifest, "staging") {
		t.Errorf("retained manifest must contain only PVCs:\n%s", manifest)
	}
}

func TestRenderSecretBootstrapStagesSecretsOnStagingVolume(t *testing.T) {
	manifest := renderSecretBootstrap("t31", installInputFixture(), "repo/quoin@sha256:abc", "repo/plinth@sha256:def")
	for _, required := range []string{
		"name: t31-bootstrap-staging", "kind: Job", "name: t31-secret-bootstrap",
		"backoffLimit: 0", "restartPolicy: Never", "/stage/root-key",
		"claimName: t31-quoin-data", "claimName: t31-bootstrap-staging",
		`command: ["/quoin"]`, `args: ["secrets", "bootstrap", "--config", "/etc/quoin/component.yaml", "--kubernetes-secret", "t31-secrets"]`,
		"kind: ServiceAccount", "kind: Role", "kind: RoleBinding", "resourceNames: [\"t31-secrets\"]", "verbs: [\"get\", \"update\"]", "automountServiceAccountToken: true",
	} {
		if !strings.Contains(manifest, required) {
			t.Errorf("secret bootstrap manifest missing %q:\n%s", required, manifest)
		}
	}
	for _, forbidden := range []string{"stringData:", "runtime-tls.crt: ", "helm.sh/hook"} {
		if strings.Contains(manifest, forbidden) {
			t.Errorf("secret bootstrap manifest must not contain %q:\n%s", forbidden, manifest)
		}
	}
}

func TestRenderAdminBootstrapUsesAttachedTTYPod(t *testing.T) {
	manifest := renderAdminBootstrap("t31", "repo/quoin@sha256:abc", "repo/plinth@sha256:def", "https://quoin.example.test")
	for _, required := range []string{"stdin: true", "tty: true", "restartPolicy: Never", "secretName: t31-secrets", "configMap: {name: t31-admin-bootstrap-config}", "publicOrigin: https://quoin.example.test"} {
		if !strings.Contains(manifest, required) {
			t.Errorf("admin bootstrap manifest missing %q:\n%s", required, manifest)
		}
	}
	if strings.Contains(manifest, "backoffLimit") {
		t.Errorf("admin bootstrap must be a one-shot pod, not a Job:\n%s", manifest)
	}
}
