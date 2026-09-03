package bootstrap

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("sha256", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("sha256: expected one argument")
		}
		var input []byte
		switch value := args[0].(type) {
		case string:
			input = []byte(value)
		case []byte:
			input = value
		default:
			return nil, fmt.Errorf("sha256: unsupported argument type %T", args[0])
		}
		sum := sha256.Sum256(input)
		return sum[:], nil
	})
}

const verifierPlaintext = "quoin-root-key-verifier-v1"

type Database struct {
	SQL  *sql.DB
	lock *sharedops.DirectoryLock
}

func OpenDatabase(ctx context.Context, dataDirectory, rootKeyFile string) (*Database, error) {
	lock, err := sharedops.AcquireDirectory(dataDirectory)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Database, error) {
		_ = lock.Close()
		return nil, err
	}
	rootKey, err := os.ReadFile(rootKeyFile)
	if err != nil {
		return fail(fmt.Errorf("read root key: %w", err))
	}
	if len(rootKey) != 32 {
		return fail(fmt.Errorf("root key must contain exactly 32 bytes"))
	}
	databasePath := filepath.Join(dataDirectory, "quoin.db")
	_, statErr := os.Stat(databasePath)
	fresh := os.IsNotExist(statErr)
	if statErr != nil && !fresh {
		return fail(fmt.Errorf("inspect database: %w", statErr))
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)&_pragma=synchronous(FULL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fail(err)
	}
	if fresh {
		if err := initializeDatabase(ctx, db, rootKey); err != nil {
			_ = db.Close()
			_ = os.Remove(databasePath)
			return fail(err)
		}
		if err := os.Chmod(databasePath, 0o600); err != nil {
			_ = db.Close()
			return fail(err)
		}
	} else if err := verifyDatabase(ctx, db, rootKey); err != nil {
		_ = db.Close()
		return fail(err)
	}
	return &Database{SQL: db, lock: lock}, nil
}

// PeekHasUsers answers whether an administrator already exists using a
// read-only connection that never takes the data-directory write lock, so a
// rerunning admin-bootstrap cannot race a running Quoin for the lock. It is
// only a fast existence probe; the creation path still re-checks under the
// exclusive lock.
func PeekHasUsers(dataDirectory string) bool {
	databasePath := filepath.Join(dataDirectory, "quoin.db")
	if _, err := os.Stat(databasePath); err != nil {
		return false
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// PeekSchemaState reads the persisted schema identity through a read-only
// connection that never takes the data-directory write lock, mirroring
// PeekHasUsers. `quoin migrate preflight` uses it to give the first-release
// schema gate's unsupported-version rejection its stable code before any
// exclusive lock is attempted.
func PeekSchemaState(dataDirectory string) (version, digest string, ok bool) {
	databasePath := filepath.Join(dataDirectory, "quoin.db")
	if _, err := os.Stat(databasePath); err != nil {
		return "", "", false
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", "", false
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT schema_version,schema_digest FROM schema_state WHERE id=1`).Scan(&version, &digest); err != nil {
		return "", "", false
	}
	return version, digest, true
}

func (database *Database) Close() error {
	if database == nil {
		return nil
	}
	dbErr := database.SQL.Close()
	lockErr := database.lock.Close()
	if dbErr != nil {
		return dbErr
	}
	return lockErr
}

func initializeDatabase(ctx context.Context, db *sql.DB, rootKey []byte) error {
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&journal); err != nil || strings.ToLower(journal) != "wal" {
		return fmt.Errorf("enable WAL journal: value=%q err=%w", journal, err)
	}
	body := strings.Replace(gen.SchemaSQL, schemaPragmaPrefix(), "", 1)
	if body == gen.SchemaSQL {
		return fmt.Errorf("generated schema pragma block changed unexpectedly")
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	nonce, ciphertext, err := SealRootKeyVerifier(rootKey, 1)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("create current schema: %w", err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,?)`, []any{hex.EncodeToString(digest[:]), now}},
		{`INSERT INTO root_key_state(id,binding_revision,verifier_nonce,verifier_ciphertext,bound_at) VALUES(1,1,?,?,?)`, []any{nonce, ciphertext, now}},
		{`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`, nil},
		{`INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,?),('lintel','unregistered',1,?)`, []any{now, now}},
		{`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,?)`, []any{now}},
		{`INSERT INTO backup_settings(id,enabled,schedule_cron,timezone,retention_count,schedule_enabled_at,row_version,updated_at) VALUES(1,1,'0 0 * * *','UTC',30,?,1,?)`, []any{now, now}},
		{`INSERT INTO backup_retention_health(id) VALUES(1)`, nil},
		{`INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,?)`, []any{now}},
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("initialize singleton state: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	if err := conn.Close(); err != nil {
		return err
	}
	return verifyPragmas(ctx, db)
}

func verifyDatabase(ctx context.Context, db *sql.DB, rootKey []byte) error {
	if err := verifyPragmas(ctx, db); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	var version, storedDigest string
	if err := db.QueryRowContext(ctx, `SELECT schema_version,schema_digest FROM schema_state WHERE id=1`).Scan(&version, &storedDigest); err != nil {
		return fmt.Errorf("read schema state: %w", err)
	}
	if version != "v1" || storedDigest != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("database schema version or digest does not match this release")
	}
	var revision int
	var nonce, ciphertext []byte
	if err := db.QueryRowContext(ctx, `SELECT binding_revision,verifier_nonce,verifier_ciphertext FROM root_key_state WHERE id=1`).Scan(&revision, &nonce, &ciphertext); err != nil {
		return fmt.Errorf("read root key binding: %w", err)
	}
	if err := VerifyRootKeyVerifier(rootKey, revision, nonce, ciphertext); err != nil {
		return fmt.Errorf("root key does not authenticate the database binding")
	}
	return nil
}

func verifyPragmas(ctx context.Context, db *sql.DB) error {
	checks := []struct {
		query string
		want  string
	}{{`PRAGMA journal_mode`, "wal"}, {`PRAGMA synchronous`, "2"}, {`PRAGMA foreign_keys`, "1"}, {`PRAGMA recursive_triggers`, "1"}}
	for _, check := range checks {
		var got string
		if err := db.QueryRowContext(ctx, check.query).Scan(&got); err != nil {
			return err
		}
		if strings.ToLower(got) != check.want {
			return fmt.Errorf("%s=%s, want %s", check.query, got, check.want)
		}
	}
	return nil
}

// SealRootKeyVerifier is the canonical root-binding verifier constructor.
// Bootstrap and the offline rebind command share it so their cryptographic
// format cannot drift into competing authorities.
func SealRootKeyVerifier(key []byte, revision int) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(verifierPlaintext), verifierAAD(revision))
	return nonce, ciphertext, nil
}

// VerifyRootKeyVerifier authenticates the canonical root-binding verifier.
func VerifyRootKeyVerifier(key []byte, revision int, nonce, ciphertext []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, verifierAAD(revision))
	if err != nil || string(plaintext) != verifierPlaintext {
		return fmt.Errorf("verifier authentication failed")
	}
	return nil
}

func verifierAAD(revision int) []byte {
	return []byte(fmt.Sprintf("quoin:root-key-verifier:v1:%d", revision))
}

func schemaPragmaPrefix() string {
	return "PRAGMA journal_mode = WAL;\nPRAGMA synchronous = FULL;\nPRAGMA foreign_keys = ON;\nPRAGMA recursive_triggers = ON;\n"
}
