package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/config"
	"gopkg.in/yaml.v3"
)

const (
	legacyMetricsBusinessMigrationID  = "20260910_metrics_business_v1"
	legacyMetricsBusinessSchemaDigest = "ccf511559c56f885e585be2b61fa1a42c6dda48ec394632d1325448761bc55dc"

	// directInvestigationMetricsSchemaDigest is the only released #97 canonical
	// schema accepted for this narrow trigger correction. It permits no generic
	// digest upgrade and preserves every existing row through canonical rebuild.
	directInvestigationMetricsMigrationID  = "20260910_direct_investigation_metrics_v1"
	directInvestigationMetricsSchemaDigest = "f8adbfd4285eb5fd76acbf33f32b26fdc217ef08c6104aab0f6e5c0beb28d5c9"
	declarationCutoverMigrationID          = "20260911_declaration_cutover_v1"
	declarationCutoverSchemaDigest         = "3a95baf7b2ecab6a5f81fe084334fe953a5c23a3a71db3de1cc1d8c0ee107eb2"
)

var (
	ErrLegacyMigrationBlocked  = errors.New("legacy_migration_blocked")
	ErrLegacyMigrationRequired = errors.New("legacy_migration_required")
)

type LegacyMigrationReport struct {
	MigrationID             string  `json:"migrationId"`
	LegacySchemaDigest      string  `json:"legacySchemaDigest"`
	MappedMetricsConnection *int64  `json:"mappedMetricsConnectionId,omitempty"`
	LegacyConfigVersions    int64   `json:"legacyConfigVersions"`
	RetiredRefreshAttempts  []int64 `json:"retiredRefreshAttemptIds,omitempty"`
}

// migrateReleasedSchemaTransaction owns one all-or-nothing release conversion.
// The completion callback belongs inside this transaction: a canonical rebuild
// must never commit separately from the maintenance exit that accepts writes.
func migrateReleasedSchemaTransaction(ctx context.Context, db *sql.DB, migrate func(context.Context, *sql.Conn) (LegacyMigrationReport, error), complete func(context.Context, *sql.Conn, LegacyMigrationReport) error) (LegacyMigrationReport, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	defer conn.Close()
	// SQLite requires this before BEGIN for a table rebuild with cyclic history.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return LegacyMigrationReport{}, err
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return LegacyMigrationReport{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := preflightOperationalOn(ctx, conn); err != nil {
		return LegacyMigrationReport{}, err
	}
	report, err := migrate(ctx, conn)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	if err := verifyForeignKeys(ctx, conn); err != nil {
		return LegacyMigrationReport{}, err
	}
	if err := complete(ctx, conn, report); err != nil {
		return LegacyMigrationReport{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return LegacyMigrationReport{}, err
	}
	committed = true
	return report, nil
}

func migrateLegacyMetricsBusinessOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, legacyMetricsBusinessMigrationID, legacyMetricsBusinessSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_system_config_versions`).Scan(&report.LegacyConfigVersions); err != nil {
		return report, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT id FROM connections WHERE type='thanos' AND enabled=1 AND revalidation_required=0 ORDER BY id`)
	if err != nil {
		return report, err
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return report, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Close(); err != nil {
		return report, err
	}
	if report.LegacyConfigVersions != 0 && len(candidates) != 1 {
		return report, fmt.Errorf("%w: expected exactly one enabled legacy metrics connection, found %d", ErrLegacyMigrationBlocked, len(candidates))
	}
	if len(candidates) == 1 {
		report.MappedMetricsConnection = &candidates[0]
	}
	if err := rebuildCanonicalSchema(ctx, conn, report.MappedMetricsConnection, false); err != nil {
		return report, err
	}
	if err := retireLegacyRefreshAttempts(ctx, conn, &report); err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, legacyMetricsBusinessMigrationID, legacyMigrationDigest(), migrationNow()); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow()); err != nil {
		return report, err
	}
	return report, nil
}

