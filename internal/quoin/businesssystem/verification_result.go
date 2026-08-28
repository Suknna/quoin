package businesssystem

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// VerificationProposal is the fixed non-secret PromQL result sealed by the
// supervisor. It deliberately carries response facts, never connection data.
type VerificationProposal struct {
	SchemaKind        string          `json:"schemaKind"`
	AttemptID         int64           `json:"attemptId"`
	VerificationRunID int64           `json:"verificationRunId"`
	PlanKey           string          `json:"planKey"`
	CheckKey          string          `json:"checkKey"`
	Outcome           string          `json:"outcome"`
	ObservedAt        string          `json:"observedAt"`
	Result            json.RawMessage `json:"result"`
	Warnings          []string        `json:"warnings"`
	Errors            []string        `json:"errors"`
	GapReason         *string         `json:"gapReason"`
}

// CommitVerificationProposal atomically persists a supervisor result as
// Evidence, a per-check outcome, the fenced Attempt terminal state and (once
// all config checks have settled) the parent Run terminal state.
func (service *Service) CommitVerificationProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal VerificationProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("config verification result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != "config_promql_result_v1" || proposal.AttemptID != attemptID || proposal.VerificationRunID < 1 || proposal.PlanKey == "" || proposal.CheckKey == "" {
		return fmt.Errorf("config verification result has an invalid identity envelope")
	}
	if _, err := time.Parse(time.RFC3339Nano, proposal.ObservedAt); err != nil {
		return fmt.Errorf("config verification observedAt is not RFC3339: %w", err)
	}
	validGap := map[string]bool{"query_failed": true, "partial_response": true, "no_data": true, "cancelled": true, "interrupted": true}
	isNull := string(proposal.Result) == "null"
	switch proposal.Outcome {
	case "success":
		var object map[string]json.RawMessage
		if proposal.GapReason != nil || len(proposal.Errors) != 0 || isNull || json.Unmarshal(proposal.Result, &object) != nil || object == nil {
			return fmt.Errorf("successful config verification result has invalid result shape")
		}
	case "error", "gap":
		if proposal.GapReason == nil || !validGap[*proposal.GapReason] || !isNull {
			return fmt.Errorf("non-success config verification result has invalid gap shape")
		}
	default:
		return fmt.Errorf("config verification result has invalid outcome")
	}

	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var runID int64
	var planKey, checkKey string
	err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id,a.plan_key,a.check_key
		FROM execution_attempts a
		WHERE a.id=? AND a.scope_type='config_verification_run' AND a.attempt_type='inspection_collection'`, attemptID).
		Scan(&runID, &planKey, &checkKey)
	if err != nil {
		return err
	}
	if proposal.VerificationRunID != runID || proposal.PlanKey != planKey || proposal.CheckKey != checkKey {
		return fmt.Errorf("config verification result identity does not match attempt")
	}
	// A duplicate ResultProposal is a harmless replay only when it sealed the
	// same immutable result digest; a different payload can never overwrite it.
	digest := sha256.Sum256(raw)
	var existing []byte
	replayErr := conn.QueryRowContext(ctx, `SELECT result_digest FROM config_verification_run_check_results WHERE verification_run_id=? AND plan_key=? AND check_key=?`, runID, planKey, checkKey).Scan(&existing)
	if replayErr == nil {
		if string(existing) == string(digest[:]) {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("config verification result replay digest conflicts")
	}
	if replayErr != sql.ErrNoRows {
		return replayErr
	}

	status := "ok"
	var gapReason string
	if proposal.Outcome != "success" {
		status, gapReason = "gap", *proposal.GapReason
	}
	// Gap warnings stay machine-readable on the check result; there is no
	// Evidence row for a gap, so this column is their only durable home.
	var warningsJSON any
	if len(proposal.Warnings) != 0 {
		marshalled, err := json.Marshal(proposal.Warnings)
		if err != nil {
			return err
		}
		warningsJSON = string(marshalled)
	}
	var evidenceID any
	if proposal.Outcome == "success" {
		params, _ := json.Marshal(map[string]string{"planKey": planKey, "checkKey": checkKey})
		warnings, _ := json.Marshal(proposal.Warnings)
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,warnings_json,integrity,created_at)
			VALUES(?,'config_verification_run',?,?,?,?,?,?,?)`,
			attemptID, runID, string(params), proposal.ObservedAt, string(proposal.Result), string(warnings), "complete", service.nowText())
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
	if gapReason != "" {
		nullableGap = gapReason
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO config_verification_run_check_results(verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,warnings_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, runID, planKey, checkKey, status, evidenceID, attemptID, digest[:], nullableGap, warningsJSON, service.nowText()); err != nil {
		return err
	}
	if err := attempt.NewService(service.db).CommitResultOn(ctx, conn, attemptID, bootID, epoch, proposal.Outcome != "error", "tool_error"); err != nil {
		return err
	}
	var pending, failed int
	if err := conn.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id WHERE p.config_version_id=r.config_version_id)
		  - (SELECT COUNT(*) FROM config_verification_run_check_results x WHERE x.verification_run_id=r.id),
		  (SELECT COUNT(*) FROM config_verification_run_check_results x WHERE x.verification_run_id=r.id AND x.status <> 'ok')
		FROM config_verification_runs r WHERE r.id=?`, runID).Scan(&pending, &failed); err != nil {
		return err
	}
	if pending == 0 {
		state, detail := "Passed", any(nil)
		if failed > 0 {
			state, detail = "Failed", "存在未通过、部分或失败的配置验证检查"
		}
		if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, detail, runID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// convergeVerificationRunOn closes the parent Run once every configured check
// has settled; Passed requires full ok+Evidence coverage (DATA-CONFIG-007).
// It runs inside the caller's open transaction.
func convergeVerificationRunOn(ctx context.Context, conn *sql.Conn, runID int64) error {
	var pending, failed int
	if err := conn.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id WHERE p.config_version_id=r.config_version_id)
		  - (SELECT COUNT(*) FROM config_verification_run_check_results x WHERE x.verification_run_id=r.id),
		  (SELECT COUNT(*) FROM config_verification_run_check_results x WHERE x.verification_run_id=r.id AND x.status <> 'ok')
		FROM config_verification_runs r WHERE r.id=?`, runID).Scan(&pending, &failed); err != nil {
		return err
	}
	if pending != 0 {
		return nil
	}
	state, detail := "Passed", any(nil)
	if failed > 0 {
		state, detail = "Failed", "存在未通过、部分或失败的配置验证检查"
	}
	_, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state=?,result_detail=?,row_version=row_version+1 WHERE id=? AND state='Running'`, state, detail, runID)
	return err
}

// RecordVerificationTechnicalGap closes a mechanically interrupted PromQL
// Attempt without inventing Evidence. It is used only after the generic
// Attempt authority has durably placed the child in a terminal state.
func (service *Service) RecordVerificationTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var runID int64
	var planKey, checkKey string
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT a.scope_id,a.plan_key,a.check_key,a.state FROM execution_attempts a WHERE a.id=? AND a.scope_type='config_verification_run' AND a.attempt_type='inspection_collection'`, attemptID).Scan(&runID, &planKey, &checkKey, &state); err != nil {
		return err
	}
	if state != "Failed" && state != "Cancelled" && state != "Interrupted" {
		return fmt.Errorf("attempt %d is not terminal", attemptID)
	}
	var parentState string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM config_verification_runs WHERE id=?`, runID).Scan(&parentState); err != nil {
		return err
	}
	if parentState != "Running" {
		// The parent already closed (e.g. the cancel fence ran first); the
		// child's terminal transition needs no further closure row.
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return err
		}
		committed = true
		return nil
	}
	gapReason := "interrupted"
	if reason == "cancelled" {
		gapReason = reason
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO config_verification_run_check_results(verification_run_id,plan_key,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at) VALUES(?,?,?,'gap',NULL,?,NULL,?,?)`, runID, planKey, checkKey, attemptID, gapReason, service.nowText()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE config_verification_runs SET state='Interrupted',row_version=row_version+1,result_detail=?
		WHERE id=? AND state='Running' AND NOT EXISTS (
			SELECT 1 FROM execution_attempts WHERE scope_type='config_verification_run' AND scope_id=?
			AND state IN ('Queued','Assigned','Running','Cancelling')
		)`, "采集 Runtime 已中断", runID, runID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
