package execution

// The runner tests execute against the applied contract schema
// (internal/gen/contracts/schema.sql) plus one private business table, so the
// coordinated correlation extension and its CHECK constraints are exercised
// exactly as main applied them.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/audit"
	_ "modernc.org/sqlite"
)

const businessTableSchema = `
CREATE TABLE quoin_items (
  id   INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
  name TEXT NOT NULL
) STRICT;
`

type itemResult struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "quoin.db")+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(businessTableSchema); err != nil {
		t.Fatal(err)
	}
	return db
}

// newTestRunner registers item.create with an authorization callback the test
// can flip to simulate a principal being revoked after a first execution.
func newTestRunner(t *testing.T, db *sql.DB) (*Runner, *Operation, *bool) {
	t.Helper()
	allowed := true
	registry := NewRegistry()
	op, err := registry.Register(Operation{
		Name:       "item.create",
		Class:      ClassWrite,
		ObjectType: "quoin_item",
		Authorize: func(ctx context.Context, tx *Tx) error {
			if !allowed {
				return errors.New("principal revoked")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewRunner(db, registry, audit.NewWriter()), op, &allowed
}

func metadataContext(t *testing.T, correlation string) context.Context {
	t.Helper()
	return metadataContextWithActor(t, correlation, Principal{Kind: PrincipalUser, ID: 7})
}

func metadataContextWithActor(t *testing.T, correlation string, actor Principal) context.Context {
	t.Helper()
	ctx, err := WithMetadata(context.Background(), Metadata{
		CorrelationID: correlation,
		Actor:         actor,
		Source:        Source{Kind: SourceHTTP, RequestID: "req-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func userCommand(id string) Command {
	return Command{PrincipalType: "user", PrincipalID: 7, ClientCommandID: id, Digest: strings.Repeat("a", 64)}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestRunPersistsBusinessLedgerAndAuditWithoutManualAuditCalls(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-first")

	outcome, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		// Business code writes only its own row; no audit call anywhere.
		result, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('alpha')`)
		if err != nil {
			return itemResult{}, Changed, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return itemResult{}, Changed, err
		}
		return itemResult{ID: id, Name: "alpha"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Replayed || outcome.Change != Changed || outcome.CorrelationID != "corr-first" {
		t.Fatalf("outcome=%+v, want first execution with current correlation", outcome)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM quoin_items WHERE id=?`, outcome.Result.ID).Scan(&name); err != nil || name != "alpha" {
		t.Fatalf("business row name=%q err=%v, want alpha", name, err)
	}

	var actorType, action, auditOutcome, phase, correlation, request, commandID, refType string
	var actorID, initiatorID, refID int64
	var initiatorType sql.NullString
	if err := db.QueryRow(`SELECT actor_type,actor_id,action,outcome,phase,correlation_id,request_id,client_command_id,COALESCE(initiator_type,''),initiator_id,domain_ref_type,domain_ref_id FROM audit_events`).
		Scan(&actorType, &actorID, &action, &auditOutcome, &phase, &correlation, &request, &commandID, &initiatorType, &initiatorID, &refType, &refID); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != 7 || action != "item.create" || auditOutcome != "success" {
		t.Fatalf("audit actor=%s/%d action=%s outcome=%s", actorType, actorID, action, auditOutcome)
	}
	if phase != "execute" || correlation != "corr-first" || request != "req-1" || commandID != "cmd-1" {
		t.Fatalf("audit correlation fields phase=%s corr=%s req=%s cmd=%s", phase, correlation, request, commandID)
	}
	if initiatorType.String != "user" || initiatorID != 7 || refType != "quoin_item" || refID != outcome.Result.ID {
		t.Fatalf("audit initiator=%s/%d ref=%s/%d", initiatorType.String, initiatorID, refType, refID)
	}
	if countRows(t, db, "audit_events") != 1 || countRows(t, db, "audit_event_targets") != 1 {
		t.Fatalf("audit rows=%d targets=%d, want exactly one each", countRows(t, db, "audit_events"), countRows(t, db, "audit_event_targets"))
	}

	var ledgerOutcome, ledgerCorrelation, payload string
	if err := db.QueryRow(`SELECT outcome,COALESCE(correlation_id,''),result_payload_json FROM client_commands`).
		Scan(&ledgerOutcome, &ledgerCorrelation, &payload); err != nil {
		t.Fatal(err)
	}
	if ledgerOutcome != LedgerCommitted || ledgerCorrelation != "corr-first" || !strings.Contains(payload, `"name":"alpha"`) {
		t.Fatalf("ledger outcome=%s corr=%s payload=%s", ledgerOutcome, ledgerCorrelation, payload)
	}
}

func TestDeterministicRejectionRollsBackBusinessWritesAndReplaysStoredRejection(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-reject")

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); err != nil {
			return itemResult{}, Changed, err
		}
		// A write already happened; the deterministic rejection must not
		// carry it into the commit.
		return itemResult{}, Changed, &Rejection{Code: "validation_failed", Detail: "name not allowed"}
	}, func(itemResult) int64 { return 0 })
	if err == nil {
		t.Fatal("want rejection error")
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Code != "validation_failed" {
		t.Fatalf("err=%v, want *Rejection validation_failed", err)
	}
	if countRows(t, db, "quoin_items") != 0 {
		t.Fatalf("rejection committed %d business rows, want clean savepoint rollback", countRows(t, db, "quoin_items"))
	}
	var outcome, payload string
	if err := db.QueryRow(`SELECT outcome,result_payload_json FROM client_commands`).Scan(&outcome, &payload); err != nil {
		t.Fatal(err)
	}
	if outcome != LedgerRejectedKnown || !strings.Contains(payload, "validation_failed") {
		t.Fatalf("ledger outcome=%s payload=%s, want rejected_known with code", outcome, payload)
	}
	var auditOutcome string
	if err := db.QueryRow(`SELECT outcome FROM audit_events`).Scan(&auditOutcome); err != nil || auditOutcome != "rejected" {
		t.Fatalf("audit outcome=%s err=%v, want rejected", auditOutcome, err)
	}

	// Replay returns the stored rejection with the original correlation and
	// never re-runs the business stage.
	ran := false
	replayOutcome, replayErr := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		ran = true
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 })
	if ran {
		t.Fatal("business stage ran on replay")
	}
	if !errors.As(replayErr, &rejection) || rejection.Code != "validation_failed" {
		t.Fatalf("replay err=%v, want stored rejection", replayErr)
	}
	if !replayOutcome.Replayed || replayOutcome.CorrelationID != "corr-reject" {
		t.Fatalf("replay outcome=%+v, want original correlation", replayOutcome)
	}
	if countRows(t, db, "audit_events") != 1 || countRows(t, db, "client_commands") != 1 {
		t.Fatalf("replay produced new rows: audit=%d ledger=%d", countRows(t, db, "audit_events"), countRows(t, db, "client_commands"))
	}
}

func TestAuditFailureRollsBackBusinessWrites(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	if _, err := db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events WHEN NEW.action='item.create' BEGIN SELECT RAISE(ABORT,'audit denied'); END;`); err != nil {
		t.Fatal(err)
	}
	ctx := metadataContext(t, "corr-audit-fail")

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); err != nil {
			return itemResult{}, Changed, err
		}
		return itemResult{Name: "ghost"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err == nil {
		t.Fatal("want audit failure")
	}
	// Audit failure is an infrastructure failure: full rollback, no durable
	// trace, and the command key stays free for a retry.
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatalf("audit failure left rows: items=%d ledger=%d audit=%d", countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"), countRows(t, db, "audit_events"))
	}
}

func TestReplayReturnsStoredResultWithoutNewAuditAndRejectsDigestConflict(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-replay")

	first, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		result, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('alpha')`)
		if err != nil {
			return itemResult{}, Changed, err
		}
		id, _ := result.LastInsertId()
		return itemResult{ID: id, Name: "alpha"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	replayed, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		ran = true
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 })
	if ran {
		t.Fatal("business stage ran on replay")
	}
	if err != nil || !replayed.Replayed || replayed.Result != first.Result {
		t.Fatalf("replay=%+v err=%v, want stored result", replayed, err)
	}
	if replayed.CorrelationID != "corr-replay" {
		t.Fatalf("replay correlation=%q, want original", replayed.CorrelationID)
	}
	if countRows(t, db, "audit_events") != 1 || countRows(t, db, "client_commands") != 1 {
		t.Fatalf("replay produced new rows: audit=%d ledger=%d", countRows(t, db, "audit_events"), countRows(t, db, "client_commands"))
	}

	conflict := userCommand("cmd-1")
	conflict.Digest = strings.Repeat("b", 64)
	if _, err := Run(ctx, runner, op, conflict, func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("digest conflict err=%v, want ErrCommandReused", err)
	}
	if countRows(t, db, "audit_events") != 1 || countRows(t, db, "client_commands") != 1 {
		t.Fatalf("conflict produced new rows: audit=%d ledger=%d", countRows(t, db, "audit_events"), countRows(t, db, "client_commands"))
	}
}

func TestExecuteRecordedFailureCommitsStateAndAuditsFailure(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-otp")

	// A failed OTP attempt: the failure counter is an intended state change
	// that must commit, and the attempt must be audited as a failure.
	_, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('failure-counter-1')`); execErr != nil {
			return itemResult{}, execErr
		}
		return itemResult{}, &RecordedFailure{Code: "otp_invalid", Detail: "code does not match"}
	}, func(itemResult) int64 { return 0 })
	var failure *RecordedFailure
	if !errors.As(err, &failure) || failure.Code != "otp_invalid" {
		t.Fatalf("err=%v, want *RecordedFailure otp_invalid", err)
	}
	// The intended counter state committed.
	var counters int
	if err := db.QueryRow(`SELECT COUNT(*) FROM quoin_items WHERE name='failure-counter-1'`).Scan(&counters); err != nil || counters != 1 {
		t.Fatalf("counters=%d err=%v, want the recorded-failure state committed", counters, err)
	}
	// The attempt was audited as a failure without ledger rows and without
	// the non-secret detail entering any free-form column.
	var action, auditOutcome, commandID, storedText string
	if err := db.QueryRow(`SELECT action,outcome,COALESCE(client_command_id,''),action||COALESCE(correlation_id,'') FROM audit_events`).
		Scan(&action, &auditOutcome, &commandID, &storedText); err != nil {
		t.Fatal(err)
	}
	if auditOutcome != audit.OutcomeFailure || action != "item.create" || commandID != "" {
		t.Fatalf("audit action=%s outcome=%s command=%q, want failure without command id", action, auditOutcome, commandID)
	}
	if strings.Contains(storedText, "code does not match") {
		t.Fatal("failure detail must not be persisted in audit columns")
	}
	if countRows(t, db, "client_commands") != 0 {
		t.Fatalf("recorded failure wrote %d ledger rows", countRows(t, db, "client_commands"))
	}

	// Every failed attempt is a real execution: the second attempt commits
	// its own counter and its own failure audit.
	_, err = Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('failure-counter-2')`); execErr != nil {
			return itemResult{}, execErr
		}
		return itemResult{}, &RecordedFailure{Code: "otp_invalid", Detail: "code does not match"}
	}, func(itemResult) int64 { return 0 })
	if !errors.As(err, &failure) {
		t.Fatalf("second attempt err=%v, want *RecordedFailure", err)
	}
	if countRows(t, db, "quoin_items") != 2 || countRows(t, db, "audit_events") != 2 {
		t.Fatalf("items=%d audits=%d, want two recorded attempts", countRows(t, db, "quoin_items"), countRows(t, db, "audit_events"))
	}
}

