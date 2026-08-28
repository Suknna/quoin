package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// VerificationAttempts exposes the generic Attempt authority configured to
// reconstruct Config Verification's frozen inputs (PromQL or Journey) from
// their immutable projections. It never reads a connection secret.
func (service *Service) VerificationAttempts() *attempt.Service {
	attempts := attempt.NewService(service.db)
	attempts.SnapshotRebuilder = service.rebuildVerificationAttempt
	return attempts
}

// rebuildVerificationAttempt dispatches to the check-kind rebuilder: PromQL
// children rebuild config_verification_execution_v1, browser children
// rebuild the frozen inspection_collection_v1 journey input.
func (service *Service) rebuildVerificationAttempt(ctx context.Context, attemptID int64) ([]byte, error) {
	var kind string
	if err := service.db.QueryRowContext(ctx, `
		SELECT c.kind FROM execution_attempts a
		JOIN config_verification_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.id=?`, attemptID).Scan(&kind); err != nil {
		return nil, err
	}
	if kind == "browser" {
		return service.rebuildJourneyVerificationInput(ctx, attemptID)
	}
	return service.rebuildVerificationAttemptInput(ctx, attemptID)
}

// QueuedVerificationAttempts returns only the supervisor-only PromQL work
// created by RunVerification. Browser children dispatch to Lintel through
// their journey operation (QueuedJourneyDispatches), never through Plinth.
func (service *Service) QueuedVerificationAttempts(ctx context.Context) ([]int64, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		JOIN config_verification_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run'
		  AND a.state='Queued' AND r.state='Running' AND c.kind='promql'
		ORDER BY a.id`)
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

func (service *Service) rebuildVerificationAttemptInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID int64
	var grant sql.NullInt64
	var check promQLVerificationCheck
	err := service.db.QueryRowContext(ctx, `
		SELECT a.scope_id,p.plan_key,c.check_key,c.query_mode,c.expression,c.range_seconds,c.step_seconds,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query')
		FROM execution_attempts a
		JOIN config_verification_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=a.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='config_verification_run' AND c.kind='promql'`, attemptID).
		Scan(&runID, &check.PlanKey, &check.CheckKey, &check.Mode, &check.Expression, &check.RangeSeconds, &check.StepSeconds, &grant)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen config_thanos_query grant", attemptID)
	}
	var rangeSeconds, stepSeconds *int64
	if check.RangeSeconds.Valid {
		rangeSeconds = &check.RangeSeconds.Int64
	}
	if check.StepSeconds.Valid {
		stepSeconds = &check.StepSeconds.Int64
	}
	canonical, err := json.Marshal(configVerificationInput{
		SchemaKind: configVerificationSchemaKind, AttemptID: attemptID, VerificationRunID: runID,
		PlanKey: check.PlanKey, CheckKey: check.CheckKey, GrantID: grant.Int64,
		Query: configVerificationQuery{Mode: check.Mode, Expression: check.Expression, RangeSeconds: rangeSeconds, StepSeconds: stepSeconds},
	})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	var expected string
	if err := service.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&expected); err != nil {
		return nil, err
	}
	if hex.EncodeToString(digest[:]) != expected {
		return nil, fmt.Errorf("config verification input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}
