// Package recovery restores one verified Quoin backup while Quoin is stopped.
// It owns the offline replacement boundary: validation and trust isolation are
// complete before the restored database is published into the live data root.
package recovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

var (
	ErrInvalidRequest    = errors.New("invalid restore request")
	ErrMaintenance       = errors.New("backup database is already in maintenance")
	ErrNoContinuation    = errors.New("no published restore continuation")
	ErrContinuationFence = errors.New("restore continuation fence is invalid")
)

// Request contains only file identities and the temporary recovery credential.
// The password must come from an attached TTY and is deliberately never logged.
type Request struct {
	DataDirectory, BackupDirectory, BackupID, RootKeyFile, RollbackDirectory string
	AdminUsername, TemporaryPassword                                         string
}

// Result is a non-secret recovery receipt for the deployment helper.
type Result struct {
	MaintenanceReason   string
	MaintenanceRevision int64
	RollbackDirectory   string
}

// Preflight verifies an archived backup without opening or locking the live
// data directory. Deployment helpers run it before their destructive fence so
// the confirmation is bound to the exact manifest digest they display.
type PreflightResult struct {
	BackupID       string
	Release        string
	ManifestSHA256 string
}

// ContinueResult proves that the current live database is the already-published
// result of exactly this restore attempt. It never changes data: deployment
// helpers use it only after stopping every workload, to resume the maintenance
// completion path instead of running offline restore again.
type ContinueResult struct {
	MaintenanceRevision int64
	RollbackDirectory   string
}

// Continue verifies the durable recovery fence: live Restore maintenance and
// the backup-ID-bound original-data rollback point must both exist. Absence of
// maintenance is the sole normal non-continuation result; any partial fence is
// an error so a helper cannot overwrite a published but damaged recovery.
func Continue(ctx context.Context, request Request) (ContinueResult, error) {
	if request.DataDirectory == "" || request.BackupID == "" || request.RootKeyFile == "" || filepath.Base(request.BackupID) != request.BackupID || request.BackupID == "." || request.BackupID == ".." {
		return ContinueResult{}, fmt.Errorf("%w: required continuation field", ErrInvalidRequest)
	}
	rollbackName := ".restore-rollback-" + request.BackupID
	if request.RollbackDirectory != "" && request.RollbackDirectory != rollbackName {
		return ContinueResult{}, fmt.Errorf("%w: rollback identifier", ErrInvalidRequest)
	}
	// A prior process's WAL/SHM proves its SQLite closure is incomplete. Check
	// before OpenDatabase, which then owns the exclusive directory lock for the
	// rest of inspection; a concurrent opener either wins that lock (and makes
	// this call fail) or starts only after continuation has released it.
	liveDB := filepath.Join(request.DataDirectory, "quoin.db")
	if err := ensureNoSQLiteSidecars(liveDB); err != nil {
		return ContinueResult{}, err
	}
	database, err := bootstrap.OpenDatabase(ctx, request.DataDirectory, request.RootKeyFile)
	if err != nil {
		return ContinueResult{}, fmt.Errorf("validate published restore database: %w", err)
	}
	defer database.Close()
	var active int
	var reason string
	var revision int64
	if err := database.SQL.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return ContinueResult{}, err
	}
	if active == 0 {
		return ContinueResult{}, ErrNoContinuation
	}
	if reason != "Restore" {
		return ContinueResult{}, fmt.Errorf("%w: active maintenance reason %q is not Restore", ErrContinuationFence, reason)
	}
	rollback := filepath.Join(request.DataDirectory, rollbackName)
	if filepath.Dir(rollback) != filepath.Clean(request.DataDirectory) {
		return ContinueResult{}, fmt.Errorf("%w: rollback path", ErrContinuationFence)
	}
	for _, path := range []string{rollback, filepath.Join(rollback, "quoin.db"), filepath.Join(rollback, "artifacts")} {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&fs.ModeSymlink != 0 || (path == rollback || strings.HasSuffix(path, "artifacts")) && !info.IsDir() || strings.HasSuffix(path, "quoin.db") && !info.Mode().IsRegular() {
			return ContinueResult{}, fmt.Errorf("%w: expected rollback member %s", ErrContinuationFence, path)
		}
	}
	return ContinueResult{MaintenanceRevision: revision, RollbackDirectory: rollback}, nil
}