func TestExecuteRecordedUnknownOutcomeCommitsStateAndAuditsUnknown(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-unknown")

	// An external delivery whose provider status was never confirmed: the
	// delivery status row is intended state that commits, and the audit
	// records outcome unknown — never success, never a definite failure.
	_, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('delivery-status')`); execErr != nil {
			return itemResult{}, execErr
		}
		return itemResult{}, &RecordedFailure{Code: "delivery_unknown", Outcome: audit.OutcomeUnknown, Detail: "provider status unavailable"}
	}, func(itemResult) int64 { return 0 })
	var failure *RecordedFailure
	if !errors.As(err, &failure) || failure.Code != "delivery_unknown" {
		t.Fatalf("err=%v, want *RecordedFailure delivery_unknown", err)
	}
	if countRows(t, db, "quoin_items") != 1 {
		t.Fatalf("items=%d, want the delivery status committed", countRows(t, db, "quoin_items"))
	}
	var auditOutcome string
	if err := db.QueryRow(`SELECT outcome FROM audit_events`).Scan(&auditOutcome); err != nil {
		t.Fatal(err)
	}
	if auditOutcome != audit.OutcomeUnknown {
		t.Fatalf("audit outcome=%s, want unknown", auditOutcome)
	}
	if countRows(t, db, "client_commands") != 0 {
		t.Fatal("recorded unknown wrote ledger rows")
	}

	// A forged success outcome through the recorded-attempt path is
	// rejected and rolls the attempt back entirely.
	_, err = Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); execErr != nil {
			return itemResult{}, execErr
		}
		return itemResult{}, &RecordedFailure{Code: "otp_invalid", Outcome: audit.OutcomeSuccess}
	}, func(itemResult) int64 { return 0 })
	if err == nil || !strings.Contains(err.Error(), "must be empty, failure or unknown") {
		t.Fatalf("err=%v, want forged-success rejection", err)
	}
	if countRows(t, db, "quoin_items") != 1 || countRows(t, db, "audit_events") != 1 {
		t.Fatalf("forged outcome left rows: items=%d audits=%d", countRows(t, db, "quoin_items"), countRows(t, db, "audit_events"))
	}
}

func TestRunForbidsRecordedFailure(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-forbid")

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); execErr != nil {
			return itemResult{}, Changed, execErr
		}
		return itemResult{}, Changed, &RecordedFailure{Code: "otp_invalid", Detail: "code does not match"}
	}, func(itemResult) int64 { return 0 })
	if err == nil || !strings.Contains(err.Error(), "not supported by ledger commands") {
		t.Fatalf("err=%v, want ledger-command recorded-failure prohibition", err)
	}
	var rejection *Rejection
	if errors.As(err, &rejection) {
		t.Fatal("recorded failure must not surface as a rejection")
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatalf("forbidden recorded failure left rows: items=%d ledger=%d audit=%d", countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"), countRows(t, db, "audit_events"))
	}
}

func TestTxGuardBlocksCommentAndMultiStatementBypasses(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		blocked bool
	}{
		{"plain select", `SELECT COUNT(*) FROM quoin_items`, false},
		{"common table expression", `WITH recent AS (SELECT id FROM quoin_items) SELECT id FROM recent`, false},
		{"replace statement", `REPLACE INTO quoin_items(id,name) VALUES(1,'ok')`, false},
		{"delete statement", `DELETE FROM quoin_items WHERE name='ok'`, false},
		{"trailing semicolon", `INSERT INTO quoin_items(name) VALUES('ok');`, false},
		{"comment masked commit", "/*x*/ COMMIT", true},
		{"comment masked begin", "-- setup\nBEGIN IMMEDIATE", true},
		{"second statement commit", `SELECT 1; COMMIT`, true},
		{"second statement rollback", `INSERT INTO quoin_items(name) VALUES('x'); ROLLBACK;`, true},
		{"bare savepoint", `SAVEPOINT sneaky`, true},
		{"release savepoint", `RELEASE SAVEPOINT sneaky`, true},
		{"end transaction", `END`, true},
		{"keyword prefix boundary", `SELECT committed_at FROM quoin_items LIMIT 1`, false},
		{"semicolon inside string literal", `INSERT INTO quoin_items(name) VALUES('a;b')`, false},
		{"unterminated string", `INSERT INTO quoin_items(name) VALUES('open`, true},
		{"unterminated comment", `/* never closed COMMIT`, true},
		{"pragma writable schema", `PRAGMA writable_schema=1`, true},
		{"pragma table info", `PRAGMA table_info(quoin_items)`, true},
		{"attach side database", `ATTACH DATABASE 'file:/tmp/x.db' AS side`, true},
		{"detach side database", `DETACH DATABASE side`, true},
		{"drop trigger", `DROP TRIGGER trg_client_commands_no_update`, true},
		{"create trigger", `CREATE TRIGGER sneaky BEFORE INSERT ON quoin_items BEGIN SELECT 1; END`, true},
		{"alter table", `ALTER TABLE quoin_items ADD COLUMN x TEXT`, true},
		{"vacuum", `VACUUM`, true},
		{"explain disguised write", `EXPLAIN INSERT INTO quoin_items(name) VALUES('x')`, true},
		{"keyword split by comment stays rejected", `COM/*x*/MIT`, true},
	}
	for _, testCase := range cases {
		err := guardStatement(testCase.query)
		if testCase.blocked && err == nil {
			t.Fatalf("%s: query %q passed the guard, want rejection", testCase.name, testCase.query)
		}
		if !testCase.blocked && err != nil {
			t.Fatalf("%s: query %q rejected: %v", testCase.name, testCase.query, err)
		}
	}

	// Through a live transaction, a comment-masked COMMIT must fail the
	// business stage without any durable trace.
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-bypass")

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); execErr != nil {
			return itemResult{}, Changed, execErr
		}
		if _, execErr := tx.ExecContext(ctx, "/*x*/ COMMIT"); execErr != nil {
			return itemResult{}, Changed, execErr
		}
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 })
	if err == nil || !strings.Contains(err.Error(), "may run inside the runner transaction") {
		t.Fatalf("err=%v, want statement whitelist guard", err)
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatal("comment-masked commit path persisted rows")
	}
}

