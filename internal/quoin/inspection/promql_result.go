// PromQL ResultProposal closure for run_check children
// (CFG-INSPECTRUN-001, DATA-TX-018): the typed proposal becomes Evidence, one
// check result, and the Attempt's Succeeded terminal state in one transaction.
// The frozen SQL commit trigger owns the Attempt transition; this file owns
// envelope validation, boot/epoch fencing, replay idempotence, and Run
// convergence.
package inspection

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

type promqlProposal struct {
	SchemaKind      string          `json:"schemaKind"`
	AttemptID       int64           `json:"attemptId"`
	InspectionRunID int64           `json:"inspectionRunId"`
	CheckKey        string          `json:"checkKey"`
	QueryMode       string          `json:"queryMode"`
	Outcome         string          `json:"outcome"`
	ObservedAt      string          `json:"observedAt"`
	ExecutionWindow json.RawMessage `json:"executionWindow"`
	Result          json.RawMessage `json:"result"`
	Warnings        []string        `json:"warnings"`
	Errors          []string        `json:"errors"`
	GapReason       *string         `json:"gapReason"`
}

// CommitPromQLProposal atomically persists a supervisor result. A duplicate
// proposal replays only when it sealed the same immutable digest.
func (s *Service) CommitPromQLProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal promqlProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("inspection PromQL result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != "inspection_promql_result_v1" || proposal.AttemptID != attemptID ||
		proposal.InspectionRunID < 1 || proposal.CheckKey == "" ||
		(proposal.QueryMode != "instant" && proposal.QueryMode != "range") {
		return fmt.Errorf("inspection PromQL result has an invalid identity envelope")
	}
	if _, err := time.Parse(time.RFC3339Nano, proposal.ObservedAt); err != nil {
		return fmt.Errorf("inspection PromQL observedAt is not RFC3339: %w", err)
	}
	if proposal.QueryMode == "instant" && string(proposal.ExecutionWindow) != "null" {
		return fmt.Errorf("instant inspection PromQL result must have a null executionWindow")
	}
	if proposal.QueryMode == "range" {
		if string(proposal.ExecutionWindow) == "null" {
			return fmt.Errorf("range inspection PromQL result must carry its executionWindow")
		}
		var window struct {
			StartAt     string `json:"startAt"`
			EndAt       string `json:"endAt"`
			StepSeconds int64  `json:"stepSeconds"`
		}
		if err := json.Unmarshal(proposal.ExecutionWindow, &window); err != nil || window.StepSeconds < 1 {
			return fmt.Errorf("range inspection PromQL executionWindow is malformed")
		}
		if _, err := time.Parse(time.RFC3339Nano, window.StartAt); err != nil {
			return fmt.Errorf("range inspection PromQL executionWindow startAt is not RFC3339")
		}
		if _, err := time.Parse(time.RFC3339Nano, window.EndAt); err != nil {
			return fmt.Errorf("range inspection PromQL executionWindow endAt is not RFC3339")
		}
	}
	validGap := map[string]bool{"query_failed": true, "partial_response": true, "no_data": true, "cancelled": true, "interrupted": true}
	switch proposal.Outcome {
	case "success":
		if proposal.GapReason != nil || len(proposal.Errors) != 0 || string(proposal.Result) == "null" {
			return fmt.Errorf("successful inspection PromQL result has invalid shape")
		}
		if err := validatePrometheusResult(proposal.Result); err != nil {
			return err
		}
	case "error", "gap":
		if proposal.GapReason == nil || !validGap[*proposal.GapReason] || string(proposal.Result) != "null" {
			return fmt.Errorf("non-success inspection PromQL result has invalid gap shape")
		}
	default:
		return fmt.Errorf("inspection PromQL result has invalid outcome")
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

	var runID int64
	var checkKey, mode string
	err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id, a.check_key, c.query_mode
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check' AND c.kind='promql'`, attemptID).
		Scan(&runID, &checkKey, &mode)
	if err != nil {
		return err
	}
	if proposal.InspectionRunID != runID || proposal.CheckKey != checkKey || proposal.QueryMode != mode {
		return fmt.Errorf("inspection PromQL result identity does not match frozen attempt")
	}
	digest := sha256.Sum256(raw)
	var existing []byte
	replayErr := conn.QueryRowContext(ctx, `
		SELECT result_digest FROM inspection_check_results WHERE run_id=? AND check_key=?`, runID, checkKey).Scan(&existing)
	if replayErr == nil {
		// Idempotent replay adjudicates before any liveness fence: a committed
		// result can never be overwritten, only acknowledged.
		if string(existing) == string(digest[:]) {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("inspection PromQL result replay digest conflicts")
	}
	if replayErr != sql.ErrNoRows {
		return replayErr
	}
	// Boot/epoch/cancel fence: the frozen commit trigger performs the terminal
	// transition, so enforce the dispatch binding here as the guard.
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
		params, _ := json.Marshal(map[string]string{"check_key": checkKey})
		warnings, _ := json.Marshal(proposal.Warnings)
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,warnings_json,integrity,created_at)
			VALUES(?,'inspection_run',?,?,?,?,?,'complete',?)`,
			attemptID, runID, string(params), proposal.ObservedAt, string(proposal.Result), string(warnings), s.nowText())
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

// validatePrometheusResult enforces the closed result payload of the frozen
// schema: one of vector/matrix/scalar/string with non-empty typed samples.
func validatePrometheusResult(raw json.RawMessage) error {
	var payload struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("inspection PromQL result payload is malformed: %w", err)
	}
	sample := func(value json.RawMessage) error {
		var pair []json.RawMessage
		if err := json.Unmarshal(value, &pair); err != nil || len(pair) != 2 || len(pair[1]) == 0 {
			return fmt.Errorf("inspection PromQL sample must be a [timestamp, value] pair")
		}
		return nil
	}
	switch payload.ResultType {
	case "vector":
		var series []struct {
			Metric map[string]string `json:"metric"`
			Value  json.RawMessage   `json:"value"`
		}
		if err := json.Unmarshal(payload.Result, &series); err != nil || len(series) == 0 {
			return fmt.Errorf("vector inspection PromQL result must be a non-empty series array")
		}
		for _, item := range series {
			if err := sample(item.Value); err != nil {
				return err
			}
		}
	case "matrix":
		var series []struct {
			Metric map[string]string `json:"metric"`
			Values []json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(payload.Result, &series); err != nil || len(series) == 0 {
			return fmt.Errorf("matrix inspection PromQL result must be a non-empty series array")
		}
		for _, item := range series {
			if len(item.Values) == 0 {
				return fmt.Errorf("matrix inspection PromQL series must carry samples")
			}
			for _, value := range item.Values {
				if err := sample(value); err != nil {
					return err
				}
			}
		}
	case "scalar", "string":
		if err := sample(payload.Result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("inspection PromQL result has a closed resultType")
	}
	return nil
}
