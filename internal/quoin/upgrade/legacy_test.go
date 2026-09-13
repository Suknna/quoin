package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	// Bootstrap registers the sha256 SQLite scalar required by the frozen schema
	// triggers. The test runs the released schema, not a hand-built facsimile.
	_ "github.com/Suknna/quoin/internal/quoin/bootstrap"
	_ "modernc.org/sqlite"
)

const legacyConfigInsert = `INSERT INTO business_system_config_versions(
	business_system_id,version_seq,state,yaml_body,parser_version,schema_version,
	label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,
	system_key,display_name,enabled,timezone,resource_refresh_interval_seconds,created_at)
	VALUES(1,1,'draft',?,'p','v1',1,
	'0000000000000000000000000000000000000000000000000000000000000000','catalog',
	'0000000000000000000000000000000000000000000000000000000000000000',
	'legacy','Legacy',1,'UTC',300,'2026-01-01T00:00:00Z')`

// newLegacyMigrationFixture executes the exact schema published at 5547ea0.
// It disables its guards only while deliberately constructing historical facts,
// then reinstates the released immutable-version guard that the migrator must
// temporarily lift. This exercises actual FK-bearing tables, checks and the
// released trigger shape rather than a synthetic five-table substitute.
func newLegacyMigrationFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "legacy-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "legacy.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	// Construct a released persisted database without pretending these are
	// application writes. The migration's own behavior runs with all constraints.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		triggers = append(triggers, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range triggers {
		if _, err := db.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
	seed := []string{
		`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1','` + legacyMetricsBusinessSchemaDigest + `','2026-01-01T00:00:00Z')`,
		`INSERT INTO label_contracts(id,version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at) VALUES(1,1,'label_contract: {}','{}','0000000000000000000000000000000000000000000000000000000000000000','p','v1','draft',1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'legacy','Legacy',0,1,'2026-01-01T00:00:00Z')`,
	}
	for _, statement := range seed {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER trg_business_config_versions_no_content_update BEFORE UPDATE OF
		business_system_id, version_seq, yaml_body, parser_version, schema_version,
		label_contract_version_id, journey_catalog_digest, journey_catalog_version,
		system_key, display_name, enabled, timezone, resource_refresh_interval_seconds,
		digest, created_by, created_at ON business_system_config_versions
		BEGIN SELECT RAISE(ABORT, 'business_system_config_version content is immutable'); END`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedLegacyConfig(t *testing.T, db *sql.DB, yaml string) {
	t.Helper()
	if _, err := db.Exec(legacyConfigInsert, yaml); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMigrationMapsOnlyUniqueConnectionAndRetiresRefreshWork(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	if _, err := db.Exec(`INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(7,'legacy-metrics','thanos',1,1,0,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seedLegacyConfig(t, db, "resource_refresh_interval_seconds: 300")
	if _, err := db.Exec(`INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES(21,'inspection_collection','resource_refresh_run',1,'pods','Queued','v1','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	report, err := migrateLegacyMetricsBusinessOn(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if report.MappedMetricsConnection == nil || *report.MappedMetricsConnection != 7 || report.LegacyConfigVersions != 1 {
		t.Fatalf("report=%+v", report)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	var mapped int64
	var yaml string
	if err := db.QueryRow(`SELECT metrics_connection_id,yaml_body FROM business_system_config_versions WHERE id=1`).Scan(&mapped, &yaml); err != nil || mapped != 7 || yaml != "resource_refresh_interval_seconds: 300" {
		t.Fatalf("config mapping/yaml=%d/%q err=%v", mapped, yaml, err)
	}
	var queued string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=21`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != "Cancelled" {
		t.Fatalf("refresh queued state=%s", queued)
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, legacyMetricsBusinessMigrationID).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger=%d err=%v", ledger, err)
	}
}

func TestLegacyMigrationBlocksMissingOrAmbiguousMappingsAtomically(t *testing.T) {
	for _, connections := range []string{"", `INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(1,'first','thanos',1,1,0,'2026-01-01T00:00:00Z'),(2,'second','thanos',1,1,0,'2026-01-01T00:00:00Z')`} {
		t.Run("candidate-set", func(t *testing.T) {
			db := newLegacyMigrationFixture(t)
			if connections != "" {
				if _, err := db.Exec(`DROP INDEX ux_connections_one_enabled_thanos`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(connections); err != nil {
					t.Fatal(err)
				}
			}
			seedLegacyConfig(t, db, "historical: true")
			if _, err := db.Exec(`INSERT INTO execution_attempts(id,attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES(21,'inspection_collection','resource_refresh_run',1,'pods','Queued','v1','2026-01-01T00:00:00Z')`); err != nil {
				t.Fatal(err)
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
				t.Fatal(err)
			}
			_, err = migrateLegacyMetricsBusinessOn(context.Background(), conn)
			if !errors.Is(err, ErrLegacyMigrationBlocked) {
				t.Fatalf("err=%v", err)
			}
			if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
				t.Fatal(err)
			}
			var columns int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('business_system_config_versions') WHERE name='metrics_connection_id'`).Scan(&columns); err != nil || columns != 0 {
				t.Fatalf("atomic rollback left metrics column=%d err=%v", columns, err)
			}
			var state string
			if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=21`).Scan(&state); err != nil || state != "Queued" {
				t.Fatalf("atomic rollback state=%s err=%v", state, err)
			}
		})
	}
}

