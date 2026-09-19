package observation

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/audit"
)

// runDetailOn projects one observation run with its per-object rows. It backs
// the StartRun admission path, which returns the already-active run instead of
// forking a second one.
func (service *Service) runDetailOn(ctx context.Context, conn audit.Reader, connectionName string, runID int64) (SourceObservationRun, error) {
	var detail SourceObservationRun
	detail.ConnectionName = connectionName
	var evidenceAt, resultDetail sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT trigger_kind,state,row_version,evidence_at,result_detail,created_at FROM observation_runs WHERE id=?`, runID).Scan(
		&detail.TriggerKind, &detail.State, &detail.RowVersion, &evidenceAt, &resultDetail, &detail.CreatedAt)
	if err != nil {
		return SourceObservationRun{}, err
	}
	detail.ID = fmt.Sprint(runID)
	if evidenceAt.Valid {
		detail.EvidenceAt = &evidenceAt.String
	}
	if resultDetail.Valid {
		detail.ResultDetail = &resultDetail.String
	}
	rows, err := conn.QueryContext(ctx, `SELECT object_type,status,gap_reason,attempt_id,evidence_id FROM observation_run_objects WHERE observation_run_id=? ORDER BY object_type`, runID)
	if err != nil {
		return SourceObservationRun{}, err
	}
	defer rows.Close()
	objects := []SourceObservationRunObject{}
	for rows.Next() {
		var object SourceObservationRunObject
		var objectType, status string
		var gapReason sql.NullString
		var attemptID, evidenceID sql.NullInt64
		if err := rows.Scan(&objectType, &status, &gapReason, &attemptID, &evidenceID); err != nil {
			return SourceObservationRun{}, err
		}
		object.ObjectType, object.Status = objectType, status
		if gapReason.Valid {
			object.GapReason = &gapReason.String
		}
		if attemptID.Valid {
			id := fmt.Sprint(attemptID.Int64)
			object.AttemptID = &id
		}
		if evidenceID.Valid {
			id := fmt.Sprint(evidenceID.Int64)
			object.EvidenceID = &id
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return SourceObservationRun{}, err
	}
	detail.Objects = objects
	return detail, nil
}
