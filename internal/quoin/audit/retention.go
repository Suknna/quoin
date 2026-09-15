// Retention settings, preview and bounded cleanup (docs/audit-design.md §7).
//
// The database is the retention authority: settings live in the audit_retention
// singleton, and deletes are only possible while the audit_cleanup_permits
// singleton is armed by an active cleanup run — schema triggers validate the
// cutoff against the configured retention and the trusted SQLite clock, and
// refuse every delete outside the armed range. This file only drives that
// mechanism; it never deletes audit rows outside a permitted batch.
//
// Concurrency contract: settings changes are mutated by UpdateSettingsOn
// INSIDE the caller's runner-owned transaction — this package never commits
// business commands and never imports execution, so the runner wraps these
// functions with authorization, ledger and audit records. Only the background
// cleanup controller opens private transactions, and every batch record goes
// through the required shared writer so audit maintenance never bypasses (or
// recurses into) the audit path.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MinRetentionMonths is the design floor (docs/audit-design.md §7); the
// schema CHECK enforces it independently.
const MinRetentionMonths = 6

// defaultCleanupBatchSize bounds every delete transaction; batches stay short
// so no single transaction holds the write lock for a whole backlog.
const defaultCleanupBatchSize = 500

// permitStaleAfter allows recovering the singleton permit if a crashed run
// never released it; a live run finishes far below this bound.
const permitStaleAfter = time.Hour

// cleanupAction is the system operation recorded for every cleanup batch.
const cleanupAction = "audit.retention.cleanup"

var (
	// ErrRetentionTooShort rejects configured retention below six months.
	ErrRetentionTooShort = errors.New("audit: retention below the six natural month minimum")
	// ErrSettingsConflict reports an optimistic row_version mismatch.
	ErrSettingsConflict = errors.New("audit: retention settings changed concurrently")
	// ErrInvalidSettings rejects malformed updates (unknown actor, missing data).
	ErrInvalidSettings = errors.New("audit: invalid retention settings update")
	// ErrCleanupDisabled reports cleanup_enabled=0; existing history stays safe.
	ErrCleanupDisabled = errors.New("audit: retention cleanup is not enabled")
	// ErrCleanupActive reports another cleanup run holding the permit.
	ErrCleanupActive = errors.New("audit: another retention cleanup run is active")
	// ErrPermitRejected reports the schema guard refusing to arm the permit
	// (cutoff outside configured retention or guard inconsistency).
	ErrPermitRejected = errors.New("audit: cleanup permit rejected by retention guard")
	// ErrCleanupRecordRequired rejects a run without an explicit system
	// record (principal, initiator and correlation from the scheduled root);
	// identity is never defaulted.
	ErrCleanupRecordRequired = errors.New("audit: cleanup requires an explicit system record with correlation and initiator")
	// ErrCleanupRunning rejects retention policy changes while a cleanup run
	// holds a fresh permit, so an armed cutoff can never race a retention
	// change into deleting beyond the newly configured window.
	ErrCleanupRunning = errors.New("audit: retention settings cannot change while cleanup is running")
	// ErrNilWriter rejects a controller without the required shared writer.
	ErrNilWriter = errors.New("audit: cleanup requires the shared audit writer")
)

// CleanupStatus is the latest cleanup run outcome stored beside the settings
// (mirrors the admin API projection).
type CleanupStatus struct {
	LastRunAt               string
	LastSuccessCutoffAt     string
	LastSuccessDeletedCount int64
	LastFailureAt           string
	// LastErrorCode is a stable mapped code, never a raw driver message.
	LastErrorCode string
}

// Settings is the retention singleton row.
type Settings struct {
	RetentionMonths    int
	MinRetentionMonths int
	CleanupEnabled     bool
	RowVersion         int64
	UpdatedAt          string
	UpdatedByType      string
	UpdatedByID        int64
	Cleanup            CleanupStatus
}