func TestTxQueryRowContextCommitAttemptIsBlockedSafely(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-queryrow")

	// QueryRowContext cannot return an error, so a rejected statement must
	// surface as a guaranteed Scan failure while the command itself
	// continues safely — no commit, no partial statement.
	outcome, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		var sentinel int
		if scanErr := tx.QueryRowContext(ctx, "COMMIT").Scan(&sentinel); scanErr == nil {
			return itemResult{}, Changed, errors.New("guarded COMMIT row scanned successfully")
		}
		result, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('alpha')`)
		if execErr != nil {
			return itemResult{}, Changed, execErr
		}
		id, _ := result.LastInsertId()
		return itemResult{ID: id, Name: "alpha"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatalf("run err=%v, want the guarded attempt handled inside business", err)
	}
	if outcome.Result.Name != "alpha" {
		t.Fatalf("outcome=%+v", outcome)
	}
	if countRows(t, db, "quoin_items") != 1 || countRows(t, db, "client_commands") != 1 || countRows(t, db, "audit_events") != 1 {
		t.Fatalf("rows items=%d ledger=%d audit=%d, want the clean command committed", countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"), countRows(t, db, "audit_events"))
	}
}

func TestTxHandleExpiresAfterTransactionEnd(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-expiry")

	var retained *Tx
	if _, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		retained = tx
		result, execErr := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('alpha')`)
		if execErr != nil {
			return itemResult{}, Changed, execErr
		}
		id, _ := result.LastInsertId()
		return itemResult{ID: id, Name: "alpha"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID }); err != nil {
		t.Fatal(err)
	}

	// After the runner ended the transaction the retained handle is dead:
	// it must never check the pooled connection back out.
	if retained.Active() {
		t.Fatal("handle still active after transaction end")
	}
	if _, err := retained.ExecContext(context.Background(), `INSERT INTO quoin_items(name) VALUES('leak')`); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("exec err=%v, want expired handle rejection", err)
	}
	if _, err := retained.QueryContext(context.Background(), `SELECT 1`); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("query err=%v, want expired handle rejection", err)
	}
	var sentinel int
	scanErr := retained.QueryRowContext(context.Background(), `SELECT 1`).Scan(&sentinel)
	if scanErr == nil || !errors.Is(scanErr, sql.ErrConnDone) && !errors.Is(scanErr, context.Canceled) {
		t.Fatalf("expired QueryRowContext scan err=%v, want a guaranteed failure (ErrConnDone or canceled context) without touching the database", scanErr)
	}
	if countRows(t, db, "quoin_items") != 1 {
		t.Fatalf("items=%d, want only the committed row", countRows(t, db, "quoin_items"))
	}
}

func TestUnchangedCommandIsRecordedAndReplaysUnchanged(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-noop")

	outcome, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		// Semantic no-op: no business write, still durably recorded.
		return itemResult{ID: 3, Name: "same"}, Unchanged, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Change != Unchanged {
		t.Fatalf("change=%v, want Unchanged", outcome.Change)
	}
	var ledgerOutcome, payload string
	if err := db.QueryRow(`SELECT outcome,result_payload_json FROM client_commands`).Scan(&ledgerOutcome, &payload); err != nil {
		t.Fatal(err)
	}
	if ledgerOutcome != LedgerCommitted || !strings.Contains(payload, classUnchanged) {
		t.Fatalf("ledger outcome=%s payload=%s, want committed unchanged classification", ledgerOutcome, payload)
	}
	if countRows(t, db, "audit_events") != 1 {
		t.Fatalf("audit rows=%d, want the no-op audited", countRows(t, db, "audit_events"))
	}

	replayed, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, errors.New("must not run")
	}, func(itemResult) int64 { return 0 })
	if err != nil || !replayed.Replayed || replayed.Change != Unchanged || replayed.Result.Name != "same" {
		t.Fatalf("replay=%+v err=%v, want stored unchanged result", replayed, err)
	}
}

func TestRunRequiresExecutionContext(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)

	_, err := Run(context.Background(), runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 })
	if !errors.Is(err, ErrMissingContext) {
		t.Fatalf("err=%v, want ErrMissingContext", err)
	}
	if _, err := Execute(context.Background(), runner, op, func(tx *Tx) (itemResult, error) {
		return itemResult{}, nil
	}, func(itemResult) int64 { return 0 }); !errors.Is(err, ErrMissingContext) {
		t.Fatalf("execute err=%v, want ErrMissingContext", err)
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatal("missing-context runs persisted rows")
	}
}

