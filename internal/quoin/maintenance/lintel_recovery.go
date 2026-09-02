package maintenance

// Lintel recovery offline finalization (T35, OPS-HELPER-005 /
// VERIFY-RECOVERY-002..004): the stopped, same-Release `quoin maintenance
// recover-lintel --phase finalize` command owns the single SQLite authority
// for the recovery receipt. It validates the frozen fence digests, requires
// the replacement credential's real first-authenticated Hello fact, closes
// the retired storage disposition in the domain (stop confirmations, identity
// demotion — never reclassifying old results), retires the old credential,
// appends the immutable receipt, marks the checklist Safe and exits the
// helper-owned maintenance revision in one transaction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
)

var (
	// ErrLintelRecoveryInput reports malformed finalize inputs; no state is
	// touched.
	ErrLintelRecoveryInput = errors.New("lintel recovery finalize inputs are invalid")
	// ErrLintelRecoveryNotReady reports the durable state does not satisfy
	// the frozen pre-transaction post-verify proofs (missing first-auth fact,
	// fence mismatch, wrong slot shape, no active recovery).
	ErrLintelRecoveryNotReady = errors.New("lintel recovery state is not ready for finalization")
	// ErrLintelRecoveryReceiptConflict reports the idempotence key already
	// exists with different evidence digests.
	ErrLintelRecoveryReceiptConflict = errors.New("lintel recovery receipt exists with different evidence")
)

// LintelRecoveryFinalizeRequest carries only non-secret evidence digests
// computed by the deployment helper from its own backend observations.
// OldGeneration, when non-zero, pins the frozen idempotence tuple
// (old_slot_id, old_token_generation, disposition_digest) for replays that
// must match a specific historical recovery.
type LintelRecoveryFinalizeRequest struct {
	DataDirectory string
	RootKeyFile   string
	Disposition   string // exclusively_reattached | retired
	OldGeneration int64

	DispositionDigest    string
	FenceReportDigest    string
	RecoveryReportDigest string
	PostVerifyDigest     string
}

// LintelRecoveryFinalizeResult is non-secret terminal evidence.
type LintelRecoveryFinalizeResult struct {
	MaintenanceRevision   int64
	OldGeneration         int64
	ReplacementGeneration int64
	ClosedOperations      int64
	DemotedIdentities     int64
	AlreadyFinalized      bool
}

// FinalizeLintelRecovery runs the stopped offline finalizer.
func FinalizeLintelRecovery(ctx context.Context, request LintelRecoveryFinalizeRequest) (LintelRecoveryFinalizeResult, error) {
	if request.Disposition != "exclusively_reattached" && request.Disposition != "retired" {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: storage disposition must be exclusively_reattached or retired", ErrLintelRecoveryInput)
	}
	for name, digest := range map[string]string{
		"disposition": request.DispositionDigest, "fence-report": request.FenceReportDigest,
		"recovery-report": request.RecoveryReportDigest, "post-verify": request.PostVerifyDigest,
	} {
		if len(digest) != 64 || strings.ToLower(digest) != digest || strings.ContainsAny(digest, "ghijklmnopqrstuvwxyz") {
			return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: %s digest must be lowercase sha256 hex", ErrLintelRecoveryInput, name)
		}
	}
	lock, err := sharedops.AcquireDirectory(request.DataDirectory)
	if err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	defer lock.Close()
	db, err := openRecoveryDatabase(ctx, filepath.Join(request.DataDirectory, "quoin.db"))
	if err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	defer db.Close()
	if err := verifyRecoveryDatabase(ctx, db); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	return finalizeLintelRecoveryOn(ctx, db, request)
}

