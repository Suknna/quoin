package audit

// Retention tests execute against the applied contract schema
// (internal/gen/contracts/schema.sql), which carries the permit cutoff guard,
// the delete-guard triggers and the cleanup batch ledger, so cleanup is
// verified against the same database protections main applies. Seeded events
// are placed relative to the real clock because the triggers validate against
// the trusted SQLite 'now'.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	_ "modernc.org/sqlite"
)

func newRetentionDB(t *testing.T, cleanupEnabled bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "retention.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	enabled := 0
	if cleanupEnabled {
		enabled = 1
	}
	if _, err := db.Exec(`INSERT INTO audit_retention(id, retention_months, cleanup_enabled) VALUES(1, 6, ?)`, enabled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audit_cleanup_permits(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedRetentionEvent inserts an event recorded `monthsAgo` months before the
// real current time, with the given correlation ("" keeps the historical
// no-correlation shape) and one target when withTarget is set.
func seedRetentionEvent(t *testing.T, db *sql.DB, monthsAgo int, action, correlation string, withTarget bool) int64 {
	t.Helper()
	createdAt := CanonicalTimestamp(time.Now().UTC().AddDate(0, -monthsAgo, 0))
	var correlationArg any
	if correlation != "" {
		correlationArg = correlation
	}
	res, err := db.Exec(`INSERT INTO audit_events(actor_type,actor_id,action,outcome,correlation_id,created_at)
		VALUES('user', 7, ?, 'success', ?, ?)`, action, correlationArg, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if withTarget {
		if _, err := db.Exec(`INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?, 'item', ?)`, id, id*10); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func retentionCounts(t *testing.T, db *sql.DB) (events, targets int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM audit_events),
			(SELECT COUNT(*) FROM audit_event_targets)`).
		Scan(&events, &targets); err != nil {
		t.Fatal(err)
	}
	return events, targets
}

// cleanupRecord is the explicit acting record a scheduled root would map from
// its system execution metadata.
func cleanupRecord() Record {
	return Record{
		ActorType:     ActorSystem,
		ActorID:       0,
		CorrelationID: "cleanup-run-corr-0123456789abcde",
		InitiatorType: ActorSystem,
		InitiatorID:   0,
	}
}

// armPermit arms the singleton permit the way a live run would (controller
// cutoff margin included) and reports an arming failure.
func armPermit(t *testing.T, db *sql.DB, acquiredAt time.Time) {
	t.Helper()
	cutoff := CanonicalTimestamp(CutoffUTC(time.Now().UTC(), 6).Add(-time.Second))
	if _, err := db.Exec(`UPDATE audit_cleanup_permits
		SET active = 1, cutoff_at = ?, upper_event_id = (SELECT COALESCE(MAX(id),0) FROM audit_events), acquired_at = ?
		WHERE id = 1`, cutoff, CanonicalTimestamp(acquiredAt)); err != nil {
		t.Fatal(err)
	}
}

func TestReadSettingsReturnsSingletonDefaults(t *testing.T) {
	db := newRetentionDB(t, false)
	settings, err := ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.RetentionMonths != 6 || settings.MinRetentionMonths != 6 || settings.CleanupEnabled {
		t.Fatalf("settings months=%d enabled=%v, want 6/false", settings.RetentionMonths, settings.CleanupEnabled)
	}
	if settings.RowVersion != 1 || settings.Cleanup.LastRunAt != "" {
		t.Fatalf("settings row_version=%d status=%+v", settings.RowVersion, settings.Cleanup)
	}
}

func TestUpdateSettingsOnMutatesInsideCallerTransaction(t *testing.T) {
	db := newRetentionDB(t, false)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	settings, err := UpdateSettingsOn(ctx, tx, SettingsUpdate{
		RetentionMonths: 12, CleanupEnabled: &enabled, ExpectedRowVersion: 1,
		ActorType: ActorUser, ActorID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if settings.RetentionMonths != 12 || !settings.CleanupEnabled || settings.RowVersion != 2 {
		t.Fatalf("updated settings months=%d enabled=%v rv=%d", settings.RetentionMonths, settings.CleanupEnabled, settings.RowVersion)
	}
	if settings.UpdatedByType != ActorUser || settings.UpdatedByID != 7 || settings.UpdatedAt == "" {
		t.Fatalf("update actor=%s/%d at=%q", settings.UpdatedByType, settings.UpdatedByID, settings.UpdatedAt)
	}
	persisted, err := ReadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RetentionMonths != 12 || persisted.RowVersion != 2 {
		t.Fatalf("persisted months=%d rv=%d", persisted.RetentionMonths, persisted.RowVersion)
	}

	if _, err := UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 12, ExpectedRowVersion: 1, ActorType: ActorUser, ActorID: 7,
	}); !errors.Is(err, ErrSettingsConflict) {
		t.Fatalf("stale row_version err=%v, want ErrSettingsConflict", err)
	}
	if _, err := UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 5, ExpectedRowVersion: 2, ActorType: ActorUser, ActorID: 7,
	}); !errors.Is(err, ErrRetentionTooShort) {
		t.Fatalf("short retention err=%v, want ErrRetentionTooShort", err)
	}
	if _, err := UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 9, ExpectedRowVersion: 2, ActorType: "robot", ActorID: 7,
	}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("bad actor err=%v, want ErrInvalidSettings", err)
	}
	// Nil CleanupEnabled keeps the current flag.
	settings, err = UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 9, ExpectedRowVersion: 2, ActorType: ActorSystem, ActorID: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.RetentionMonths != 9 || !settings.CleanupEnabled || settings.RowVersion != 3 {
		t.Fatalf("nil-enabled update months=%d enabled=%v rv=%d", settings.RetentionMonths, settings.CleanupEnabled, settings.RowVersion)
	}
}

func TestCutoffUTCClampsToNaturalMonth(t *testing.T) {
	cases := []struct {
		now    time.Time
		months int
		want   time.Time
	}{
		{time.Date(2026, 9, 15, 10, 30, 0, 500, time.UTC), 6, time.Date(2026, 3, 15, 10, 30, 0, 500, time.UTC)},
		{time.Date(2026, 3, 31, 8, 0, 0, 0, time.UTC), 1, time.Date(2026, 2, 28, 8, 0, 0, 0, time.UTC)},
		{time.Date(2024, 3, 31, 23, 59, 59, 0, time.UTC), 1, time.Date(2024, 2, 29, 23, 59, 59, 0, time.UTC)},
		{time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), 1, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), 24, time.Date(2024, 12, 31, 12, 0, 0, 0, time.UTC)},
		{time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), 12, time.Date(2025, 5, 31, 0, 0, 0, 0, time.UTC)},
	}
	for _, testCase := range cases {
		got := CutoffUTC(testCase.now, testCase.months)
		if !got.Equal(testCase.want) {
			t.Fatalf("CutoffUTC(%s, %d)=%s, want %s", testCase.now, testCase.months, got, testCase.want)
		}
	}
}

func TestPreviewSettingsEstimatesExpirableImpact(t *testing.T) {
	db := newRetentionDB(t, false)
	if _, err := db.Exec(`UPDATE audit_retention SET retention_months = 12 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	correlations := []string{"corr-shared", "corr-shared", ""}
	for i, monthsAgo := range []int{7, 8, 9} {
		seedRetentionEvent(t, db, monthsAgo, "old.event", correlations[i], false)
	}
	seedRetentionEvent(t, db, 1, "new.event", "", false)

	preview, err := PreviewSettings(context.Background(), db, 6, now)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RetentionMonths != 6 || preview.CurrentRetentionMonths != 12 || !preview.Shortening {
		t.Fatalf("preview months=%d current=%d shortening=%v", preview.RetentionMonths, preview.CurrentRetentionMonths, preview.Shortening)
	}
	if preview.CutoffAt != CanonicalTimestamp(CutoffUTC(now, 6)) {
		t.Fatalf("preview cutoff=%s", preview.CutoffAt)
	}
	if preview.EstimatedExpirableEvents != 3 || preview.EstimatedExpirableCorrelations != 1 {
		t.Fatalf("preview events=%d correlations=%d, want 3/1", preview.EstimatedExpirableEvents, preview.EstimatedExpirableCorrelations)
	}
	if _, err := PreviewSettings(context.Background(), db, 5, now); !errors.Is(err, ErrRetentionTooShort) {
		t.Fatalf("preview months=5 err=%v, want ErrRetentionTooShort", err)
	}
}

func TestCleanupDeletesOnlyExpiredBatchesWithWriterRecords(t *testing.T) {
	db := newRetentionDB(t, true)
	ctx := context.Background()
	var oldEvents []int64
	for i := 0; i < 5; i++ {
		oldEvents = append(oldEvents, seedRetentionEvent(t, db, 8, "old.event", "", true))
	}
	newEvent := seedRetentionEvent(t, db, 1, "new.event", "", true)

	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Cleanup(ctx, cleanupRecord())
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedEvents != 5 || result.DeletedTargets != 5 || result.Batches != 1 {
		t.Fatalf("result batches=%d events=%d targets=%d, want 1/5/5", result.Batches, result.DeletedEvents, result.DeletedTargets)
	}
	// The recent business event and its target survive; the writer added one
	// batch audit event (no targets) for the single batch.
	events, targets := retentionCounts(t, db)
	if events != 2 || targets != 1 {
		t.Fatalf("remaining events=%d targets=%d, want the recent event with its target plus one batch record", events, targets)
	}
	var recent int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'old.event'`).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if recent != 0 {
		t.Fatalf("%d expired events survived", recent)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_event_targets WHERE audit_event_id = ?`, newEvent).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if recent != 1 {
		t.Fatal("recent event lost its target")
	}

	// The batch ledger holds one final row; its audit event carries the run
	// correlation and the batch row as domain reference.
	var cutoff string
	var upper, deletedEvents, deletedTargets, finalRow int64
	if err := db.QueryRow(`SELECT cutoff_at,upper_event_id,deleted_events,deleted_targets,final
		FROM audit_cleanup_batches`).Scan(&cutoff, &upper, &deletedEvents, &deletedTargets, &finalRow); err != nil {
		t.Fatal(err)
	}
	// The armed bound is the highest existing id (recent events included;
	// the cutoff check is what protects them).
	if finalRow != 1 || deletedEvents != 5 || deletedTargets != 5 || upper != newEvent {
		t.Fatalf("batch row cutoff=%s upper=%d events=%d targets=%d final=%d (want upper=%d)", cutoff, upper, deletedEvents, deletedTargets, finalRow, newEvent)
	}
	var action, refType, correlation string
	var refID int64
	if err := db.QueryRow(`SELECT action,domain_ref_type,domain_ref_id,correlation_id FROM audit_events
		WHERE domain_ref_type = 'audit_cleanup_batch'`).Scan(&action, &refType, &refID, &correlation); err != nil {
		t.Fatal(err)
	}
	if action != cleanupAction || refID == 0 || len(correlation) != 32 {
		t.Fatalf("batch audit action=%s ref=%s/%d correlation=%q", action, refType, refID, correlation)
	}

	settings, err := ReadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastSuccessCutoffAt != result.CutoffAt {
		t.Fatalf("success cutoff=%q, want %q", settings.Cleanup.LastSuccessCutoffAt, result.CutoffAt)
	}
	if settings.Cleanup.LastSuccessDeletedCount != 5 || settings.Cleanup.LastRunAt == "" || settings.Cleanup.LastErrorCode != "" {
		t.Fatalf("success status=%+v", settings.Cleanup)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("permit still active after run")
	}

	// The survivor stays protected by the release triggers even afterwards.
	if _, err := db.Exec(`DELETE FROM audit_events WHERE id = ?`, newEvent); err == nil {
		t.Fatal("delete without permit succeeded")
	}
}

func TestCleanupSplitsBoundedBatches(t *testing.T) {
	db := newRetentionDB(t, true)
	for i := 0; i < 5; i++ {
		seedRetentionEvent(t, db, 8, "old.event", "", false)
	}
	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.batchSize = 2
	result, err := controller.Cleanup(context.Background(), cleanupRecord())
	if err != nil {
		t.Fatal(err)
	}
	if result.Batches != 3 || result.DeletedEvents != 5 {
		t.Fatalf("result batches=%d events=%d, want 3/5", result.Batches, result.DeletedEvents)
	}
	var batches, finals int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(final),0) FROM audit_cleanup_batches`).Scan(&batches, &finals); err != nil {
		t.Fatal(err)
	}
	if batches != 3 || finals != 1 {
		t.Fatalf("batch rows=%d finals=%d, want 3/1", batches, finals)
	}
	// Every expired event is gone; what remains is one batch audit event per
	// batch recorded by the writer.
	events, _ := retentionCounts(t, db)
	if events != 3 {
		t.Fatalf("events=%d left, want only the 3 batch audit records", events)
	}
	var expired int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'old.event'`).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Fatalf("%d expired events survived", expired)
	}
}

func TestCleanupEmptyHistoryRecordsCompletedPass(t *testing.T) {
	db := newRetentionDB(t, true)
	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Cleanup(context.Background(), cleanupRecord())
	if err != nil {
		t.Fatal(err)
	}
	if result.Batches != 0 || result.DeletedEvents != 0 {
		t.Fatalf("empty run result=%+v", result)
	}
	var finalRow int64
	if err := db.QueryRow(`SELECT final FROM audit_cleanup_batches`).Scan(&finalRow); err != nil {
		t.Fatal(err)
	}
	if finalRow != 1 {
		t.Fatalf("empty pass final=%d", finalRow)
	}
	settings, err := ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastSuccessCutoffAt == "" {
		t.Fatalf("empty pass status=%+v", settings.Cleanup)
	}
}

func TestCleanupRespectsDisabledAndActiveGuards(t *testing.T) {
	db := newRetentionDB(t, false)
	seedRetentionEvent(t, db, 8, "old.event", "", false)
	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Cleanup(context.Background(), cleanupRecord()); !errors.Is(err, ErrCleanupDisabled) {
		t.Fatalf("disabled cleanup err=%v, want ErrCleanupDisabled", err)
	}
	events, _ := retentionCounts(t, db)
	if events != 1 {
		t.Fatalf("disabled run deleted rows")
	}

	// Arm the permit as a live run would (only possible with cleanup enabled;
	// the schema refuses arming a disabled policy); a second run must be
	// refused and leave the armed permit untouched. The helper arms one
	// second earlier than the exact clamp so the guard's float comparison
	// cannot flip.
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE audit_retention SET cleanup_enabled = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	armPermit(t, db, time.Now().UTC())
	if _, err := controller.Cleanup(ctx, cleanupRecord()); !errors.Is(err, ErrCleanupActive) {
		t.Fatalf("active cleanup err=%v, want ErrCleanupActive", err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatal("first run's permit was released by the refused second run")
	}
	events, _ = retentionCounts(t, db)
	if events != 1 {
		t.Fatalf("refused run deleted rows")
	}
}

type failingWriter struct{}

func (f *failingWriter) RecordCleanupBatch(ctx context.Context, tx *sql.Tx, record Record, batch CleanupBatch) error {
	return errors.New("injected writer failure")
}

// finalFailingWriter records non-final batches through the real writer and
// injects a failure exactly when the run would declare itself complete.
type finalFailingWriter struct {
	inner *Writer
}

func (f *finalFailingWriter) RecordCleanupBatch(ctx context.Context, tx *sql.Tx, record Record, batch CleanupBatch) error {
	if batch.Final {
		return errors.New("injected final failure")
	}
	return f.inner.RecordCleanupBatch(ctx, tx, record, batch)
}

func TestCleanupWriterFailureRollsBackAndRecordsFailure(t *testing.T) {
	db := newRetentionDB(t, true)
	seedRetentionEvent(t, db, 8, "old.event", "", true)
	controller, err := NewController(db, &failingWriter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Cleanup(context.Background(), cleanupRecord()); err == nil || strings.Contains(err.Error(), "nil") {
		t.Fatalf("cleanup err=%v, want injected failure", err)
	}
	// The rolled-back batch keeps its data; the failure is recorded and the
	// permit is free again.
	events, targets := retentionCounts(t, db)
	if events != 1 || targets != 1 {
		t.Fatalf("failed run lost data: events=%d targets=%d", events, targets)
	}
	var batches int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_cleanup_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("failed run recorded %d batches", batches)
	}
	settings, err := ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastErrorCode != "cleanup_failed" || settings.Cleanup.LastFailureAt == "" {
		t.Fatalf("failure status=%+v", settings.Cleanup)
	}
	if settings.Cleanup.LastSuccessCutoffAt != "" {
		t.Fatalf("failed run declared success: %+v", settings.Cleanup)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("permit left active after failure")
	}
	// A healthy run afterwards recovers and completes.
	if _, err := db.Exec(`DELETE FROM audit_cleanup_batches`); err != nil {
		t.Fatal(err)
	}
	recovered := NewWriter()
	controller2, err := NewController(db, recovered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller2.Cleanup(context.Background(), cleanupRecord()); err != nil {
		t.Fatalf("recovery run err=%v", err)
	}
	settings, err = ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastErrorCode != "" || settings.Cleanup.LastSuccessDeletedCount != 1 {
		t.Fatalf("recovery status=%+v", settings.Cleanup)
	}
}

func TestPermitTriggersProtectHistoryWithoutArmedRun(t *testing.T) {
	db := newRetentionDB(t, true)
	oldEvent := seedRetentionEvent(t, db, 8, "old.event", "", false)
	newEvent := seedRetentionEvent(t, db, 1, "new.event", "", true)

	// No permit: even eight-month-old history cannot be deleted.
	if _, err := db.Exec(`DELETE FROM audit_events WHERE id = ?`, oldEvent); err == nil {
		t.Fatal("delete without permit succeeded")
	}

	// The permit cutoff guard refuses a cutoff inside the retention window.
	if _, err := db.Exec(`UPDATE audit_cleanup_permits
		SET active = 1, cutoff_at = ?, upper_event_id = (SELECT MAX(id) FROM audit_events), acquired_at = ?
		WHERE id = 1`, CanonicalTimestamp(time.Now().UTC()), CanonicalTimestamp(time.Now().UTC())); err == nil {
		t.Fatal("arming a permit with an in-retention cutoff succeeded")
	}

	// A validly armed permit covers only rows at or below its upper id and
	// older than its cutoff; like the controller, it arms one second earlier
	// than the exact clamp because the schema guard compares float paths of
	// the same instant whose double rounding flips an exact boundary.
	validCutoff := CanonicalTimestamp(CutoffUTC(time.Now().UTC(), 6).Add(-time.Second))
	if _, err := db.Exec(`UPDATE audit_cleanup_permits
		SET active = 1, cutoff_at = ?, upper_event_id = 1, acquired_at = ?
		WHERE id = 1`, validCutoff, CanonicalTimestamp(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM audit_events WHERE id = ?`, newEvent); err == nil {
		t.Fatal("delete of in-retention event succeeded")
	}
	if _, err := db.Exec(`DELETE FROM audit_event_targets WHERE audit_event_id = ?`, newEvent); err == nil {
		t.Fatal("delete of in-retention target succeeded")
	}
	if _, err := db.Exec(`DELETE FROM audit_events WHERE id = ?`, oldEvent); err != nil {
		t.Fatalf("delete of expired event under permit failed: %v", err)
	}
}

func TestNewControllerRequiresSharedWriter(t *testing.T) {
	db := newRetentionDB(t, true)
	if _, err := NewController(nil, NewWriter(), nil); !errors.Is(err, ErrNilWriter) {
		t.Fatalf("nil db err=%v, want ErrNilWriter", err)
	}
	if _, err := NewController(db, nil, nil); !errors.Is(err, ErrNilWriter) {
		t.Fatalf("nil writer err=%v, want ErrNilWriter", err)
	}
}

func TestCleanupRejectsMissingOrNonSystemRecord(t *testing.T) {
	db := newRetentionDB(t, true)
	seedRetentionEvent(t, db, 8, "old.event", "", true)
	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rejected := []struct {
		name   string
		record Record
	}{
		{"empty record", Record{}},
		{"user principal", Record{ActorType: ActorUser, ActorID: 7, CorrelationID: "corr-x", InitiatorType: ActorUser, InitiatorID: 7}},
		{"system actor with id", Record{ActorType: ActorSystem, ActorID: 9, CorrelationID: "corr-x", InitiatorType: ActorSystem}},
		{"missing correlation", Record{ActorType: ActorSystem, InitiatorType: ActorSystem}},
		{"user initiator", Record{ActorType: ActorSystem, CorrelationID: "corr-x", InitiatorType: ActorUser, InitiatorID: 7}},
		{"initiator without type", Record{ActorType: ActorSystem, CorrelationID: "corr-x", InitiatorID: 0}},
	}
	for _, testCase := range rejected {
		if _, err := controller.Cleanup(context.Background(), testCase.record); !errors.Is(err, ErrCleanupRecordRequired) {
			t.Fatalf("%s: err=%v, want ErrCleanupRecordRequired", testCase.name, err)
		}
	}
	// A rejected run never armed the permit, recorded batches or deleted rows.
	events, targets := retentionCounts(t, db)
	if events != 1 || targets != 1 {
		t.Fatalf("rejected run deleted data: events=%d targets=%d", events, targets)
	}
	var batches int64
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_cleanup_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || active != 0 {
		t.Fatalf("rejected run touched state: batches=%d permitActive=%d", batches, active)
	}
}

func TestUpdateSettingsOnRejectsWhileCleanupActive(t *testing.T) {
	db := newRetentionDB(t, true)
	ctx := context.Background()
	armPermit(t, db, time.Now().UTC())

	// A fresh permit means a live run: policy changes must wait so the armed
	// cutoff can never delete beyond a newly configured window.
	if _, err := UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 12, ExpectedRowVersion: 1, ActorType: ActorUser, ActorID: 7,
	}); !errors.Is(err, ErrCleanupRunning) {
		t.Fatalf("settings during cleanup err=%v, want ErrCleanupRunning", err)
	}
	settings, err := ReadSettings(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.RetentionMonths != 6 || settings.RowVersion != 1 {
		t.Fatalf("rejected update persisted months=%d rv=%d", settings.RetentionMonths, settings.RowVersion)
	}

	// A permit stale beyond the takeover bound no longer blocks: a crashed
	// run must not freeze retention policy forever.
	if _, err := db.Exec(`UPDATE audit_cleanup_permits SET acquired_at = ? WHERE id = 1`,
		CanonicalTimestamp(time.Now().UTC().Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSettingsOn(ctx, db, SettingsUpdate{
		RetentionMonths: 12, ExpectedRowVersion: 1, ActorType: ActorUser, ActorID: 7,
	}); err != nil {
		t.Fatalf("settings after stale permit err=%v", err)
	}
}

func TestCleanupTakesOverStalePermit(t *testing.T) {
	db := newRetentionDB(t, true)
	seedRetentionEvent(t, db, 8, "old.event", "", false)
	seedRetentionEvent(t, db, 8, "old.event", "", false)
	// A crashed run left the permit armed two hours ago.
	armPermit(t, db, time.Now().UTC().Add(-2*time.Hour))

	controller, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Cleanup(context.Background(), cleanupRecord())
	if err != nil {
		t.Fatalf("stale permit takeover err=%v", err)
	}
	if result.DeletedEvents != 2 {
		t.Fatalf("takeover result=%+v, want both expired events deleted", result)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("permit left active after takeover run")
	}
}

func TestCleanupWriterFailureOnFinalBatchRollsBackCompletion(t *testing.T) {
	db := newRetentionDB(t, true)
	for i := 0; i < 3; i++ {
		seedRetentionEvent(t, db, 8, "old.event", "", false)
	}
	controller, err := NewController(db, &finalFailingWriter{inner: NewWriter()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.batchSize = 2
	if _, err := controller.Cleanup(context.Background(), cleanupRecord()); err == nil {
		t.Fatal("final batch failure not surfaced")
	}
	// The first (non-final) batch stays committed; the failed final batch
	// leaves its event in place and never declares success.
	var remaining int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'old.event'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining expired events=%d, want the final batch's one", remaining)
	}
	var batches, finals int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(final),0) FROM audit_cleanup_batches`).Scan(&batches, &finals); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || finals != 0 {
		t.Fatalf("batch rows=%d finals=%d, want only the committed non-final batch", batches, finals)
	}
	settings, err := ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastSuccessCutoffAt != "" || settings.Cleanup.LastErrorCode != "cleanup_failed" {
		t.Fatalf("status=%+v, want failure recorded without success", settings.Cleanup)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("permit left active after final batch failure")
	}
	// A healthy run afterwards completes the backlog and declares success.
	recovered, err := NewController(db, NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovered.Cleanup(context.Background(), cleanupRecord())
	if err != nil {
		t.Fatalf("recovery run err=%v", err)
	}
	if result.DeletedEvents != 1 {
		t.Fatalf("recovery deleted=%d, want the final backlog event", result.DeletedEvents)
	}
	settings, err = ReadSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cleanup.LastErrorCode != "" || settings.Cleanup.LastSuccessDeletedCount != 1 {
		t.Fatalf("recovery status=%+v", settings.Cleanup)
	}
}
