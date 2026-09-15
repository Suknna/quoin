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
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
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
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	// The admin's live session: initialized, unexpired, issued at the current
	// auth revision — the proof newCommandContext references.
	if _, err := db.Exec(`INSERT INTO users(username,display_name,role,password_phc,enabled,auth_revision,initialized,created_at,updated_at) VALUES('admin','Admin','admin','hash',1,1,1,?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,?,1,'backup-test-agent',?,?,?,?)`, make([]byte, 32), stamp, stamp, now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO backup_settings(id,enabled,timezone,retention_count,schedule_enabled_at,row_version,updated_at) VALUES(1,1,'UTC',2,?,1,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, Config{DataDirectory: dir, BackupDirectory: filepath.Join(dir, "backup"), ArtifactDirectory: filepath.Join(dir, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	// Release the service-owned reader pool after the test's own defer
	// database.Close(); a leaked pool would keep the WAL sidecar alive in the
	// temporary data directory. Close is ownership-aware, so tests that attach
	// a shared reader keep owning its lifecycle.
	t.Cleanup(func() { _ = service.Close() })
	return service, db
}

// newCommandContext attaches the execution metadata a real HTTP entry point
// would provide: user actor, session proof reference, correlation and request
// identity. Command tests inject it at the entry exactly like production
// wiring will; the runner fails closed without it.
func newCommandContext(t *testing.T, actor int64) context.Context {
	t.Helper()
	stamp := time.Now().UnixNano()
	ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: fmt.Sprintf("test-correlation-%d", stamp),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: actor},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: actor},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: fmt.Sprintf("test-request-%d", stamp)},
		Session:       execution.SessionRef{ID: 1, AuthRevision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestQueueManualDurablyReplaysCommand(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := newCommandContext(t, 1)
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
	var commands, audits, backups int
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE principal_id=1`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.trigger'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if commands != 2 || audits != 2 || backups != 1 {
		t.Fatalf("commands=%d audits=%d backups=%d, want committed trigger plus rejected active command and no business write for the rejection", commands, audits, backups)
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM client_commands WHERE client_command_id='backup-command-2'`).Scan(&outcome); err != nil || outcome != "rejected_known" {
		t.Fatalf("active command outcome=%q err=%v, want rejected_known", outcome, err)
	}
}

// TestCommandWithoutExecutionContextIsRejected pins the fail-closed entry
// contract: without execution metadata the audited commands are refused, and
// no business row, ledger row or audit event is synthesized to mask the gap.
func TestCommandWithoutExecutionContextIsRejected(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	if _, err := service.QueueManual(ctx, 1, "context-free-command"); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("trigger without metadata error=%v, want ErrMissingContext", err)
	}
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "context-free-settings", nil, pointer("UTC"), nil, nil); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("settings without metadata error=%v, want ErrMissingContext", err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("backups=%d err=%v, want no synthesized business row", rows, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("client_commands=%d err=%v, want no synthesized ledger row", rows, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("audit_events=%d err=%v, want no synthesized audit row", rows, err)
	}
}

// TestAuditFailureAfterWriteRollsBackBusiness proves the runner's atomic
// contract: when the automatic audit cannot be persisted, the business stage
// and the command ledger row roll back instead of shipping an unaudited
// mutation.
func TestAuditFailureAfterWriteRollsBackBusiness(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE audit_event_targets`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.QueueManual(newCommandContext(t, 1), 1, "unauditable-command"); err == nil {
		t.Fatal("command unexpectedly succeeded without its audit row")
	}
	var backups, commands int
	if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&backups); err != nil || backups != 0 {
		t.Fatalf("backups=%d err=%v, want business stage rolled back", backups, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("client_commands=%d err=%v, want no forged committed ledger row", commands, err)
	}
}

// TestCommandWithStaleSessionProofIsRejected pins the in-transaction session
// recheck: a context whose session proof references an unknown or expired
// session is refused with ErrActorChanged even though the account row itself
// is still an enabled admin, and nothing is written.
func TestCommandWithStaleSessionProofIsRejected(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	stale := func(t *testing.T, session execution.SessionRef) context.Context {
		t.Helper()
		stamp := time.Now().UnixNano()
		ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
			CorrelationID: fmt.Sprintf("stale-correlation-%d", stamp),
			Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 1},
			Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: fmt.Sprintf("stale-request-%d", stamp)},
			Session:       session,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	for name, session := range map[string]execution.SessionRef{
		"unknownSession": {ID: 999, AuthRevision: 1},
		"staleRevision":  {ID: 1, AuthRevision: 99},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.QueueManual(stale(t, session), 1, "stale-proof-command"); !errors.Is(err, auth.ErrActorChanged) {
				t.Fatalf("stale session proof error=%v, want ErrActorChanged", err)
			}
		})
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("backups=%d err=%v, want no business write on stale proof", rows, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("client_commands=%d err=%v, want no ledger row on stale proof", rows, err)
	}
}

func TestSettingsAndRetentionCommandsDurablyReplay(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := newCommandContext(t, 1)
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

func TestInvalidScheduleSettingsAreRejectedWithoutBusinessWrite(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := newCommandContext(t, 1)
	invalid := "not a cron"
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "invalid-schedule", nil, &invalid, nil, nil); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("invalid cron error=%v, want ErrInvalidSettings", err)
	}
	// The deterministic rejection is durably recorded while the business row
	// stays untouched: no unexpected modification rides with a rejection.
	var rowVersion int64
	if err := db.QueryRow(`SELECT row_version FROM backup_settings WHERE id=1`).Scan(&rowVersion); err != nil || rowVersion != 1 {
		t.Fatalf("settings row_version=%d err=%v, want untouched 1", rowVersion, err)
	}
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM client_commands WHERE client_command_id='invalid-schedule'`).Scan(&outcome); err != nil || outcome != "rejected_known" {
		t.Fatalf("invalid schedule outcome=%q err=%v, want rejected_known", outcome, err)
	}
}

