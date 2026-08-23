package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

const resourceRefreshSchemaKind = "resource_refresh_execution_v1"

type ResourceRefreshRunDetail struct {
	ID                     string  `json:"id"`
	BusinessSystemID       string  `json:"businessSystemId"`
	ConfigVersionID        string  `json:"configVersionId"`
	LabelContractVersionID string  `json:"labelContractVersionId"`
	TriggerKind            string  `json:"triggerKind"`
	State                  string  `json:"state"`
	RowVersion             int64   `json:"rowVersion"`
	EvidenceAt             *string `json:"evidenceAt,omitempty"`
	ResultDetail           *string `json:"resultDetail,omitempty"`
	CreatedAt              string  `json:"createdAt"`
}

type resourceDiscoveryInput struct {
	SchemaKind           string   `json:"schemaKind"`
	AttemptID            int64    `json:"attemptId"`
	ResourceRefreshRunID int64    `json:"resourceRefreshRunId"`
	ConfigVersionID      int64    `json:"configVersionId"`
	DiscoveryKey         string   `json:"discoveryKey"`
	Selector             string   `json:"selector"`
	IdentityLabels       []string `json:"identityLabels"`
	GrantID              int64    `json:"grantId"`
}

// StartResourceRefresh atomically freezes the current published configuration,
// its Label Contract, every discovery, and one Thanos grant per discovery.
func (service *Service) StartResourceRefresh(ctx context.Context, principalID int64, clientCommandID, systemKey string) (ResourceRefreshRunDetail, error) {
	commandDigest := auth.DigestCommand("resource_refresh.start", map[string]any{"systemKey": systemKey})
	if detail, replayed, err := service.replayResourceRefresh(ctx, principalID, clientCommandID, commandDigest); replayed || err != nil {
		return detail, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if detail, replayed, err := service.replayResourceRefreshOn(ctx, conn, principalID, clientCommandID, commandDigest); replayed || err != nil {
		return detail, err
	}
	var systemID, versionID, contractID int64
	if err := conn.QueryRowContext(ctx, `SELECT b.id,b.current_config_version_id,v.label_contract_version_id FROM business_systems b JOIN business_system_config_versions v ON v.id=b.current_config_version_id WHERE b.key=?`, systemKey).Scan(&systemID, &versionID, &contractID); err != nil {
		if err == sql.ErrNoRows {
			return ResourceRefreshRunDetail{}, ErrNotFound
		}
		return ResourceRefreshRunDetail{}, err
	}
	// An active Run is the system's single refresh owner; a new command is a
	// user-visible conflict, not an internal uniqueness failure.
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM resource_refresh_runs WHERE business_system_id=? AND state IN ('Queued','Running')`, systemID).Scan(&active); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if active > 0 {
		return ResourceRefreshRunDetail{}, &ConflictError{Code: "active_conflict", Detail: "该系统已有正在进行的资源刷新，请等待完成或稍后重试。", SystemKey: systemKey, ObjectID: systemID}
	}
	now := service.nowText()
	insert, err := conn.ExecContext(ctx, `INSERT INTO resource_refresh_runs(business_system_id,config_version_id,label_contract_version_id,trigger_kind,state,row_version,created_by,created_at) VALUES(?,?,?,'manual','Queued',1,?,?)`, systemID, versionID, contractID, principalID, now)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	runID, _ := insert.LastInsertId()
	if _, err := conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Running',evidence_at=?,row_version=2 WHERE id=?`, now, runID); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT discovery_key,selector,identity_labels_json FROM config_discoveries WHERE config_version_id=? ORDER BY discovery_key`, versionID)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	for rows.Next() {
		var key, selector, labelsJSON string
		if err := rows.Scan(&key, &selector, &labelsJSON); err != nil {
			rows.Close()
			return ResourceRefreshRunDetail{}, err
		}
		var labels []string
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			rows.Close()
			return ResourceRefreshRunDetail{}, err
		}
		if err := createResourceDiscoveryAttempt(ctx, conn, runID, versionID, contractID, key, selector, labels, now); err != nil {
			rows.Close()
			return ResourceRefreshRunDetail{}, err
		}
	}
	if err := rows.Close(); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='resource_refresh_run' AND scope_id=?`, runID).Scan(&count); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if count == 0 {
		if _, err := conn.ExecContext(ctx, `UPDATE resource_refresh_runs SET state='Completed',row_version=3 WHERE id=?`, runID); err != nil {
			return ResourceRefreshRunDetail{}, err
		}
	}
	detail, err := resourceRefreshDetailOn(ctx, conn, systemID, runID)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if err := recordCommandResult(ctx, conn, principalID, clientCommandID, "resource_refresh.start", commandDigest, "resource_refresh_run", runID, detail); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	committed = true
	return detail, nil
}

