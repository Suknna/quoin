package businesssystem

import (
	"context"
	"database/sql"
	"fmt"
)

// ResourceRefreshRunDetail exposes immutable historical refresh-run facts. New
// runs are no longer produced; this type remains for retained history reads.
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

// GetResourceRefresh reads a historical refresh run without recreating its
// producer or scheduling semantics.
func (service *Service) GetResourceRefresh(ctx context.Context, systemKey string, runID int64) (ResourceRefreshRunDetail, error) {
	var detail ResourceRefreshRunDetail
	var id, systemID, versionID int64
	var contractID sql.NullInt64
	var evidenceAt, resultDetail sql.NullString
	err := service.db.QueryRowContext(ctx, `
		SELECT r.id,r.business_system_id,r.config_version_id,r.label_contract_version_id,
			r.trigger_kind,r.state,r.row_version,r.evidence_at,r.result_detail,r.created_at
		FROM resource_refresh_runs r
		JOIN business_systems b ON b.id=r.business_system_id
		WHERE b.key=? AND r.id=?`, systemKey, runID).Scan(
		&id, &systemID, &versionID, &contractID, &detail.TriggerKind, &detail.State,
		&detail.RowVersion, &evidenceAt, &resultDetail, &detail.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return ResourceRefreshRunDetail{}, ErrNotFound
		}
		return ResourceRefreshRunDetail{}, err
	}
	detail.ID = fmt.Sprint(id)
	detail.BusinessSystemID = fmt.Sprint(systemID)
	detail.ConfigVersionID = fmt.Sprint(versionID)
	if contractID.Valid {
		detail.LabelContractVersionID = fmt.Sprint(contractID.Int64)
	}
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	if resultDetail.Valid {
		detail.ResultDetail = &resultDetail.String
	}
	return detail, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