// ReadSettings reads the singleton; sql.ErrNoRows passes through when the
// bootstrap has not seeded it yet.
func ReadSettings(ctx context.Context, r Reader) (Settings, error) {
	var (
		settings  Settings
		enabled   int
		lastRun   sql.NullString
		successAt sql.NullString
		successN  sql.NullInt64
		failureAt sql.NullString
		errorCode sql.NullString
		updatedBy sql.NullString
		updatedID sql.NullInt64
		updatedAt sql.NullString
	)
	err := r.QueryRowContext(ctx, `SELECT retention_months, cleanup_enabled, row_version,
			last_run_at, last_success_cutoff_at, last_success_deleted_events,
			last_failure_at, last_error_code, updated_by_type, updated_by_id, updated_at
		FROM audit_retention WHERE id = 1`).
		Scan(&settings.RetentionMonths, &enabled, &settings.RowVersion,
			&lastRun, &successAt, &successN, &failureAt, &errorCode,
			&updatedBy, &updatedID, &updatedAt)
	if err != nil {
		return Settings{}, err
	}
	settings.MinRetentionMonths = MinRetentionMonths
	settings.CleanupEnabled = enabled == 1
	settings.Cleanup = CleanupStatus{
		LastRunAt:               lastRun.String,
		LastSuccessCutoffAt:     successAt.String,
		LastSuccessDeletedCount: successN.Int64,
		LastFailureAt:           failureAt.String,
		LastErrorCode:           errorCode.String,
	}
	settings.UpdatedAt = updatedAt.String
	settings.UpdatedByType = updatedBy.String
	settings.UpdatedByID = updatedID.Int64
	return settings, nil
}

// Tx is the in-transaction mutation seam. It is satisfied by *sql.DB,
// *sql.Conn, *sql.Tx and the execution runner's transaction wrapper, so this
// package never imports execution (execution imports audit).
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SettingsUpdate changes the retention singleton. CleanupEnabled is nil to
// keep the current flag; the caller's runner records the command ledger and
// audit event in the same transaction.
type SettingsUpdate struct {
	RetentionMonths    int
	CleanupEnabled     *bool
	ExpectedRowVersion int64
	ActorType          string
	ActorID            int64
}

// UpdateSettingsOn applies the optimistic retention change inside the caller's
// transaction (no BEGIN/COMMIT here). The row_version guard fails with
// ErrSettingsConflict so shortening always goes through its preview and
// confirmation cycle before landing.
func UpdateSettingsOn(ctx context.Context, tx Tx, update SettingsUpdate) (Settings, error) {
	if update.RetentionMonths < MinRetentionMonths {
		return Settings{}, ErrRetentionTooShort
	}
	switch update.ActorType {
	case ActorUser, ActorService, ActorSystem:
	default:
		return Settings{}, fmt.Errorf("%w: actor_type %q", ErrInvalidSettings, update.ActorType)
	}
	// A live cleanup run holds the permit with an armed cutoff snapshot;
	// changing retention mid-run could make that snapshot delete beyond the
	// newly configured window, so policy changes wait until the run ends.
	// The schema delete guard stays as the per-row backstop.
	var active int
	err := tx.QueryRowContext(ctx, `SELECT active FROM audit_cleanup_permits
		WHERE id = 1 AND active = 1 AND acquired_at >= ?`,
		CanonicalTimestamp(time.Now().Add(-permitStaleAfter))).Scan(&active)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Settings{}, fmt.Errorf("check cleanup permit: %w", err)
	}
	if err == nil {
		return Settings{}, ErrCleanupRunning
	}
	var enabled any
	if update.CleanupEnabled != nil {
		enabled = 0
		if *update.CleanupEnabled {
			enabled = 1
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE audit_retention
		SET retention_months = ?, cleanup_enabled = COALESCE(?, cleanup_enabled),
			row_version = row_version + 1, updated_by_type = ?, updated_by_id = ?, updated_at = ?
		WHERE id = 1 AND row_version = ?`,
		update.RetentionMonths, enabled, update.ActorType, update.ActorID,
		CanonicalTimestamp(time.Now()), update.ExpectedRowVersion)
	if err != nil {
		return Settings{}, fmt.Errorf("update retention settings: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT row_version FROM audit_retention WHERE id = 1`).Scan(&current); err != nil {
			return Settings{}, err
		}
		return Settings{}, ErrSettingsConflict
	}
	return ReadSettings(ctx, tx)
}

// SettingsPreview is the impact estimate shown before a retention change; the
// estimate is never a deletion promise.
type SettingsPreview struct {
	RetentionMonths                int
	CurrentRetentionMonths         int
	Shortening                     bool
	CutoffAt                       string
	EstimatedExpirableEvents       int64
	EstimatedExpirableCorrelations int64
}

