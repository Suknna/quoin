package inspection

// PromQL run_check technical gap settlement: the frozen closure admits no
// check result without a Running attempt, so a terminally lost child makes
// the Run itself Interrupted once no active child remains. The local
// executor's input-rebuild convergence (ConvergeCollectionGap) records the
// runtime_unavailable technical gap the frozen closure admits for unbound
// Failed children instead.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// RecordPromQLTechnicalGap closes a terminally lost run_check PromQL child:
// the frozen closure admits no check result without a Running attempt, so the
// Run itself becomes Interrupted once no active child remains. The stage runs
// through the shared execution runner (ADR-0006) under the attempt's restored
// system task scope; a no-transition miss (an active child remains) rolls
// back and records nothing.
func (s *Service) RecordPromQLTechnicalGap(ctx context.Context, attemptID int64, reason string) error {
	_ = reason
	scope, err := s.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(scope, s.runner, s.promqlGap,
		func(tx *execution.Tx) (int64, error) {
			var runID int64
			var state string
			if err := tx.QueryRowContext(ctx, `
		SELECT a.scope_id, a.state FROM execution_attempts a
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
				Scan(&runID, &state); err != nil {
				return 0, err
			}
			if state != "Failed" && state != "Cancelled" && state != "Interrupted" {
				return 0, fmt.Errorf("attempt %d is not terminal", attemptID)
			}
			result, err := tx.ExecContext(ctx, `
		UPDATE inspection_runs SET state='Interrupted', row_version=row_version+1
		WHERE id=? AND state='Running' AND NOT EXISTS (
			SELECT 1 FROM execution_attempts WHERE scope_type='run_check' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling'))`,
				runID, runID)
			if err != nil {
				return 0, err
			}
			if affected, _ := result.RowsAffected(); affected == 0 {
				return 0, fmt.Errorf("%w: run %d still has active run_check children", execution.ErrNoTransition, runID)
			}
			return runID, nil
		},
		func(runID int64) int64 { return runID })
	return err
}

// ConvergeCollectionGap terminates one permanently unexecutable Queued
// collection child of the local executor (ADR-0011): a frozen input that can
// no longer be rebuilt (plugin/template drift, missing grant) will never
// become executable, so the child closes as an unbound Failed attempt with
// the runtime_unavailable technical gap the frozen closure admits, and the
// Run converges instead of retrying the same failure every scan. The
// convergence runs as an audited system operation; an already-dispatched or
// terminal child is a proven no-op.
func (s *Service) ConvergeCollectionGap(ctx context.Context, attemptID int64) error {
	scope, err := s.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	var runID int64
	_, err = execution.Execute(scope, s.runner, s.collectionGap, func(tx *execution.Tx) (struct{}, error) {
		var state string
		err := tx.QueryRowContext(scope, `
			SELECT a.scope_id, a.state, a.check_key FROM execution_attempts a
			WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
			Scan(&runID, &state)
		if errors.Is(err, sql.ErrNoRows) {
			// Not a run_check child (or already swept): nothing to converge.
			runID = 0
			return struct{}{}, nil
		}
		if err != nil {
			return struct{}{}, err
		}
		if state != "Queued" {
			// Already claimed by an executor or terminal: the winner owns the
			// closure, and a replayed convergence records nothing.
			runID = 0
			return struct{}{}, nil
		}
		var checkKey string
		if err := tx.QueryRowContext(scope, `SELECT check_key FROM execution_attempts WHERE id=?`, attemptID).Scan(&checkKey); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(scope, `
			UPDATE execution_attempts SET state='Failed',ended_at=? WHERE id=? AND state='Queued'`, s.nowText(), attemptID); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(scope, `
			INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
			VALUES(?,?,'gap',NULL,?,NULL,'runtime_unavailable',?)`, runID, checkKey, attemptID, s.nowText()); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, s.convergeOn(scope, tx, runID)
	}, func(struct{}) int64 { return runID })
	return err
}
