package execution

// ErrNoTransition contract tests (main-constrained scope): the sentinel is a
// transient, Execute-only condition. A partial business write returned
// together with the sentinel must never commit; ledger commands (Run) must
// reject it instead of going silent, because their unchanged outcomes are
// declared through the typed Change classification and stay durably
// recorded. Authorization always runs before the business stage, so a
// rejected principal can never reach the sentinel path.

import (
	"errors"
	"strings"
	"testing"
)

// TestExecuteNoTransitionDiscardsPartialWritesAndRecordsNothing proves the
// central no-transition condition: the business stage writes and THEN
// returns the sentinel (the contract forbids this, the runner must still be
// safe), the savepoint rollback discards the write, no audit row is
// persisted, and the sentinel surfaces to the caller.
func TestExecuteNoTransitionDiscardsPartialWritesAndRecordsNothing(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-no-transition")

	_, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('must-not-survive')`); execErr != nil {
			return itemResult{}, execErr
		}
		return itemResult{}, ErrNoTransition
	}, func(itemResult) int64 { return 0 })
	if !errors.Is(err, ErrNoTransition) {
		t.Fatalf("err=%v, want ErrNoTransition", err)
	}
	if countRows(t, db, "quoin_items") != 0 {
		t.Fatalf("sentinel committed a partial business write: %d rows", countRows(t, db, "quoin_items"))
	}
	if countRows(t, db, "audit_events") != 0 {
		t.Fatalf("no-transition execution recorded %d audit rows, want none", countRows(t, db, "audit_events"))
	}
	if countRows(t, db, "client_commands") != 0 {
		t.Fatalf("no-transition execution recorded %d ledger rows, want none", countRows(t, db, "client_commands"))
	}
}

// TestRunRejectsNoTransitionInsteadOfGoingSilent pins the ledger-command
// boundary: Run must never treat the sentinel as a silent no-op — the
// durable unchanged classification belongs to the typed Change channel.
func TestRunRejectsNoTransitionInsteadOfGoingSilent(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-run-no-transition")

	_, err := Run(ctx, runner, op, userCommand("cmd-no-transition"), func(tx *Tx) (itemResult, Change, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); execErr != nil {
			return itemResult{}, Changed, execErr
		}
		return itemResult{}, Unchanged, ErrNoTransition
	}, func(itemResult) int64 { return 0 })
	if err == nil || !strings.Contains(err.Error(), "not supported by ledger commands") {
		t.Fatalf("err=%v, want ledger-command no-transition prohibition", err)
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatalf("rejected no-transition left rows: items=%d ledger=%d audit=%d",
			countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"), countRows(t, db, "audit_events"))
	}
}