func Preflight(backupDirectory, backupID string) (PreflightResult, error) {
	source := filepath.Join(backupDirectory, backupID)
	if backupDirectory == "" || backupID == "" || filepath.Dir(source) != filepath.Clean(backupDirectory) {
		return PreflightResult{}, fmt.Errorf("%w: unsafe backup identifier", ErrInvalidRequest)
	}
	if err := backup.Verify(source); err != nil {
		return PreflightResult{}, fmt.Errorf("verify backup: %w", err)
	}
	if err := backup.VerifyRelease(source, buildinfo.Release); err != nil {
		return PreflightResult{}, fmt.Errorf("verify backup release: %w", err)
	}
	manifest, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	if err != nil {
		return PreflightResult{}, fmt.Errorf("read backup manifest: %w", err)
	}
	digest := sha256.Sum256(manifest)
	return PreflightResult{BackupID: backupID, Release: buildinfo.Release, ManifestSHA256: hex.EncodeToString(digest[:])}, nil
}

// Restore verifies a published backup, stages it on the data filesystem,
// isolates every restored trust boundary in one SQLite transaction, then swaps
// the database and matching artifact set while holding the Quoin data lock.
func Restore(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	lock, err := sharedops.AcquireDirectory(request.DataDirectory)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()

	source := filepath.Join(request.BackupDirectory, request.BackupID)
	if filepath.Dir(source) != filepath.Clean(request.BackupDirectory) {
		return Result{}, fmt.Errorf("%w: unsafe backup identifier", ErrInvalidRequest)
	}
	if err := backup.Verify(source); err != nil {
		return Result{}, fmt.Errorf("verify backup: %w", err)
	}
	if err := backup.VerifyRelease(source, buildinfo.Release); err != nil {
		return Result{}, fmt.Errorf("verify backup release: %w", err)
	}
	if err := ensureNoSQLiteSidecars(filepath.Join(request.DataDirectory, "quoin.db")); err != nil {
		return Result{}, err
	}

	staging, err := os.MkdirTemp(request.DataDirectory, ".restore-stage-")
	if err != nil {
		return Result{}, fmt.Errorf("create restore staging: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := stageSnapshot(source, staging, buildinfo.Release); err != nil {
		return Result{}, err
	}
	if err := normalizeStagedSQLite(ctx, filepath.Join(staging, "quoin.db")); err != nil {
		return Result{}, err
	}
	stagedDB, err := bootstrap.OpenDatabase(ctx, staging, request.RootKeyFile)
	if err != nil {
		return Result{}, fmt.Errorf("validate staged database: %w", err)
	}
	if err := verifySQLite(ctx, stagedDB.SQL); err != nil {
		_ = stagedDB.Close()
		return Result{}, err
	}
	revision, err := isolate(ctx, stagedDB.SQL, request.AdminUsername, request.TemporaryPassword)
	closeErr := stagedDB.Close()
	if err != nil {
		return Result{}, err
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close staged database: %w", closeErr)
	}
	if err := ensureNoSQLiteSidecars(filepath.Join(staging, "quoin.db")); err != nil {
		return Result{}, err
	}
	rollback, err := publish(request.DataDirectory, staging, request.RollbackDirectory)
	if err != nil {
		return Result{}, err
	}
	return Result{MaintenanceReason: "Restore", MaintenanceRevision: revision, RollbackDirectory: rollback}, nil
}

func validateRequest(request Request) error {
	for _, value := range []string{request.DataDirectory, request.BackupDirectory, request.BackupID, request.RootKeyFile, request.AdminUsername, request.TemporaryPassword} {
		if value == "" {
			return fmt.Errorf("%w: required field is empty", ErrInvalidRequest)
		}
	}
	if filepath.Base(request.BackupID) != request.BackupID || request.BackupID == "." || request.BackupID == ".." {
		return fmt.Errorf("%w: backup identifier", ErrInvalidRequest)
	}
	if filepath.Clean(request.DataDirectory) == filepath.Clean(request.BackupDirectory) {
		return fmt.Errorf("%w: data and backup directories must differ", ErrInvalidRequest)
	}
	if request.RollbackDirectory != "" && (filepath.Base(request.RollbackDirectory) != request.RollbackDirectory || !strings.HasPrefix(request.RollbackDirectory, ".restore-rollback-")) {
		return fmt.Errorf("%w: rollback directory", ErrInvalidRequest)
	}
	return nil
}

// stageSnapshot copies the archive once into an isolated staging layout, then
// validates that copied exact set before reshaping artifacts for the live store.
// This closes the verification/copy time-of-check-time-of-use boundary.
func stageSnapshot(source, staging, expectedRelease string) error {
	archive := filepath.Join(staging, ".archive")
	if err := os.Mkdir(archive, 0o700); err != nil {
		return fmt.Errorf("create staged archive: %w", err)
	}
	for _, name := range []string{"manifest.json", "quoin.db"} {
		if err := copyVerifiedFile(source, name, filepath.Join(archive, name)); err != nil {
			return fmt.Errorf("stage %s: %w", name, err)
		}
	}
	artifacts := filepath.Join(source, "artifacts")
	entries, err := os.ReadDir(artifacts)
	if err != nil {
		return fmt.Errorf("read backup artifacts: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("backup artifact %q is not a regular file", entry.Name())
		}
		if err := copyVerifiedFile(artifacts, entry.Name(), filepath.Join(archive, "artifacts", entry.Name())); err != nil {
			return fmt.Errorf("stage artifact %q: %w", entry.Name(), err)
		}
	}
	if err := backup.Verify(archive); err != nil {
		return fmt.Errorf("verify copied backup: %w", err)
	}
	if err := backup.VerifyRelease(archive, expectedRelease); err != nil {
		return fmt.Errorf("verify copied backup release: %w", err)
	}
	if err := os.Rename(filepath.Join(archive, "quoin.db"), filepath.Join(staging, "quoin.db")); err != nil {
		return fmt.Errorf("stage verified database: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "artifacts", "blobs"), 0o700); err != nil {
		return fmt.Errorf("create restored artifact directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(archive, "artifacts", entry.Name()), filepath.Join(staging, "artifacts", "blobs", entry.Name())); err != nil {
			return fmt.Errorf("stage verified artifact %q: %w", entry.Name(), err)
		}
	}
	if err := os.RemoveAll(archive); err != nil {
		return fmt.Errorf("remove verified archive staging: %w", err)
	}
	// fsync only persists the named directory's entries; artifact members live
	// two levels below staging and must be made durable from child to parent.
	for _, directory := range []string{filepath.Join(staging, "artifacts", "blobs"), filepath.Join(staging, "artifacts"), staging} {
		if err := syncDirectory(directory); err != nil {
			return fmt.Errorf("sync staged restore directory %s: %w", directory, err)
		}
	}
	return nil
}

func copyVerifiedFile(root, relative, destination string) error {
	if filepath.Clean(relative) != relative || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("unsafe backup member path")
	}
	source := filepath.Join(root, relative)
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("backup member is not a regular file")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// normalizeStagedSQLite converts VACUUM INTO's rollback-journal snapshot to
// Quoin's persisted WAL settings before bootstrap performs its current-schema
// and root-key verification. It touches only the untrusted staging copy.
func normalizeStagedSQLite(ctx context.Context, path string) error {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open staged database: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	var journal string
	if err := database.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil || strings.ToLower(journal) != "wal" {
		return fmt.Errorf("normalize staged journal mode=%q err=%w", journal, err)
	}
	return nil
}

func verifySQLite(ctx context.Context, database *sql.DB) error {
	var integrity string
	if err := database.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("staged integrity check=%q err=%w", integrity, err)
	}
	rows, err := database.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("staged foreign key check found violations")
	}
	return rows.Err()
}

