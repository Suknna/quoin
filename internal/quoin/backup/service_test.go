package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	sharedops "github.com/Suknna/quoin/internal/ops"
	_ "modernc.org/sqlite"
)

func newServiceForTest(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "quoin.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO backup_settings(id,enabled,timezone,retention_count,schedule_enabled_at,row_version,updated_at) VALUES(1,1,'UTC',2,?,1,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(username,display_name,role,password_phc,enabled,auth_revision,created_at,updated_at) VALUES('admin','Admin','admin','hash',1,1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, Config{DataDirectory: dir, BackupDirectory: filepath.Join(dir, "backup"), ArtifactDirectory: filepath.Join(dir, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	return service, db
}

func TestQueueManualDurablyReplaysCommand(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	first, err := service.QueueManual(ctx, 1, "backup-command-1")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.QueueManual(ctx, 1, "backup-command-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != replayed.ID {
		t.Fatalf("replay id=%s, want %s", replayed.ID, first.ID)
	}
	if _, err := service.QueueManual(ctx, 1, "backup-command-2"); !errors.Is(err, ErrActive) {
		t.Fatalf("second command error=%v, want ErrActive", err)
	}
	var commands, audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE principal_id=1`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.trigger'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if commands != 2 || audits != 2 {
		t.Fatalf("commands=%d audits=%d, want committed trigger plus rejected active command", commands, audits)
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM client_commands WHERE client_command_id='backup-command-2'`).Scan(&outcome); err != nil || outcome != "rejected_known" {
		t.Fatalf("active command outcome=%q err=%v, want rejected_known", outcome, err)
	}
}
func TestSettingsAndRetentionCommandsDurablyReplay(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	enabled := false
	first, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-command", &enabled, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-command", &enabled, nil, nil, nil)
	if err != nil || replay.RowVersion != first.RowVersion || replay.Enabled != first.Enabled {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = service.UpdateSettingsCommand(ctx, 1, 1, "settings-command", nil, nil, pointer("UTC"), nil); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("reused settings command err=%v", err)
	}
	retention, err := service.UpdateArtifactRetentionCommand(ctx, 1, 1, 7, "retention-command")
	if err != nil {
		t.Fatal(err)
	}
	retentionReplay, err := service.UpdateArtifactRetentionCommand(ctx, 1, 1, 7, "retention-command")
	if err != nil || retentionReplay.RowVersion != retention.RowVersion {
		t.Fatalf("retention replay=%+v err=%v", retentionReplay, err)
	}
}

func TestInvalidScheduleSettingsAreRejectedBeforePersistence(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	invalid := "not a cron"
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "invalid-schedule", nil, &invalid, nil, nil); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

func TestReconcileFailsActiveRunBeforeNewTriggers(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	queued, err := service.QueueManual(ctx, 1, "interrupted-command")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	value, err := service.Get(ctx, mustID(t, queued.ID))
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "failed" || value.Stage != "queued" || value.ErrorCode == nil || *value.ErrorCode != "interrupted" {
		t.Fatalf("reconciled value=%+v", value)
	}
}

