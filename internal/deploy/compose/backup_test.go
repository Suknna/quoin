package compose

import (
	"errors"
	"testing"

	deploybackup "github.com/Suknna/quoin/internal/deploy/backup"
)

func TestOfflineFallbackRejectsComposeTransportFailuresAsUnavailabilityProof(t *testing.T) {
	for _, scenario := range []struct {
		name, output string
	}{
		{name: "docker-api", output: "Cannot connect to the Docker daemon"},
		{name: "launcher", output: "executable file not found"},
		{name: "network", output: "dial unix /var/run/docker.sock: connect: permission denied"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if deploybackup.UnavailabilityProven(scenario.output, errors.New("launcher failed")) {
				t.Fatalf("Compose transport failure was accepted as an ops-listener proof: %q", scenario.output)
			}
		})
	}
}

func TestParseBackupObservationRequiresFrozenMetricBaseline(t *testing.T) {
	body := `quoin_accepting_work 1
quoin_maintenance{reason="restore"} 0
quoin_backup_active 0
process_start_time_seconds 10
quoin_backup_last_online_manual_success_timestamp_seconds 2
quoin_backup_failures_total 3
`
	value, err := deploybackup.ParseObservation(body)
	if err != nil || !value.Available() || value.ProcessStart != 10 || value.Failures != 3 {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	if _, err := deploybackup.ParseObservation("quoin_accepting_work 1\n"); err == nil {
		t.Fatal("missing metric must reject online observation")
	}
}
