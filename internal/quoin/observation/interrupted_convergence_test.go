package observation

// Recovery convergence coverage: a child the runtime terminalized outside a
// result proposal (loss interruption or accepted cancellation) must record an
// honest gap on its object row and converge the Run in one audited
// transaction — the stuck-Running-forever failure mode of the 2026-09-16
// mall-shop incident.

import (
	"context"
	"testing"
)

func TestConvergeInterruptedChildRecordsGapAndClosesRun(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "converge-interrupted", "enablement", nil)
	attemptID := h.singleChildAttemptID(t, run.ID)

	// Loss path: the child was dispatched and Running when its lease expired;
	// the sweep terminalized it outside any result proposal.
	h.bindChildToRunning(t, attemptID)
	attempts := h.service.Attempts()
	if _, err := attempts.Interrupt(context.Background(), attemptID, "lease_expired"); err != nil {
		t.Fatalf("interrupt observation child: %v", err)
	}
	if err := h.service.ConvergeInterruptedChild(context.Background(), attemptID, "interrupted"); err != nil {
		t.Fatalf("converge interrupted child: %v", err)
	}

	var runState string
	if err := h.db.QueryRow(`SELECT state FROM observation_runs WHERE id=?`, parseID(t, run.ID)).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != "CompletedWithWarnings" {
		t.Fatalf("run state %s, want CompletedWithWarnings with an honest interrupted gap", runState)
	}
	var status, gapReason string
	if err := h.db.QueryRow(`SELECT status,gap_reason FROM observation_run_objects WHERE attempt_id=?`, attemptID).Scan(&status, &gapReason); err != nil {
		t.Fatal(err)
	}
	if status != "gap" || gapReason != "interrupted" {
		t.Fatalf("object row is %s/%s, want gap/interrupted", status, gapReason)
	}

	// A repeat is the proven no-op of the sealed object row and terminal Run.
	if err := h.service.ConvergeInterruptedChild(context.Background(), attemptID, "interrupted"); err != nil {
		t.Fatalf("repeat convergence must be an idempotent no-op: %v", err)
	}
}

func TestConvergeInterruptedChildRejectsUnknownGapReason(t *testing.T) {
	h := newHarness(t, "prometheus")
	run := h.startRun(t, "converge-invalid-gap", "enablement", nil)
	attemptID := h.singleChildAttemptID(t, run.ID)
	if err := h.service.ConvergeInterruptedChild(context.Background(), attemptID, "made_up_reason"); err == nil {
		t.Fatal("unknown gap reason must fail closed, not fabricate an unprojectable fact")
	}
}

// singleChildAttemptID returns the one discovery child of a Run.
func (h *harness) singleChildAttemptID(t *testing.T, runID string) int64 {
	t.Helper()
	var attemptID int64
	if err := h.db.QueryRow(`SELECT id FROM execution_attempts WHERE scope_type='observation_run' AND scope_id=?`, parseID(t, runID)).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	return attemptID
}
