package inspection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// AttemptSummary is the frozen AttemptSummary response used when an explicit
// re-analysis has accepted a new immutable report producer.
type AttemptSummary struct {
	AttemptID         int64   `json:"-"`
	ID                string  `json:"id"`
	Type              string  `json:"type"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	StartedAt         *string `json:"startedAt,omitempty"`
	EndedAt           *string `json:"endedAt,omitempty"`
	TerminationReason *string `json:"terminationReason,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// ReanalyzeRun queues one fresh analysis against the Run's existing immutable
// check results and Evidence. It does not recollect or alter an old report.
func (s *Service) ReanalyzeRun(ctx context.Context, principalID int64, clientCommandID, systemKey string, runID int64) (AttemptSummary, error) {
	const command = "inspection_run.reanalyze"
	digest := auth.DigestCommand(command, map[string]any{"systemKey": systemKey, "runId": runID})
	if result, replayed, err := s.replayAttempt(ctx, principalID, clientCommandID, digest); replayed || err != nil {
		return result, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return AttemptSummary{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return AttemptSummary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if result, replayed, err := s.replayAttemptOn(ctx, conn, principalID, clientCommandID, digest); replayed || err != nil {
		if replayed {
			if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
				return AttemptSummary{}, commitErr
			}
			committed = true
		}
		return result, err
	}
	var state string
	if err = conn.QueryRowContext(ctx, `
		SELECT r.state FROM inspection_runs r
		JOIN business_systems b ON b.id=r.business_system_id
		WHERE r.id=? AND b.key=?`, runID, systemKey).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "not_found", Detail: "巡检 Run 不存在", SystemKey: systemKey, ObjectID: runID}, &committed)
	} else if err != nil {
		return AttemptSummary{}, err
	}
	if state != "Completed" && state != "CompletedWithGaps" {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "巡检采证尚未完成，不能重新分析", SystemKey: systemKey, ObjectID: runID}, &committed)
	}
	var activeID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?
		  AND state IN ('Queued','Assigned','Running','Cancelling')
		ORDER BY id DESC LIMIT 1`, runID).Scan(&activeID)
	if err == nil {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "该巡检 Run 已有进行中的分析", SystemKey: systemKey, ObjectID: activeID}, &committed)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AttemptSummary{}, err
	}
	attemptID, err := s.createReportAnalysisOn(ctx, conn, runID, s.nowText(), true)
	if err != nil {
		return AttemptSummary{}, err
	}
	if attemptID == 0 {
		return AttemptSummary{}, fmt.Errorf("reanalysis did not create an attempt")
	}
	result, err := attemptSummaryOn(ctx, conn, attemptID)
	if err != nil {
		return AttemptSummary{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, runID, s.nowText()); err != nil {
		return AttemptSummary{}, err
	}
	if err = recordAttemptCommand(ctx, conn, principalID, clientCommandID, command, digest, runID, result); err != nil {
		return AttemptSummary{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return AttemptSummary{}, err
	}
	committed = true
	return result, nil
}

// RerunInspection creates a new manual collection run bound to the exact
// immutable configuration and label-contract snapshots of sourceRunID. The
// new run starts a new evidence chain and records its sole lineage pointer.
func (s *Service) RerunInspection(ctx context.Context, principalID int64, clientCommandID, systemKey string, sourceRunID int64) (RunDetail, error) {
	const command = "inspection_run.rerun"
	digest := auth.DigestCommand(command, map[string]any{"systemKey": systemKey, "runId": sourceRunID})
	if detail, replayed, err := s.replay(ctx, principalID, clientCommandID, digest); replayed || err != nil {
		return detail, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if detail, replayed, err := s.replayOn(ctx, conn, principalID, clientCommandID, digest); replayed || err != nil {
		if replayed {
			if _, commitErr := conn.ExecContext(ctx, "COMMIT"); commitErr != nil {
				return RunDetail{}, commitErr
			}
			committed = true
		}
		return detail, err
	}
	var systemID, configVersionID, contractID, planID int64
	var planKey, sourceState string
	var systemEnabled bool
	err = conn.QueryRowContext(ctx, `
		SELECT r.business_system_id,r.config_version_id,r.label_contract_version_id,p.id,r.plan_key,r.state,b.enabled
		FROM inspection_runs r
		JOIN business_systems b ON b.id=r.business_system_id AND b.key=?
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		WHERE r.id=?`, systemKey, sourceRunID).
		Scan(&systemID, &configVersionID, &contractID, &planID, &planKey, &sourceState, &systemEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "not_found", Detail: "巡检 Run 不存在", SystemKey: systemKey, ObjectID: sourceRunID}, &committed)
	}
	if err != nil {
		return RunDetail{}, err
	}
	if !systemEnabled {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "state_conflict", Detail: "业务系统已禁用，不能重新采证", SystemKey: systemKey, ObjectID: sourceRunID}, &committed)
	}
	switch sourceState {
	case "Queued", "Running":
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "进行中的巡检 Run 不能重新采证", SystemKey: systemKey, ObjectID: sourceRunID}, &committed)
	case "Completed", "CompletedWithGaps", "Failed", "Cancelled", "Interrupted":
		// These are exactly the immutable terminal source states admitted by
		// trg_inspection_runs_closure for a re-collection lineage.
	default:
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "state_conflict", Detail: "该巡检 Run 没有可重新采证的终态结果", SystemKey: systemKey, ObjectID: sourceRunID}, &committed)
	}
	checks, err := loadChecks(ctx, conn, planID)
	if err != nil {
		return RunDetail{}, err
	}
	if len(checks) == 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "empty_plan", Detail: "源巡检 Run 的计划没有检查项", SystemKey: systemKey, ObjectID: sourceRunID}, &committed)
	}
	now := s.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_runs(business_system_id,plan_key,config_version_id,label_contract_version_id,trigger_kind,state,rerun_of_id,created_at)
		VALUES(?,?,?,?,'manual','Queued',?,?)`, systemID, planKey, configVersionID, contractID, sourceRunID, now)
	if err != nil {
		var active int64
		_ = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE business_system_id=? AND plan_key=? AND state IN ('Queued','Running')`, systemID, planKey).Scan(&active)
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "该巡检计划已有进行中的 Run", SystemKey: systemKey, ObjectID: active}, &committed)
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		return RunDetail{}, err
	}
	for _, check := range checks {
		if check.kind == "promql" {
			err = s.promqlChild(ctx, conn, runID, configVersionID, contractID, check, now)
		} else {
			err = s.browserChild(ctx, conn, runID, configVersionID, contractID, planKey, systemID, check, now)
		}
		if err != nil {
			if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
				return RunDetail{}, fmt.Errorf("%w: 尚无可用的 Thanos 指标连接，请先创建并启用连接后重试", err)
			}
			return RunDetail{}, err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return RunDetail{}, err
	}
	detail, err := s.detailOn(ctx, conn, systemKey, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, runID, now); err != nil {
		return RunDetail{}, err
	}
	if err = s.recordCommand(ctx, conn, principalID, clientCommandID, command, digest, runID, detail); err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	committed = true
	return detail, nil
}

func attemptSummaryOn(ctx context.Context, conn *sql.Conn, attemptID int64) (AttemptSummary, error) {
	var summary AttemptSummary
	var startedAt, endedAt, reason sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT id,attempt_type,state,row_version,started_at,ended_at,termination_reason,created_at
		FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&summary.AttemptID, &summary.Type, &summary.State, &summary.RowVersion, &startedAt, &endedAt, &reason, &summary.CreatedAt)
	if err != nil {
		return AttemptSummary{}, err
	}
	summary.ID = locatorID(summary.AttemptID)
	if startedAt.Valid {
		summary.StartedAt = &startedAt.String
	}
	if endedAt.Valid {
		summary.EndedAt = &endedAt.String
	}
	if reason.Valid {
		summary.TerminationReason = &reason.String
	}
	return summary, nil
}

func (s *Service) replayAttempt(ctx context.Context, principalID int64, clientCommandID, digest string) (AttemptSummary, bool, error) {
	record, found, err := auth.LookupCommand(ctx, s.db, principalID, clientCommandID)
	if err != nil {
		return AttemptSummary{}, false, err
	}
	return decodeAttemptReplay(record, found, digest)
}

func (s *Service) replayAttemptOn(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, digest string) (AttemptSummary, bool, error) {
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return AttemptSummary{}, false, err
	}
	return decodeAttemptReplay(record, found, digest)
}

func decodeAttemptReplay(record auth.CommandRecord, found bool, digest string) (AttemptSummary, bool, error) {
	if !found {
		return AttemptSummary{}, false, nil
	}
	if record.RequestDigest != digest {
		return AttemptSummary{}, true, ErrCommandReused
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		var rejection RejectionError
		if err := json.Unmarshal([]byte(record.ResultPayload), &rejection); err != nil {
			return AttemptSummary{}, true, err
		}
		return AttemptSummary{}, true, &rejection
	}
	var result AttemptSummary
	if err := json.Unmarshal([]byte(record.ResultPayload), &result); err != nil {
		return AttemptSummary{}, true, err
	}
	if id, err := strconv.ParseInt(result.ID, 10, 64); err == nil {
		result.AttemptID = id
	}
	return result, true, nil
}

func recordAttemptCommand(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, runID int64, result AttemptSummary) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeCommitted, "inspection_run", runID, string(payload))
}

func (s *Service) rejectAttempt(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, rejection *RejectionError, committed *bool) (AttemptSummary, error) {
	payload, _ := json.Marshal(rejection)
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeRejectedKnown, "inspection_run", rejection.ObjectID, string(payload)); err != nil {
		return AttemptSummary{}, err
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,?,?,'rejected','inspection_run',?,?)`, principalID, command, clientCommandID, rejection.ObjectID, s.nowText()); err != nil {
		return AttemptSummary{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return AttemptSummary{}, err
	}
	*committed = true
	return AttemptSummary{}, rejection
}