func openRecoveryDatabase(ctx context.Context, databasePath string) (*sql.DB, error) {
	info, err := os.Lstat(databasePath)
	if err != nil {
		return nil, fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("database must be a regular file")
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)&_pragma=synchronous(FULL)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func finalizeLintelRecoveryOn(ctx context.Context, db *sql.DB, request LintelRecoveryFinalizeRequest) (LintelRecoveryFinalizeResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if active == 0 {
		return alreadyFinalizedResult(ctx, conn, request)
	}
	if reason != "LintelRecovery" {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: maintenance %s is active", ErrLintelRecoveryNotReady, reason)
	}
	// The frozen fence must match the finalize evidence exactly.
	rows, err := conn.QueryContext(ctx, `SELECT object_key FROM maintenance_items WHERE maintenance_revision=? AND kind='LintelRecoveryFence'`, revision)
	if err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	frozen := map[string]struct{}{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return LintelRecoveryFinalizeResult{}, err
		}
		frozen[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	expected := map[string]struct{}{
		"disposition:" + request.DispositionDigest:  {},
		"fence-report:" + request.FenceReportDigest: {},
	}
	if len(frozen) != len(expected) || !sameKeys(frozen, expected) {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: frozen fence digests do not match the finalize evidence", ErrLintelRecoveryNotReady)
	}

	// Slot shape: replacement current with the real first-auth fact, the
	// predecessor retiring, no pending rotation.
	var state string
	var currentID, retiringID int64
	var pendingID sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT state,current_credential_id,pending_credential_id,retiring_credential_id FROM runtime_slots WHERE slot='lintel'`).Scan(&state, &currentID, &pendingID, &retiringID); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if state != "registered" || pendingID.Valid || retiringID == 0 {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: lintel slot is not in the post-registration recovery shape", ErrLintelRecoveryNotReady)
	}
	var oldGen int64
	var oldRetired sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT generation,retired_at FROM runtime_credentials WHERE id=?`, retiringID).Scan(&oldGen, &oldRetired); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	var replGen int64
	var replConfirmed, replFirstAuth, replRetired sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT generation,confirmed_at,first_authenticated_at,retired_at FROM runtime_credentials WHERE id=?`, currentID).Scan(&replGen, &replConfirmed, &replFirstAuth, &replRetired); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if oldRetired.Valid || !replConfirmed.Valid || !replFirstAuth.Valid || replRetired.Valid {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: replacement credential lacks its first-authenticated Hello fact", ErrLintelRecoveryNotReady)
	}

	// Idempotence key: same (old_slot, old generation, disposition digest)
	// with different evidence conflicts (VERIFY-RECOVERY-002).
	var existingDigests sql.NullString
	idempotenceQuery := `SELECT disposition_digest FROM lintel_recovery_receipts WHERE old_slot_id='lintel' AND old_token_generation=?`
	idempotenceArgs := []any{oldGen}
	if request.OldGeneration > 0 {
		idempotenceArgs = append(idempotenceArgs, request.DispositionDigest)
		idempotenceQuery += ` AND disposition_digest=?`
	}
	if err := conn.QueryRowContext(ctx, idempotenceQuery, idempotenceArgs...).Scan(&existingDigests); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LintelRecoveryFinalizeResult{}, err
	} else if existingDigests.Valid && existingDigests.String != request.DispositionDigest {
		return LintelRecoveryFinalizeResult{}, ErrLintelRecoveryReceiptConflict
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	result := LintelRecoveryFinalizeResult{
		MaintenanceRevision: revision, OldGeneration: oldGen, ReplacementGeneration: replGen,
	}
	if request.Disposition == "retired" {
		closed, demoted, closureErr := retireLintelStorageDomain(ctx, conn, now)
		if closureErr != nil {
			return LintelRecoveryFinalizeResult{}, closureErr
		}
		result.ClosedOperations = closed
		result.DemotedIdentities = demoted
	}
	// Clear the retiring pointer; the schema trigger retires the old token
	// only after the replacement's first authenticated Hello.
	if _, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET retiring_credential_id=NULL,row_version=row_version+1 WHERE slot='lintel' AND retiring_credential_id=? AND current_credential_id=? AND pending_credential_id IS NULL`, retiringID, currentID); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO lintel_recovery_receipts(maintenance_revision,old_slot_id,old_runtime_credential_id,old_token_generation,replacement_runtime_credential_id,replacement_token_generation,storage_disposition,disposition_digest,fence_report_digest,recovery_report_digest,post_verify_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		revision, "lintel", retiringID, oldGen, currentID, replGen, request.Disposition,
		request.DispositionDigest, request.FenceReportDigest, request.RecoveryReportDigest, request.PostVerifyDigest, now); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_items SET safe_state='Safe',detail_code='lintel_recovery_finalized',updated_at=? WHERE maintenance_revision=?`, now, revision); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=0,reason=NULL,entered_at=NULL,entered_by_type=NULL,entered_by_id=NULL,exited_at=?,exited_by_type='deployment_helper',exited_by_id=0,row_version=row_version+1 WHERE id=1 AND active=1 AND reason='LintelRecovery' AND row_version=?`, now, revision); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('system',0,'lintel_recovery.finalize','success','maintenance',?,?)`, revision, now); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	committed = true
	return result, nil
}

// retireLintelStorageDomain closes the retired disposition inside the same
// transaction (VERIFY-RECOVERY-003): every affected browser operation gets an
// externally-fenced stop confirmation (undispatched queues keep the
// not_dispatched basis), and Ready identities demote to
// AuthenticationRequired. Historic outcomes are never reclassified.
func retireLintelStorageDomain(ctx context.Context, conn *sql.Conn, now string) (int64, int64, error) {
	closed, err := conn.ExecContext(ctx, `
		UPDATE browser_operations SET
			state='Interrupted', ended_at=?, terminal_reason='shutdown',
			stop_confirmed_at=?, stop_confirmation_basis=CASE WHEN start_dispatched_at IS NULL THEN 'not_dispatched' ELSE 'externally_fenced_storage_retired' END,
			row_version=row_version+1
		WHERE state IN ('Queued','WaitingForCapacity','Starting','Running','AwaitingReconnect')`, now, now)
	if err != nil {
		return 0, 0, err
	}
	terminalClosed, err := conn.ExecContext(ctx, `
		UPDATE browser_operations SET stop_confirmed_at=?, stop_confirmation_basis='externally_fenced_storage_retired', row_version=row_version+1
		WHERE stop_confirmed_at IS NULL AND state IN ('Succeeded','Failed','Cancelled','Interrupted')`, now)
	if err != nil {
		return 0, 0, err
	}
	demoted, err := conn.ExecContext(ctx, `UPDATE browser_identities SET state='AuthenticationRequired',row_version=row_version+1 WHERE state='Ready'`)
	if err != nil {
		return 0, 0, err
	}
	first, _ := closed.RowsAffected()
	second, _ := terminalClosed.RowsAffected()
	identities, _ := demoted.RowsAffected()
	return first + second, identities, nil
}

func alreadyFinalizedResult(ctx context.Context, conn *sql.Conn, request LintelRecoveryFinalizeRequest) (LintelRecoveryFinalizeResult, error) {
	query := `SELECT maintenance_revision,old_token_generation,replacement_token_generation,storage_disposition,disposition_digest,fence_report_digest,recovery_report_digest,post_verify_digest FROM lintel_recovery_receipts WHERE old_slot_id='lintel'`
	arguments := []any{}
	if request.OldGeneration > 0 {
		// Frozen replay key: exactly the historical recovery this caller
		// pinned (schema UNIQUE (old_slot_id, old_token_generation,
		// disposition_digest)).
		query += ` AND old_token_generation=?`
		arguments = append(arguments, request.OldGeneration)
	} else {
		query += ` ORDER BY id DESC`
	}
	query += ` LIMIT 1`
	var revision, oldGen, replGen int64
	var disposition string
	var dispositionDigest, fenceDigest, recoveryDigest, postVerifyDigest string
	err := conn.QueryRowContext(ctx, query, arguments...).Scan(&revision, &oldGen, &replGen, &disposition, &dispositionDigest, &fenceDigest, &recoveryDigest, &postVerifyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return LintelRecoveryFinalizeResult{}, fmt.Errorf("%w: no active lintel recovery maintenance and no receipt to replay", ErrLintelRecoveryNotReady)
	}
	if err != nil {
		return LintelRecoveryFinalizeResult{}, err
	}
	if disposition != request.Disposition || dispositionDigest != request.DispositionDigest || fenceDigest != request.FenceReportDigest || recoveryDigest != request.RecoveryReportDigest || postVerifyDigest != request.PostVerifyDigest {
		return LintelRecoveryFinalizeResult{}, ErrLintelRecoveryReceiptConflict
	}
	return LintelRecoveryFinalizeResult{MaintenanceRevision: revision, OldGeneration: oldGen, ReplacementGeneration: replGen, AlreadyFinalized: true}, nil
}

func sameKeys(left, right map[string]struct{}) bool {
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

// verifyRecoveryDatabase reuses the frozen offline database gate.
func verifyRecoveryDatabase(ctx context.Context, db *sql.DB) error {
	return verifyRebindDatabase(ctx, db)
}