// PreviewSettings estimates what the configured retention would expire,
// including the first-ever cleanup impact for existing history.
func PreviewSettings(ctx context.Context, r Reader, retentionMonths int, now time.Time) (SettingsPreview, error) {
	if retentionMonths < MinRetentionMonths {
		return SettingsPreview{}, ErrRetentionTooShort
	}
	current, err := ReadSettings(ctx, r)
	if err != nil {
		return SettingsPreview{}, err
	}
	cutoff := CanonicalTimestamp(CutoffUTC(now, retentionMonths))
	preview := SettingsPreview{
		RetentionMonths:        retentionMonths,
		CurrentRetentionMonths: current.RetentionMonths,
		Shortening:             retentionMonths < current.RetentionMonths,
		CutoffAt:               cutoff,
	}
	if err := r.QueryRowContext(ctx, `SELECT
			COUNT(*),
			COUNT(DISTINCT CASE WHEN correlation_id IS NOT NULL AND correlation_id <> '' THEN correlation_id END)
		FROM audit_events WHERE julianday(created_at) < julianday(?)`, cutoff).
		Scan(&preview.EstimatedExpirableEvents, &preview.EstimatedExpirableCorrelations); err != nil {
		return SettingsPreview{}, fmt.Errorf("estimate expirable audit events: %w", err)
	}
	return preview, nil
}

// CutoffUTC subtracts retentionMonths natural months from now in UTC and
// clamps the day of month to the target month's last valid day
// (2026-03-31 minus one month is 2026-02-28, never March 3rd via rollover).
func CutoffUTC(now time.Time, retentionMonths int) time.Time {
	if retentionMonths < 0 {
		retentionMonths = 0
	}
	now = now.UTC()
	y, m, d := now.Date()
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -retentionMonths, 0)
	lastDay := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, -1).Day()
	if d > lastDay {
		d = lastDay
	}
	hour, min, sec := now.Clock()
	return time.Date(first.Year(), first.Month(), d, hour, min, sec, now.Nanosecond(), time.UTC)
}

// CleanupBatchWriter is the required shared-writer seam: each batch row and
// its audit event are recorded inside the batch transaction by the audit
// writer, so audit maintenance never produces un-audited or recursive audit
// writes.
type CleanupBatchWriter interface {
	RecordCleanupBatch(ctx context.Context, tx *sql.Tx, record Record, batch CleanupBatch) error
}

// Controller runs retention cleanup as a system operation. Callers must pass
// an explicit system execution context (never a user request context).
type Controller struct {
	db        *sql.DB
	writer    CleanupBatchWriter
	now       func() time.Time
	batchSize int
}

// NewController requires the shared writer; there is no direct-insert
// fallback, so cleanup can never record batches outside the audit path.
func NewController(db *sql.DB, writer CleanupBatchWriter, now func() time.Time) (*Controller, error) {
	if db == nil || writer == nil {
		return nil, ErrNilWriter
	}
	if now == nil {
		now = time.Now
	}
	return &Controller{db: db, writer: writer, now: now, batchSize: defaultCleanupBatchSize}, nil
}

// CleanupResult summarizes one cleanup run.
type CleanupResult struct {
	CutoffAt       string
	Batches        int
	DeletedEvents  int64
	DeletedTargets int64
}

// Cleanup expires events older than the configured retention in bounded
// batches. The acting record must carry the system principal, the system
// initiator and the run correlation from the caller's scheduled root context:
// audit cannot read execution's context (execution imports audit), so the
// scheduler maps its execution metadata into the record explicitly and
// identity is never defaulted to system. Every batch (deletes +
// writer-recorded batch row, plus the final permit release and success
// status) commits atomically; any failure rolls the batch back, keeps the
// data, records the failure and surfaces the error.
func (c *Controller) Cleanup(ctx context.Context, acting Record) (CleanupResult, error) {
	batchRecord, err := cleanupBatchRecord(acting)
	if err != nil {
		return CleanupResult{}, err
	}
	settings, err := ReadSettings(ctx, c.db)
	if err != nil {
		return CleanupResult{}, fmt.Errorf("read retention settings: %w", err)
	}
	if !settings.CleanupEnabled {
		return CleanupResult{}, ErrCleanupDisabled
	}
	if err := ctx.Err(); err != nil {
		return CleanupResult{}, err
	}
	now := c.now().UTC()
	cutoff := CutoffUTC(now, settings.RetentionMonths)
	// Arm one second earlier than the exact clamp: the schema guard compares
	// two float computation paths of the same instant at julian-day
	// magnitude, where a single double ulp is ~40µs, so an exact-equality
	// boundary flips arbitrarily. One second dwarfs that noise while being
	// semantically negligible (it only keeps one more second of history).
	armedCutoff := CanonicalTimestamp(cutoff.Add(-time.Second))
	upper, err := c.acquire(ctx, armedCutoff, CanonicalTimestamp(now), CanonicalTimestamp(now.Add(-permitStaleAfter)))
	if err != nil {
		// Acquire failures never touch the release path: the permit either
		// belongs to another live run or was never armed by this one.
		return CleanupResult{}, err
	}
	result := CleanupResult{CutoffAt: armedCutoff}
	if upper == 0 {
		// Nothing recorded yet: still close the run through the writer so the
		// batch ledger shows the completed (empty) pass.
		err = c.finishEmpty(ctx, result.CutoffAt, CanonicalTimestamp(now), batchRecord)
	} else {
		result, err = c.runBatches(ctx, upper, cutoff.Add(-time.Second), batchRecord, result, CanonicalTimestamp(now))
	}
	if err != nil {
		if markErr := c.markFailure(ctx, now, err); markErr != nil {
			return CleanupResult{}, errors.Join(err, markErr)
		}
		return CleanupResult{}, err
	}
	return result, nil
}

