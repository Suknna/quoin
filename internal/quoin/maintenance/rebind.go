package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	ErrRebindActiveWithOtherKey = errors.New("root key rebind is already active with a different root key")
	ErrRootKeyAlreadyCurrent    = errors.New("replacement root key already authenticates the database")
	ErrOtherMaintenanceActive   = errors.New("another maintenance operation is active")
)

// RebindResult is non-secret terminal evidence for the offline command.
type RebindResult struct {
	BindingRevision     int
	MaintenanceRevision int64
	ConnectionCount     int
	AlreadyRebound      bool
}

// RebindRootKey atomically enters RootKeyRebind, isolates every Connection,
// replaces only the root verifier and records the frozen connection checklist.
// It intentionally retains every old credential generation unchanged: the new
// binding revision makes those envelopes structurally ineligible for a grant.
//
// The command runs through the shared execution authority: one registered
// operation, one runner-owned IMMEDIATE transaction whose automatic audit row
// carries a fresh per-invocation correlation and the CLI source, and a full
// rollback when anything — including the audit write — fails. A matching
// active rebind is a replay: it performs no state transition, so the runner
// records nothing new and the stored state stays the only authority.
func RebindRootKey(ctx context.Context, dataDirectory, rootKeyFile string) (RebindResult, error) {
	// The offline command is its own operation root: refuse a caller
	// correlation before anything is opened, so no unrelated metadata can
	// leak into the audit rows.
	runCtx, err := withOfflineMetadata(ctx)
	if err != nil {
		return RebindResult{}, err
	}
	key, err := os.ReadFile(rootKeyFile)
	if err != nil {
		return RebindResult{}, fmt.Errorf("read replacement root key: %w", err)
	}
	if len(key) != 32 {
		return RebindResult{}, errors.New("replacement root key must contain exactly 32 bytes")
	}
	lock, err := sharedops.AcquireDirectory(dataDirectory)
	if err != nil {
		return RebindResult{}, err
	}
	defer lock.Close()

	databasePath := filepath.Join(dataDirectory, "quoin.db")
	// The offline verification gate is an actual SQLite read-only open: the
	// pass that judges the database provably cannot mutate it, which the
	// statement guards inside the runner cannot express before the runner
	// exists. This is the minimal controlled raw-open authority of the
	// offline boundary — not a package-wide exemption.
	if err := verifyOfflineDatabase(ctx, databasePath); err != nil {
		return RebindResult{}, err
	}
	db, err := openOfflineDatabase(ctx, databasePath)
	if err != nil {
		return RebindResult{}, err
	}
	defer db.Close()
	return rebindOn(runCtx, db, key)
}

// checkOfflineDatabaseFile rejects anything but a plain regular file before a
// SQLite open is attempted.
func checkOfflineDatabaseFile(databasePath string) error {
	info, err := os.Lstat(databasePath)
	if err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("database must be a regular file")
	}
	return nil
}

// offlineDatabasePragmas are the connection pragmas both offline opens set.
// The read-only verification handle carries the same pragmas it asserts, so
// its PRAGMA reads judge the persistent database facts (journal mode) and the
// exact connection posture the mutation handle will run under.
const offlineDatabasePragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)&_pragma=synchronous(FULL)"

