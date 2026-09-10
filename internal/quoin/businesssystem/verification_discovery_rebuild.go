package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// rebuildVerificationDiscoveryAttemptInput replays the exact draft-only
// discovery snapshot. It never consults current config or any global metric
// connection, so later publication cannot redirect an in-flight verification.
func (service *Service) rebuildVerificationDiscoveryAttemptInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID, versionID int64
	var key, selector, labelsJSON string
	var grant sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT a.scope_id,r.config_version_id,a.discovery_key,d.selector,d.identity_labels_json,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query')
		FROM execution_attempts a JOIN config_verification_runs r ON r.id=a.scope_id
		JOIN config_discoveries d ON d.config_version_id=r.config_version_id AND d.discovery_key=a.discovery_key
		WHERE a.id=? AND a.scope_type='config_verification_run' AND a.discovery_key IS NOT NULL`, attemptID).
		Scan(&runID, &versionID, &key, &selector, &labelsJSON, &grant)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen config_thanos_query grant", attemptID)
	}
	var labels []string
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(configVerificationDiscoveryInput{SchemaKind: configVerificationDiscoverySchemaKind, AttemptID: attemptID, VerificationRunID: runID, DiscoveryKey: key, Selector: selector, IdentityLabels: labels, GrantID: grant.Int64})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	var expected string
	if err = service.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	if expected != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("verification discovery input digest no longer matches frozen snapshot")
	}
	_ = versionID
	return canonical, nil
}