func TestLegacyMigrationRebuildsKnowledgeFTSAndPreservesSequence(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	// These linked rows are real retained search facts, not direct FTS shadow
	// writes. The rebuilt index must answer the same query after triggers were
	// deliberately suspended during the schema copy.
	seed := []string{
		`INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(9,'knowledge_import','0000000000000000000000000000000000000000000000000000000000000000',25,'postgres failover runbook','2026-01-01T00:00:00Z')`,
		`INSERT INTO knowledge_import_batches(id,source_material_id,state,created_at) VALUES(9,9,'Completed','2026-01-01T00:00:00Z')`,
		`INSERT INTO knowledge_candidates(id,import_batch_id,source_type,source_id,state,original_suggestion_json,created_at) VALUES(9,9,'source_material',9,'Confirmed','{}','2026-01-01T00:00:00Z')`,
		`INSERT INTO reusable_knowledge(id,created_at) VALUES(9,'2026-01-01T00:00:00Z')`,
		`INSERT INTO knowledge_versions(id,knowledge_id,version_seq,title,body,source_candidate_id,created_at) VALUES(9,9,1,'Postgres failover','postgres failover runbook',9,'2026-01-01T00:00:00Z')`,
		`INSERT INTO knowledge_version_retrieval_state(knowledge_version_id,updated_at) VALUES(9,'2026-01-01T00:00:00Z')`,
		`INSERT INTO knowledge_search_docs(knowledge_version_id,title,body) VALUES(9,'Postgres failover','postgres failover runbook')`,
	}
	for _, statement := range seed {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateLegacyMetricsBusinessOn(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM knowledge_fts WHERE knowledge_fts MATCH 'failover'`).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("FTS hits=%d err=%v", hits, err)
	}
	var sequence int
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='knowledge_versions'`).Scan(&sequence); err != nil || sequence < 9 {
		t.Fatalf("sequence=%d err=%v", sequence, err)
	}
}

func TestDirectInvestigationMetricsMigrationPreservesHistoryAndUpdatesDigest(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	// Model the immediately preceding rc2 canonical schema, including retained
	// historical facts that the canonical rebuild must copy byte-for-byte.
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest=? WHERE id=1`, directInvestigationMetricsSchemaDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO source_materials(id,kind,digest,size_bytes,content,created_at) VALUES(41,'knowledge_import','0000000000000000000000000000000000000000000000000000000000000000',7,'history','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	report, err := migrateDirectInvestigationMetricsOn(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if report.MigrationID != directInvestigationMetricsMigrationID || report.LegacySchemaDigest != directInvestigationMetricsSchemaDigest {
		t.Fatalf("report=%+v", report)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM source_materials WHERE id=41`).Scan(&content); err != nil || content != "history" {
		t.Fatalf("preserved history=%q err=%v", content, err)
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, directInvestigationMetricsMigrationID).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger=%d err=%v", ledger, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	target := sha256.Sum256([]byte(gen.SchemaSQL))
	if stored != hex.EncodeToString(target[:]) {
		t.Fatalf("schema digest=%s want %x", stored, target)
	}
}

func TestReleasedSchemaMigrationRollsBackWhenCompletionCannotExitMaintenance(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest=? WHERE id=1`, directInvestigationMetricsSchemaDigest); err != nil {
		t.Fatal(err)
	}
	// The converter may have rebuilt every table and written its ledger before
	// completion is reached. A failure there must still retain the predecessor
	// digest and zero history so the supported Migrate command can retry.
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,enabled,password_phc,auth_revision,created_at,updated_at) VALUES(1,'upgrade-admin','Upgrade Admin','admin',1,'x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_state(id,active,reason,entered_at,entered_by_type,entered_by_id,row_version) VALUES(1,1,'Upgrade','2026-01-01T00:00:00Z','system',0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(1,'BackupPreflight','pre_upgrade_backup','Safe','backup_verified','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO backups(status,stage,trigger_kind,execution_mode,db_sha256,manifest_sha256,artifact_count,size_bytes,manifest_path,row_version,created_at,updated_at,started_at,completed_at,triggered_by) VALUES('succeeded','completed','upgrade','online',?,?,?,?,?,1,?,?,?,?,1)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0, 1, "/backup/manifest.json", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	completionFailure := errors.New("simulated_crash_before_maintenance_exit")
	if _, err := migrateReleasedSchemaTransaction(context.Background(), db, migrateDirectInvestigationMetricsOn, func(context.Context, *sql.Conn, LegacyMigrationReport) error { return completionFailure }); !errors.Is(err, completionFailure) {
		t.Fatalf("migration error=%v", err)
	}
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != directInvestigationMetricsSchemaDigest {
		t.Fatalf("schema digest after rollback=%q err=%v", digest, err)
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, directInvestigationMetricsMigrationID).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("ledger after rollback=%d err=%v", ledger, err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("maintenance after rollback active=%d err=%v", active, err)
	}
}

func TestDirectInvestigationMetricsMigrationRejectsUnknownDigest(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDirectInvestigationMetricsOn(context.Background(), conn); !errors.Is(err, ErrLegacyMigrationBlocked) {
		t.Fatalf("err=%v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMigrationAllowsEmptyDeploymentWithoutMetrics(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	report, err := migrateLegacyMetricsBusinessOn(context.Background(), conn)
	if err != nil || report.MappedMetricsConnection != nil || report.LegacyConfigVersions != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMigrationRejectsUnknownHistory(t *testing.T) {
	db := newLegacyMigrationFixture(t)
	if _, err := db.Exec(`UPDATE schema_state SET schema_digest='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	_, err = migrateLegacyMetricsBusinessOn(context.Background(), conn)
	if !errors.Is(err, ErrLegacyMigrationBlocked) {
		t.Fatalf("err=%v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}