// beginReleasedSchemaMigration applies a single exact-digest admission policy.
// The caller must already own the exclusive transaction and operational gate.
func beginReleasedSchemaMigration(ctx context.Context, conn *sql.Conn, migrationID, acceptedDigest string) (string, LegacyMigrationReport, error) {
	var stored string
	if err := conn.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		return "", LegacyMigrationReport{}, err
	}
	if stored != acceptedDigest {
		return "", LegacyMigrationReport{}, fmt.Errorf("%w: unsupported legacy schema digest %q", ErrLegacyMigrationBlocked, stored)
	}
	var existing int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, migrationID).Scan(&existing); err != nil {
		return "", LegacyMigrationReport{}, err
	}
	if migrationID == declarationCutoverMigrationID {
		var direct int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, directInvestigationMetricsMigrationID).Scan(&direct); err != nil {
			return "", LegacyMigrationReport{}, err
		}
		if existing != 0 || direct != 1 {
			return "", LegacyMigrationReport{}, ErrLegacyMigrationRequired
		}
	} else if existing != 0 {
		return "", LegacyMigrationReport{}, ErrLegacyMigrationRequired
	}
	return stored, LegacyMigrationReport{MigrationID: migrationID, LegacySchemaDigest: stored}, nil
}

func migrateDeclarationCutoverOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	stored, report, err := beginReleasedSchemaMigration(ctx, conn, declarationCutoverMigrationID, declarationCutoverSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report.LegacySchemaDigest = stored
	if err := rebuildCanonicalSchema(ctx, conn, nil, true); err != nil {
		return report, err
	}
	// The ledger row is written before mappings so their FK makes every old-to-new
	// link durable only as part of this one atomic release conversion.
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, declarationCutoverMigrationID, migrationDigest(declarationCutoverMigrationID), migrationNow()); err != nil {
		return report, err
	}
	if err := cutoverCurrentLegacyConfigurations(ctx, conn); err != nil {
		return report, fmt.Errorf("append declaration successors: %w", err)
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if _, err := conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow()); err != nil {
		return report, err
	}
	return report, nil
}

func migrateDirectInvestigationMetricsOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	_, report, err := beginReleasedSchemaMigration(ctx, conn, directInvestigationMetricsMigrationID, directInvestigationMetricsSchemaDigest)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	if err := rebuildCanonicalSchema(ctx, conn, nil, true); err != nil {
		return report, err
	}
	// Do not advertise the declaration-era schema while a current legacy row
	// remains projectionless. Earlier admitted releases cross the declaration
	// boundary atomically, including both ordered ledger facts and successors.
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, directInvestigationMetricsMigrationID, migrationDigest(directInvestigationMetricsMigrationID), migrationNow()); err != nil {
		return report, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO migration_ledger(migration_id,digest,applied_at) VALUES(?,?,?)`, declarationCutoverMigrationID, migrationDigest(declarationCutoverMigrationID), migrationNow()); err != nil {
		return report, err
	}
	if err := cutoverCurrentLegacyConfigurations(ctx, conn); err != nil {
		return report, fmt.Errorf("append declaration successors: %w", err)
	}
	digest := sha256.Sum256([]byte(gen.SchemaSQL))
	if _, err := conn.ExecContext(ctx, `UPDATE schema_state SET schema_digest=?,upgraded_at=? WHERE id=1`, hex.EncodeToString(digest[:]), migrationNow()); err != nil {
		return report, err
	}
	return report, nil
}

func rebuildCanonicalSchema(ctx context.Context, conn *sql.Conn, metrics *int64, preserveArchivedDeclarations bool) error {
	objects, err := schemaObjects(ctx, conn, "main")
	if err != nil {
		return err
	}
	for _, object := range objects.tables {
		if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE `+qid("legacy_"+object)+` AS SELECT * FROM main.`+qid(object)); err != nil {
			return err
		}
	}
	// sqlite_sequence is an internal table and therefore absent from the normal
	// object listing, yet its AUTOINCREMENT high-water values are persisted facts.
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE legacy_sqlite_sequence AS SELECT * FROM main.sqlite_sequence`); err != nil {
		return fmt.Errorf("snapshot autoincrement high-water marks: %w", err)
	}
	for _, view := range objects.views {
		if _, err := conn.ExecContext(ctx, `DROP VIEW main.`+qid(view)); err != nil {
			return err
		}
	}
	for i := len(objects.tables) - 1; i >= 0; i-- {
		if _, err := conn.ExecContext(ctx, `DROP TABLE main.`+qid(objects.tables[i])); err != nil {
			return err
		}
	}
	body := gen.SchemaSQL
	// Fresh bootstrap pragmas cannot execute within the rebuild transaction.
	for _, pragma := range []string{"PRAGMA journal_mode = WAL;\n", "PRAGMA synchronous = FULL;\n", "PRAGMA foreign_keys = ON;\n", "PRAGMA recursive_triggers = ON;\n"} {
		body = strings.Replace(body, pragma, "", 1)
	}
	if _, err := conn.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("create canonical schema: %w", err)
	}
	target, err := schemaObjects(ctx, conn, "main")
	if err != nil {
		return err
	}
	// Target triggers enforce normal command ordering (for example, a business
	// system is created disabled before its version is published). A raw historic
	// snapshot has no such insertion order. Preserve their exact canonical SQL,
	// suspend only the trigger programs for the copy, then restore them before
	// foreign-key validation and commit.
	triggerSQL, err := captureAndDropTriggers(ctx, conn)
	if err != nil {
		return err
	}
	for _, table := range target.tables {
		// FTS shadow/config tables are reconstructed by their virtual table, not
		// copied as application history. Their canonical seed rows are unique.
		if strings.HasPrefix(table, "knowledge_fts_") {
			continue
		}
		oldCols, err := tableColumns(ctx, conn, "temp", "legacy_"+table)
		if err != nil {
			return err
		}
		if len(oldCols) == 0 {
			continue
		}
		newCols, err := tableColumns(ctx, conn, "main", table)
		if err != nil {
			return err
		}
		oldSet := map[string]bool{}
		for _, c := range oldCols {
			oldSet[c] = true
		}
		var cols, selects []string
		for _, c := range newCols {
			if oldSet[c] {
				cols = append(cols, qid(c))
				selects = append(selects, qid(c))
				continue
			}
			if table == "business_system_config_versions" && c == "metrics_connection_id" && metrics != nil {
				cols = append(cols, qid(c))
				selects = append(selects, fmt.Sprint(*metrics))
			}
			// The declaration cutover preserves every old version without a
			// projection. Only its appended successor is executable. Earlier
			// released conversions retain their historical fail-closed JSON.
			if table == "business_system_config_versions" && c == "declaration_json" {
				cols = append(cols, qid(c))
				if preserveArchivedDeclarations {
					selects = append(selects, `NULL`)
				} else {
					metricsJSON := `NULL`
					if oldSet["metrics_connection_id"] {
						metricsJSON = `metrics_connection_id`
					} else if metrics != nil {
						metricsJSON = fmt.Sprint(*metrics)
					}
					selects = append(selects, `json_object('legacy',true,'systemKey',system_key,'displayName',display_name,'metricsConnectionID',`+metricsJSON+`,'resources',json_array())`)
				}
			}

			if table == "business_system_config_versions" && c == "description" {
				cols = append(cols, qid(c))
				selects = append(selects, `''`)
			}
			if table == "business_system_config_versions" && c == "discovery_refresh_seconds" {
				cols = append(cols, qid(c))
				selects = append(selects, `300`)
			}

		}
		if len(cols) == 0 {
			continue
		}
		query := `INSERT INTO main.` + qid(table) + ` (` + strings.Join(cols, ",") + `) SELECT ` + strings.Join(selects, ",") + ` FROM temp.` + qid("legacy_"+table)
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	// External-content FTS has no durable index when its synchronization

	// triggers are suspended for the historic copy. Rebuild it from the copied
	// authoritative search documents before restoring those triggers.
	if _, err := conn.ExecContext(ctx, `INSERT INTO knowledge_fts(knowledge_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild knowledge full-text index: %w", err)
	}
	if err := preserveSQLiteSequence(ctx, conn); err != nil {
		return err
	}
	for _, statement := range triggerSQL {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("restore canonical trigger: %w", err)
		}
	}
	return nil
}