func TestScheduleCatchupQueuesOnlyLatestBoundary(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	base := time.Date(2026, 1, 2, 10, 5, 0, 0, time.UTC)
	service.now = func() time.Time { return base }
	// Seed an historical pre-migration setting; production cannot rewrite this
	// anchor without an enabled transition (covered separately below).
	if _, err := db.Exec(`DROP TRIGGER trg_backup_settings_schedule_enabled_at_transition`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE backup_settings SET schedule_cron='*/5 * * * *',timezone='UTC',schedule_enabled_at=?,updated_at=?,row_version=row_version+1`, "2026-01-02T09:50:00Z", "2026-01-02T09:50:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := service.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := service.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TriggerKind != "scheduled" || rows[0].ScheduledFor == nil || *rows[0].ScheduledFor != "2026-01-02T10:05:00Z" {
		t.Fatalf("catch-up rows=%+v", rows)
	}
}

func TestScheduleEnabledAtIgnoresUnrelatedSettingsEdits(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	service.now = func() time.Time { return time.Date(2026, 1, 2, 10, 5, 0, 0, time.UTC) }
	if _, err := db.Exec(`DROP TRIGGER trg_backup_settings_schedule_enabled_at_transition`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE backup_settings SET schedule_cron='*/5 * * * *', schedule_enabled_at='2026-01-02T09:50:00Z', updated_at='2026-01-02T10:00:00Z', row_version=row_version+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	retention := int64(7)
	if _, err := service.UpdateSettingsCommand(context.Background(), 1, 2, "keep-schedule-anchor", nil, nil, nil, &retention); err != nil {
		t.Fatal(err)
	}
	if err := service.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := service.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ScheduledFor == nil || *rows[0].ScheduledFor != "2026-01-02T10:05:00Z" {
		t.Fatalf("catch-up after unrelated setting edit=%+v", rows)
	}
}

func TestScheduleEnabledAtRejectsMutationWithoutEnabledTransition(t *testing.T) {
	_, db := newServiceForTest(t)
	defer db.Close()
	if _, err := db.Exec(`UPDATE backup_settings SET schedule_enabled_at='2026-01-02T10:00:00Z',row_version=row_version+1 WHERE id=1`); err == nil {
		t.Fatal("schedule anchor changed without enabled transition")
	}
}

func TestScheduleEnabledAtTracksDisableAndReenable(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	service.now = func() time.Time { return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) }
	disabled := false
	settings, err := service.UpdateSettingsCommand(ctx, 1, 1, "disable-schedule", &disabled, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var anchor sql.NullString
	if err := db.QueryRow(`SELECT schedule_enabled_at FROM backup_settings WHERE id=1`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if anchor.Valid {
		t.Fatalf("disabled schedule anchor=%q, want NULL", anchor.String)
	}
	service.now = func() time.Time { return time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC) }
	enabled := true
	settings, err = service.UpdateSettingsCommand(ctx, 1, settings.RowVersion, "reenable-schedule", &enabled, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT schedule_enabled_at FROM backup_settings WHERE id=1`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if !anchor.Valid || anchor.String != "2026-01-02T11:00:00Z" {
		t.Fatalf("reenabled schedule anchor=%q valid=%t", anchor.String, anchor.Valid)
	}
	retention := int64(8)
	service.now = func() time.Time { return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) }
	if _, err := service.UpdateSettingsCommand(ctx, 1, settings.RowVersion, "preserve-schedule-anchor", nil, nil, nil, &retention); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT schedule_enabled_at FROM backup_settings WHERE id=1`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if !anchor.Valid || anchor.String != "2026-01-02T11:00:00Z" {
		t.Fatalf("unrelated update changed schedule anchor=%q valid=%t", anchor.String, anchor.Valid)
	}
}

func TestSucceededBackupProjectsExactArchiveSetSize(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()

	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(service.config.BackupDirectory, value.ID)
	var want int64
	for _, name := range []string{"manifest.json", "quoin.db"} {
		info, statErr := os.Stat(filepath.Join(root, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		want += info.Size()
	}
	if value.SizeBytes != want {
		t.Fatalf("sizeBytes=%d, want archive-set member bytes %d", value.SizeBytes, want)
	}
	var persisted int64
	if err := db.QueryRow(`SELECT size_bytes FROM backups WHERE id=?`, mustID(t, value.ID)).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != want {
		t.Fatalf("persisted size_bytes=%d, want %d", persisted, want)
	}
}

func TestSucceededArchiveIsVerifiedAndAuditedBeforeDownload(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "succeeded" {
		t.Fatalf("status=%s", value.Status)
	}
	var body bytes.Buffer
	if err := service.WriteArchive(context.Background(), mustID(t, value.ID), &body); err != nil {
		t.Fatal(err)
	}
	if body.Len() == 0 {
		t.Fatal("verified archive was empty")
	}
	if err := service.RecordDownloadAudit(context.Background(), 1, mustID(t, value.ID)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.download_started' AND domain_ref_id=?`, mustID(t, value.ID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("download audit count=%d, want 1", count)
	}
}

func TestMetricsProjectActiveAndInterruptedFailure(t *testing.T) {
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
	if _, err := service.QueueManual(context.Background(), 1, "metric-command"); err != nil {
		t.Fatal(err)
	}
	metricsText := func() string {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		return recorder.Body.String()
	}
	if !strings.Contains(metricsText(), "quoin_backup_active 1") || !strings.Contains(metricsText(), "process_start_time_seconds") {
		t.Fatalf("queued backup/process baseline was not projected:\n%s", metricsText())
	}
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := metricsText()
	if !strings.Contains(body, "quoin_backup_active 0") || !strings.Contains(body, "quoin_backup_failures_total 1") {
		t.Fatalf("reconciled metrics not exact:\n%s", body)
	}
}

