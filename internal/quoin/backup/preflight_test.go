package backup

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"golang.org/x/sys/unix"
)

func TestTicket32BackupPreflightMarksCapacityFailureNotReadyAndRecovers(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	server, err := sharedops.New("quoin", ":0", sharedops.Ready)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := server.BackupMetrics()
	if err != nil {
		t.Fatal(err)
	}
	service.SetMetrics(metrics)

	requirement, err := service.preflightRequirement(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requirement.BackupBytes < requirement.DatabaseBytes+requirement.ManifestBytes {
		t.Fatalf("backup requirement=%+v omits database or manifest bytes", requirement)
	}
	service.capacity = func(directory string) (uint64, uint64, error) {
		if filepath.Clean(directory) == filepath.Clean(service.config.BackupDirectory) {
			return requirement.BackupBytes - 1, 4096, nil
		}
		return ^uint64(0), 4096, nil
	}
	service.probeDirectory = func(string) error { return nil }

	queued, err := service.QueueManual(context.Background(), 1, "capacity-failure")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.Run(context.Background(), queued.ID)
	if err == nil {
		t.Fatal("capacity preflight unexpectedly succeeded")
	}
	if failed.Status != "failed" || failed.Stage != "preflight" || failed.ErrorCode == nil || *failed.ErrorCode != "storage_unavailable" {
		t.Fatalf("capacity failure=%+v", failed)
	}
	metricsText := func() string {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		return recorder.Body.String()
	}
	if !strings.Contains(metricsText(), `quoin_storage_writable{quoin_storage="backup"} 0`) {
		t.Fatalf("backup capacity failure did not project storage health:\n%s", metricsText())
	}
	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 503 {
		t.Fatalf("readyz status=%d, want 503 after storage failure", ready.Code)
	}

	service.capacity = func(string) (uint64, uint64, error) { return ^uint64(0), 4096, nil }
	retry, err := service.QueueManual(context.Background(), 1, "capacity-recovery")
	if err != nil {
		t.Fatal(err)
	}
	succeeded, err := service.Run(context.Background(), retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.Status != "succeeded" {
		t.Fatalf("recovery backup=%+v", succeeded)
	}
	if !strings.Contains(metricsText(), `quoin_storage_writable{quoin_storage="backup"} 1`) {
		t.Fatalf("backup recovery did not restore storage health:\n%s", metricsText())
	}
	ready = httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 200 {
		t.Fatalf("readyz status=%d, want 200 after recovery", ready.Code)
	}

	service.probeDirectory = func(directory string) error {
		if filepath.Clean(directory) == filepath.Clean(service.config.DataDirectory) {
			return unix.EROFS
		}
		return nil
	}
	dataFailure, err := service.QueueManual(context.Background(), 1, "data-readonly")
	if err != nil {
		t.Fatal(err)
	}
	failed, err = service.Run(context.Background(), dataFailure.ID)
	if err == nil || failed.ErrorCode == nil || *failed.ErrorCode != "storage_unavailable" {
		t.Fatalf("data durable-probe failure=%+v err=%v", failed, err)
	}
	if !strings.Contains(metricsText(), `quoin_storage_writable{quoin_storage="data"} 0`) {
		t.Fatalf("data durable-probe failure did not project storage health:\n%s", metricsText())
	}
	ready = httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != 503 {
		t.Fatalf("readyz status=%d, want 503 after data probe failure", ready.Code)
	}
}

func TestTicket32ExactPostSnapshotGateFailsBeforeArtifactCopy(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	service.probeDirectory = func(string) error { return nil }
	backupChecks := 0
	service.capacity = func(directory string) (uint64, uint64, error) {
		if filepath.Clean(directory) == filepath.Clean(service.config.BackupDirectory) {
			backupChecks++
			if backupChecks > 1 {
				return 0, 4096, nil
			}
		}
		return ^uint64(0), 4096, nil
	}
	queued, err := service.QueueManual(context.Background(), 1, "post-snapshot-capacity")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.Run(context.Background(), queued.ID)
	if err == nil {
		t.Fatal("post-snapshot capacity gate unexpectedly succeeded")
	}
	if failed.Status != "failed" || failed.Stage != "artifact_copy" || failed.ErrorCode == nil || *failed.ErrorCode != "storage_unavailable" {
		t.Fatalf("post-snapshot failure=%+v", failed)
	}
	if _, statErr := os.Stat(filepath.Join(service.config.BackupDirectory, queued.ID, "manifest.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("manifest stat error=%v, want no published manifest", statErr)
	}
}