// openReadOnlyDatabase opens the stopped database through SQLite's real
// read-only mode (mode=ro): SQLite itself rejects any write attempt on the
// connection — including INSERTs issued through a query call — so the
// verification pass cannot corrupt the database it is judging.
func openReadOnlyDatabase(ctx context.Context, databasePath string) (*sql.DB, error) {
	if err := checkOfflineDatabaseFile(databasePath); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro&" + offlineDatabasePragmas}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openOfflineDatabase opens the mutation handle of the offline boundary: the
// one raw read-write open under the exclusive directory lock, after the
// read-only verification pass has judged the database.
func openOfflineDatabase(ctx context.Context, databasePath string) (*sql.DB, error) {
	if err := checkOfflineDatabaseFile(databasePath); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: offlineDatabasePragmas}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// verifyOfflineDatabase is the shared stopped-process gate of both offline
// commands: open read-only, run the frozen verification, close.
func verifyOfflineDatabase(ctx context.Context, databasePath string) error {
	db, err := openReadOnlyDatabase(ctx, databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	return verifyRebindDatabase(ctx, db)
}

func rebindOn(ctx context.Context, db *sql.DB, key []byte) (RebindResult, error) {
	runner, ops := newOfflineRunner(db)
	var result RebindResult
	_, err := execution.Execute(ctx, runner, ops.rebind, func(tx *execution.Tx) (int64, error) {
		var active int
		var reason string
		var maintenanceRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &maintenanceRevision); err != nil {
			return 0, err
		}
		var bindingRevision int
		var nonce, ciphertext []byte
		if err := tx.QueryRowContext(ctx, `SELECT binding_revision,verifier_nonce,verifier_ciphertext FROM root_key_state WHERE id=1`).Scan(&bindingRevision, &nonce, &ciphertext); err != nil {
			return 0, err
		}
		if active == 1 {
			if reason != "RootKeyRebind" {
				return 0, ErrOtherMaintenanceActive
			}
			if err := bootstrap.VerifyRootKeyVerifier(key, bindingRevision, nonce, ciphertext); err != nil {
				return 0, ErrRebindActiveWithOtherKey
			}
			count, err := verifyExistingRebind(ctx, tx, maintenanceRevision)
			if err != nil {
				return 0, err
			}
			// A matching active rebind is a pure replay: no state transition
			// and — as before the runner — no new audit row. The sentinel
			// discards the read-only stage and surfaces the captured result.
			result = RebindResult{BindingRevision: bindingRevision, MaintenanceRevision: maintenanceRevision, ConnectionCount: count, AlreadyRebound: true}
			return 0, execution.ErrNoTransition
		}
		if err := bootstrap.VerifyRootKeyVerifier(key, bindingRevision, nonce, ciphertext); err == nil {
			return 0, ErrRootKeyAlreadyCurrent
		}

		nextBinding := bindingRevision + 1
		newNonce, newCiphertext, err := bootstrap.SealRootKeyVerifier(key, nextBinding)
		if err != nil {
			return 0, err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		update, err := tx.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason='RootKeyRebind',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, now, maintenanceRevision)
		if err != nil {
			return 0, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return 0, ErrOtherMaintenanceActive
		}
		maintenanceRevision++
		// The schema trigger requires this isolation before the binding can advance.
		if _, err := tx.ExecContext(ctx, `UPDATE connections SET enabled=0,revalidation_required=1,row_version=row_version+1 WHERE enabled<>0 OR revalidation_required<>1`); err != nil {
			return 0, err
		}
		var connectionCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections`).Scan(&connectionCount); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) SELECT ?, 'Connection', name, 'Blocking', 'root_key_rebind_required', ? FROM connections`, maintenanceRevision, now); err != nil {
			return 0, err
		}
		// Exit has a structural non-empty checklist requirement even when there are
		// no configured connections.
		if _, err := tx.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?, 'Integrity', 'root-key-binding', 'Safe', 'replacement_root_key_verified', ?)`, maintenanceRevision, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE root_key_state SET binding_revision=?,verifier_nonce=?,verifier_ciphertext=?,bound_at=? WHERE id=1`, nextBinding, newNonce, newCiphertext, now); err != nil {
			return 0, err
		}
		// The runner writes the automatic audit row in this same transaction;
		// an audit failure rolls the whole rebind back.
		result = RebindResult{BindingRevision: nextBinding, MaintenanceRevision: maintenanceRevision, ConnectionCount: connectionCount}
		return maintenanceRevision, nil
	}, func(revision int64) int64 { return revision })
	if errors.Is(err, execution.ErrNoTransition) {
		return result, nil
	}
	if err != nil {
		return RebindResult{}, err
	}
	return result, nil
}

func verifyExistingRebind(ctx context.Context, tx *execution.Tx, revision int64) (int, error) {
	var total, isolated, checklist int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN enabled=0 AND revalidation_required=1 THEN 1 ELSE 0 END),0) FROM connections`).Scan(&total, &isolated); err != nil {
		return 0, err
	}
	if total != isolated {
		return 0, errors.New("active root key rebind has a non-isolated connection")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND kind='Connection'`, revision).Scan(&checklist); err != nil {
		return 0, err
	}
	if total != checklist {
		return 0, errors.New("active root key rebind checklist does not cover every connection")
	}
	return total, nil
}

func verifyRebindDatabase(ctx context.Context, db *sql.DB) error {
	for _, check := range []struct{ query, want string }{{"PRAGMA journal_mode", "wal"}, {"PRAGMA synchronous", "2"}, {"PRAGMA foreign_keys", "1"}, {"PRAGMA recursive_triggers", "1"}} {
		var got string
		if err := db.QueryRowContext(ctx, check.query).Scan(&got); err != nil || strings.ToLower(got) != check.want {
			return fmt.Errorf("%s=%q, want %s: %w", check.query, got, check.want, err)
		}
	}
	var quickCheck string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil || quickCheck != "ok" {
		return fmt.Errorf("database quick_check=%q: %w", quickCheck, err)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("database foreign_key_check found a violation")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	var version, storedDigest string
	if err := db.QueryRowContext(ctx, `SELECT schema_version,schema_digest FROM schema_state WHERE id=1`).Scan(&version, &storedDigest); err != nil {
		return err
	}
	if version != "v1" || storedDigest != hex.EncodeToString(digest[:]) {
		return errors.New("database schema version or digest does not match this release")
	}
	return nil
}