func isolate(ctx context.Context, database *sql.DB, username, password string) (int64, error) {
	conn, err := database.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var active int
	if err := conn.QueryRowContext(ctx, `SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil {
		return 0, err
	}
	if active != 0 {
		return 0, ErrMaintenance
	}
	var adminID int64
	var role, displayName string
	if err := conn.QueryRowContext(ctx, `SELECT id,role,display_name FROM users WHERE username=?`, username).Scan(&adminID, &role, &displayName); err != nil {
		return 0, fmt.Errorf("select recovery administrator: %w", err)
	}
	if role != "admin" {
		return 0, errors.New("selected recovery user is not an administrator")
	}
	normalized, err := auth.ValidateNewPassword(password, username, displayName)
	if err != nil {
		return 0, err
	}
	phc, err := auth.HashPassword(normalized)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE sessions SET revoked_at=? WHERE revoked_at IS NULL`, []any{now}},
		{`UPDATE users SET enabled=0,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id<>? AND enabled=1`, []any{now, adminID}},
		{`UPDATE users SET enabled=1,password_phc=?,password_change_required=1,password_change_required_at=?,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=?`, []any{phc, now, now, adminID}},
		{`UPDATE runtime_slots SET state='revoked',current_credential_id=NULL,pending_credential_id=NULL,retiring_credential_id=NULL,row_version=row_version+1 WHERE state<>'revoked'`, nil},
		{`UPDATE runtime_credentials SET retired_at=?,row_version=row_version+1 WHERE retired_at IS NULL`, []any{now}},
		{`UPDATE alert_sources SET enabled=0,disabled_at=?,row_version=row_version+1 WHERE enabled=1`, []any{now}},
		// A never-used replacement cannot retire its Active predecessor under the
		// ordinary rotation rule. First move that replacement to its existing
		// PendingRetirement state (without fabricating first_used_at), then retire
		// both accepted generations while the source is disabled.
		{`UPDATE alert_source_credentials SET state='PendingRetirement',pending_retirement_at=?,row_version=row_version+1 WHERE state='Active' AND supersedes_credential_id IS NOT NULL`, []any{now}},
		{`UPDATE alert_source_credentials SET state='Retired',retired_at=?,row_version=row_version+1 WHERE state<>'Retired'`, []any{now}},
		// A restored connection cannot be trusted until an Admin records it again.
		// Disabled is the safe terminal state, so exitMaintenance stays reachable
		// without exposing ordinary connection writes during maintenance.
		{`UPDATE connections SET enabled=0,revalidation_required=1,row_version=row_version+1 WHERE enabled=1 OR revalidation_required=0`, nil},
		{`UPDATE browser_identities SET state='AuthenticationRequired',row_version=row_version+1 WHERE state='Ready'`, nil},
		{`UPDATE maintenance_state SET active=1,reason='Restore',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1 AND active=0`, []any{now}},
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return 0, err
		}
	}
	var storedPassword string
	var passwordChangeRequired, enabled int
	if err := conn.QueryRowContext(ctx, `SELECT password_phc,password_change_required,enabled FROM users WHERE id=?`, adminID).Scan(&storedPassword, &passwordChangeRequired, &enabled); err != nil {
		return 0, fmt.Errorf("verify recovery administrator: %w", err)
	}
	if enabled != 1 || passwordChangeRequired != 1 || !auth.VerifyPassword(normalized, storedPassword) {
		return 0, errors.New("recovery administrator isolation did not apply")
	}
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT row_version FROM maintenance_state WHERE id=1`).Scan(&revision); err != nil {
		return 0, err
	}
	if err := insertChecklist(ctx, conn, revision, adminID, now); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('system',0,'maintenance.restore.enter','success','maintenance',?,?)`, revision, now); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return revision, nil
}

