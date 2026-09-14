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

// CommitPluginProposal 原子持久化一个 supervisor 插件采集结果。相同 proposal
// 仅在其封存了同一不可变 digest 时幂等重放。
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

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// 身份复核以 Run 冻结的检查目录为权威：模板的查询形状决定 executionWindow
	// 的合法形状，历史声明 Run 永远不会进入该路径。
	var runID int64
	var checkKey, templateID string
	var paramsJSON string
	err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id, a.check_key, c.template_id, c.params_json
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id AND r.plan_id IS NOT NULL
		JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
		Scan(&runID, &checkKey, &templateID, &paramsJSON)
	if err != nil {
		return fmt.Errorf("inspection plugin result does not match a frozen plan check: %w", err)
	}
	if proposal.InspectionRunID != runID || proposal.CheckKey != checkKey {
		return fmt.Errorf("inspection plugin result identity does not match frozen attempt")
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
				return fmt.Errorf("range inspection plugin executionWindow is malformed")
			}
			if _, err := time.Parse(time.RFC3339Nano, window.StartAt); err != nil {
				return fmt.Errorf("range inspection plugin executionWindow startAt is not RFC3339")
			}
			if _, err := time.Parse(time.RFC3339Nano, window.EndAt); err != nil {
				return fmt.Errorf("range inspection plugin executionWindow endAt is not RFC3339")
			}
		}
	} else if templateID != "" && string(proposal.ExecutionWindow) != "null" && len(proposal.ExecutionWindow) != 0 {
		return fmt.Errorf("instant inspection plugin result must have a null executionWindow")
	}
	digest := sha256.Sum256(raw)
	var existing []byte
	replayErr := conn.QueryRowContext(ctx, `
		SELECT result_digest FROM inspection_check_results WHERE run_id=? AND check_key=?`, runID, checkKey).Scan(&existing)
	if replayErr == nil {
		// 幂等重放在任何活性 fence 之前裁决：已提交结果不可覆盖，只能确认。
		if string(existing) == string(digest[:]) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("inspection plugin result replay digest conflicts")
	}
	if replayErr != sql.ErrNoRows {
		return replayErr
	}
	// Boot/epoch/取消 fence：冻结提交触发器执行终态迁移，这里只做派发绑定守卫。
	var bound int
	if err = conn.QueryRowContext(ctx, `
		SELECT 1 FROM execution_attempts
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&bound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ErrLateResult
		}
		return err
	}
	var evidenceID any
	if proposal.Outcome == "success" {
		paramsPayload, _ := json.Marshal(map[string]string{"check_key": checkKey})
		warnings, _ := json.Marshal(proposal.Warnings)
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,warnings_json,integrity,created_at)
			VALUES(?,'inspection_run',?,?,?,?,?,'complete',?)`,
			attemptID, runID, string(paramsPayload), proposal.ObservedAt, string(proposal.Result), string(warnings), s.nowText())
		if err != nil {
			return err
		}
		id, err := insert.LastInsertId()
		if err != nil {
			return err
		}
		evidenceID = id
	}
	var nullableGap any
	if proposal.GapReason != nil {
		nullableGap = *proposal.GapReason
	}
	status := "ok"
	if proposal.Outcome != "success" {
		status = "gap"
	}
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, runID, checkKey, status, evidenceID, attemptID, digest[:], nullableGap, s.nowText()); err != nil {
		return err
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}
