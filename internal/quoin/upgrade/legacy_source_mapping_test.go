package upgrade

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// The tests in this file pin the conservative alert-source mapping decided at
// the declaration cutover: an unrestricted predecessor scope may only shrink
// to the sources that immutably delivered attributed occurrences for the same
// business, an unevidenced scope must block instead of guessing, and explicit
// references are never rewritten.

// seedAttributedLegacyOccurrences writes historic alert occurrences the way
// intake froze them: each row is an immutable delivery fact attributed to one
// source and one business system. The released schema maintains its change-log
// trigger, so these rows are indistinguishable from production history.
func seedAttributedLegacyOccurrences(t *testing.T, db *sql.DB, sourceID, systemID, count int64) {
	t.Helper()
	const at = "2026-01-01T00:00:00Z"
	for i := int64(1); i <= count; i++ {
		fingerprint := make([]byte, 8)
		binary.BigEndian.PutUint64(fingerprint, uint64(i))
		if _, err := db.Exec(`INSERT INTO alert_occurrences(source_id,fingerprint,starts_at,state,labels_canonical,labels_digest,business_system_id,first_seen_at,last_state_change_at,resolved_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			sourceID, fingerprint, at, "Resolved", `{"alertname":"legacy-down"}`, strings.Repeat("a", 64), systemID, at, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// runDeclarationCutover drives the released conversion inside its own
// transaction. A failure is rolled back so callers can assert atomicity; a
// success commits so callers can inspect the appended successor.
func runDeclarationCutover(t *testing.T, db *sql.DB) (LegacyMigrationReport, error) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	report, err := migrateDeclarationCutoverOn(context.Background(), conn)
	if err != nil {
		if _, rollErr := conn.ExecContext(context.Background(), `ROLLBACK`); rollErr != nil {
			t.Fatal(rollErr)
		}
		return LegacyMigrationReport{}, err
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	return report, nil
}

// declarationCutoverSuccessor returns the one appended successor recorded for
// a migrated legacy version.
func declarationCutoverSuccessor(t *testing.T, db *sql.DB, legacyID int64) int64 {
	t.Helper()
	var successor int64
	if err := db.QueryRow(`SELECT canonical_config_version_id FROM legacy_config_version_mappings WHERE legacy_config_version_id=? AND migration_id=?`, legacyID, declarationCutoverMigrationID).Scan(&successor); err != nil {
		t.Fatal(err)
	}
	return successor
}

// successorAlertSourceKeys reads the explicit alert source keys projected on a
// successor version, in their canonical deterministic order.
func successorAlertSourceKeys(t *testing.T, db *sql.DB, versionID int64) []string {
	t.Helper()
	rows, err := db.Query(`SELECT s.source_key FROM config_alert_source_refs r JOIN alert_sources s ON s.id=r.alert_source_id WHERE r.config_version_id=? ORDER BY s.source_key`, versionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestDeclarationCutoverInfersSoleAlertSourceFromAttributedHistory(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	// A second enabled source exists in the deployment, but only source 9 ever
	// delivered for this business: the successor must subscribe exactly that
	// one, never the whole enabled pool.
	if _, err := db.Exec(`INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(10,'mall-gui-alertmanager','alertmanager',1,1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM config_alert_source_refs WHERE config_version_id=?`, legacyID); err != nil {
		t.Fatal(err)
	}
	seedAttributedLegacyOccurrences(t, db, 9, 1, 3)
	if _, err := runDeclarationCutover(t, db); err != nil {
		t.Fatal(err)
	}
	successor := declarationCutoverSuccessor(t, db, legacyID)
	if keys := successorAlertSourceKeys(t, db, successor); len(keys) != 1 || keys[0] != "legacy-alerts" {
		t.Fatalf("successor source refs=%v want [legacy-alerts]", keys)
	}
	var document struct {
		AlertSourceIDs []int64 `json:"alertSourceIDs"`
	}
	var declaration string
	if err := db.QueryRow(`SELECT declaration_json FROM business_system_config_versions WHERE id=?`, successor).Scan(&declaration); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(declaration), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.AlertSourceIDs) != 1 || document.AlertSourceIDs[0] != 9 {
		t.Fatalf("successor alert source ids=%v want [9]", document.AlertSourceIDs)
	}
	var yamlBody string
	if err := db.QueryRow(`SELECT yaml_body FROM business_system_config_versions WHERE id=?`, successor).Scan(&yamlBody); err != nil {
		t.Fatal(err)
	}
	// The inferred source becomes an explicit declaration fact: the persisted
	// mapping YAML names it, and the unused enabled source appears nowhere.
	if strings.Contains(yamlBody, "mall-gui-alertmanager") {
		t.Fatalf("successor YAML leaked an unused enabled source: %s", yamlBody)
	}
	parsed, fields := config.ParseBusinessSystem([]byte(yamlBody), config.Limits{})
	if len(fields) != 0 {
		t.Fatalf("successor YAML did not pass canonical parser: %v", fields)
	}
	if len(parsed.Spec.Alerts.SourceRefs) != 1 || parsed.Spec.Alerts.SourceRefs[0] != "legacy-alerts" {
		t.Fatalf("successor YAML sourceRefs=%v want [legacy-alerts]", parsed.Spec.Alerts.SourceRefs)
	}
}

func TestDeclarationCutoverBlocksSourceMappingWithoutRefsOrAttributableHistory(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	if _, err := db.Exec(`DELETE FROM config_alert_source_refs WHERE config_version_id=?`, legacyID); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeclarationCutover(t, db); !errors.Is(err, ErrLegacyMigrationBlocked) {
		t.Fatalf("err=%v want ErrLegacyMigrationBlocked", err)
	} else if !strings.Contains(err.Error(), "no explicit alert source mapping or attributed source history") {
		t.Fatalf("blocked without the specific source mapping reason: %v", err)
	}
	// The blocked conversion must leave the exact predecessor behind so the
	// stopped deployment can resume once an operator declares sources. The
	// rollback also restores the predecessor schema, so these assertions only
	// read predecessor-era tables.
	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions WHERE business_system_id=1`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("versions after block=%d err=%v", versions, err)
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, declarationCutoverMigrationID).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("ledger after block=%d err=%v", ledger, err)
	}
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != declarationCutoverSchemaDigest {
		t.Fatalf("schema digest after block=%q err=%v", digest, err)
	}
}

func TestDeclarationCutoverKeepsExplicitAlertSourceRefsUntouchedByHistory(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	// Explicit references are the operator's decision. Historic deliveries
	// from another source must never widen the explicit set.
	if _, err := db.Exec(`INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(10,'mall-gui-alertmanager','alertmanager',1,1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	seedAttributedLegacyOccurrences(t, db, 10, 1, 3)
	if _, err := runDeclarationCutover(t, db); err != nil {
		t.Fatal(err)
	}
	successor := declarationCutoverSuccessor(t, db, legacyID)
	if keys := successorAlertSourceKeys(t, db, successor); len(keys) != 1 || keys[0] != "legacy-alerts" {
		t.Fatalf("successor source refs=%v want exactly the explicit [legacy-alerts]", keys)
	}
	var yamlBody string
	if err := db.QueryRow(`SELECT yaml_body FROM business_system_config_versions WHERE id=?`, successor).Scan(&yamlBody); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(yamlBody, "mall-gui-alertmanager") {
		t.Fatalf("successor YAML widened explicit refs with history: %s", yamlBody)
	}
}

func TestDeclarationCutoverNeverFallsBackToAllEnabledAlertSources(t *testing.T) {
	db := declarationCutoverFixture(t)
	legacyID := seedCurrentLegacyConfiguration(t, db)
	if _, err := db.Exec(`INSERT INTO alert_sources(id,source_key,protocol,enabled,row_version,created_at) VALUES(10,'mall-gui-alertmanager','alertmanager',1,1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// The predecessor scope is unrestricted and two sources are currently
	// enabled, but neither has any attributable delivery for this business.
	// The old wildcard semantics must not resurface as an enabled-pool
	// fallback: the migration blocks and writes nothing.
	if _, err := db.Exec(`DELETE FROM config_alert_source_refs WHERE config_version_id=?`, legacyID); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeclarationCutover(t, db); !errors.Is(err, ErrLegacyMigrationBlocked) {
		t.Fatalf("err=%v want ErrLegacyMigrationBlocked instead of an enabled-source fallback", err)
	}
	// No successor may exist and the predecessor schema must be restored
	// byte-for-byte by the rollback: nothing was written anywhere.
	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM business_system_config_versions WHERE business_system_id=1`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("versions after fallback refusal=%d err=%v", versions, err)
	}
	var digest string
	if err := db.QueryRow(`SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&digest); err != nil || digest != declarationCutoverSchemaDigest {
		t.Fatalf("schema digest after fallback refusal=%q err=%v", digest, err)
	}
}