// cleanupBatchRecord forces the stable cleanup action and outcome onto the
// caller's system principal and run correlation. A missing or non-system
// record is rejected — retention cleanup is a system operation and its
// identity is never invented here (docs/audit-design.md §3).
func cleanupBatchRecord(acting Record) (Record, error) {
	if acting.ActorType != ActorSystem || acting.ActorID != 0 ||
		acting.InitiatorType != ActorSystem || acting.InitiatorID != 0 ||
		acting.CorrelationID == "" {
		return Record{}, ErrCleanupRecordRequired
	}
	return Record{
		ActorType:     ActorSystem,
		ActorID:       0,
		Action:        cleanupAction,
		Outcome:       QueryOutcomeSuccess,
		CorrelationID: acting.CorrelationID,
		InitiatorType: ActorSystem,
		InitiatorID:   0,
	}, nil
}

func (c *Controller) runBatches(ctx context.Context, upper int64, armedCutoff time.Time, batchRecord Record, result CleanupResult, nowText string) (CleanupResult, error) {
	// Select one millisecond earlier than the armed cutoff: the delete
	// triggers compare millisecond-rounded julianday values, so rows within
	// the last millisecond before the cutoff stay selectable for a later run
	// instead of aborting this batch.
	selectCutoff := CanonicalTimestamp(armedCutoff.Add(-time.Millisecond))
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		done, err := c.runBatch(ctx, batchRun{
			upper:        upper,
			selectCutoff: selectCutoff,
			cutoffAt:     result.CutoffAt,
			record:       batchRecord,
			nowText:      nowText,
			result:       &result,
		})
		if err != nil {
			return result, err
		}
		if done {
			return result, nil
		}
	}
}

type batchRun struct {
	upper        int64
	selectCutoff string
	cutoffAt     string
	record       Record
	nowText      string
	result       *CleanupResult
}

