package upgrade

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

// oidcAuthRetireFixture opens the byte-exact predecessor capture（仍带
// auth_flows / auth_challenges / auth_delivery_settings 的最后一个验收版本）
// 并写入其钉死的身份。
func oidcAuthRetireFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "oidc-auth-retire-predecessor.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	schema, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if hex.EncodeToString(digest[:]) != oidcAuthRetirePredecessorSchemaDigest {
		t.Fatalf("predecessor identity differs: %x", digest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "predecessor.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-20T00:00:00Z')`, oidcAuthRetirePredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedOIDCAuthRetireState plants predecessor history the conversion must drop
// (one pending login flow with its challenge and the deployment delivery
// preset) while the durable auth surface — users, contacts, sessions —
// survives verbatim.
func seedOIDCAuthRetireState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-20T00:00:00Z"
	flowDigest := make([]byte, 32)
	challengeDigest := make([]byte, 32)
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(2,'op','Operator','operator',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO user_contacts(id,user_id,channel,target,created_at,updated_at) VALUES(1,1,'email','admin@quoin.test','" + now + "','" + now + "')",
		"INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,x'" + hex.EncodeToString(flowDigest) + "',1,'Browser on Test','" + now + "','" + now + "','" + now + "','" + now + "')",
		"INSERT INTO auth_flows(id,flow_type,user_id,flow_token_digest,auth_revision_at_issue,password_set,client_label,status,created_at,expires_at) VALUES(1,'login',2,x'" + hex.EncodeToString(challengeDigest) + "',1,1,'Browser on Test','pending','" + now + "','" + now + "')",
		"INSERT INTO auth_challenges(id,flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,created_at,expires_at) VALUES(1,1,2,'second_factor',1,1,1,x'" + hex.EncodeToString(challengeDigest) + "','d-1','" + now + "','" + now + "')",
		"INSERT INTO auth_delivery_settings(id,source,configuration_json,root_binding_revision,row_version,updated_at) VALUES(1,'deployment','{}',1,1,'" + now + "')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func TestOIDCAuthRetireDropsFlowTables(t *testing.T) {
	db := oidcAuthRetireFixture(t)
	seedOIDCAuthRetireState(t, db)
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := Preflight(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	// 退役三表与其索引整体消失，新 identities 表就位。
	for _, object := range []string{
		"auth_flows", "auth_challenges", "auth_delivery_settings",
		"idx_auth_flows_user", "idx_auth_challenges_flow",
	} {
		var present int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, object).Scan(&present); err != nil || present != 0 {
			t.Fatalf("retired object %s still present: %d %v", object, present, err)
		}
	}
	for _, object := range []string{"identities", "idx_identities_user"} {
		var present int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, object).Scan(&present); err != nil || present != 1 {
			t.Fatalf("canonical object %s missing: %d %v", object, present, err)
		}
	}
	// 用户、联系方式与会话逐字保留；既有用户 initial_password_expires_at 落 NULL。
	var users, contacts, sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil || users != 2 {
		t.Fatalf("users changed: %d %v", users, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_contacts`).Scan(&contacts); err != nil || contacts != 1 {
		t.Fatalf("contacts changed: %d %v", contacts, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions changed: %d %v", sessions, err)
	}
	var expiryNull int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users WHERE initial_password_expires_at IS NULL`).Scan(&expiryNull); err != nil || expiryNull != 2 {
		t.Fatalf("initial password expiry not defaulted to NULL: %d %v", expiryNull, err)
	}
	var identities int
	if err := db.QueryRow(`SELECT COUNT(*) FROM identities`).Scan(&identities); err != nil || identities != 0 {
		t.Fatalf("identities not empty: %d %v", identities, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if stored != hex.EncodeToString(digest[:]) {
		t.Fatal("target digest not stamped")
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=? AND digest=?`,
		oidcAuthRetireMigrationID, migrationDigest(oidcAuthRetireMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("authentic oidc-auth ledger missing: %d %v", ledger, err)
	}
	if _, err := Migrate(context.Background(), db); !errors.Is(err, ErrNotUpgradeMaintenance) {
		t.Fatalf("completed migration retry=%v", err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := verifySchemaGate(context.Background(), conn, &PreflightResult{}); err != nil {
		t.Fatalf("authentic migrated canonical rejected: %v", err)
	}
}

func TestOIDCAuthRetireRejectsForgedHistory(t *testing.T) {
	db := oidcAuthRetireFixture(t)
	seedOIDCAuthRetireState(t, db)
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES('forged',printf('%064d',0),'2026-09-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(context.Background(), db); !errors.Is(err, ErrSchemaHistoryPresent) {
		t.Fatalf("got=%v want=%v", err, ErrSchemaHistoryPresent)
	}
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state`).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != oidcAuthRetirePredecessorSchemaDigest {
		t.Fatal("failed migration changed schema")
	}
	var flows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_flows`).Scan(&flows); err != nil || flows != 1 {
		t.Fatalf("failed migration changed data: %d %v", flows, err)
	}
}
