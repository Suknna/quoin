package upgrade

// ADR-0004 前置发布的迁移测试：a7d990（最后的声明治理发布版）到当前规范的
// 转换是纯增量——新增表为空、历史行逐列保留、账本与 digest 一次性落定。
// 固定装置就是前置发布 schema 本身，digest 断言即准入证明。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

func pluginRegistryFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "plugin-registry-predecessor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if actual := hex.EncodeToString(digest[:]); actual != pluginRegistrySchemaDigest {
		t.Fatalf("predecessor fixture digest=%s want=%s", actual, pluginRegistrySchemaDigest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "plugin-registry.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-01-01T00:00:00Z')`, pluginRegistrySchemaDigest); err != nil {
		t.Fatal(err)
	}
	// 构造历史事实：真实数据库不会由应用写入这些组合（活动写闭合触发器仍在），
	// 因此固定装置按既有迁移测试模式暂时卸载触发器；被测迁移在约束齐全的
	// 事务中自行捕获/重建触发器。
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
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	const now = "2026-09-13T00:00:00Z"
	seed := []string{
		`INSERT INTO maintenance_state(id,active,row_version) VALUES(1,0,1)`,
		`INSERT INTO runtime_slots(slot,state,row_version,created_at) VALUES('plinth','unregistered',1,'` + now + `')`,
		`INSERT INTO label_contract_state(id,row_version,updated_at) VALUES(1,1,'` + now + `')`,
		`INSERT INTO backup_settings(id,enabled,schedule_cron,timezone,retention_count,schedule_enabled_at,row_version,updated_at) VALUES(1,1,'0 0 * * *','UTC',30,'2026-01-01T00:00:00Z',1,'` + now + `')`,
		`INSERT INTO artifact_retention_settings(id,generated_retention_days,row_version,updated_at) VALUES(1,90,1,'` + now + `')`,
		`INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'legacy','Legacy',1,1,'` + now + `')`,
		"INSERT INTO credential_generations(id,connection_id,generation_seq,envelope_version,key_binding_revision,nonce,ciphertext,created_at) VALUES(1,1,1,1,1,zeroblob(12),zeroblob(16),'" + now + "')",
		"INSERT INTO connection_revisions(id,connection_id,revision_seq,config_json,created_at) VALUES(1,1,1,'{}','" + now + "')",
		`INSERT INTO connections(id,name,type,enabled,row_version,current_revision_id,current_credential_generation_id,created_at) VALUES(1,'legacy-metrics','thanos',1,1,1,1,'` + now + `')`,
		`INSERT INTO browser_identities(id,business_system_id,current_revision_id,state,row_version,created_at) VALUES(1,1,1,'AuthenticationRequired',1,'` + now + `')`,
		`INSERT INTO browser_identity_revisions(id,business_system_id,revision,name,start_url,probe_journey_id,probe_journey_version,probe_params_json,journey_catalog_digest,journey_catalog_version,created_at)
		 VALUES(1,1,1,'历史身份','https://fixture.internal/login','authentication.url-prefix.v1',1,'{}','` + digest64() + `','test','` + now + `')`,
		"INSERT INTO business_system_config_versions(id,business_system_id,version_seq,state,yaml_body,parser_version,schema_version,journey_catalog_digest,journey_catalog_version,digest,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(1,1,1,'published','legacy','legacy','legacy','" + digest64() + "','test','" + digest64() + "','" + now + "','legacy','Legacy',1,1,'UTC')",
		`UPDATE business_systems SET current_config_version_id=1 WHERE id=1`,
		`INSERT INTO inspection_runs(id,business_system_id,plan_key,config_version_id,trigger_kind,state,row_version,created_at) VALUES(1,1,'legacy-plan',1,'manual','Cancelled',1,'` + now + `')`,
		`INSERT INTO observed_resources(id,business_system_id,discovery_key,identity_key,labels_json,current,created_at) VALUES(1,1,'legacy','instance=1','{}',1,'` + now + `')`,
	}
	for _, statement := range seed {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %s: %v", statement[:40], err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPluginRegistryMigrationPreservesHistoryAndAddsCapacity(t *testing.T) {
	db := pluginRegistryFixture(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	report, err := migratePluginRegistryOn(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if report.MigrationID != pluginRegistryMigrationID || report.LegacySchemaDigest != pluginRegistrySchemaDigest {
		t.Fatalf("migration report = %+v", report)
	}
	// 历史事实逐列保留：业务系统、声明版本、历史 Run、观测资源、浏览器身份。
	var enabled int
	if err := conn.QueryRowContext(context.Background(), `SELECT enabled FROM business_systems WHERE id=1`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("business system history lost: %v %v", enabled, err)
	}
	var runState string
	if err := conn.QueryRowContext(context.Background(), `SELECT state FROM inspection_runs WHERE id=1`).Scan(&runState); err != nil || runState != "Cancelled" {
		t.Fatalf("legacy run history lost: %v %v", runState, err)
	}
	var identityCurrent int
	if err := conn.QueryRowContext(context.Background(), `SELECT current FROM observed_resources WHERE id=1`).Scan(&identityCurrent); err != nil || identityCurrent != 1 {
		t.Fatalf("observed resource history lost: %v %v", identityCurrent, err)
	}
	var identityKey sql.NullString
	if err := conn.QueryRowContext(context.Background(), `SELECT identity_key FROM browser_identities WHERE id=1`).Scan(&identityKey); err != nil || identityKey.Valid {
		t.Fatalf("bound identity must keep NULL identity_key: %v %v", identityKey, err)
	}
	// 新容量为空表：独立计划、来源观测、业务视图。
	var plans, observationRuns, views int
	if err := conn.QueryRowContext(context.Background(), `SELECT (SELECT COUNT(*) FROM inspection_plans),(SELECT COUNT(*) FROM observation_runs),(SELECT COUNT(*) FROM business_views)`).Scan(&plans, &observationRuns, &views); err != nil {
		t.Fatal(err)
	}
	if plans != 0 || observationRuns != 0 || views != 0 {
		t.Fatalf("new capacity must start empty: plans=%d observation=%d views=%d", plans, observationRuns, views)
	}
	// 历史声明 Run 仍然满足重塑后的互斥 CHECK：计划列全空。
	var planID, frozenParams sql.NullInt64
	if err := conn.QueryRowContext(context.Background(), `SELECT plan_id, frozen_params_json FROM inspection_runs WHERE id=1`).Scan(&planID, &frozenParams); err != nil {
		t.Fatal(err)
	}
	if planID.Valid || frozenParams.Valid {
		t.Fatalf("legacy run must keep the legacy shape: plan_id=%v frozen=%v", planID, frozenParams)
	}
	// 账本与 schema digest 一次性切换到当前规范。
	var ledgerRows int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, pluginRegistryMigrationID).Scan(&ledgerRows); err != nil || ledgerRows != 1 {
		t.Fatalf("plugin registry ledger rows=%d err=%v", ledgerRows, err)
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	var stored string
	if err := conn.QueryRowContext(context.Background(), `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hex.EncodeToString(digest[:]) {
		t.Fatalf("schema digest=%s want current release digest", stored)
	}
	// 迁移后的库对当前引导程序可见：verifySchemaGate 现在必须拒绝（非前置）。
	if err := verifySchemaGate(context.Background(), conn, &PreflightResult{}); err == nil {
		t.Fatal("migrated database must not be treated as a predecessor any more")
	}
}