// acquire arms the singleton permit inside one transaction and returns the
// upper event id snapshot; the schema trigger independently validates the
// cutoff against the configured retention and the trusted SQLite clock. A
// permit left active by a crashed run older than the staleness bound is taken
// over; a fresh live run's permit refuses the arm.
func (c *Controller) acquire(ctx context.Context, cutoff, acquiredAt, staleBefore string) (int64, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin cleanup acquire: %w", err)
	}
	defer rollbackTx(tx)
	var upper int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_events`).Scan(&upper); err != nil {
		return 0, fmt.Errorf("snapshot audit event bound: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_permits
		SET active = 1, cutoff_at = ?, upper_event_id = ?, acquired_at = ?
		WHERE id = 1 AND (active = 0 OR acquired_at < ?)`,
		cutoff, upper, acquiredAt, staleBefore)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrPermitRejected, err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT active FROM audit_cleanup_permits WHERE id = 1`).Scan(&active); err != nil {
			return 0, fmt.Errorf("read cleanup permit: %w", err)
		}
		return 0, ErrCleanupActive
	}
	return upper, tx.Commit()
}

// runBatch deletes one bounded batch atomically with its writer-recorded batch
// row; the final batch also releases the permit and records the run success.
// It reports whether the run is complete.
func (c *Controller) runBatch(ctx context.Context, run batchRun) (bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin cleanup batch: %w", err)
	}
	defer rollbackTx(tx)

	ids, err := selectBatch(ctx, tx, run.selectCutoff, run.upper, c.batchSize)
	if err != nil {
		return false, err
	}
	final := len(ids) < c.batchSize
	var deletedTargets int64
	if len(ids) > 0 {
		deletedTargets, err = deleteBatch(ctx, tx, ids)
		if err != nil {
			return false, err
		}
		run.result.Batches++
		run.result.DeletedEvents += int64(len(ids))
		run.result.DeletedTargets += deletedTargets
	}
	batch := CleanupBatch{
		CutoffAt:       run.cutoffAt,
		UpperEventID:   run.upper,
		DeletedEvents:  int64(len(ids)),
		DeletedTargets: deletedTargets,
		Final:          final,
		CreatedAt:      run.nowText,
	}
	if err := c.writer.RecordCleanupBatch(ctx, tx, run.record, batch); err != nil {
		return false, fmt.Errorf("record cleanup batch: %w", err)
	}
	if final {
		if err := finishSuccess(ctx, tx, run.nowText, run.cutoffAt, run.result); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit cleanup batch: %w", err)
	}
	return final, nil
}

// finishEmpty closes a run that found no recorded events at all.
func (c *Controller) finishEmpty(ctx context.Context, cutoff, nowText string, record Record) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cleanup completion: %w", err)
	}
	defer rollbackTx(tx)
	batch := CleanupBatch{CutoffAt: cutoff, Final: true, CreatedAt: nowText}
	if err := c.writer.RecordCleanupBatch(ctx, tx, record, batch); err != nil {
		return fmt.Errorf("record cleanup batch: %w", err)
	}
	var result CleanupResult
	if err := finishSuccess(ctx, tx, nowText, cutoff, &result); err != nil {
		return err
	}
	return tx.Commit()
}

func finishSuccess(ctx context.Context, tx *sql.Tx, nowText, cutoff string, result *CleanupResult) error {
	if _, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_permits SET active = 0, acquired_at = '' WHERE id = 1`); err != nil {
		return fmt.Errorf("release cleanup permit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_retention
		SET last_run_at = ?, last_success_cutoff_at = ?, last_success_deleted_events = ?,
			last_failure_at = NULL, last_error_code = NULL
		WHERE id = 1`, nowText, cutoff, result.DeletedEvents); err != nil {
		return fmt.Errorf("record cleanup success: %w", err)
	}
	return nil
}

// markFailure releases the permit and records the failed run with a stable
// error code; committed batches stay committed and their data stays deleted,
// while everything else is preserved for the next run.
func (c *Controller) markFailure(ctx context.Context, now time.Time, cause error) error {
	code := "cleanup_failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		code = "cancelled"
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("begin cleanup failure record: %w", err))
	}
	defer rollbackTx(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_permits SET active = 0, acquired_at = '' WHERE id = 1`); err != nil {
		return errors.Join(cause, fmt.Errorf("release cleanup permit: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit_retention
		SET last_run_at = ?, last_failure_at = ?, last_error_code = ? WHERE id = 1`,
		CanonicalTimestamp(now), CanonicalTimestamp(now), code); err != nil {
		return errors.Join(cause, fmt.Errorf("record cleanup failure: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(cause, fmt.Errorf("commit cleanup failure record: %w", err))
	}
	return nil
}

// deleteBatch removes one batch's target rows first, then their events, in
// the caller's transaction guarded by the armed permit triggers.
func deleteBatch(ctx context.Context, tx *sql.Tx, ids []int64) (int64, error) {
	placeholders, args := placeholdersFor(ids)
	res, err := tx.ExecContext(ctx, `DELETE FROM audit_event_targets
		WHERE audit_event_id IN (`+placeholders+")", args...)
	if err != nil {
		return 0, fmt.Errorf("delete expired audit targets: %w", err)
	}
	deletedTargets, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE id IN (`+placeholders+")", idsToAny(ids)...); err != nil {
		return 0, fmt.Errorf("delete expired audit events: %w", err)
	}
	return deletedTargets, nil
}

func selectBatch(ctx context.Context, tx *sql.Tx, selectCutoff string, upper int64, limit int) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM audit_events
		WHERE created_at < ? AND id <= ? ORDER BY id LIMIT ?`, selectCutoff, upper, limit)
	if err != nil {
		return nil, fmt.Errorf("select expired audit events: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired audit event: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func placeholdersFor(ids []int64) (string, []any) {
	placeholders := ""
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	return placeholders, args
}

func idsToAny(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func rollbackTx(tx *sql.Tx) {
	_ = tx.Rollback() // sql.ErrTxDone after a successful Commit is fine
}