func TestExpiredContextRollsBackOpenTransaction(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx, cancel := context.WithCancel(metadataContext(t, "corr-expired"))
	defer cancel()

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); err != nil {
			return itemResult{}, Changed, err
		}
		// The request ends mid-command: the context is dead before the
		// runner records the outcome. Nothing may commit, and cleanup must
		// not depend on the dead context.
		cancel()
		return itemResult{Name: "ghost"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err == nil {
		t.Fatal("want context expiry failure")
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatalf("expired context left rows: items=%d ledger=%d audit=%d", countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"), countRows(t, db, "audit_events"))
	}
}

func TestAuthorizeRunsBeforeReplayAndBusiness(t *testing.T) {
	db := newTestDB(t)
	runner, op, allowed := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-authz")

	first, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		result, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('alpha')`)
		if err != nil {
			return itemResult{}, Changed, err
		}
		id, _ := result.LastInsertId()
		return itemResult{ID: id, Name: "alpha"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatal(err)
	}

	// Revoke the principal, then replay the same command key: authorization
	// must run before the replay lookup, so the stored result never leaks.
	*allowed = false
	replayed, replayErr := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, errors.New("business must not run")
	}, func(itemResult) int64 { return 0 })
	if replayErr == nil || strings.Contains(replayErr.Error(), first.Result.Name) {
		t.Fatalf("replay after revoke err=%v, want authorization failure without stored result", replayErr)
	}
	if replayed.Replayed {
		t.Fatal("revoked replay must not surface a replayed outcome")
	}
	if countRows(t, db, "audit_events") != 1 || countRows(t, db, "client_commands") != 1 {
		t.Fatal("authorization failure recorded durable rows")
	}

	// Authorization also precedes business execution on a fresh command.
	*allowed = true
	ran := false
	if _, err := Run(ctx, runner, op, userCommand("cmd-2"), func(tx *Tx) (itemResult, Change, error) {
		ran = true
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err != nil || !ran {
		t.Fatalf("authorized command err=%v ran=%v", err, ran)
	}
}

func TestRunRejectsActorCommandPrincipalMismatch(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-mismatch") // actor user/7

	mismatch := userCommand("cmd-1")
	mismatch.PrincipalID = 8
	if _, err := Run(ctx, runner, op, mismatch, func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err == nil || !strings.Contains(err.Error(), "principal does not match") {
		t.Fatalf("err=%v, want principal mismatch rejection", err)
	}
	wrongKind := userCommand("cmd-1")
	wrongKind.PrincipalType = "system"
	wrongKind.PrincipalID = 0
	if _, err := Run(ctx, runner, op, wrongKind, func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err == nil || !strings.Contains(err.Error(), "principal does not match") {
		t.Fatalf("err=%v, want principal mismatch rejection", err)
	}
	if countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatal("mismatched principal persisted rows")
	}
}

func TestSystemPrincipalCommandRunsThroughLedger(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	actor := Principal{Kind: PrincipalSystem, ID: 0}
	ctx := metadataContextWithActor(t, "corr-system", actor)

	command := Command{PrincipalType: "system", PrincipalID: 0, ClientCommandID: "scheduled:item:2026-09-15T00:00:00Z", Digest: strings.Repeat("c", 64)}
	outcome, err := Run(ctx, runner, op, command, func(tx *Tx) (itemResult, Change, error) {
		result, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('scheduled')`)
		if err != nil {
			return itemResult{}, Changed, err
		}
		id, _ := result.LastInsertId()
		return itemResult{ID: id, Name: "scheduled"}, Changed, nil
	}, func(v itemResult) int64 { return v.ID })
	if err != nil {
		t.Fatal(err)
	}
	var principalType, initiatorType string
	if err := db.QueryRow(`SELECT principal_type FROM client_commands`).Scan(&principalType); err != nil || principalType != "system" {
		t.Fatalf("ledger principal=%q err=%v", principalType, err)
	}
	if err := db.QueryRow(`SELECT actor_type,COALESCE(initiator_type,'') FROM audit_events`).Scan(&principalType, &initiatorType); err != nil {
		t.Fatal(err)
	}
	if principalType != "system" || initiatorType != "system" || outcome.Replayed {
		t.Fatalf("audit actor=%s initiator=%s outcome=%+v", principalType, initiatorType, outcome)
	}
}

