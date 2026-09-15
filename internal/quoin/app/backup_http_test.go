package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/backup"
)

// backupSurface is the shared fixture for the backup HTTP tests: the shared
// real-auth fixture plus a real backup service attached to the same database.
type backupSurface struct {
	*authScenario
	backupService *backup.Service
	backupDir     string
}

// newBackupSurface boots the fixture; the administrator is initialized
// through the real flow before any request is served.
func newBackupSurface(t *testing.T) *backupSurface {
	t.Helper()
	surface := &backupSurface{}
	surface.authScenario = newAuthScenarioWith(t, func(scenario *authScenario) http.Handler {
		dataDir := scenario.rootDir + "/data"
		surface.backupDir = scenario.rootDir + "/backups"
		service, err := backup.NewService(scenario.db, backup.Config{DataDirectory: dataDir, BackupDirectory: surface.backupDir, ArtifactDirectory: dataDir + "/artifacts"})
		if err != nil {
			t.Fatal(err)
		}
		surface.backupService = service
		application := app.NewAPIServer(scenario.auth, scenario.db, scenario.rootKeyFile)
		if err := application.SetReadOnlyReader(scenario.reader); err != nil {
			t.Fatal(err)
		}
		if err := application.SetBackupService(service); err != nil {
			t.Fatal(err)
		}
		handler, err := app.NewHandler(application, scenario.publicOrigin)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	})
	return surface
}

// TestBackupCommandsOverSameOriginHandler exercises the real cookie, CSRF and
// durable command ledger path rather than calling a handler directly.
func TestBackupCommandsOverSameOriginHandler(t *testing.T) {
	surface := newBackupSurface(t)
	server := surface.server
	admin := surface.sessionHeaders(surface.login(t, "admin", surface.adminPassword))
	first := mustPost(t, server, admin, "/api/v1/backups", `{"clientCommandId":"backup-http-command"}`, http.StatusAccepted)
	replay := mustPost(t, server, admin, "/api/v1/backups", `{"clientCommandId":"backup-http-command"}`, http.StatusAccepted)
	// The accepted POST runs the backup asynchronously; the staging
	// directory keeps moving until the run reaches a terminal state.
	// Waiting for it here keeps t.TempDir's RemoveAll from racing the
	// run goroutine's final writes (a "directory not empty" flake).
	waitBackupTerminal(t, surface.backupService, first.body)
	var a, b struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(first.body), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(replay.body), &b); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("same command did not replay durable row: first=%s replay=%s", first.body, replay.body)
	}
	settings := mustDo(t, server, http.MethodGet, admin, "/api/v1/backups/settings", "", http.StatusOK)
	var projection struct {
		BackupTarget string `json:"backupTarget"`
	}
	if err := json.Unmarshal([]byte(settings.body), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.BackupTarget != surface.backupDir {
		t.Fatalf("backupTarget=%q, want process-visible %q", projection.BackupTarget, surface.backupDir)
	}
}

func TestBackupListRejectsMalformedPagination(t *testing.T) {
	surface := newBackupSurface(t)
	server := surface.server
	admin := surface.sessionHeaders(surface.login(t, "admin", surface.adminPassword))
	for _, endpoint := range []string{"/api/v1/backups?limit=0", "/api/v1/backups?cursor=tampered"} {
		response := mustDo(t, server, http.MethodGet, admin, endpoint, "", http.StatusBadRequest)
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(response.body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "malformed_request" {
			t.Fatalf("%s code=%q", endpoint, body.Code)
		}
	}
}

func TestBackupSettingsScheduleCronJSONPresenceAndNull(t *testing.T) {
	surface := newBackupSurface(t)
	server := surface.server
	admin := surface.sessionHeaders(surface.login(t, "admin", surface.adminPassword))
	mustDo(t, server, http.MethodPut, admin, "/api/v1/backups/settings", `{"clientCommandId":"set-cron-1","expectedRowVersion":1,"scheduleCron":"*/5 * * * *"}`, http.StatusOK)
	settings, err := surface.backupService.Settings(context.Background())
	if err != nil || settings.ScheduleCron == nil || *settings.ScheduleCron != "*/5 * * * *" {
		t.Fatalf("set cron settings=%+v err=%v", settings, err)
	}
	mustDo(t, server, http.MethodPut, admin, "/api/v1/backups/settings", `{"clientCommandId":"preserve-cron-2","expectedRowVersion":2,"retentionCount":3}`, http.StatusOK)
	settings, err = surface.backupService.Settings(context.Background())
	if err != nil || settings.ScheduleCron == nil || *settings.ScheduleCron != "*/5 * * * *" {
		t.Fatalf("omitted cron changed settings=%+v err=%v", settings, err)
	}
	mustDo(t, server, http.MethodPut, admin, "/api/v1/backups/settings", `{"clientCommandId":"clear-cron-3","expectedRowVersion":3,"scheduleCron":null}`, http.StatusOK)
	settings, err = surface.backupService.Settings(context.Background())
	if err != nil || settings.ScheduleCron != nil {
		t.Fatalf("null cron did not clear settings=%+v err=%v", settings, err)
	}
}

// waitBackupTerminal polls the asynchronous run's persisted state to a
// terminal value so the test's TempDir cleanup never races the
// runner's final writes (a "directory not empty" flake under load).
func waitBackupTerminal(t *testing.T, service *backup.Service, accepted string) {
	t.Helper()
	var created struct {
		ID any `json:"id"`
	}
	if err := json.Unmarshal([]byte(accepted), &created); err != nil {
		return
	}
	id, err := strconv.ParseInt(fmt.Sprint(created.ID), 10, 64)
	if err != nil {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		summary, err := service.Get(context.Background(), id)
		if err == nil && (summary.Status == "succeeded" || summary.Status == "failed") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