func TestReconcileFailsActiveRunBeforeNewTriggers(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "interrupted-command")
	if err != nil {
		t.Fatal(err)
	}
	// Recovery is a system entry: a user context must not drive it, so
	// Reconcile is invoked without inherited request metadata.
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, err := service.Get(context.Background(), mustID(t, queued.ID))
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != "failed" || value.Stage != "queued" || value.ErrorCode == nil || *value.ErrorCode != "interrupted" {
		t.Fatalf("reconciled value=%+v", value)
	}
	// The restart-recovery fact is audited per run by the system executor.
	var interrupted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.run.interrupted' AND domain_ref_id=?`, mustID(t, queued.ID)).Scan(&interrupted); err != nil {
		t.Fatal(err)
	}
	if interrupted != 1 {
		t.Fatalf("interrupted audit rows=%d, want exactly one per reconciled run", interrupted)
	}
}

// TestRestartRestoresQueuedRunCorrelation proves the restart contract end to
// end: the queue command persists its correlation and initiator on the queued
// row in the same transaction, and a rebuilt service (a fresh process with no
// context of its own) restores them, so every lifecycle fact of the executed
// run lands on the original operation.
func TestRestartRestoresQueuedRunCorrelation(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "restart-command")
	if err != nil {
		t.Fatal(err)
	}
	var commandCorrelation, rowCorrelation string
	if err := db.QueryRow(`SELECT correlation_id FROM client_commands WHERE client_command_id='restart-command'`).Scan(&commandCorrelation); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT correlation_id FROM backups WHERE id=?`, mustID(t, queued.ID)).Scan(&rowCorrelation); err != nil {
		t.Fatal(err)
	}
	if rowCorrelation != commandCorrelation {
		t.Fatalf("queued row correlation=%q, want the command correlation %q", rowCorrelation, commandCorrelation)
	}
	// The restart: same durable state, new service, no inherited context.
	restarted, err := NewService(db, Config{
		DataDirectory: service.config.DataDirectory, BackupDirectory: service.config.BackupDirectory,
		ArtifactDirectory: service.config.ArtifactDirectory, ArtifactStore: service.artifactStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := restarted.Run(context.Background(), queued.ID)
	if err != nil || value.Status != "succeeded" {
		t.Fatalf("restarted run value=%+v err=%v; want succeeded", value, err)
	}
	var correlation, initiatorType string
	var initiatorID int64
	if err := db.QueryRow(`SELECT DISTINCT correlation_id,initiator_type,initiator_id FROM audit_events WHERE action LIKE 'backup.run.%' AND domain_ref_id=?`, mustID(t, queued.ID)).Scan(&correlation, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	if correlation != commandCorrelation || initiatorType != "user" || initiatorID != 1 {
		t.Fatalf("restored lifecycle facts correlation=%q initiator=%s/%d, want %q user/1", correlation, initiatorType, initiatorID, commandCorrelation)
	}
}

// TestInterruptedRecoveryFactsCarryRunCorrelation pins the recovery-link
// rule: a correlated run's interruption is recorded on the run's own
// correlation, while a legacy correlation-less row is recovered as a
// deliberate, separately correlated operation — its NULL correlation is kept
// as the historical fact and never backfilled.
func TestInterruptedRecoveryFactsCarryRunCorrelation(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "interrupted-correlated")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var commandCorrelation, correlation, initiatorType string
	var initiatorID int64
	if err := db.QueryRow(`SELECT correlation_id FROM client_commands WHERE client_command_id='interrupted-correlated'`).Scan(&commandCorrelation); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT correlation_id,initiator_type,initiator_id FROM audit_events WHERE action='backup.run.interrupted' AND domain_ref_id=?`, mustID(t, queued.ID)).Scan(&correlation, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	if correlation != commandCorrelation || initiatorType != "user" || initiatorID != 1 {
		t.Fatalf("interrupted fact correlation=%q initiator=%s/%d, want the run's %q user/1", correlation, initiatorType, initiatorID, commandCorrelation)
	}

	// A legacy correlation-less queued row (pre-migration shape).
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,scheduled_for,row_version,created_at,updated_at,correlation_id,initiator_type) VALUES('queued','queued','scheduled','online',?,1,?,?,NULL,NULL)`, at, at, at); err != nil {
		t.Fatal(err)
	}
	var legacyID int64
	if err := db.QueryRow(`SELECT id FROM backups WHERE correlation_id IS NULL`).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var legacyRowCorrelation sql.NullString
	var recoveryCorrelation string
	if err := db.QueryRow(`SELECT correlation_id FROM backups WHERE id=?`, legacyID).Scan(&legacyRowCorrelation); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT correlation_id FROM audit_events WHERE action='backup.run.interrupted' AND domain_ref_id=?`, legacyID).Scan(&recoveryCorrelation); err != nil {
		t.Fatal(err)
	}
	if legacyRowCorrelation.Valid {
		t.Fatalf("legacy row correlation backfilled to %q, want NULL preserved", legacyRowCorrelation.String)
	}
	if recoveryCorrelation == "" || recoveryCorrelation == commandCorrelation {
		t.Fatalf("legacy recovery fact correlation=%q, want the deliberate recovery operation's own correlation", recoveryCorrelation)
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
	// The scheduler entry owns its system scope: the queued boundary carries an
	// automatic audit row from the shared runner.
	var actorType string
	if err := db.QueryRow(`SELECT actor_type FROM audit_events WHERE action='backup.schedule.queue'`).Scan(&actorType); err != nil || actorType != "system" {
		t.Fatalf("scheduled queue audit actor=%q err=%v, want system", actorType, err)
	}
	rows, err := service.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TriggerKind != "scheduled" || rows[0].ScheduledFor == nil || *rows[0].ScheduledFor != "2026-01-02T10:05:00Z" {
		t.Fatalf("catch-up rows=%+v", rows)
	}
}

