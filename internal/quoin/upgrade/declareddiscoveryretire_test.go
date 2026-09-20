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

// declaredDiscoveryRetireFixture opens the byte-exact predecessor capture（仍带
// config_discoveries / observed_resources / observed_resource_identity_labels
// 的最后一个发布版本）并写入其钉死的身份。
func declaredDiscoveryRetireFixture(t *testing.T) *sql.DB {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "declared-discovery-retire-predecessor.sql.gz"))
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
	if hex.EncodeToString(digest[:]) != declaredDiscoveryRetirePredecessorSchemaDigest {
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
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-09-20T00:00:00Z')`, declaredDiscoveryRetirePredecessorSchemaDigest); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedDeclaredDiscoveryState plants predecessor history the conversion must
// drop: one draft version carrying a declared discovery plus one observed
// resource projection with its identity labels, while the authoritative
// business rows survive verbatim.
func seedDeclaredDiscoveryState(t *testing.T, db *sql.DB) {
	t.Helper()
	const now = "2026-09-20T00:00:00Z"
	statements := []string{
		"INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Administrator','admin',1,1,'fixture-hash',1,'" + now + "','" + now + "')",
		"INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'metrics','thanos',1,1,'" + now + "')",
		"INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'mall','Mall',0,1,'" + now + "')",
		"INSERT INTO business_system_config_versions(id,business_system_id,version_seq,state,yaml_body,parser_version,schema_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(1,1,1,'draft','yaml','p1','s1','3636363636363636363636363636363636363636363636363636363636363636','" + now + "','mall','Mall',1,1,'UTC')",
		"INSERT INTO config_discoveries(config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(1,'pods','Pods','up','[\"job\",\"instance\"]')",
		"INSERT INTO observed_resources(id,business_system_id,discovery_key,identity_key,labels_json,current,created_at) VALUES(1,1,'pods','job=\"checkout\",instance=\"i-1\"','{}',1,'" + now + "')",
		"INSERT INTO observed_resource_identity_labels(observed_resource_id,name,value) VALUES(1,'job','checkout'),(1,'instance','i-1')",
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func TestDeclaredDiscoveryRetireDropsThreeTables(t *testing.T) {
	db := declaredDiscoveryRetireFixture(t)
	seedDeclaredDiscoveryState(t, db)
	seedAuthAuditUpgradeWindow(t, db, 1)
	if _, err := Preflight(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	result, err := Migrate(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupID != 1 || len(result.ManifestSHA256) != 64 {
		t.Fatalf("migration summary lost backup provenance: %+v", result)
	}
	// 三表及其触发器整体消失。
	for _, object := range []string{
		"config_discoveries", "observed_resources", "observed_resource_identity_labels",
		"trg_config_discoveries_no_update", "trg_observed_resources_no_delete", "trg_observed_resource_identity_labels_no_update",
	} {
		var present int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, object).Scan(&present); err != nil || present != 0 {
			t.Fatalf("retired object %s still present: %d %v", object, present, err)
		}
	}
	// 权威业务行逐字保留。
	var versionState, systemKey string
	if err := db.QueryRow(`SELECT state FROM business_system_config_versions WHERE id=1`).Scan(&versionState); err != nil || versionState != "draft" {
		t.Fatalf("config version history changed: %q %v", versionState, err)
	}
	if err := db.QueryRow(`SELECT key FROM business_systems WHERE id=1`).Scan(&systemKey); err != nil || systemKey != "mall" {
		t.Fatalf("business history changed: %q %v", systemKey, err)
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
		declaredDiscoveryRetireMigrationID, migrationDigest(declaredDiscoveryRetireMigrationID)).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("authentic declared-discovery ledger missing: %d %v", ledger, err)
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

func TestDeclaredDiscoveryRetireRejectsForgedHistory(t *testing.T) {
	db := declaredDiscoveryRetireFixture(t)
	seedDeclaredDiscoveryState(t, db)
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
	if digest != declaredDiscoveryRetirePredecessorSchemaDigest {
		t.Fatal("failed migration changed schema")
	}
	var discoveries int
	if err := db.QueryRow(`SELECT COUNT(*) FROM config_discoveries`).Scan(&discoveries); err != nil || discoveries != 1 {
		t.Fatalf("failed migration changed data: %d %v", discoveries, err)
	}
}
