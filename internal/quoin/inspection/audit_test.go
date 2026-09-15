package inspection

// Automatic audit contracts for the migrated inspection commands (ADR-0006 /
// docs/audit-design.md): audit failure rolls the whole command back, replays
// never duplicate success events, and Runtime result commits land on the
// original operation correlation restored from the attempt row.

import (
	"context"
	"errors"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestAuditFailureRollsBackRunCreation proves the automatic audit is not
// advisory: when the audit_events INSERT fails inside the runner transaction,
// the business stage rolls back completely — no Run, no child attempts, no
// command ledger row, no partial audit state.
func TestAuditFailureRollsBackRunCreation(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	if _, err := h.db.Exec(`CREATE TRIGGER trg_test_audit_down BEFORE INSERT ON audit_events
		BEGIN SELECT RAISE(ABORT, 'audit storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := commandContext(t)
	if _, err := h.service.CreatePlanRun(ctx, h.principal, "audit-down-0001", "mixed-plan"); err == nil {
		t.Fatal("audit storage failure must fail the command closed")
	}
	// The harness seed also writes through migrated runners (connection
	// creation and its probe lifecycle), so assert the absence of THIS
	// command's durable traces specifically.
	if countLedgerRows(t, h, CommandCreateRun) != 0 {
		t.Fatal("audit failure must leave no inspection_run.create ledger row")
	}
	if countAuditEvents(t, h, CommandCreateRun) != 0 {
		t.Fatal("audit failure must leave no inspection_run.create audit event")
	}
	for name, query := range map[string]string{
		"runs":     `SELECT COUNT(*) FROM inspection_runs`,
		"children": `SELECT COUNT(*) FROM execution_attempts WHERE scope_type='run_check'`,
	} {
		var count int
		if err := h.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("audit failure must leave no durable %s trace, got %d", name, count)
		}
	}
}

// TestCommandReplayRecordsNoDuplicateAudit proves the ledger replay path
// returns the stored result without writing a second success event: the audit
// and ledger row counts are identical before and after the replay.
func TestCommandReplayRecordsNoDuplicateAudit(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	ctx := commandContext(t)
	first, err := h.service.CreatePlanRun(ctx, h.principal, "replay-audit-0001", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	auditCount := countAuditEvents(t, h, CommandCreateRun)
	ledgerCount := countLedgerRows(t, h, CommandCreateRun)
	if auditCount == 0 || ledgerCount == 0 {
		t.Fatalf("first execution must record audit and ledger, audit=%d ledger=%d", auditCount, ledgerCount)
	}
	replayed, err := h.service.CreatePlanRun(ctx, h.principal, "replay-audit-0001", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.RunID != first.RunID {
		t.Fatalf("replay must return the stored run %d, got %d", first.RunID, replayed.RunID)
	}
	if after := countAuditEvents(t, h, CommandCreateRun); after != auditCount {
		t.Fatalf("replay must not duplicate success audit: %d -> %d", auditCount, after)
	}
	if after := countLedgerRows(t, h, CommandCreateRun); after != ledgerCount {
		t.Fatalf("replay must not add ledger rows: %d -> %d", ledgerCount, after)
	}
}

// TestResultCommitLandsOnOriginalCorrelation walks the restore path: the
// Runtime delivers its result on a plain scope, the module restores the system
// task context from the attempt's persisted correlation, and the commit audit
// cites the original correlation with the user initiator and the system actor.
// An identical redelivery records nothing new.
func TestResultCommitLandsOnOriginalCorrelation(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	ctx := commandContext(t)
	detail, err := h.service.CreatePlanRun(ctx, h.principal, "commit-corr-0001", "mixed-plan")
	if err != nil {
		t.Fatal(err)
	}
	attemptID := h.promqlAttemptID(t, detail.RunID)
	h.dispatchPromQL(t, attemptID)
	body := pluginSuccessProposal(t, h, attemptID, detail.RunID, "success")

	// The runtime scope carries no execution metadata: the commit restores it
	// from the attempt's persisted correlation.
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatal(err)
	}
	var correlationID, initiatorType string
	var initiatorID int64
	if err := h.db.QueryRow(`
		SELECT correlation_id, initiator_type, initiator_id FROM audit_events WHERE action=?`,
		commandPluginResult).Scan(&correlationID, &initiatorType, &initiatorID); err != nil {
		t.Fatal(err)
	}
	var attemptCorrelation string
	if err := h.db.QueryRow(`SELECT operation_correlation_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptCorrelation); err != nil {
		t.Fatal(err)
	}
	if correlationID == "" || correlationID != attemptCorrelation {
		t.Fatalf("result commit audit correlation %q must equal the creating operation %q", correlationID, attemptCorrelation)
	}
	if initiatorType != "user" || initiatorID != h.principal {
		t.Fatalf("result commit must preserve the original initiator, got %s/%d", initiatorType, initiatorID)
	}
	committed := countAuditEvents(t, h, commandPluginResult)
	// An identical redelivery replays silently: no second success event.
	if err := h.service.CommitPluginProposal(context.Background(), attemptID, "plinth-boot", 1, body); err != nil {
		t.Fatalf("identical result replay must be idempotent: %v", err)
	}
	if after := countAuditEvents(t, h, commandPluginResult); after != committed {
		t.Fatalf("result replay must not duplicate audit: %d -> %d", committed, after)
	}
}

// TestMissingCommandContextFailsClosed proves a user command without execution
// metadata is refused instead of guessing an identity, and leaves no trace.
func TestMissingCommandContextFailsClosed(t *testing.T) {
	h := newTestHarness(t)
	h.seedPlan(t, "mixed-plan")
	if _, err := h.service.CreatePlanRun(context.Background(), h.principal, "no-context-0001", "mixed-plan"); !errors.Is(err, execution.ErrMissingContext) {
		t.Fatalf("missing execution context must fail closed, got %v", err)
	}
	var runs int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM inspection_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("refused command must leave no runs, got %d", runs)
	}
}

func countAuditEvents(t *testing.T, h *testHarness, action string) int {
	t.Helper()
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countLedgerRows(t *testing.T, h *testHarness, command string) int {
	t.Helper()
	var count int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type=?`, command).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
