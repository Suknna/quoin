package inspection

// 插件采集 ResultProposal 收口（ADR-0004）：类型化 proposal 在一个事务中成为
// Evidence、一个 check result 和 Attempt 的 Succeeded 终态。冻结 SQL 触发器
// 拥有 Attempt 迁移与运行收口；本文件拥有 envelope 校验、boot/epoch fence、
// 重放幂等与计划检查目录的身份复核（inspection_run_checks，而非历史
// config_checks）。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

type pluginProposal struct {
	SchemaKind      string          `json:"schemaKind"`
	AttemptID       int64           `json:"attemptId"`
	InspectionRunID int64           `json:"inspectionRunId"`
	CheckKey        string          `json:"checkKey"`
	Outcome         string          `json:"outcome"`
	ObservedAt      string          `json:"observedAt"`
	ExecutionWindow json.RawMessage `json:"executionWindow"`
	Result          json.RawMessage `json:"result"`
	Warnings        []string        `json:"warnings"`
	Errors          []string        `json:"errors"`
	GapReason       *string         `json:"gapReason"`
}

// CommitPluginProposal 原子持久化一个 supervisor 插件采集结果。提交经共享执
// 行器（ADR-0006）：系统任务作用域从 Attempt 持久关联恢复，boot/epoch fence
// 维持原判，自动审计与结果同事务落库。相同 proposal 仅在其封存了同一不可变
// digest 时幂等重放（不重复记录成功审计）。
func (s *Service) CommitPluginProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal pluginProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("inspection plugin result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != "inspection_plugin_result_v1" || proposal.AttemptID != attemptID ||
		proposal.InspectionRunID < 1 || proposal.CheckKey == "" {
		return fmt.Errorf("inspection plugin result has an invalid identity envelope")
	}
	if _, err := time.Parse(time.RFC3339Nano, proposal.ObservedAt); err != nil {
		return fmt.Errorf("inspection plugin observedAt is not RFC3339: %w", err)
	}
	validGap := map[string]bool{"query_failed": true, "partial_response": true, "no_data": true, "cancelled": true, "interrupted": true}
	switch proposal.Outcome {
	case "success":
		if proposal.GapReason != nil || len(proposal.Errors) != 0 || string(proposal.Result) == "null" {
			return fmt.Errorf("successful inspection plugin result has invalid shape")
		}
		if err := validatePrometheusResult(proposal.Result); err != nil {
			return err
		}
	case "error", "gap":
		if proposal.GapReason == nil || !validGap[*proposal.GapReason] || string(proposal.Result) != "null" {
			return fmt.Errorf("non-success inspection plugin result has invalid gap shape")
		}
	default:
		return fmt.Errorf("inspection plugin result has invalid outcome")
	}

	commandCtx, err := s.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(commandCtx, s.runner, s.pluginResult, func(tx *execution.Tx) (struct{}, error) {
		// 身份复核以 Run 冻结的检查目录为权威：模板的查询形状决定 executionWindow
		// 的合法形状，历史声明 Run 永远不会进入该路径。
		var runID int64
		var checkKey, templateID string
		var paramsJSON string
		err := tx.QueryRowContext(commandCtx, `
			SELECT a.scope_id, a.check_key, c.template_id, c.params_json
			FROM execution_attempts a
			JOIN inspection_runs r ON r.id=a.scope_id AND r.plan_id IS NOT NULL
			JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
			WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
			Scan(&runID, &checkKey, &templateID, &paramsJSON)
		if err != nil {
			return struct{}{}, fmt.Errorf("inspection plugin result does not match a frozen plan check: %w", err)
		}
		if proposal.InspectionRunID != runID || proposal.CheckKey != checkKey {
			return struct{}{}, fmt.Errorf("inspection plugin result identity does not match frozen attempt")
		}
		var params struct {
			RangeSeconds *int64 `json:"rangeSeconds"`
			StepSeconds  *int64 `json:"stepSeconds"`
		}
		_ = json.Unmarshal([]byte(paramsJSON), &params)
		if params.RangeSeconds != nil {
			// 成功的范围结果必须携带实际执行窗口；失败/gap 的窗口是冻结输入元数据，
			// 可缺省（没有执行就没有实际窗口），携带时仍校验形状，不伪造执行事实。
			if proposal.Outcome == "success" || (len(proposal.ExecutionWindow) != 0 && string(proposal.ExecutionWindow) != "null") {
				var window struct {
					StartAt     string `json:"startAt"`
					EndAt       string `json:"endAt"`
					StepSeconds int64  `json:"stepSeconds"`
				}
				if err := json.Unmarshal(proposal.ExecutionWindow, &window); err != nil || window.StepSeconds < 1 {
					return struct{}{}, fmt.Errorf("range inspection plugin executionWindow is malformed")
				}
				if _, err := time.Parse(time.RFC3339Nano, window.StartAt); err != nil {
					return struct{}{}, fmt.Errorf("range inspection plugin executionWindow startAt is not RFC3339")
				}
				if _, err := time.Parse(time.RFC3339Nano, window.EndAt); err != nil {
					return struct{}{}, fmt.Errorf("range inspection plugin executionWindow endAt is not RFC3339")
				}
			}
		} else if templateID != "" && string(proposal.ExecutionWindow) != "null" && len(proposal.ExecutionWindow) != 0 {
			return struct{}{}, fmt.Errorf("instant inspection plugin result must have a null executionWindow")
		}
		digest := sha256.Sum256(raw)
		var existing []byte
		replayErr := tx.QueryRowContext(commandCtx, `
			SELECT result_digest FROM inspection_check_results WHERE run_id=? AND check_key=?`, runID, checkKey).Scan(&existing)
		if replayErr == nil {
			// 幂等重放在任何活性 fence 之前裁决：已提交结果不可覆盖，只能确认。
			if string(existing) == string(digest[:]) {
				return struct{}{}, errResultReplayed
			}
			return struct{}{}, fmt.Errorf("inspection plugin result replay digest conflicts")
		}
		if replayErr != sql.ErrNoRows {
			return struct{}{}, replayErr
		}
		// Boot/epoch/取消 fence：冻结提交触发器执行终态迁移，这里只做派发绑定守卫。
		var bound int
		if err := tx.QueryRowContext(commandCtx, `
			SELECT 1 FROM execution_attempts
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return struct{}{}, attempt.ErrLateResult
			}
			return struct{}{}, err
		}
		var evidenceID any
		if proposal.Outcome == "success" {
			// 冻结闭包校验 evidence params 的 check_key 键（形状保持不变）；
			// 采集元数据（observedAt/warnings/执行窗口事实）统一由检查结果的
			// meta_json 一次性承载，成功与 gap 同一来源，绝不双写第二份。
			paramsPayload := fmt.Sprintf(`{"check_key":%q}`, checkKey)
			warnings, _ := json.Marshal(proposal.Warnings)
			insert, err := tx.ExecContext(commandCtx, `
				INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,warnings_json,integrity,created_at)
				VALUES(?,'inspection_run',?,?,?,?,?,'complete',?)`,
				attemptID, runID, paramsPayload, proposal.ObservedAt, string(proposal.Result), string(warnings), s.nowText())
			if err != nil {
				return struct{}{}, err
			}
			id, err := insert.LastInsertId()
			if err != nil {
				return struct{}{}, err
			}
			evidenceID = id
		}
		var nullableGap any
		if proposal.GapReason != nil {
			nullableGap = *proposal.GapReason
		}
		// 采集元数据冻结（observedAt/真实 warnings/执行窗口事实）随检查结果一
		// 次性写入：gap/error 没有 Evidence，其观察时间与 warnings 只由本列承
		// 载，分析清单据此保持缺口可见。窗口是采集实际执行的查询窗口（以冻结
		// evidence_at 为终点），gap 行携带的是请求窗口元数据，绝不是伪造的成
		// 功结果；即时查询与未携带窗口的失败保持缺失。
		warningsJSON, marshalErr := json.Marshal(proposal.Warnings)
		if marshalErr != nil {
			return struct{}{}, marshalErr
		}
		metaPayload := fmt.Sprintf(`{"observedAt":%q,"warnings":%s`, proposal.ObservedAt, string(warningsJSON))
		if len(proposal.ExecutionWindow) != 0 && string(proposal.ExecutionWindow) != "null" {
			metaPayload += `,"executionWindow":` + string(proposal.ExecutionWindow)
		}
		metaPayload += `}`
		status := "ok"
		if proposal.Outcome != "success" {
			status = "gap"
		}
		if _, err := tx.ExecContext(commandCtx, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,meta_json,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, runID, checkKey, status, evidenceID, attemptID, digest[:], nullableGap, metaPayload, s.nowText()); err != nil {
			return struct{}{}, err
		}
		if err := s.convergeOn(commandCtx, tx, runID); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, func(struct{}) int64 { return proposal.InspectionRunID })
	if err != nil {
		if errors.Is(err, errResultReplayed) {
			return nil
		}
		return err
	}
	return nil
}
