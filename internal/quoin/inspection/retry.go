package inspection

// 重分析（ReanalyzeRun）与重采证（RerunInspection）命令（ADR-0006）：二者经
// 共享执行器的账本命令路径执行——会话复核、幂等重放、业务修改、命令台账与
// 审计事件由执行器在同一事务统一提交；确定性拒绝在保存点回滚后的干净事务中
// 持久记录，其余错误整事务回滚、不留痕。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
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
	digest := auth.DigestCommand(CommandReanalyzeRun, map[string]any{"runId": runID})
	outcome, err := execution.Run(ctx, s.runner, s.reanalyzeRun, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (AttemptSummary, execution.Change, error) {
		var state string
		err := tx.QueryRowContext(ctx, `
			SELECT r.state FROM inspection_runs r WHERE r.id=?`, runID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return AttemptSummary{}, execution.Unchanged, &execution.Rejection{Code: "not_found", Detail: "巡检 Run 不存在", ObjectID: runID}
		}
		if err != nil {
			return AttemptSummary{}, execution.Unchanged, err
		}
		if state != "Completed" && state != "CompletedWithGaps" {
			return AttemptSummary{}, execution.Unchanged, &execution.Rejection{Code: "active_conflict", Detail: "巡检采证尚未完成，不能重新分析", ObjectID: runID}
		}
		var activeID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM execution_attempts
			WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?
			  AND state IN ('Queued','Assigned','Running','Cancelling')
			ORDER BY id DESC LIMIT 1`, runID).Scan(&activeID)
		if err == nil {
			return AttemptSummary{}, execution.Unchanged, &execution.Rejection{Code: "active_conflict", Detail: "该巡检 Run 已有进行中的分析", ObjectID: activeID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return AttemptSummary{}, execution.Unchanged, err
		}
		attemptID, err := s.createReportAnalysisOn(ctx, tx, runID, s.nowText(), true)
		if err != nil {
			return AttemptSummary{}, execution.Unchanged, err
		}
		if attemptID == 0 {
			return AttemptSummary{}, execution.Unchanged, fmt.Errorf("reanalysis did not create an attempt")
		}
		result, err := attemptSummaryOn(ctx, tx, attemptID)
		if err != nil {
			return AttemptSummary{}, execution.Unchanged, err
		}
		return result, execution.Changed, nil
	}, func(result AttemptSummary) int64 { return runID })
	if err != nil {
		return AttemptSummary{}, translateCommandError(err)
	}
	summary := outcome.Result
	if outcome.Replayed {
		summary.AttemptID = mustLocator(summary.ID)
	}
	return summary, nil
}

// RerunInspection creates a new manual collection run copying the source
// run's frozen plan binding (template, params, scope, connection). Legacy
// declaration runs are history: their producer was removed, so re-collection
// requires a real plan instead of resurrecting a retired declaration.
func (s *Service) RerunInspection(ctx context.Context, principalID int64, clientCommandID string, sourceRunID int64) (RunDetail, error) {
	digest := auth.DigestCommand(CommandRerunRun, map[string]any{"runId": sourceRunID})
	outcome, err := execution.Run(ctx, s.runner, s.rerunRun, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (RunDetail, execution.Change, error) {
		var sourceState string
		var planID, connectionID sql.NullInt64
		var planKey string
		err := tx.QueryRowContext(ctx, `
			SELECT r.state, r.plan_id, r.connection_id, r.plan_key
			FROM inspection_runs r WHERE r.id=?`, sourceRunID).
			Scan(&sourceState, &planID, &connectionID, &planKey)
		if errors.Is(err, sql.ErrNoRows) {
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "not_found", Detail: "巡检 Run 不存在", ObjectID: sourceRunID}
		}
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if !planID.Valid {
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "legacy_run", Detail: "该 Run 来自历史业务声明计划，不支持重新采证；请创建巡检计划后重试", ObjectID: sourceRunID}
		}
		switch sourceState {
		case "Queued", "Running":
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "active_conflict", Detail: "进行中的巡检 Run 不能重新采证", ObjectID: sourceRunID}
		case "Completed", "CompletedWithGaps", "Failed", "Cancelled", "Interrupted":
			// Exactly the terminal source states admitted by
			// trg_inspection_runs_closure for a re-collection lineage.
		default:
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "state_conflict", Detail: "该巡检 Run 没有可重新采证的终态结果", ObjectID: sourceRunID}
		}
		var planEnabled, connectionEnabled int
		err = tx.QueryRowContext(ctx, `
			SELECT p.enabled, c.enabled FROM inspection_plans p JOIN connections c ON c.id=p.connection_id WHERE p.id=?`, planID.Int64).
			Scan(&planEnabled, &connectionEnabled)
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if planEnabled == 0 || connectionEnabled == 0 {
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "plan_disabled", Detail: "计划或其来源接入未启用，不能重新采证", ObjectID: sourceRunID}
		}
		// 重新采证逐字段复制源 Run 的冻结绑定（模板/参数/范围/接入），不受计划
		// 当前定义影响；展开的检查目录同样来自源 Run。
		now := s.nowText()
		insert, err := tx.ExecContext(ctx, `
			INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,rerun_of_id,state,created_at)
			SELECT plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,'manual',id,'Queued',?
			FROM inspection_runs WHERE id=?`, now, sourceRunID)
		if err != nil {
			var active int64
			_ = tx.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE plan_id=? AND state IN ('Queued','Running')`, planID.Int64).Scan(&active)
			return RunDetail{}, execution.Unchanged, &execution.Rejection{Code: "active_conflict", Detail: "该巡检计划已有进行中的 Run", ObjectID: active}
		}
		runID, err := insert.LastInsertId()
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,target_json,created_at)
			SELECT ?,check_key,display_name,plugin_id,template_id,template_version,params_json,target_json,?
			FROM inspection_run_checks WHERE run_id=?`, runID, now, sourceRunID); err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT check_key FROM inspection_run_checks WHERE run_id=? ORDER BY check_key`, runID)
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		var checkKeys []string
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				rows.Close()
				return RunDetail{}, execution.Unchanged, err
			}
			checkKeys = append(checkKeys, key)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		for _, key := range checkKeys {
			if err = s.pluginChildForRerun(ctx, tx, runID, sourceRunID, key, now); err != nil {
				if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
					return RunDetail{}, execution.Unchanged, fmt.Errorf("%w: 尚无可用的指标连接，请先创建并启用连接后重试", err)
				}
				return RunDetail{}, execution.Unchanged, err
			}
		}
		if err = s.convergeOn(ctx, tx, runID); err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		detail, err := s.detailOn(ctx, tx, runID)
		if err != nil {
			return RunDetail{}, execution.Unchanged, err
		}
		return detail, execution.Changed, nil
	}, func(detail RunDetail) int64 { return detail.RunID })
	if err != nil {
		return RunDetail{}, translateCommandError(err)
	}
	detail := outcome.Result
	if outcome.Replayed {
		detail.RunID = mustLocator(detail.ID)
	}
	return detail, nil
}

func attemptSummaryOn(ctx context.Context, q audit.Reader, attemptID int64) (AttemptSummary, error) {
	var summary AttemptSummary
	var startedAt, endedAt, reason sql.NullString
	err := q.QueryRowContext(ctx, `
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