func insertChecklist(ctx context.Context, conn *sql.Conn, revision, adminID int64, now string) error {
	items := []checkItem{{"Integrity", "snapshot", "Safe", "verified"}, {"AdminPassword", fmt.Sprintf("%d", adminID), "Blocking", "temporary_password_change_required"}}
	rows, err := conn.QueryContext(ctx, `SELECT id,enabled FROM users ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, enabled int64
		if err := rows.Scan(&id, &enabled); err != nil {
			rows.Close()
			return err
		}
		state := "Safe"
		code := "disabled"
		if id == adminID {
			code = "recovery_admin_enabled"
		}
		items = append(items, checkItem{"User", fmt.Sprintf("%d", id), state, code})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	connectionRows, err := conn.QueryContext(ctx, `SELECT name,enabled FROM connections ORDER BY name`)
	if err != nil {
		return err
	}
	for connectionRows.Next() {
		var name string
		var enabled int
		if err := connectionRows.Scan(&name, &enabled); err != nil {
			connectionRows.Close()
			return err
		}
		_ = enabled // disabled plus revalidation_required is an explicitly safe containment state.
		items = append(items, checkItem{"Connection", name, "Safe", "disabled_revalidation_required"})
	}
	if err := connectionRows.Err(); err != nil {
		connectionRows.Close()
		return err
	}
	if err := connectionRows.Close(); err != nil {
		return err
	}
	for _, table := range []struct{ kind, query, code string }{
		{"RuntimeSlot", `SELECT slot FROM runtime_slots ORDER BY slot`, "revoked"},
		{"AlertSource", `SELECT source_key FROM alert_sources ORDER BY source_key`, "disabled"},
		{"BrowserIdentity", `SELECT id FROM browser_identities ORDER BY id`, "authentication_required"},
	} {
		rows, err := conn.QueryContext(ctx, table.query)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key any
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return err
			}
			items = append(items, checkItem{table.kind, fmt.Sprint(key), "Safe", table.code})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].kind == items[right].kind {
			return items[left].key < items[right].key
		}
		return items[left].kind < items[right].kind
	})
	for _, item := range items {
		if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?)`, revision, item.kind, item.key, item.state, item.code, now); err != nil {
			return err
		}
	}
	return nil
}