func mustID(t *testing.T, value string) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscan(value, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNoOpSettingsAndRetentionCommandsRecordLedgerWithoutAudit(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-empty-command", nil, nil, nil, nil); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("empty settings command error=%v; want invalid settings", err)
	}
	// A field that is present but unchanged is a valid, replayable no-op.
	enabled := true
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-noop-command", &enabled, nil, nil, nil); err != nil {
		t.Fatalf("settings no-op error=%v", err)
	}
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-noop-command", &enabled, nil, nil, nil); err != nil {
		t.Fatalf("replayed settings no-op error=%v", err)
	}
	if _, err := service.UpdateArtifactRetentionCommand(ctx, 1, 1, 90, "retention-noop-command"); err != nil {
		t.Fatalf("retention no-op error=%v", err)
	}
	if _, err := service.UpdateArtifactRetentionCommand(ctx, 1, 1, 90, "retention-noop-command"); err != nil {
		t.Fatalf("replayed retention no-op error=%v", err)
	}

	var commands, audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE client_command_id IN ('settings-noop-command','retention-noop-command')`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('backup.settings.update','artifact_retention.update')`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if commands != 2 || audits != 0 {
		t.Fatalf("commands=%d audits=%d; want two ledger rows and no state-change audits", commands, audits)
	}
}

func TestRetentionCleanupRetriesOnSchedulerPass(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()

	first, err := service.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retention := int64(1)
	if _, err = service.UpdateSettingsCommand(ctx, 1, 1, "retain-one", nil, nil, nil, &retention); err != nil {
		t.Fatal(err)
	}
	service.removeAll = func(path string) error {
		if path == filepath.Join(service.config.BackupDirectory, first.ID) {
			return errors.New("simulated retained backup deletion failure")
		}
		return os.RemoveAll(path)
	}
	second, err := service.RunOffline(ctx)
	if err == nil || second.Status != "succeeded" {
		t.Fatalf("second backup=%+v err=%v; want succeeded run and surfaced cleanup failure", second, err)
	}
	if _, statErr := os.Stat(filepath.Join(service.config.BackupDirectory, first.ID)); statErr != nil {
		t.Fatalf("retained backup removed despite injected failure: %v", statErr)
	}

	service.removeAll = os.RemoveAll
	service.runDue(ctx)
	if _, statErr := os.Stat(filepath.Join(service.config.BackupDirectory, first.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("scheduler did not retry retained cleanup: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(service.config.BackupDirectory, second.ID)); statErr != nil {
		t.Fatalf("latest backup was removed by retention retry: %v", statErr)
	}
}

func TestRunWithFixedClockReachesSucceededTerminalState(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	fixed := time.Date(2026, 3, 1, 1, 2, 3, 0, time.UTC)
	service.now = func() time.Time { return fixed }
	value, err := service.RunOffline(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "succeeded" || value.CompletedAt == nil || *value.CompletedAt == value.CreatedAt {
		t.Fatalf("fixed-clock run did not advance durable state: %+v", value)
	}
}

func TestCancelledPostPublishCommitCleansManifestAndRecordsFailure(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	service.afterPublish = cancel
	queued, err := service.QueueManual(ctx, 1, "cancel-after-publish")
	if err != nil {
		t.Fatal(err)
	}
	value, err := service.Run(ctx, queued.ID)
	if err == nil || value.Status != "failed" {
		t.Fatalf("post-publish cancellation value=%+v err=%v; want failed state", value, err)
	}
	if _, statErr := os.Stat(filepath.Join(service.config.BackupDirectory, queued.ID, "manifest.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed run retained published manifest: %v", statErr)
	}
}

func TestReconcileDeletesLeakedArchiveAndFailedPublishedDirectory(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	queued, err := service.QueueManual(ctx, 1, "reconcile-failed-publish")
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t, queued.ID)
	if _, err = db.Exec(`UPDATE backups SET status='failed',completed_at='2026-03-01T00:00:01Z',updated_at='2026-03-01T00:00:01Z',error_code='backup_failed',retryable=1,error_detail='test',row_version=row_version+1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(service.config.BackupDirectory, queued.ID)
	if err = os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "manifest.json"), []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	leakedArchive := filepath.Join(service.config.BackupDirectory, ".archive-crashed.tar")
	if err = os.WriteFile(leakedArchive, []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, leakedArchive} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reconcile retained %s: %v", path, statErr)
		}
	}
}