// cutoverCurrentLegacyConfigurations appends one executable declaration for each
// current published legacy version. It never writes archive content or projections:
// their original YAML and digest remain audit facts and the durable mapping explains
// the active successor.
func cutoverCurrentLegacyConfigurations(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT b.id,b.current_config_version_id FROM business_systems b JOIN business_system_config_versions v ON v.id=b.current_config_version_id WHERE v.state='published' ORDER BY b.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type current struct{ systemID, versionID int64 }
	var currents []current
	for rows.Next() {
		var item current
		if err := rows.Scan(&item.systemID, &item.versionID); err != nil {
			return err
		}
		currents = append(currents, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range currents {
		if err := cutoverCurrentLegacyConfiguration(ctx, conn, item.systemID, item.versionID); err != nil {
			return err
		}
	}
	return nil
}

func cutoverCurrentLegacyConfiguration(ctx context.Context, conn *sql.Conn, systemID, legacyID int64) error {
	var browser int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id WHERE p.config_version_id=? AND c.kind='browser'`, legacyID).Scan(&browser); err != nil {
		return err
	}
	if browser != 0 {
		return fmt.Errorf("%w: current config version %d contains browser checks; preview and convert it explicitly", ErrLegacyMigrationBlocked, legacyID)
	}
	var discoveries int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM config_discoveries WHERE config_version_id=?`, legacyID).Scan(&discoveries); err != nil {
		return err
	}
	if discoveries != 1 {
		return fmt.Errorf("%w: current config version %d has %d legacy discoveries; upload an explicit declaration", ErrLegacyMigrationBlocked, legacyID, discoveries)
	}

	var key, display, timezone, selector, identityJSON string
	var enabled, connectionID int64
	// The declaration-era schema removed the former root refresh projection. Its
	// successor owns refresh policy, so read only retained legacy version facts.
	if err := conn.QueryRowContext(ctx, `SELECT v.system_key,v.display_name,v.timezone,v.enabled,d.selector,d.identity_labels_json FROM business_system_config_versions v JOIN config_discoveries d ON d.config_version_id=v.id WHERE v.id=?`, legacyID).Scan(&key, &display, &timezone, &enabled, &selector, &identityJSON); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT metrics_connection_id FROM business_system_config_versions WHERE id=?`, legacyID).Scan(&connectionID); err != nil {
		return fmt.Errorf("%w: current config version %d has no metrics connection: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}
	var connectionRef string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM connections WHERE id=?`, connectionID).Scan(&connectionRef); err != nil {
		return fmt.Errorf("%w: current config version %d metrics connection: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}
	metric, labels, err := legacySelectorScope(selector)
	if err != nil {
		return fmt.Errorf("%w: current config version %d discovery: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}
	var identity []string
	if err := json.Unmarshal([]byte(identityJSON), &identity); err != nil {
		return fmt.Errorf("%w: current config version %d identity labels: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}
	allowed, err := legacyMetrics(ctx, conn, legacyID, selector)
	if err != nil {
		return fmt.Errorf("%w: current config version %d metric allowlist: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}

	alertRefs, alertLabels, err := legacyAlertScope(ctx, conn, legacyID, key)
	if err != nil {
		return err
	}
	inspections, err := legacyInspections(ctx, conn, legacyID, "legacy-resource")
	if err != nil {
		return err
	}
	// Refresh policy did not exist in this predecessor. The declaration default is
	// deliberate, then re-parsed below so the normal schema and compiler are the
	// only authority for the persisted YAML, projections, and digest.
	declaration := config.BusinessSystem{APIVersion: "quoin/v1", Kind: "BusinessSystem", Metadata: config.BusinessSystemMetadata{Name: key, DisplayName: display, Description: ""}, Spec: config.BusinessSystemSpec{Enabled: enabled != 0, Metrics: config.MetricsScope{ConnectionRef: connectionRef, MatchLabels: labels, Resources: []config.ResourceScope{{Name: "legacy-resource", DisplayName: display, MatchLabels: labels, DiscoveryMetric: metric, IdentityLabels: identity, AllowedMetrics: allowed}}}, Alerts: config.AlertScope{SourceRefs: alertRefs, MatchLabels: alertLabels}, Inspections: inspections, Discovery: config.DiscoverySettings{Refresh: config.DefaultDiscoveryRefresh}}}
	yamlBody, err := canonicalLegacyYAML(declaration)
	if err != nil {
		return err
	}
	declaration, fields := config.ParseBusinessSystem(yamlBody, config.Limits{})
	if len(fields) != 0 {
		return fmt.Errorf("%w: current config version %d generated invalid declaration: %v", ErrLegacyMigrationBlocked, legacyID, fields)
	}
	document, err := config.CompileBusinessSystemDocument(declaration)
	if err != nil {
		return fmt.Errorf("%w: current config version %d cannot compile declaration: %v", ErrLegacyMigrationBlocked, legacyID, err)
	}
	document.MetricsConnectionID = connectionID
	if err := resolveLegacyAlertIDs(ctx, conn, &document); err != nil {
		return err
	}
	declarationJSON, err := json.Marshal(document)
	if err != nil {
		return err
	}
	digest := document.Digest()
	catalogVersion, catalogDigest, err := legacyCatalog()
	if err != nil {
		return err
	}
	var next int64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version_seq)+1 FROM business_system_config_versions WHERE business_system_id=?`, systemID).Scan(&next); err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO business_system_config_versions(business_system_id,version_seq,state,yaml_body,parser_version,schema_version,label_contract_version_id,declaration_json,description,discovery_refresh_seconds,journey_catalog_digest,journey_catalog_version,digest,created_by,created_at,system_key,display_name,metrics_connection_id,enabled,timezone) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, systemID, next, "draft", string(yamlBody), config.ParserVersion, config.SchemaVersion(config.SchemaBusinessSystem), nil, string(declarationJSON), document.Description, document.DiscoveryRefreshIntervalSeconds, catalogDigest, catalogVersion, digest, nil, migrationNow(), document.SystemKey, document.DisplayName, connectionID, enabled, document.Timezone)
	if err != nil {
		return err
	}
	successor, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err := insertLegacySuccessorProjections(ctx, conn, successor, document); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE business_systems SET current_config_version_id=?,display_name=?,enabled=?,timezone=?,row_version=row_version+1 WHERE id=? AND current_config_version_id=?`, successor, document.DisplayName, enabled, document.Timezone, systemID, legacyID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO legacy_config_version_mappings(legacy_config_version_id,canonical_config_version_id,migration_id,created_at) VALUES(?,?,?,?)`, legacyID, successor, declarationCutoverMigrationID, migrationNow()); err != nil {
		return err
	}
	return nil
}

func legacyCatalog() (string, string, error) { _, v, d, e := config.JourneyCatalog(); return v, d, e }

func legacyAlertScope(ctx context.Context, conn *sql.Conn, versionID int64, key string) ([]string, map[string]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT s.source_key FROM config_alert_source_refs r JOIN alert_sources s ON s.id=r.alert_source_id WHERE r.config_version_id=? ORDER BY s.source_key`, versionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(refs) == 0 {
		// The predecessor treated an empty source list as unrestricted. Only
		// immutable, already-attributed deliveries can justify a narrower successor.
		history, err := conn.QueryContext(ctx, `SELECT DISTINCT s.source_key FROM alert_occurrences o JOIN alert_sources s ON s.id=o.source_id JOIN business_system_config_versions v ON v.business_system_id=o.business_system_id WHERE v.id=? ORDER BY s.source_key`, versionID)
		if err != nil {
			return nil, nil, err
		}
		for history.Next() {
			var ref string
			if err := history.Scan(&ref); err != nil {
				history.Close()
				return nil, nil, err
			}
			refs = append(refs, ref)
		}
		err = history.Err()
		history.Close()
		if err != nil {
			return nil, nil, err
		}
		if len(refs) == 0 {
			return nil, nil, fmt.Errorf("%w: current config version %d has no explicit alert source mapping or attributed source history", ErrLegacyMigrationBlocked, versionID)
		}
	}
	labels := map[string]string{}
	rows, err = conn.QueryContext(ctx, `SELECT label_name,label_value FROM config_alert_label_conditions WHERE config_version_id=? ORDER BY label_name`, versionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, nil, err
		}
		labels[n] = v
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(labels) != 0 {
		return refs, labels, nil
	}
	var contractJSON string
	err = conn.QueryRowContext(ctx, `SELECT l.contract_json FROM business_system_config_versions v JOIN label_contracts l ON l.id=v.label_contract_version_id WHERE v.id=?`, versionID).Scan(&contractJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: current config version %d has no explicit alert labels or legacy contract: %v", ErrLegacyMigrationBlocked, versionID, err)
	}
	var contract map[string]any
	if err := json.Unmarshal([]byte(contractJSON), &contract); err != nil {
		return nil, nil, err
	}
	// Historical contract_json is config.LabelContractDocument.CanonicalJSON:

	// {"label_contract":{"business_system_label":"…"}}. Accept the earlier
	// flat spelling only for released malformed projections, never infer a
	// wildcard attribution source or label when neither carries the authority.
	label := ""
	if nested, ok := contract["label_contract"].(map[string]any); ok {
		label, _ = nested["business_system_label"].(string)
	}
	if label == "" {
		label, _ = contract["businessSystemLabel"].(string)
	}
	if label == "" {
		label, _ = contract["business_system_label"].(string)
	}
	if label == "" {
		return nil, nil, fmt.Errorf("%w: current config version %d has no explicit alert labels and legacy contract has no business label", ErrLegacyMigrationBlocked, versionID)
	}
	return refs, map[string]string{label: key}, nil
}

func legacyInspections(ctx context.Context, conn *sql.Conn, versionID int64, resource string) ([]config.Inspection, error) {
	rows, err := conn.QueryContext(ctx, `SELECT p.plan_key,p.display_name,p.timezone,p.cron,c.check_key,c.display_name,c.analysis_question,c.query_mode,c.expression,c.range_seconds,c.step_seconds FROM config_plans p JOIN config_checks c ON c.plan_id=p.id WHERE p.config_version_id=? AND c.kind='promql' ORDER BY p.id,c.id`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := map[string]*config.Inspection{}
	var order []string
	for rows.Next() {
		var key, display, tz string
		var cron sql.NullString
		var ck, cd, q, mode, expr string
		var rs, ss sql.NullInt64
		if err := rows.Scan(&key, &display, &tz, &cron, &ck, &cd, &q, &mode, &expr, &rs, &ss); err != nil {
			return nil, err
		}
		p := plans[key]
		if p == nil {
			schedule := ""
			if cron.Valid {
				schedule = cron.String
			}
			p = &config.Inspection{Name: key, DisplayName: display, Schedule: schedule, Timezone: tz}
			plans[key] = p
			order = append(order, key)
		}
		check := config.InspectionCheck{Name: ck, ResourceRef: resource, Expression: expr, Question: q, QueryMode: mode}
		if rs.Valid {
			check.RangeSeconds = rs.Int64
		}
		if ss.Valid {
			check.StepSeconds = ss.Int64
		}
		p.Checks = append(p.Checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]config.Inspection, 0, len(order))
	for _, key := range order {
		out = append(out, *plans[key])
	}
	return out, nil
}

func resolveLegacyAlertIDs(ctx context.Context, conn *sql.Conn, document *config.BusinessSystemDocument) error {
	for _, ref := range document.AlertSourceRefs {
		var id int64
		if err := conn.QueryRowContext(ctx, `SELECT id FROM alert_sources WHERE source_key=?`, ref).Scan(&id); err != nil {
			return err
		}
		document.AlertSourceIDs = append(document.AlertSourceIDs, id)
	}
	return nil
}

func canonicalLegacyYAML(document config.BusinessSystem) ([]byte, error) {
	resources := make([]map[string]any, 0, len(document.Spec.Metrics.Resources))
	for _, resource := range document.Spec.Metrics.Resources {
		resources = append(resources, map[string]any{"name": resource.Name, "displayName": resource.DisplayName, "matchLabels": resource.MatchLabels, "discoveryMetric": resource.DiscoveryMetric, "identityLabels": resource.IdentityLabels, "allowedMetrics": resource.AllowedMetrics})
	}
	inspections := make([]map[string]any, 0, len(document.Spec.Inspections))
	for _, inspection := range document.Spec.Inspections {
		checks := make([]map[string]any, 0, len(inspection.Checks))
		for _, check := range inspection.Checks {
			value := map[string]any{"name": check.Name, "resourceRef": check.ResourceRef, "expression": check.Expression}
			if check.Question != "" {
				value["question"] = check.Question
			}
			if check.QueryMode != "" {
				value["queryMode"] = check.QueryMode
			}
			if check.RangeSeconds > 0 {
				value["rangeSeconds"] = check.RangeSeconds
				value["stepSeconds"] = check.StepSeconds
			}
			checks = append(checks, value)
		}
		inspections = append(inspections, map[string]any{"name": inspection.Name, "displayName": inspection.DisplayName, "schedule": inspection.Schedule, "timezone": inspection.Timezone, "checks": checks})
	}
	// time.Duration.String emits values such as "5m0s", while the public schema
	// deliberately accepts exactly one duration unit. Canonicalize at seconds to
	// preserve every valid whole-second refresh policy without bypassing schema.
	refresh := strconv.FormatInt(int64(document.DiscoveryRefresh()/time.Second), 10) + "s"
	return yaml.Marshal(map[string]any{"apiVersion": "quoin/v1", "kind": "BusinessSystem", "metadata": map[string]any{"name": document.Metadata.Name, "displayName": document.Metadata.DisplayName, "description": document.Metadata.Description}, "spec": map[string]any{"enabled": document.Spec.Enabled, "metrics": map[string]any{"connectionRef": document.Spec.Metrics.ConnectionRef, "matchLabels": document.Spec.Metrics.MatchLabels, "resources": resources}, "alerts": map[string]any{"sourceRefs": document.Spec.Alerts.SourceRefs, "matchLabels": document.Spec.Alerts.MatchLabels}, "inspections": inspections, "discovery": map[string]any{"refresh": refresh}}})
}

func insertLegacySuccessorProjections(ctx context.Context, conn *sql.Conn, versionID int64, document config.BusinessSystemDocument) error {
	for _, r := range document.Resources {
		selectors, _ := json.Marshal(r.MatchLabels)
		identity, _ := json.Marshal(r.IdentityLabels)
		allowed, _ := json.Marshal(r.AllowedMetrics)
		if _, err := conn.ExecContext(ctx, `INSERT INTO config_resource_scopes(config_version_id,resource_key,display_name,discovery_metric,selectors_json,identity_labels_json,allowed_metrics_json) VALUES(?,?,?,?,?,?,?)`, versionID, r.Name, r.DisplayName, r.DiscoveryMetric, string(selectors), string(identity), string(allowed)); err != nil {
			return err
		}
	}
	for _, id := range document.AlertSourceIDs {
		if _, err := conn.ExecContext(ctx, `INSERT INTO config_alert_source_refs(config_version_id,alert_source_id) VALUES(?,?)`, versionID, id); err != nil {
			return err
		}
	}
	for n, v := range document.AlertSourceLabels {
		if _, err := conn.ExecContext(ctx, `INSERT INTO config_alert_label_conditions(config_version_id,label_name,label_value) VALUES(?,?,?)`, versionID, n, v); err != nil {
			return err
		}
	}
	for _, d := range document.Discoveries {
		ids, _ := json.Marshal(d.IdentityLabels)
		if _, err := conn.ExecContext(ctx, `INSERT INTO config_discoveries(config_version_id,discovery_key,display_name,selector,identity_labels_json) VALUES(?,?,?,?,?)`, versionID, d.Key, d.DisplayName, d.Selector, string(ids)); err != nil {
			return err
		}
	}
	for _, p := range document.Plans {
		r, err := conn.ExecContext(ctx, `INSERT INTO config_plans(config_version_id,plan_key,display_name,timezone,cron) VALUES(?,?,?,?,?)`, versionID, p.Key, p.DisplayName, p.Timezone, p.Cron)
		if err != nil {
			return err
		}
		pid, _ := r.LastInsertId()
		for _, c := range p.Checks {
			var rs, ss any
			if c.QueryMode == "range" {
				rs, ss = c.RangeSeconds, c.StepSeconds
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO config_checks(plan_id,check_key,display_name,analysis_question,kind,query_mode,expression,range_seconds,step_seconds) VALUES(?,?,?,?,?,?,?,?,?)`, pid, c.Key, c.DisplayName, c.AnalysisQuestion, "promql", c.QueryMode, c.Expression, rs, ss); err != nil {
				return err
			}
		}
	}
	return nil
}

func legacyMetrics(ctx context.Context, conn *sql.Conn, vid int64, selector string) ([]string, error) {
	seen := map[string]bool{}
	add := func(x string) error {
		e, err := parser.NewParser(parser.Options{}).ParseExpr(x)
		if err != nil {
			return err
		}
		parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
			if v, ok := n.(*parser.VectorSelector); ok && v.Name != "" {
				seen[v.Name] = true
			}
			return nil
		})
		return nil
	}
	if err := add(selector); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT c.expression FROM config_checks c JOIN config_plans p ON p.id=c.plan_id WHERE p.config_version_id=? AND c.kind='promql'`, vid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var expression string
		if err := rows.Scan(&expression); err != nil {
			return nil, err
		}
		if err := add(expression); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for metric := range seen {
		out = append(out, metric)
	}
	sort.Strings(out)
	return out, nil
}

func legacySelectorScope(expression string) (string, map[string]string, error) {
	e, err := parser.NewParser(parser.Options{}).ParseExpr(expression)
	if err != nil {
		return "", nil, err
	}
	vector, ok := e.(*parser.VectorSelector)
	if !ok || vector.Name == "" {
		return "", nil, errors.New("not a named vector selector")
	}
	scopeLabels := map[string]string{}
	for _, matcher := range vector.LabelMatchers {
		if matcher.Type != labels.MatchEqual {
			return "", nil, fmt.Errorf("non-exact matcher %s", matcher.Name)
		}
		// Prometheus represents the metric name as a synthetic __name__ matcher.
		// It identifies the discovery metric, not a mandatory cross-metric scope;
		// retaining it would make mysql_up/redis_up impossible to compile.
		if matcher.Name != labels.MetricName {
			scopeLabels[matcher.Name] = matcher.Value
		}
	}
	return vector.Name, scopeLabels, nil
}

func preserveSQLiteSequence(ctx context.Context, conn *sql.Conn) error {
	oldCols, err := tableColumns(ctx, conn, "temp", "legacy_sqlite_sequence")
	if err != nil || len(oldCols) == 0 {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE main.sqlite_sequence
		SET seq=MAX(seq,COALESCE((SELECT old.seq FROM temp.legacy_sqlite_sequence old WHERE old.name=main.sqlite_sequence.name),0))`); err != nil {
		return fmt.Errorf("preserve existing autoincrement high-water marks: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO main.sqlite_sequence(name,seq)
		SELECT old.name,old.seq FROM temp.legacy_sqlite_sequence old
		WHERE NOT EXISTS (SELECT 1 FROM main.sqlite_sequence current WHERE current.name=old.name)`); err != nil {
		return fmt.Errorf("preserve missing autoincrement high-water marks: %w", err)
	}
	return nil
}

func captureAndDropTriggers(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name,sql FROM main.sqlite_master WHERE type='trigger' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type trigger struct{ name, sql string }
	var triggers []trigger
	for rows.Next() {
		var t trigger
		if err := rows.Scan(&t.name, &t.sql); err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range triggers {
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER main.`+qid(t.name)); err != nil {
			return nil, err
		}
	}
	out := make([]string, len(triggers))
	for i, t := range triggers {
		out[i] = t.sql
	}
	return out, nil
}

type schemaListing struct{ tables, views []string }

func schemaObjects(ctx context.Context, conn *sql.Conn, schema string) (schemaListing, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name,type FROM `+qid(schema)+`.sqlite_master WHERE name NOT LIKE 'sqlite_%' AND type IN ('table','view') ORDER BY name`)
	if err != nil {
		return schemaListing{}, err
	}
	defer rows.Close()
	var out schemaListing
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return out, err
		}
		if k == "table" {
			out.tables = append(out.tables, n)
		} else {
			out.views = append(out.views, n)
		}
	}
	sort.Strings(out.tables)
	sort.Strings(out.views)
	return out, rows.Err()
}

func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM `+qid(schema)+`.pragma_table_info(`+quote(table)+`) ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func qid(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quote(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

func verifyForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return err
		}
		return fmt.Errorf("foreign key check failed: table=%s row=%v parent=%s fk=%d", table, rowID, parent, fkID)
	}
	return rows.Err()
}

func retireLegacyRefreshAttempts(ctx context.Context, conn *sql.Conn, report *LegacyMigrationReport) error {
	rows, err := conn.QueryContext(ctx, `SELECT id,state FROM execution_attempts WHERE scope_type='resource_refresh_run' AND state IN ('Queued','Assigned','Running','Cancelling') ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			return err
		}
		report.RetiredRefreshAttempts = append(report.RetiredRefreshAttempts, id)
		if state == "Queued" {
			if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelled',ended_at=?,termination_reason='cancelled',row_version=row_version+1 WHERE id=?`, migrationNow(), id); err != nil {
				return err
			}
		} else if state != "Cancelling" {
			if _, err := conn.ExecContext(ctx, `UPDATE execution_attempts SET state='Cancelling',row_version=row_version+1 WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
func legacyMigrationDigest() string { return migrationDigest(legacyMetricsBusinessMigrationID) }

func migrationDigest(migrationID string) string {
	d := sha256.Sum256([]byte(migrationID))
	return hex.EncodeToString(d[:])
}
func migrationNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