type checkItem struct{ kind, key, state, code string }

func publish(dataDirectory, staging, rollbackName string) (string, error) {
	return publishWithSync(dataDirectory, staging, rollbackName, syncDirectory)
}

var renamePath = os.Rename

// publishWithSync replaces the database and artifact tree as one filesystem
// publication unit. The injected sync is intentionally narrow: it lets the
// failure path prove that an acknowledged restore never leaves a mixed tree.
func publishWithSync(dataDirectory, staging, rollbackName string, sync func(string) error) (string, error) {
	var rollback string
	var err error
	if rollbackName == "" {
		rollback, err = os.MkdirTemp(dataDirectory, ".restore-rollback-")
	} else {
		rollback = filepath.Join(dataDirectory, rollbackName)
		err = os.Mkdir(rollback, 0o700)
	}
	if err != nil {
		return "", fmt.Errorf("create restore rollback point: %w", err)
	}
	// Once a live member has entered rollback, an error must preserve that
	// directory unless both original members are demonstrably back in place.
	// A rollback point is more valuable than best-effort temporary cleanup.
	removeRollback := true
	defer func() {
		if removeRollback {
			_ = os.RemoveAll(rollback)
		}
	}()
	liveDB, liveArtifacts := filepath.Join(dataDirectory, "quoin.db"), filepath.Join(dataDirectory, "artifacts")
	backupDB, backupArtifacts := filepath.Join(rollback, "quoin.db"), filepath.Join(rollback, "artifacts")
	if err := renamePath(liveDB, backupDB); err != nil {
		return "", fmt.Errorf("preserve database rollback point: %w", err)
	}
	removeRollback = false
	if err := renamePath(liveArtifacts, backupArtifacts); err != nil {
		if restoreErr := renamePath(backupDB, liveDB); restoreErr == nil {
			removeRollback = true
		} else {
			return "", fmt.Errorf("preserve artifact rollback point: %w; original database retained at %s after restore failure: %v", err, backupDB, restoreErr)
		}
		return "", fmt.Errorf("preserve artifact rollback point: %w", err)
	}
	if err := renamePath(filepath.Join(staging, "quoin.db"), liveDB); err != nil {
		restoreArtifactsErr := renamePath(backupArtifacts, liveArtifacts)
		restoreDatabaseErr := renamePath(backupDB, liveDB)
		if restoreArtifactsErr == nil && restoreDatabaseErr == nil {
			removeRollback = true
			return "", fmt.Errorf("publish restored database: %w", err)
		}
		return "", fmt.Errorf("publish restored database: %w; original data retained at %s (artifact restore=%v database restore=%v)", err, rollback, restoreArtifactsErr, restoreDatabaseErr)
	}
	if err := renamePath(filepath.Join(staging, "artifacts"), liveArtifacts); err != nil {
		moveNewDatabaseErr := renamePath(liveDB, filepath.Join(staging, "quoin.db"))
		restoreArtifactsErr := renamePath(backupArtifacts, liveArtifacts)
		restoreDatabaseErr := renamePath(backupDB, liveDB)
		if moveNewDatabaseErr == nil && restoreArtifactsErr == nil && restoreDatabaseErr == nil {
			removeRollback = true
			return "", fmt.Errorf("publish restored artifacts: %w", err)
		}
		return "", fmt.Errorf("publish restored artifacts: %w; original data retained at %s (move new database=%v artifact restore=%v database restore=%v)", err, rollback, moveNewDatabaseErr, restoreArtifactsErr, restoreDatabaseErr)
	}
	if err := sync(rollback); err != nil {
		rollbackErr := rollbackPublication(liveDB, liveArtifacts, backupDB, backupArtifacts, staging)
		if rollbackErr != nil {
			return "", fmt.Errorf("sync restore rollback point: %w; rollback after unsynced publication failed; original data retained at %s: %v", err, rollback, rollbackErr)
		}
		removeRollback = true
		return "", fmt.Errorf("sync restore rollback point: %w; restored rollback point", err)
	}
	if err := sync(dataDirectory); err != nil {
		rollbackErr := rollbackPublication(liveDB, liveArtifacts, backupDB, backupArtifacts, staging)
		if rollbackErr != nil {
			return "", fmt.Errorf("sync restored data directory: %w; rollback after unsynced publication failed; original data retained at %s: %v", err, rollback, rollbackErr)
		}
		removeRollback = true
		return "", fmt.Errorf("sync restored data directory: %w; restored rollback point", err)
	}
	// Publication is durable. The deployment helper owns Finalize only after
	// it has observed the full normal-mode post-restore verification.
	return rollback, nil
}