func TestExecuteAuditsWithoutCommandLedgerAndNeverReplays(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-execute")

	runs := 0
	for i := 0; i < 2; i++ {
		// A flow-step mutation whose result carries a transient secret: the
		// token result stays in memory and never persists, and each execution
		// is a real execution (no replay, no fake command key).
		var transient string
		result, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
			runs++
			res, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('flow')`)
			if err != nil {
				return itemResult{}, err
			}
			id, _ := res.LastInsertId()
			transient = "bearer-token-in-memory-only"
			return itemResult{ID: id, Name: "flow"}, nil
		}, func(v itemResult) int64 { return v.ID })
		if err != nil {
			t.Fatal(err)
		}
		if transient == "" || result.Name != "flow" {
			t.Fatalf("execute result=%+v transient=%q", result, transient)
		}
	}

	if countRows(t, db, "client_commands") != 0 {
		t.Fatalf("execute wrote %d ledger rows, want none", countRows(t, db, "client_commands"))
	}
	if countRows(t, db, "audit_events") != 2 || countRows(t, db, "quoin_items") != 2 {
		t.Fatalf("audit=%d items=%d, want two executions each", countRows(t, db, "audit_events"), countRows(t, db, "quoin_items"))
	}
	var commandID, correlation string
	if err := db.QueryRow(`SELECT COALESCE(client_command_id,''),correlation_id FROM audit_events ORDER BY id LIMIT 1`).Scan(&commandID, &correlation); err != nil {
		t.Fatal(err)
	}
	if commandID != "" || correlation != "corr-execute" {
		t.Fatalf("execute audit command=%q corr=%q", commandID, correlation)
	}

	// Execute rejections record the rejected audit without a ledger row.
	_, err := Execute(ctx, runner, op, func(tx *Tx) (itemResult, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quoin_items(name) VALUES('ghost')`); err != nil {
			return itemResult{}, err
		}
		return itemResult{}, &Rejection{Code: "validation_failed", Detail: "flow input invalid"}
	}, func(itemResult) int64 { return 0 })
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("err=%v, want *Rejection", err)
	}
	if countRows(t, db, "quoin_items") != 2 || countRows(t, db, "client_commands") != 0 {
		t.Fatalf("execute rejection left rows: items=%d ledger=%d", countRows(t, db, "quoin_items"), countRows(t, db, "client_commands"))
	}
	if countRows(t, db, "audit_events") != 3 {
		t.Fatalf("audit rows=%d, want rejection audited", countRows(t, db, "audit_events"))
	}
	var auditOutcome string
	if err := db.QueryRow(`SELECT outcome FROM audit_events ORDER BY id DESC LIMIT 1`).Scan(&auditOutcome); err != nil || auditOutcome != "rejected" {
		t.Fatalf("audit outcome=%s err=%v", auditOutcome, err)
	}
}

