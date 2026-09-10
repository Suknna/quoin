package businesssystem

// Draft discovery verification executes every declared selector through the
// production PromQL adapter, but records its returned identities only beneath
// the Config Verification Run. It never writes observed_resources.

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

const configVerificationDiscoverySchemaKind = "config_verification_discovery_execution_v1"

type configVerificationDiscoveryInput struct {
	SchemaKind        string   `json:"schemaKind"`
	AttemptID         int64    `json:"attemptId"`
	VerificationRunID int64    `json:"verificationRunId"`
	DiscoveryKey      string   `json:"discoveryKey"`
	Selector          string   `json:"selector"`
	IdentityLabels    []string `json:"identityLabels"`
	GrantID           int64    `json:"grantId"`
}

func createDiscoveryVerificationAttempts(ctx context.Context, conn *sql.Conn, runID, configVersionID, contractID int64, now string) error {
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
		var key, selector, labelsJSON string
		if err := rows.Scan(&key, &selector, &labelsJSON); err != nil {
			return err
		}
		var labels []string
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return err
		}
		insert, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES('inspection_collection','config_verification_run',? ,?,'Queued',?,?)`, runID, key, attempt.ReleaseVersion(), now)
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
		canonical, err := json.Marshal(configVerificationDiscoveryInput{SchemaKind: configVerificationDiscoverySchemaKind, AttemptID: attemptID, VerificationRunID: runID, DiscoveryKey: key, Selector: selector, IdentityLabels: labels, GrantID: grant.GrantID})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(canonical)
		snapshot, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,?,?,?)`, attemptID, configVerificationDiscoverySchemaKind, "v1", hex.EncodeToString(digest[:]), now)
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
		contractDigest := sha256.Sum256([]byte(fmt.Sprintf("label-contract-version:%d", contractID)))
		if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id) VALUES(?,2,'label_contract',?,?)`, snapshotID, hex.EncodeToString(contractDigest[:]), contractID); err != nil {
			return err
		}
	}
	return rows.Err()
}