// Finalize removes the retained pre-restore rollback point only after the
// deployment helper has observed normal readiness and every post-restore check.
func Finalize(dataDirectory, rollbackName string) error {
	if rollbackName == "" || filepath.Base(rollbackName) != rollbackName || !strings.HasPrefix(rollbackName, ".restore-rollback-") {
		return fmt.Errorf("%w: rollback directory", ErrInvalidRequest)
	}
	rollback := filepath.Join(dataDirectory, rollbackName)
	if err := os.RemoveAll(rollback); err != nil {
		return fmt.Errorf("remove restore rollback point: %w", err)
	}
	return syncDirectory(dataDirectory)
}

func rollbackPublication(liveDB, liveArtifacts, backupDB, backupArtifacts, staging string) error {
	stagedDB, stagedArtifacts := filepath.Join(staging, "quoin.db"), filepath.Join(staging, "artifacts")
	if err := renamePath(liveArtifacts, stagedArtifacts); err != nil {
		return fmt.Errorf("move unsynced restored artifacts aside: %w", err)
	}
	if err := renamePath(liveDB, stagedDB); err != nil {
		_ = os.Rename(stagedArtifacts, liveArtifacts)
		return fmt.Errorf("move unsynced restored database aside: %w", err)
	}
	if err := renamePath(backupArtifacts, liveArtifacts); err != nil {
		return fmt.Errorf("restore artifact rollback point: %w", err)
	}
	if err := renamePath(backupDB, liveDB); err != nil {
		return fmt.Errorf("restore database rollback point: %w", err)
	}
	return syncDirectory(filepath.Dir(liveDB))
}

func ensureNoSQLiteSidecars(databasePath string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(databasePath + suffix); err == nil {
			return fmt.Errorf("restore refuses SQLite sidecar %s; stop and cleanly close the prior Quoin process before retrying", databasePath+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect SQLite sidecar %s: %w", databasePath+suffix, err)
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
