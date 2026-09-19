package inspection

// PromQL run_check technical gap settlement: the frozen closure admits no
// check result without a Running attempt, so a terminally lost child makes
// the Run itself Interrupted once no active child remains.

import (
	"context"
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