// TestScheduleCatchUpWithMetricsDoesNotHoldTheOnlyConnection proves that the
// scheduler's final metrics projection happens only after its admission
// connection is returned to the production-sized SQLite pool.
func TestScheduleCatchUpWithMetricsDoesNotHoldTheOnlyConnection(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	base := time.Date(2026, 1, 2, 10, 5, 0, 0, time.UTC)
	service.now = func() time.Time { return base }
	if _, err := db.Exec(`DROP TRIGGER trg_backup_settings_schedule_enabled_at_transition`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE backup_settings SET schedule_cron='*/5 * * * *',timezone='UTC',schedule_enabled_at=?,updated_at=?,row_version=row_version+1`, "2026-01-02T09:50:00Z", "2026-01-02T09:50:00Z"); err != nil {
		t.Fatal(err)
	}
	server, err := sharedops.New("quoin", ":0", sharedops.Ready)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := server.BackupMetrics()
	if err != nil {
		t.Fatal(err)
	}
	service.SetMetrics(metrics)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	if err := service.CatchUp(ctx); err != nil {
		t.Fatalf("catch up with one SQLite connection: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("catch up waited %s for its own SQLite connection", elapsed)
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
	if _, err := service.UpdateSettingsCommand(newCommandContext(t, 1), 1, 2, "keep-schedule-anchor", nil, nil, nil, &retention); err != nil {
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
	service.now = func() time.Time { return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) }
	disabled := false
	settings, err := service.UpdateSettingsCommand(newCommandContext(t, 1), 1, 1, "disable-schedule", &disabled, nil, nil, nil)
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
	settings, err = service.UpdateSettingsCommand(newCommandContext(t, 1), 1, settings.RowVersion, "reenable-schedule", &enabled, nil, nil, nil)
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
	if _, err := service.UpdateSettingsCommand(newCommandContext(t, 1), 1, settings.RowVersion, "preserve-schedule-anchor", nil, nil, nil, &retention); err != nil {
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
	if err := service.RecordDownloadAudit(newCommandContext(t, 1), 1, mustID(t, value.ID)); err != nil {
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
	if _, err := service.QueueManual(newCommandContext(t, 1), 1, "metric-command"); err != nil {
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

func TestNoOpSettingsAndRetentionCommandsStayReplayableWithAutomaticAudit(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := newCommandContext(t, 1)

	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-empty-command", nil, nil, nil, nil); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("empty settings command error=%v; want invalid settings", err)
	}
	// A field that is present but unchanged is a valid, replayable no-op. The
	// runner records it durably and writes the automatic audit row — business
	// code has no switch to suppress auditing, so replay must not duplicate it.
	enabled := true
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-noop-command", &enabled, nil, nil, nil); err != nil {
		t.Fatalf("settings no-op error=%v", err)
	}
	if _, err := service.UpdateArtifactRetentionCommand(ctx, 1, 1, 90, "retention-noop-command"); err != nil {
		t.Fatalf("retention no-op error=%v", err)
	}
	if _, err := service.UpdateSettingsCommand(ctx, 1, 1, "settings-noop-command", &enabled, nil, nil, nil); err != nil {
		t.Fatalf("replayed settings no-op error=%v", err)
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
	if commands != 2 || audits != 2 {
		t.Fatalf("commands=%d audits=%d; want two ledger rows with exactly one automatic audit each (replays add none)", commands, audits)
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
	if _, err = service.UpdateSettingsCommand(newCommandContext(t, 1), 1, 1, "retain-one", nil, nil, nil, &retention); err != nil {
		t.Fatal(err)
	}
	service.removeAll = func(path string) error {
		if path == filepath.Join(service.config.BackupDirectory, first.ID) {
			return errors.New("simulated retained backup deletion failure")
		}
		return os.RemoveAll(path)
	}
	second, err := service.RunOffline(ctx)
	if err != nil || second.Status != "succeeded" {
		t.Fatalf("second backup=%+v err=%v; want succeeded run despite cleanup failure", second, err)
	}
	health, err := service.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt == nil || health.ErrorDetail == nil {
		t.Fatalf("retention health=%+v err=%v; want durable cleanup failure", health, err)
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
	health, err = service.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt != nil || health.ErrorDetail != nil {
		t.Fatalf("retention health=%+v err=%v; want cleared after retry", health, err)
	}
	// Only health transitions are written and audited: the initial healthy
	// row, the first failure, and its recovery — stable passes write nothing.
	var healthAudits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='backup.retention.health'`).Scan(&healthAudits); err != nil {
		t.Fatal(err)
	}
	if healthAudits != 3 {
		t.Fatalf("retention health audits=%d, want initialized, failed and recovered transitions only", healthAudits)
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
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "cancel-after-publish")
	if err != nil {
		t.Fatal(err)
	}
	// The Run task scope: the system task executor on its own correlation,
	// cancellable so the post-publish commit fails the way a cancelled caller
	// does. A user or HTTP context would be refused by the lifecycle guard.
	taskCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "cancel-after-publish-correlation",
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(taskCtx)
	defer cancel()
	service.afterPublish = cancel
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
	queued, err := service.QueueManual(newCommandContext(t, 1), 1, "reconcile-failed-publish")
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

func TestListPageWithLatestReturnsSuccessOutsideCurrentPage(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	base := time.Now().UTC()
	insert := func(status, stage string, completed time.Time) {
		t.Helper()
		at := completed.Format(time.RFC3339Nano)
		if status == "succeeded" {
			_, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?,?,1)`, status, stage, "manual", "online", strings.Repeat("a", 64), strings.Repeat("b", 64), 0, 1, "backup/manifest.json", at, at, at, at)
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		_, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,row_version,created_at,updated_at,started_at,completed_at,triggered_by,error_code,retryable,error_detail) VALUES(?,?,?,?,1,?,?,?,?,1,'failed',1,'test')`, status, stage, "manual", "online", at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("succeeded", "completed", base)
	for i := 0; i < 30; i++ {
		insert("failed", "preflight", base.Add(time.Duration(i+1)*time.Second))
	}
	page, next, latest, err := service.ListPageWithLatest(ctx, 0, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 30 || next == nil {
		t.Fatalf("page=%d next=%v, want 30 rows and cursor", len(page), next)
	}
	if latest == nil || latest.Status != "succeeded" {
		t.Fatalf("latest=%+v, want older succeeded run", latest)
	}
}

func TestRetentionCleanupPreservesFailureOnCancelledPassAndRetriesMissingManifestResidue(t *testing.T) {
	service, db := newServiceForTest(t)
	defer db.Close()
	ctx := context.Background()
	first, err := service.RunOffline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retention := int64(1)
	if _, err = service.UpdateSettingsCommand(newCommandContext(t, 1), 1, 1, "retain-one-partial", nil, nil, nil, &retention); err != nil {
		t.Fatal(err)
	}
	firstRoot := filepath.Join(service.config.BackupDirectory, first.ID)
	partial := true
	service.removeAll = func(path string) error {
		if path == firstRoot && partial {
			partial = false
			if removeErr := os.Remove(filepath.Join(path, "manifest.json")); removeErr != nil {
				return removeErr
			}
			return errors.New("simulated partial deletion")
		}
		return os.RemoveAll(path)
	}
	if _, err = service.RunOffline(ctx); err != nil {
		t.Fatalf("new backup must remain succeeded despite retention failure: %v", err)
	}
	health, err := service.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt == nil {
		t.Fatalf("health after partial deletion=%+v err=%v", health, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = service.GC(cancelled); err == nil {
		t.Fatal("cancelled cleanup unexpectedly succeeded")
	}
	health, err = service.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt == nil {
		t.Fatalf("cancelled cleanup cleared known failure: %+v err=%v", health, err)
	}

	service.removeAll = os.RemoveAll
	if err = service.GC(ctx); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if _, err = os.Stat(firstRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("residue after missing manifest was not removed: %v", err)
	}
	health, err = service.RetentionHealth(ctx)
	if err != nil || health.LastFailureAt != nil || health.ErrorDetail != nil {
		t.Fatalf("successful durable retry did not clear health: %+v err=%v", health, err)
	}
}
