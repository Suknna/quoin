package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

const (
	resourceDiscoveryExecutionSchemaKind = "resource_discovery_execution_v1"
	resourceDiscoveryRendererVersion     = "v1"
)

type resourceDiscoveryExecutionInput struct {
	SchemaKind           string   `json:"schemaKind"`
	AttemptID            int64    `json:"attemptId"`
	ResourceRefreshRunID int64    `json:"resourceRefreshRunId"`
	DiscoveryKey         string   `json:"discoveryKey"`
	Selector             string   `json:"selector"`
	IdentityLabels       []string `json:"identityLabels"`
	GrantID              int64    `json:"grantId"`
}

// createResourceRefreshAttempts freezes one supervisor-only discovery child per
// declared selector. It runs in StartResourceRefresh's writer transaction: a
// durable Running root can therefore never exist without all of its Queued
// children, immutable snapshots, and attempt-scoped metrics grants.
func createResourceRefreshAttempts(ctx context.Context, conn *sql.Conn, runID, configVersionID int64, now string) error {
	var metricsConnectionID int64
	if err := conn.QueryRowContext(ctx, `SELECT metrics_connection_id FROM business_system_config_versions WHERE id=?`, configVersionID).Scan(&metricsConnectionID); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `SELECT discovery_key,selector,identity_labels_json FROM config_discoveries WHERE config_version_id=? ORDER BY discovery_key`, configVersionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var input resourceDiscoveryExecutionInput
		var labelsJSON string
		if err := rows.Scan(&input.DiscoveryKey, &input.Selector, &labelsJSON); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(labelsJSON), &input.IdentityLabels); err != nil {
			return err
		}
		insert, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES('inspection_collection','resource_refresh_run',? ,?,'Queued',?,?)`, runID, input.DiscoveryKey, attempt.ReleaseVersion(), now)
		if err != nil {
			return err
		}
		attemptID, err := insert.LastInsertId()
		if err != nil {
			return err
		}
		grant, err := thanos.ResolveConfigGrantForConnection(ctx, conn, attemptID, metricsConnectionID)
		if err != nil {
			return err
		}
		input.SchemaKind, input.AttemptID, input.ResourceRefreshRunID, input.GrantID = resourceDiscoveryExecutionSchemaKind, attemptID, runID, grant.GrantID
		canonical, err := json.Marshal(input)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(canonical)
		snapshot, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,?,?,?)`, attemptID, resourceDiscoveryExecutionSchemaKind, resourceDiscoveryRendererVersion, hex.EncodeToString(digest[:]), now)
		if err != nil {
			return err
		}
		snapshotID, err := snapshot.LastInsertId()
		if err != nil {
			return err
		}
		configDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", configVersionID)))
		if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id) VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(configDigest[:]), configVersionID); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ResourceRefreshAttempts configures generic attempt transitions with a
// deterministic rebuild of resource discovery inputs. It is separate from
// verification because refresh executions update observed-resource history.
func (service *Service) ResourceRefreshAttempts() *attempt.Service {
	attempts := attempt.NewService(service.db)
	attempts.SnapshotRebuilder = service.rebuildResourceRefreshAttempt
	return attempts
}

func (service *Service) QueuedResourceRefreshAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT a.id FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id WHERE a.attempt_type='inspection_collection' AND a.scope_type='resource_refresh_run' AND a.state='Queued' AND r.state='Running' ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (service *Service) rebuildResourceRefreshAttempt(ctx context.Context, attemptID int64) ([]byte, error) {
	var input resourceDiscoveryExecutionInput
	var labelsJSON string
	var grant sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT a.scope_id,a.discovery_key,d.selector,d.identity_labels_json,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query')
		FROM execution_attempts a
		JOIN resource_refresh_runs r ON r.id=a.scope_id
		JOIN config_discoveries d ON d.config_version_id=r.config_version_id AND d.discovery_key=a.discovery_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='resource_refresh_run'`, attemptID).Scan(&input.ResourceRefreshRunID, &input.DiscoveryKey, &input.Selector, &labelsJSON, &grant)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen config_thanos_query grant", attemptID)
	}
	if err := json.Unmarshal([]byte(labelsJSON), &input.IdentityLabels); err != nil {
		return nil, err
	}
	input.SchemaKind, input.AttemptID, input.GrantID = resourceDiscoveryExecutionSchemaKind, attemptID, grant.Int64
	canonical, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	var expected string
	if err := service.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	if expected != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("resource discovery input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}
