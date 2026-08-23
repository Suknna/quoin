package businesssystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// StartDueResourceRefreshes is the scheduler's only write path. It derives
// work from each enabled system's currently published configuration; unique
// active/scheduled indexes remain the final concurrent-writer fence.
func (service *Service) StartDueResourceRefreshes(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT key FROM business_systems WHERE enabled=1 AND current_config_version_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(keys))
	for _, key := range keys {
		id, created, err := service.startScheduledResourceRefresh(ctx, key)
		if err != nil {
			return ids, err
		}
		if created {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (service *Service) startScheduledResourceRefresh(ctx context.Context, systemKey string) (int64, bool, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var systemID, versionID, contractID, interval int64
	var last sql.NullString
	if err = conn.QueryRowContext(ctx, `SELECT b.id,b.current_config_version_id,v.label_contract_version_id,b.resource_refresh_interval_seconds,(SELECT MAX(evidence_at) FROM resource_refresh_runs r WHERE r.business_system_id=b.id AND r.config_version_id=b.current_config_version_id) FROM business_systems b JOIN business_system_config_versions v ON v.id=b.current_config_version_id WHERE b.key=? AND b.enabled=1`, systemKey).Scan(&systemID, &versionID, &contractID, &interval, &last); err != nil {
		if err == sql.ErrNoRows {
			_, _ = conn.ExecContext(ctx, `COMMIT`)
			committed = true
			return 0, false, nil
		}
		return 0, false, err
	}
	nowTime := service.now().UTC()
	if last.Valid {
		previous, parseErr := time.Parse(time.RFC3339Nano, last.String)
		if parseErr == nil && nowTime.Before(previous.Add(time.Duration(interval)*time.Second)) {
			_, _ = conn.ExecContext(ctx, `COMMIT`)
			committed = true
			return 0, false, nil
		}
	}
	var active int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM resource_refresh_runs WHERE business_system_id=? AND state IN ('Queued','Running')`, systemID).Scan(&active); err != nil {
		return 0, false, err
	}
	if active > 0 {
		_, _ = conn.ExecContext(ctx, `COMMIT`)
		committed = true
		return 0, false, nil
	}
	now := nowTime.Format(time.RFC3339Nano)
	result, err := conn.ExecContext(ctx, `INSERT INTO resource_refresh_runs(business_system_id,config_version_id,label_contract_version_id,trigger_kind,scheduled_for,state,row_version,created_at) VALUES(?,?,?,'schedule',?,'Queued',1,?)`, systemID, versionID, contractID, now, now)
	if err != nil {
		return 0, false, err
	}
	runID, _ := result.LastInsertId()
	if _, err = conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=?`, now, runID); err != nil {
		return 0, false, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT discovery_key,selector,identity_labels_json FROM config_discoveries WHERE config_version_id=? ORDER BY discovery_key`, versionID)
	if err != nil {
		return 0, false, err
	}
	for rows.Next() {
		var key, selector, labelsJSON string
		if err = rows.Scan(&key, &selector, &labelsJSON); err != nil {
			rows.Close()
			return 0, false, err
		}
		var labels []string
		if err = json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			rows.Close()
			return 0, false, err
		}
		if err = createResourceDiscoveryAttempt(ctx, conn, runID, versionID, contractID, key, selector, labels, now); err != nil {
			rows.Close()
			return 0, false, err
		}
	}
	if err = rows.Close(); err != nil {
		return 0, false, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Completed',row_version=3 WHERE id=? AND NOT EXISTS (SELECT 1 FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?)`, runID, runID); err != nil {
		return 0, false, err
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, false, err
	}
	committed = true
	return runID, true, nil
}
