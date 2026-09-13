package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// declarationCutoverFixture opens the exact released predecessor extracted
// from the deployment image. It intentionally does not "advance" another
// fixture then rewrite schema_state: the fixture bytes themselves are the
// precise admission proof for the one accepted predecessor.
func declarationCutoverFixture(t *testing.T) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "declaration-cutover-predecessor.sql"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if actual := hex.EncodeToString(digest[:]); actual != declarationCutoverSchemaDigest {
		t.Fatalf("predecessor fixture digest=%s want=%s", actual, declarationCutoverSchemaDigest)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "declaration-cutover.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	// The embedded schema is DDL only. Bootstrap singleton rows are persisted
	// state in a real database and must be present before exercising admission.
	if _, err := db.Exec(`INSERT INTO schema_state(id,schema_version,schema_digest,upgraded_at) VALUES(1,'v1',?,'2026-01-01T00:00:00Z')`, declarationCutoverSchemaDigest); err != nil {
		t.Fatal(err)
	}
	var schemaRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_state WHERE id=1 AND schema_digest=?`, declarationCutoverSchemaDigest).Scan(&schemaRows); err != nil || schemaRows != 1 {
		t.Fatalf("physical predecessor schema_state rows=%d err=%v", schemaRows, err)
	}
	// A declaration-cutover predecessor is a physical release state with exactly
	// the direct migration recorded, rather than a fabricated digest transition.
	if _, err := db.Exec(`INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, directInvestigationMetricsMigrationID, migrationDigest(directInvestigationMetricsMigrationID), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	// Activate the historical Label Contract through its own predecessor command
	// while there are no enabled systems. Bootstrap owns this null pointer row;
	// the release trigger permits only its later paired activation mutation.
	if _, err := db.Exec(`INSERT INTO label_contract_state(id,current_contract_id,current_activation_id,row_version,updated_at) VALUES(1,NULL,NULL,1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO label_contracts(id,version,yaml_body,contract_json,digest,parser_version,schema_version,state,row_version,created_at) VALUES(1,1,'label_contract:\n  business_system_label: system','{"label_contract":{"business_system_label":"system"}}',?, 'p','v1','draft',1,'2026-01-01T00:00:00Z')`, digest64()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO label_contract_activations(contract_id,expected_target_row_version,expected_state_row_version,expected_current_contract_id,items_json,created_at) VALUES(1,1,1,NULL,'[]','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedCurrentLegacyConfiguration(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO connections(id,name,type,enabled,row_version,revalidation_required,created_at) VALUES(7,'legacy-metrics','thanos',1,1,0,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(9,'legacy-alerts','alertmanager',1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO business_systems(id,key,display_name,enabled,row_version,created_at) VALUES(1,'legacy','Legacy',0,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,journey_catalog_digest,journey_catalog_version,digest,created_by,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(1,1,'draft','legacy bytes: do-not-change','legacy-parser','legacy-v1',1,?,'legacy-catalog',?,NULL,?,'legacy','Legacy',7,1,'UTC')`, digest64(), digest64(), now)
	if err != nil {
		t.Fatal(err)
	}
	versionID, _ := result.LastInsertId()
	if _, err := db.Exec(`INSERT INTO config_alert_source_refs(config_version_id,alert_source_id) VALUES(?,9)`, versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO config_discoveries(config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(?,'pods','Pods','up{job="checkout"}','["job","instance"]')`, versionID); err != nil {
		t.Fatal(err)
	}
	plan, err := db.Exec(`INSERT INTO config_plans(config_version_id,plan_key,display_name,cron) VALUES(?,'health','Health','*/5 * * * *')`, versionID)
	if err != nil {
		t.Fatal(err)
	}
	planID, _ := plan.LastInsertId()
	for _, check := range []struct{ key, display, question, expression string }{
		{"up", "Up", "reachable", "up"},
		{"mysql-up", "MySQL Up", "database reachable", "mysql_up"},
		{"redis-up", "Redis Up", "cache reachable", "redis_up"},
	} {
		if _, err := db.Exec(`INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression) VALUES(?,?,?,?, 'promql','instant',?)`, planID, check.key, check.display, check.question, check.expression); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE business_systems SET current_config_version_id=?,display_name='Legacy',enabled=1,timezone='UTC',row_version=row_version+1 WHERE id=1`, versionID); err != nil {
		t.Fatal(err)
	}
	return versionID
}

func digest64() string { return "0000000000000000000000000000000000000000000000000000000000000000" }

func TestDeclarationCutoverFallsBackToNestedHistoricalContractLabel(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	// No explicit alert conditions: deployed historical contract_json is nested.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDeclarationCutoverOn(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	var successor int64
	if err := db.QueryRow(`SELECT canonical_config_version_id FROM legacy_config_version_mappings WHERE legacy_config_version_id=?`, legacyID).Scan(&successor); err != nil {
		t.Fatal(err)
	}
	var label, value string
	if err := db.QueryRow(`SELECT label_name,label_value FROM config_alert_label_conditions WHERE config_version_id=?`, successor).Scan(&label, &value); err != nil || label != "system" || value != "legacy" {
		t.Fatalf("nested fallback label=%q value=%q err=%v", label, value, err)
	}
}

func TestDeclarationCutoverPreservesCurrentLegacyHistoryAndAppendsCanonicalSuccessor(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	if _, err := db.Exec(`INSERT INTO config_alert_label_conditions(config_version_id,label_name,label_value) VALUES(?,'service','checkout')`, legacyID); err != nil {
		t.Fatal(err)
	}
	var originalYAML, originalDigest, originalSelector string
	if err := db.QueryRow(`SELECT yaml_body,digest FROM business_system_config_versions WHERE id=?`, legacyID).Scan(&originalYAML, &originalDigest); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT selector FROM config_discoveries WHERE config_version_id=?`, legacyID).Scan(&originalSelector); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDeclarationCutoverOn(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	var historicalYAML, historicalDigest, historicalState, selector string
	var historicalDeclaration sql.NullString
	if err := db.QueryRow(`SELECT yaml_body,digest,declaration_json,state FROM business_system_config_versions WHERE id=?`, legacyID).Scan(&historicalYAML, &historicalDigest, &historicalDeclaration, &historicalState); err != nil {
		t.Fatal(err)
	}
	if historicalYAML != originalYAML || historicalDigest != originalDigest || historicalDeclaration.Valid || historicalState != "superseded" {
		t.Fatalf("legacy changed yaml=%q digest=%q declaration=%v state=%q", historicalYAML, historicalDigest, historicalDeclaration, historicalState)
	}
	if err := db.QueryRow(`SELECT selector FROM config_discoveries WHERE config_version_id=?`, legacyID).Scan(&selector); err != nil || selector != originalSelector {
		t.Fatalf("legacy projection changed selector=%q err=%v", selector, err)
	}
	var successorID, currentID int64
	if err := db.QueryRow(`SELECT canonical_config_version_id FROM legacy_config_version_mappings WHERE legacy_config_version_id=? AND migration_id=?`, legacyID, declarationCutoverMigrationID).Scan(&successorID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT current_config_version_id FROM business_systems WHERE id=1`).Scan(&currentID); err != nil || currentID != successorID {
		t.Fatalf("pointer=%d successor=%d err=%v", currentID, successorID, err)
	}
	var declaration string
	if err := db.QueryRow(`SELECT declaration_json FROM business_system_config_versions WHERE id=?`, successorID).Scan(&declaration); err != nil || declaration == "" {
		t.Fatalf("successor declaration=%q err=%v", declaration, err)
	}
	var compiled struct {
		Resources []struct {
			AllowedMetrics []string `json:"allowedMetrics"`
		} `json:"resources"`
		AlertSourceLabels map[string]string `json:"alertSourceLabels"`
	}
	if err := json.Unmarshal([]byte(declaration), &compiled); err != nil {
		t.Fatal(err)
	}
	if len(compiled.Resources) != 1 || len(compiled.Resources[0].AllowedMetrics) != 3 || compiled.Resources[0].AllowedMetrics[0] != "mysql_up" || compiled.Resources[0].AllowedMetrics[1] != "redis_up" || compiled.Resources[0].AllowedMetrics[2] != "up" || compiled.AlertSourceLabels["service"] != "checkout" {
		t.Fatalf("compiled=%+v", compiled)
	}
	var successorYAML, successorDigest, successorTimezone, systemTimezone string
	if err := db.QueryRow(`SELECT yaml_body,digest,timezone FROM business_system_config_versions WHERE id=?`, successorID).Scan(&successorYAML, &successorDigest, &successorTimezone); err != nil {
		t.Fatal(err)
	}
	parsed, fields := config.ParseBusinessSystem([]byte(successorYAML), config.Limits{})
	if len(fields) != 0 {
		t.Fatalf("successor YAML did not pass canonical parser: %v", fields)
	}
	parsedDocument, err := config.CompileBusinessSystemDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	parsedDocument.MetricsConnectionID = 7
	parsedDocument.AlertSourceIDs = []int64{9}
	if successorDigest != parsedDocument.Digest() || successorTimezone != parsedDocument.Timezone {
		t.Fatalf("successor canonical digest/timezone=%q/%q parsed=%q/%q", successorDigest, successorTimezone, parsedDocument.Digest(), parsedDocument.Timezone)
	}
	if err := db.QueryRow(`SELECT timezone FROM business_systems WHERE id=1`).Scan(&systemTimezone); err != nil || systemTimezone != successorTimezone {
		t.Fatalf("root timezone=%q successor timezone=%q err=%v", systemTimezone, successorTimezone, err)
	}
}