func createResourceDiscoveryAttempt(ctx context.Context, conn *sql.Conn, runID, versionID, contractID int64, key, selector string, labels []string, now string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,discovery_key,state,quoin_release_version,created_at) VALUES('inspection_collection','resource_refresh_run',?,?, 'Queued',?,?)`, runID, key, attempt.ReleaseVersion(), now)
	if err != nil {
		return err
	}
	attemptID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	grant, err := thanos.ResolveConfigGrant(ctx, conn, attemptID)
	if err != nil {
		return err
	}
	canonical, err := json.Marshal(resourceDiscoveryInput{SchemaKind: resourceRefreshSchemaKind, AttemptID: attemptID, ResourceRefreshRunID: runID, ConfigVersionID: versionID, DiscoveryKey: key, Selector: selector, IdentityLabels: labels, GrantID: grant.GrantID})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	snapshot, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at) VALUES(?,?,?,?,?)`, attemptID, resourceRefreshSchemaKind, "v1", hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return err
	}
	configDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", versionID)))
	if _, err := conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id) VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(configDigest[:]), versionID); err != nil {
		return err
	}
	contractDigest := sha256.Sum256([]byte(fmt.Sprintf("label-contract-version:%d", contractID)))
	_, err = conn.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id) VALUES(?,2,'label_contract',?,?)`, snapshotID, hex.EncodeToString(contractDigest[:]), contractID)
	return err
}

func resourceRefreshDetailOn(ctx context.Context, conn *sql.Conn, systemID, runID int64) (ResourceRefreshRunDetail, error) {
	var detail ResourceRefreshRunDetail
	var id, bs, version, contract int64
	var evidence, result sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT id,business_system_id,config_version_id,label_contract_version_id,trigger_kind,state,row_version,evidence_at,result_detail,created_at FROM resource_refresh_runs WHERE id=? AND business_system_id=?`, runID, systemID).Scan(&id, &bs, &version, &contract, &detail.TriggerKind, &detail.State, &detail.RowVersion, &evidence, &result, &detail.CreatedAt)
	if err != nil {
		return detail, err
	}
	detail.ID, detail.BusinessSystemID, detail.ConfigVersionID, detail.LabelContractVersionID = fmt.Sprint(id), fmt.Sprint(bs), fmt.Sprint(version), fmt.Sprint(contract)
	if evidence.Valid {
		detail.EvidenceAt = &evidence.String
	}
	if result.Valid {
		detail.ResultDetail = &result.String
	}
	return detail, nil
}

func (service *Service) GetResourceRefresh(ctx context.Context, systemKey string, runID int64) (ResourceRefreshRunDetail, error) {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return ResourceRefreshRunDetail{}, err
	}
	defer conn.Close()
	var systemID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM business_systems WHERE key=?`, systemKey).Scan(&systemID); err != nil {
		return ResourceRefreshRunDetail{}, ErrNotFound
	}
	return resourceRefreshDetailOn(ctx, conn, systemID, runID)
}

func (service *Service) QueuedResourceRefreshAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT a.id FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id WHERE a.scope_type='resource_refresh_run' AND a.attempt_type='inspection_collection' AND a.state='Queued' AND r.state='Running' ORDER BY a.id`)
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

var _ = time.RFC3339Nano

// ResourceRefreshAttempts exposes the same generic Attempt state machine with
// a re-builder that reconstructs only the frozen published discovery input.
func (service *Service) ResourceRefreshAttempts() *attempt.Service {
	attempts := attempt.NewService(service.db)
	attempts.SnapshotRebuilder = service.rebuildResourceRefreshAttemptInput
	return attempts
}

func (service *Service) rebuildResourceRefreshAttemptInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID, versionID int64
	var grant sql.NullInt64
	var key, selector, labelsJSON string
	err := service.db.QueryRowContext(ctx, `SELECT a.scope_id,r.config_version_id,a.discovery_key,d.selector,d.identity_labels_json,(SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query') FROM execution_attempts a JOIN resource_refresh_runs r ON r.id=a.scope_id JOIN config_discoveries d ON d.config_version_id=r.config_version_id AND d.discovery_key=a.discovery_key WHERE a.id=? AND a.scope_type='resource_refresh_run'`, attemptID).Scan(&runID, &versionID, &key, &selector, &labelsJSON, &grant)
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
	canonical, err := json.Marshal(resourceDiscoveryInput{SchemaKind: resourceRefreshSchemaKind, AttemptID: attemptID, ResourceRefreshRunID: runID, ConfigVersionID: versionID, DiscoveryKey: key, Selector: selector, IdentityLabels: labels, GrantID: grant.Int64})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	var expected string
	if err := service.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	if expected != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("resource refresh input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}

func (service *Service) replayResourceRefresh(ctx context.Context, principalID int64, commandID, digest string) (ResourceRefreshRunDetail, bool, error) {
	record, found, err := auth.LookupCommand(ctx, service.db, principalID, commandID)
	if err != nil {
		return ResourceRefreshRunDetail{}, false, err
	}
	return replayResourceRefreshRecord(record, found, digest)
}

func (service *Service) replayResourceRefreshOn(ctx context.Context, conn *sql.Conn, principalID int64, commandID, digest string) (ResourceRefreshRunDetail, bool, error) {
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, commandID)
	if err != nil {
		return ResourceRefreshRunDetail{}, false, err
	}
	return replayResourceRefreshRecord(record, found, digest)
}

func replayResourceRefreshRecord(record auth.CommandRecord, found bool, digest string) (ResourceRefreshRunDetail, bool, error) {
	if !found {
		return ResourceRefreshRunDetail{}, false, nil
	}
	if record.RequestDigest != digest {
		return ResourceRefreshRunDetail{}, true, ErrCommandReused
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		return ResourceRefreshRunDetail{}, true, errors.New("rejected resource refresh command")
	}
	var detail ResourceRefreshRunDetail
	if err := decodeStored(record.ResultPayload, &detail); err != nil {
		return ResourceRefreshRunDetail{}, true, errors.New("committed resource refresh command has an unreadable result")
	}
	if detail.ID == "" {
		return ResourceRefreshRunDetail{}, true, errors.New("committed resource refresh command has no result")
	}
	return detail, true, nil
}
