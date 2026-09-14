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
func (s *Service) ReanalyzeRun(ctx context.Context, principalID int64, clientCommandID string, runID int64) (AttemptSummary, error) {
	const command = "inspection_run.reanalyze"
	digest := auth.DigestCommand(command, map[string]any{"runId": runID})
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
		SELECT r.state FROM inspection_runs r WHERE r.id=?`, runID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "not_found", Detail: "巡检 Run 不存在", ObjectID: runID}, &committed)
	} else if err != nil {
		return AttemptSummary{}, err
	}
	if state != "Completed" && state != "CompletedWithGaps" {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "巡检采证尚未完成，不能重新分析", ObjectID: runID}, &committed)
	}
	var activeID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?
		  AND state IN ('Queued','Assigned','Running','Cancelling')
		ORDER BY id DESC LIMIT 1`, runID).Scan(&activeID)
	if err == nil {
		return s.rejectAttempt(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "该巡检 Run 已有进行中的分析", ObjectID: activeID}, &committed)
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

// RerunInspection creates a new manual collection run copying the source
// run's frozen plan binding (template, params, scope, connection). Legacy
// declaration runs are history: their producer was removed, so re-collection
// requires a real plan instead of resurrecting a retired declaration.
func (s *Service) RerunInspection(ctx context.Context, principalID int64, clientCommandID string, sourceRunID int64) (RunDetail, error) {
	const command = "inspection_run.rerun"
	digest := auth.DigestCommand(command, map[string]any{"runId": sourceRunID})
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
	var sourceState string
	var planID, connectionID sql.NullInt64
	var planKey string
	err = conn.QueryRowContext(ctx, `
		SELECT r.state, r.plan_id, r.connection_id, r.plan_key
		FROM inspection_runs r WHERE r.id=?`, sourceRunID).
		Scan(&sourceState, &planID, &connectionID, &planKey)
	if errors.Is(err, sql.ErrNoRows) {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "not_found", Detail: "巡检 Run 不存在", ObjectID: sourceRunID}, &committed)
	}
	if err != nil {
		return RunDetail{}, err
	}
	if !planID.Valid {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "legacy_run", Detail: "该 Run 来自历史业务声明计划，不支持重新采证；请创建巡检计划后重试", ObjectID: sourceRunID}, &committed)
	}
	switch sourceState {
	case "Queued", "Running":
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "active_conflict", Detail: "进行中的巡检 Run 不能重新采证", ObjectID: sourceRunID}, &committed)
	case "Completed", "CompletedWithGaps", "Failed", "Cancelled", "Interrupted":
		// Exactly the terminal source states admitted by
		// trg_inspection_runs_closure for a re-collection lineage.
	default:
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "state_conflict", Detail: "该巡检 Run 没有可重新采证的终态结果", ObjectID: sourceRunID}, &committed)
	}
	var planEnabled, connectionEnabled int
	err = conn.QueryRowContext(ctx, `
		SELECT p.enabled, c.enabled FROM inspection_plans p JOIN connections c ON c.id=p.connection_id WHERE p.id=?`, planID.Int64).
		Scan(&planEnabled, &connectionEnabled)
	if err != nil {
		return RunDetail{}, err
	}
	if planEnabled == 0 || connectionEnabled == 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest,
			&RejectionError{Code: "plan_disabled", Detail: "计划或其来源接入未启用，不能重新采证", ObjectID: sourceRunID}, &committed)
	}
	// 重新采证逐字段复制源 Run 的冻结绑定（模板/参数/范围/接入），不受计划
	// 当前定义影响；展开的检查目录同样来自源 Run。
	now := s.nowText()
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,rerun_of_id,state,created_at)
		SELECT plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,'manual',id,'Queued',?
		FROM inspection_runs WHERE id=?`, now, sourceRunID)
	if err != nil {
		var active int64
		_ = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE plan_id=? AND state IN ('Queued','Running')`, planID.Int64).Scan(&active)
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &RejectionError{Code: "active_conflict", Detail: "该巡检计划已有进行中的 Run", ObjectID: active}, &committed)
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,target_json,created_at)
		SELECT ?,check_key,display_name,plugin_id,template_id,template_version,params_json,target_json,?
		FROM inspection_run_checks WHERE run_id=?`, runID, now, sourceRunID); err != nil {
		return RunDetail{}, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT check_key FROM inspection_run_checks WHERE run_id=? ORDER BY check_key`, runID)
	if err != nil {
		return RunDetail{}, err
	}
	var checkKeys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return RunDetail{}, err
		}
		checkKeys = append(checkKeys, key)
	}
	rows.Close()
	for _, key := range checkKeys {
		if err = s.pluginChildForRerun(ctx, conn, runID, sourceRunID, key, now); err != nil {
			if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
				return RunDetail{}, fmt.Errorf("%w: 尚无可用的指标连接，请先创建并启用连接后重试", err)
			}
			return RunDetail{}, err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return RunDetail{}, err
	}
	detail, err := s.detailOn(ctx, conn, runID)
	if err != nil {
		return RunDetail{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, detail.RunID, s.nowText()); err != nil {
		return RunDetail{}, err
	}
	if err = s.recordCommand(ctx, conn, principalID, clientCommandID, command, digest, detail.RunID, detail); err != nil {
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
