package upgrade

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
)

const (
	legacyMetricsBusinessMigrationID  = "20260910_metrics_business_v1"
	legacyMetricsBusinessSchemaDigest = "ccf511559c56f885e585be2b61fa1a42c6dda48ec394632d1325448761bc55dc"
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

// MigrateLegacyMetricsBusiness replaces the whole released v1 physical schema
// with the canonical schema in one SQLite transaction. TEMP copies preserve all
// historic rows and IDs; only config versions gain the uniquely provable metrics
// reference. It never parses or changes yaml_body and never rewrites old grants.
func MigrateLegacyMetricsBusiness(ctx context.Context, db *sql.DB) (LegacyMigrationReport, error) {
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
	report, err := migrateLegacyMetricsBusinessOn(ctx, conn)
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	if err := verifyForeignKeys(ctx, conn); err != nil {
		return LegacyMigrationReport{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return LegacyMigrationReport{}, err
	}
	committed = true
	return report, nil
}

func migrateLegacyMetricsBusinessOn(ctx context.Context, conn *sql.Conn) (LegacyMigrationReport, error) {
	var stored string
	if err := conn.QueryRowContext(ctx, `SELECT schema_digest FROM schema_state WHERE id=1`).Scan(&stored); err != nil {
		return LegacyMigrationReport{}, err
	}
	if stored != legacyMetricsBusinessSchemaDigest {
		return LegacyMigrationReport{}, fmt.Errorf("%w: unsupported legacy schema digest %q", ErrLegacyMigrationBlocked, stored)
	}
	var existing int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger WHERE migration_id=?`, legacyMetricsBusinessMigrationID).Scan(&existing); err != nil {
		return LegacyMigrationReport{}, err
	}
	if existing != 0 {
		return LegacyMigrationReport{}, ErrLegacyMigrationRequired
	}
	report := LegacyMigrationReport{MigrationID: legacyMetricsBusinessMigrationID, LegacySchemaDigest: stored}
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
	if err := rebuildCanonicalSchema(ctx, conn, report.MappedMetricsConnection); err != nil {
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

func rebuildCanonicalSchema(ctx context.Context, conn *sql.Conn, metrics *int64) error {
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

// preserveSQLiteSequence prevents AUTOINCREMENT IDs from being reused after a
// deleted historic maximum. The table data may not reveal that high-water mark.
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
func legacyMigrationDigest() string {
	d := sha256.Sum256([]byte(legacyMetricsBusinessMigrationID))
	return hex.EncodeToString(d[:])
}
func migrationNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }
