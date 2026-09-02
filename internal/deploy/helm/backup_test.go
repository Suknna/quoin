package helm

import (
	"errors"
	"strings"
	"testing"

	deploybackup "github.com/Suknna/quoin/internal/deploy/backup"
)

func TestOfflineFallbackRejectsKubernetesAndRBACFailuresAsUnavailabilityProof(t *testing.T) {
	for _, scenario := range []struct {
		name, output string
	}{
		{name: "kubernetes-api", output: "Unable to connect to the server: EOF"},
		{name: "rbac", output: "Error from server (Forbidden): pods is forbidden"},
		{name: "job-launch", output: "ImagePullBackOff"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if deploybackup.UnavailabilityProven(scenario.output, errors.New("kubectl failed")) {
				t.Fatalf("Kubernetes transport failure was accepted as an ops-listener proof: %q", scenario.output)
			}
		})
	}
}

func TestOfflineBackupPodUsesReleaseVolumesWithoutWebCredentials(t *testing.T) {
	body := offlineBackupPod("q-backup", "example/quoin@sha256:abc", "q")
	for _, required := range []string{"backup", "--offline", "q-quoin-data", "q-quoin-backups", "q-component-quoin", "q-secrets"} {
		if !strings.Contains(body, required) {
			t.Fatalf("offline pod missing %q: %s", required, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "session") || strings.Contains(strings.ToLower(body), "password") {
		t.Fatalf("helper must not carry Web credentials: %s", body)
	}
}

func TestParseHelmBackupObservationRequiresMetricSet(t *testing.T) {
	body := "quoin_accepting_work 1\nquoin_maintenance 0\nquoin_backup_active 0\nprocess_start_time_seconds 10\nquoin_backup_last_online_manual_success_timestamp_seconds 2\nquoin_backup_failures_total 3\n"
	value, err := deploybackup.ParseObservation(body)
	if err != nil || !value.Available() {
		t.Fatalf("value=%+v err=%v", value, err)
	}
}

func TestComponentSelectorMatchesChartIdentity(t *testing.T) {
	if got, want := componentSelector("release", "plinth"), "app.kubernetes.io/name=quoin,app.kubernetes.io/instance=release,app.kubernetes.io/component=plinth"; got != want {
		t.Fatalf("componentSelector=%q want=%q", got, want)
	}
}