func TestRunRejectsUnregisteredOrInvalidOperations(t *testing.T) {
	db := newTestDB(t)
	_, _, _ = newTestRunner(t, db)
	ctx := metadataContext(t, "corr-registry")

	registry := NewRegistry()
	_, err := registry.Register(Operation{
		Name:       "item.create",
		Class:      ClassWrite,
		ObjectType: "quoin_item",
		Authorize:  func(ctx context.Context, tx *Tx) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	readOp, err := registry.Register(Operation{Name: "item.read", Class: ClassRead, ObjectType: "quoin_item"})
	if err != nil {
		t.Fatal(err)
	}
	sharedRunner := NewRunner(db, registry, audit.NewWriter())

	foreign := &Operation{Name: "item.create", Class: ClassWrite, ObjectType: "quoin_item"}
	if _, err := Run(ctx, sharedRunner, foreign, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err=%v, want unregistered operation rejection", err)
	}

	if _, err := Run(ctx, sharedRunner, readOp, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err == nil || !strings.Contains(err.Error(), "must not run through the command runner") {
		t.Fatalf("err=%v, want read-class rejection", err)
	}

	// A registered write operation without an authorization callback is
	// rejected before any transaction opens.
	noAuthzRegistry := NewRegistry()
	noAuthzOp, err := noAuthzRegistry.Register(Operation{Name: "item.noauthz", Class: ClassWrite, ObjectType: "quoin_item"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, NewRunner(db, noAuthzRegistry, audit.NewWriter()), noAuthzOp, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 }); err == nil || !strings.Contains(err.Error(), "authorization callback") {
		t.Fatalf("err=%v, want missing authorization rejection", err)
	}
}

func TestTxRejectsTransactionControlStatements(t *testing.T) {
	db := newTestDB(t)
	runner, op, _ := newTestRunner(t, db)
	ctx := metadataContext(t, "corr-guard")

	_, err := Run(ctx, runner, op, userCommand("cmd-1"), func(tx *Tx) (itemResult, Change, error) {
		if _, execErr := tx.ExecContext(ctx, `COMMIT`); execErr != nil {
			return itemResult{}, Changed, execErr
		}
		return itemResult{}, Changed, nil
	}, func(itemResult) int64 { return 0 })
	if err == nil || !strings.Contains(err.Error(), "may run inside the runner transaction") {
		t.Fatalf("err=%v, want statement whitelist guard", err)
	}
	if countRows(t, db, "quoin_items") != 0 || countRows(t, db, "client_commands") != 0 || countRows(t, db, "audit_events") != 0 {
		t.Fatal("guarded statement path persisted rows")
	}
}
