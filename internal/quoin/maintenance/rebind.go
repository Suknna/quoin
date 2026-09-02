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
func RebindRootKey(ctx context.Context, dataDirectory, rootKeyFile string) (RebindResult, error) {
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

	db, err := openUnverifiedDatabase(ctx, filepath.Join(dataDirectory, "quoin.db"))
	if err != nil {
		return RebindResult{}, err
	}
	defer db.Close()
	if err := verifyRebindDatabase(ctx, db); err != nil {
		return RebindResult{}, err
	}
	return rebindOn(ctx, db, key)
}

func openUnverifiedDatabase(ctx context.Context, databasePath string) (*sql.DB, error) {
	info, err := os.Lstat(databasePath)
	if err != nil {
		return nil, fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("database must be a regular file")
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)&_pragma=synchronous(FULL)"}).String()
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

func rebindOn(ctx context.Context, db *sql.DB, key []byte) (RebindResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return RebindResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RebindResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var active int
	var reason string
	var maintenanceRevision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &maintenanceRevision); err != nil {
		return RebindResult{}, err
	}
	var bindingRevision int
	var nonce, ciphertext []byte
	if err := conn.QueryRowContext(ctx, `SELECT binding_revision,verifier_nonce,verifier_ciphertext FROM root_key_state WHERE id=1`).Scan(&bindingRevision, &nonce, &ciphertext); err != nil {
		return RebindResult{}, err
	}
	if active == 1 {
		if reason != "RootKeyRebind" {
			return RebindResult{}, ErrOtherMaintenanceActive
		}
		if err := bootstrap.VerifyRootKeyVerifier(key, bindingRevision, nonce, ciphertext); err != nil {
			return RebindResult{}, ErrRebindActiveWithOtherKey
		}
		count, err := verifyExistingRebind(ctx, conn, maintenanceRevision)
		if err != nil {
			return RebindResult{}, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RebindResult{}, err
		}
		committed = true
		return RebindResult{BindingRevision: bindingRevision, MaintenanceRevision: maintenanceRevision, ConnectionCount: count, AlreadyRebound: true}, nil
	}
	if err := bootstrap.VerifyRootKeyVerifier(key, bindingRevision, nonce, ciphertext); err == nil {
		return RebindResult{}, ErrRootKeyAlreadyCurrent
	}

	nextBinding := bindingRevision + 1
	newNonce, newCiphertext, err := bootstrap.SealRootKeyVerifier(key, nextBinding)
	if err != nil {
		return RebindResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason='RootKeyRebind',entered_at=?,entered_by_type='system',entered_by_id=0,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, now, maintenanceRevision)
	if err != nil {
		return RebindResult{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return RebindResult{}, ErrOtherMaintenanceActive
	}
	maintenanceRevision++
	// The schema trigger requires this isolation before the binding can advance.
	if _, err := conn.ExecContext(ctx, `UPDATE connections SET enabled=0,revalidation_required=1,row_version=row_version+1 WHERE enabled<>0 OR revalidation_required<>1`); err != nil {
		return RebindResult{}, err
	}
	var connectionCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections`).Scan(&connectionCount); err != nil {
		return RebindResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) SELECT ?, 'Connection', name, 'Blocking', 'root_key_rebind_required', ? FROM connections`, maintenanceRevision, now); err != nil {
		return RebindResult{}, err
	}
	// Exit has a structural non-empty checklist requirement even when there are
	// no configured connections.
	if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?, 'Integrity', 'root-key-binding', 'Safe', 'replacement_root_key_verified', ?)`, maintenanceRevision, now); err != nil {
		return RebindResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE root_key_state SET binding_revision=?,verifier_nonce=?,verifier_ciphertext=?,bound_at=? WHERE id=1`, nextBinding, newNonce, newCiphertext, now); err != nil {
		return RebindResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('system',0,'root_key.rebind','success','maintenance',?,?)`, maintenanceRevision, now); err != nil {
		return RebindResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RebindResult{}, err
	}
	committed = true
	return RebindResult{BindingRevision: nextBinding, MaintenanceRevision: maintenanceRevision, ConnectionCount: connectionCount}, nil
}

func verifyExistingRebind(ctx context.Context, conn *sql.Conn, revision int64) (int, error) {
	var total, isolated, checklist int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN enabled=0 AND revalidation_required=1 THEN 1 ELSE 0 END),0) FROM connections`).Scan(&total, &isolated); err != nil {
		return 0, err
	}
	if total != isolated {
		return 0, errors.New("active root key rebind has a non-isolated connection")
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_items WHERE maintenance_revision=? AND kind='Connection'`, revision).Scan(&checklist); err != nil {
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
