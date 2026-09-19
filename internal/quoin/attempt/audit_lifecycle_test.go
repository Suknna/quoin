package attempt

// Automatic lifecycle audit tests for the standalone (transaction-owning)
// stages (ADR-0006): the agent model/tool ledger writes and the
// cancel/interrupt/sweep transitions each record exactly one audit fact on
// their own transaction, attributed to the system runtime authority on the
// attempt's PERSISTED association — never to the raw caller context. Natural
// idempotent returns record nothing, and an audit failure rolls the whole
// stage back.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// auditEventRow is the raw audit_events projection the assertions read.
type auditEventRow struct {
	ID          int64
	ActorType   string
	ActorID     int64
	Action      string
	Outcome     string
	Correlation sql.NullString
	RequestID   sql.NullString
	Initiator   sql.NullString
	InitiatorID sql.NullInt64
	RefType     sql.NullString
	RefID       sql.NullInt64
}

// auditEventsByAction loads every audit event of one action, ordered by id.
func auditEventsByAction(t *testing.T, db *sql.DB, action string) []auditEventRow {
	t.Helper()
	rows, err := db.Query(`SELECT id,actor_type,actor_id,action,outcome,correlation_id,request_id,
		initiator_type,initiator_id,domain_ref_type,domain_ref_id
		FROM audit_events WHERE action=? ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []auditEventRow
	for rows.Next() {
		var event auditEventRow
		if err := rows.Scan(&event.ID, &event.ActorType, &event.ActorID, &event.Action, &event.Outcome,
			&event.Correlation, &event.RequestID, &event.Initiator, &event.InitiatorID,
			&event.RefType, &event.RefID); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// userContext is the hostile-case context: an unrelated user identity that
// the lifecycle audit must never adopt as actor or correlation.
func userContext(t *testing.T) context.Context {
	t.Helper()
	meta := execution.Metadata{
		CorrelationID: "corr-" + strings.ToLower(strings.Repeat("u", 29)),
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: 7},
		Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-user"},
	}
	ctx, err := execution.WithMetadata(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// correlatedRunningAttempt seeds one attempt, persists the correlation
// context's association onto it and dispatches it to Running.
func correlatedRunningAttempt(t *testing.T, db *sql.DB, service *Service) int64 {
	t.Helper()
	attemptID, _ := seedAttempt(t, db)
	if err := persistCorrelationForTest(t, correlationContext(t), db, attemptID); err != nil {
		t.Fatal(err)
	}
	bindAndAccept(t, service, attemptID, "boot-audit", 1)
	return attemptID
}

// beginChatCall opens one valid chat model call on the running attempt.
func beginChatCall(t *testing.T, service *Service, attemptID int64, callSeq int) int64 {
	t.Helper()
	toolsDigest := testCatalogDigest(t)
	callID, err := service.BeginModelCall(context.Background(), BeginCall{
		AttemptID: attemptID, CallSeq: callSeq, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("a", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("b", 64), RenderedDigest: strings.Repeat("c", 64),
		InputItems: []ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return callID
}

func TestStandaloneModelAndToolStagesRecordAudit(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	attemptID := correlatedRunningAttempt(t, db, service)

	callID := beginChatCall(t, service, attemptID, 1)
	begins := auditEventsByAction(t, db, auditActionModelCallBegin)
	if len(begins) != 1 {
		t.Fatalf("begin events = %d, want 1", len(begins))
	}
	begin := begins[0]
	// The runtime machine is the acting system principal; the correlation
	// and original initiator come from the persisted association only.
	if begin.ActorType != "system" || begin.ActorID != 0 || begin.Outcome != "success" {
		t.Fatalf("begin event actor/outcome = %s/%d/%s", begin.ActorType, begin.ActorID, begin.Outcome)
	}
	if begin.Correlation.String != "corr-"+strings.Repeat("a", 29) {
		t.Fatalf("begin correlation = %q", begin.Correlation.String)
	}
	if begin.Initiator.String != "user" || begin.InitiatorID.Int64 != 42 {
		t.Fatalf("begin initiator = %s/%d, want user/42", begin.Initiator.String, begin.InitiatorID.Int64)
	}
	if begin.RefType.String != auditRefModelCall || begin.RefID.Int64 != callID {
		t.Fatalf("begin ref = %s/%d, want model_call/%d", begin.RefType.String, begin.RefID.Int64, callID)
	}

	arguments := []byte(`{"command":"echo audit-proof"}`)
	proposed := []ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-audit", ToolName: "bash",
		ArgumentsJSON: arguments, ArgumentsDigest: sha256HexString(arguments),
	}}
	_, responseDigest, err := CanonicalChatResponseJSON("", proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteModelCall(context.Background(), CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded", FinishReason: "tool_calls",
		ProposedTools: proposed, ResponseDigest: responseDigest, ResponseComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	completes := auditEventsByAction(t, db, auditActionModelCallComplete)
	if len(completes) != 1 || completes[0].Outcome != "success" || completes[0].RefID.Int64 != callID {
		t.Fatalf("complete events = %+v", completes)
	}

	// Drive the tool call through begin and a succeeded seal.
	var toolCallID int64
	if err := db.QueryRow(`SELECT id FROM tool_calls WHERE attempt_id=?`, attemptID).Scan(&toolCallID); err != nil {
		t.Fatal(err)
	}
	if err := service.BeginToolCall(context.Background(), attemptID, toolCallID); err != nil {
		t.Fatal(err)
	}
	toolBegins := auditEventsByAction(t, db, auditActionToolCallBegin)
	if len(toolBegins) != 1 || toolBegins[0].RefID.Int64 != toolCallID || toolBegins[0].Outcome != "success" {
		t.Fatalf("tool begin events = %+v", toolBegins)
	}
	if _, err := service.CompleteToolCall(context.Background(), ToolResult{
		AttemptID: attemptID, ToolCallID: toolCallID, Outcome: "succeeded",
		ResultJSON: `{"success":true,"output":"audit-proof\n"}`,
	}); err != nil {
		t.Fatal(err)
	}
	toolCompletes := auditEventsByAction(t, db, auditActionToolCallComplete)
	if len(toolCompletes) != 1 || toolCompletes[0].Outcome != "success" {
		t.Fatalf("tool complete events = %+v", toolCompletes)
	}
	// Every lifecycle event of this attempt carries the same persisted
	// correlation: the runtime context never re-roots it.
	for _, pair := range []struct {
		action string
		events []auditEventRow
	}{
		{auditActionModelCallBegin, begins},
		{auditActionModelCallComplete, completes},
		{auditActionToolCallBegin, toolBegins},
		{auditActionToolCallComplete, toolCompletes},
	} {
		for _, event := range pair.events {
			if event.Correlation.String != "corr-"+strings.Repeat("a", 29) || event.Initiator.String != "user" {
				t.Fatalf("%s event lost the persisted association: %+v", pair.action, event)
			}
		}
	}
}

func TestModelAndToolFailureOutcomesSealAsFailure(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	// The frozen sequence closure only continues after a SUCCEEDED call, so
	// the failed and the cancelled seal each run on their own attempt.
	failedID := correlatedRunningAttempt(t, db, service)
	cancelledID := correlatedRunningAttempt(t, db, service)

	callID := beginChatCall(t, service, failedID, 1)
	if _, err := service.CompleteModelCall(context.Background(), CompleteCall{
		AttemptID: failedID, CallID: callID, Outcome: "failed",
		FailureReason: "transport_error",
	}); err != nil {
		t.Fatal(err)
	}
	// A cancelled physical call also seals as failure: it never produced
	// its result (the row's status carries the precise cancelled fact).
	callID2 := beginChatCall(t, service, cancelledID, 1)
	if _, err := service.CompleteModelCall(context.Background(), CompleteCall{
		AttemptID: cancelledID, CallID: callID2, Outcome: "cancelled",
		FailureReason: "cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	completes := auditEventsByAction(t, db, auditActionModelCallComplete)
	if len(completes) != 2 {
		t.Fatalf("complete events = %d, want 2", len(completes))
	}
	for _, event := range completes {
		if event.Outcome != "failure" {
			t.Fatalf("non-success seal outcome = %s, want failure", event.Outcome)
		}
	}
}

func TestNaturalIdempotentReplaysRecordNothing(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	attemptID := correlatedRunningAttempt(t, db, service)
	ctx := context.Background()

	toolsDigest := testCatalogDigest(t)
	request := BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: []ModelInputItem{
			{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
			{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
			{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
		},
		ContextBudget: 4096, MaxOutput: 1024,
	}
	first, err := service.BeginModelCall(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// Digest-identical replay (lost Begin ack) and the lost-ack retry alias
	// both return the original row and must not add audit events.
	replay := request
	if _, err := service.BeginModelCall(ctx, replay); err != nil {
		t.Fatal(err)
	}
	alias := request
	alias.RetrySeq = 1
	if aliased, err := service.BeginModelCall(ctx, alias); err != nil || aliased != first {
		t.Fatalf("alias = %d err=%v", aliased, err)
	}
	if begins := auditEventsByAction(t, db, auditActionModelCallBegin); len(begins) != 1 {
		t.Fatalf("replays duplicated begin audit: %d events", len(begins))
	}

	assistantText := "最终结论"
	_, digest, err := CanonicalChatResponseJSON(assistantText, nil)
	if err != nil {
		t.Fatal(err)
	}
	completion := CompleteCall{
		AttemptID: attemptID, CallID: first, Outcome: "succeeded",
		AssistantText: assistantText, ResponseDigest: digest, ResponseComplete: true,
		FinishReason: "stop",
	}
	if _, err := service.CompleteModelCall(ctx, completion); err != nil {
		t.Fatal(err)
	}
	// A replayed completion rebuilds the original Ack without a new event.
	if _, err := service.CompleteModelCall(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if completes := auditEventsByAction(t, db, auditActionModelCallComplete); len(completes) != 1 {
		t.Fatalf("replay duplicated complete audit: %d events", len(completes))
	}
}

func TestCancelFenceAndInterruptAuditEachTransitionOnce(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	ctx := context.Background()

	// The fence records its transition exactly once; the terminal no-op
	// records nothing.
	fencedID := correlatedRunningAttempt(t, db, service)
	if state, err := service.CancelFence(ctx, fencedID); err != nil || state != "Cancelling" {
		t.Fatalf("fence = %q err=%v", state, err)
	}
	if state, err := service.CancelFence(ctx, fencedID); err != nil || state != "Cancelling" {
		t.Fatalf("idempotent fence = %q err=%v", state, err)
	}
	if fences := auditEventsByAction(t, db, auditActionCancelFence); len(fences) != 1 {
		t.Fatalf("fence audit events = %d, want 1", len(fences))
	}
	// Loss converging the Cancelling fence to Cancelled is the interrupt
	// stage's own transition and records once; further interrupts are
	// terminal no-ops.
	if final, err := service.Interrupt(ctx, fencedID, "lease_expired"); err != nil || final != "Cancelled" {
		t.Fatalf("cancelling interrupt = %q err=%v", final, err)
	}
	if final, err := service.Interrupt(ctx, fencedID, "lease_expired"); err != nil || final != "Cancelled" {
		t.Fatalf("terminal re-interrupt = %q err=%v", final, err)
	}
	if interrupts := auditEventsByAction(t, db, auditActionInterrupt); len(interrupts) != 1 {
		t.Fatalf("interrupt audit events = %d, want 1", len(interrupts))
	}

	// Interrupting a Running attempt records once; re-interrupt records
	// nothing.
	interruptedID := correlatedRunningAttempt(t, db, service)
	if final, err := service.Interrupt(ctx, interruptedID, "replaced"); err != nil || final != "Interrupted" {
		t.Fatalf("interrupt = %q err=%v", final, err)
	}
	if final, err := service.Interrupt(ctx, interruptedID, "replaced"); err != nil || final != "Interrupted" {
		t.Fatalf("terminal re-interrupt = %q err=%v", final, err)
	}
	interrupts := auditEventsByAction(t, db, auditActionInterrupt)
	if len(interrupts) != 2 {
		t.Fatalf("interrupt audit events = %d, want 2", len(interrupts))
	}
	// The fence on the already-terminal attempt answers the terminal state
	// without a new event.
	if state, err := service.CancelFence(ctx, interruptedID); err != nil || state != "Interrupted" {
		t.Fatalf("terminal fence = %q err=%v", state, err)
	}
	if fences := auditEventsByAction(t, db, auditActionCancelFence); len(fences) != 1 {
		t.Fatalf("terminal fence added audit: %d events", len(fences))
	}
}

func TestSweepAuditKeepsOriginalCorrelationAndExplicitSchedulerScope(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	ctx := context.Background()

	correlatedID := correlatedRunningAttempt(t, db, service)
	legacyID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, legacyID, "boot-audit", 1)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id IN (?,?)`, past, correlatedID, legacyID); err != nil {
		t.Fatal(err)
	}

	// The raw caller context claims an unrelated user identity; the sweep
	// batch must not adopt it.
	swept, err := service.SweepExpired(userContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 2 {
		t.Fatalf("swept = %+v", swept)
	}
	events := auditEventsByAction(t, db, auditActionLeaseSweep)
	if len(events) != 2 {
		t.Fatalf("sweep audit events = %d, want 2", len(events))
	}
	byRef := map[int64]auditEventRow{}
	for _, event := range events {
		byRef[event.RefID.Int64] = event
	}
	// A correlated attempt keeps its original correlation and initiator;
	// this sweep trigger is only the request identity.
	correlated := byRef[correlatedID]
	if correlated.Correlation.String != "corr-"+strings.Repeat("a", 29) || correlated.Initiator.String != "user" || correlated.InitiatorID.Int64 != 42 {
		t.Fatalf("correlated sweep event lost the original association: %+v", correlated)
	}
	if correlated.RequestID.String == "" {
		t.Fatal("sweep event must carry the trigger as the request identity")
	}
	firstTrigger := correlated.RequestID.String
	// A legacy attempt is audited under the batch's explicit scheduler
	// scope: the trigger correlation itself, never the raw caller's user
	// identity.
	legacy := byRef[legacyID]
	if legacy.Correlation.String != firstTrigger || legacy.Correlation.String == "corr-"+strings.Repeat("u", 29) {
		t.Fatalf("legacy sweep event correlation = %q, want the explicit trigger scope", legacy.Correlation.String)
	}
	// The explicit scheduler scope is both actor and initiator of the sweep
	// fact: the persisted original initiator is unknown for a legacy row and
	// is never guessed, so the machine scope it really acted as is recorded.
	if legacy.Initiator.String != "system" || legacy.InitiatorID.Int64 != 0 || legacy.ActorType != "system" {
		t.Fatalf("legacy sweep event identity = %s/%s/%d %+v", legacy.ActorType, legacy.Initiator.String, legacy.InitiatorID.Int64, legacy)
	}

	// The next trigger is a NEW operation with a fresh correlation.
	nextID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, nextID, "boot-audit", 1)
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id=?`, past, nextID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SweepExpired(ctx); err != nil {
		t.Fatal(err)
	}
	events = auditEventsByAction(t, db, auditActionLeaseSweep)
	if len(events) != 3 {
		t.Fatalf("second sweep events = %d, want 3", len(events))
	}
	for _, event := range events {
		if event.RefID.Int64 == nextID && event.RequestID.String == firstTrigger {
			t.Fatal("the next sweep trigger reused the previous correlation")
		}
	}
}

func TestRawCallerContextCannotClaimLifecycleAudit(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	attemptID := correlatedRunningAttempt(t, db, service)

	// Both standalone stages run under a hostile user context: the audit
	// must still name the system runtime authority on the persisted
	// association, never the caller's identity or correlation.
	if state, err := service.CancelFence(userContext(t), attemptID); err != nil || state != "Cancelling" {
		t.Fatalf("fence = %q err=%v", state, err)
	}
	if final, err := service.Interrupt(userContext(t), attemptID, "lease_expired"); err != nil || final != "Cancelled" {
		t.Fatalf("interrupt = %q err=%v", final, err)
	}
	for _, action := range []string{auditActionCancelFence, auditActionInterrupt} {
		events := auditEventsByAction(t, db, action)
		if len(events) != 1 {
			t.Fatalf("%s events = %d, want 1", action, len(events))
		}
		event := events[0]
		if event.ActorType != "system" || event.ActorID != 0 {
			t.Fatalf("%s claimed actor %s/%d", action, event.ActorType, event.ActorID)
		}
		if event.Correlation.String != "corr-"+strings.Repeat("a", 29) {
			t.Fatalf("%s adopted the raw caller correlation %q", action, event.Correlation.String)
		}
		if event.Initiator.String != "user" || event.InitiatorID.Int64 != 42 {
			t.Fatalf("%s initiator = %s/%d, want the persisted user/42", action, event.Initiator.String, event.InitiatorID.Int64)
		}
	}
}

func TestAuditFailureRollsTheLifecycleStageBack(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	attemptID := correlatedRunningAttempt(t, db, service)

	// Remove the audit sink: the audit write now fails as infrastructure
	// (no such table), which must fail the whole stage so no lifecycle
	// write can ever commit without its record.
	if _, err := db.Exec(`DROP TABLE audit_event_targets`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}

	if _, err := service.BeginModelCall(context.Background(), BeginCall{AttemptID: attemptID}); err == nil {
		t.Fatal("model call begin must fail when its audit record cannot persist")
	}
	var callCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=?`, attemptID).Scan(&callCount); err != nil || callCount != 0 {
		t.Fatalf("failed audit left %d model call rows (err=%v)", callCount, err)
	}
	if state, err := service.CancelFence(context.Background(), attemptID); err == nil {
		t.Fatalf("cancel fence committed without its audit record (state %q)", state)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil || state != "Running" {
		t.Fatalf("failed audit changed the attempt state to %q (err=%v)", state, err)
	}
}

// TestConcurrentFenceAndInterruptAuditEachTransitionOnce is the bounded race
// window: cancellation and loss convergence racing on one attempt must stay
// fenced (SQLite commit order), record at most one event per stage, and keep
// the audit order causally consistent (an interrupt event can only follow
// the fence it converged).
func TestConcurrentFenceAndInterruptAuditEachTransitionOnce(t *testing.T) {
	db := newTestDB(t)
	service := newTestService(t, db)
	attemptID := correlatedRunningAttempt(t, db, service)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_, _ = service.CancelFence(context.Background(), attemptID)
				return
			}
			_, _ = service.Interrupt(context.Background(), attemptID, "lease_expired")
		}(i)
	}
	close(start)
	wg.Wait()

	var state string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "Cancelled", "Cancelling", "Interrupted":
	default:
		t.Fatalf("raced fence/interrupt landed on %q", state)
	}
	fences := auditEventsByAction(t, db, auditActionCancelFence)
	interrupts := auditEventsByAction(t, db, auditActionInterrupt)
	if len(fences) > 1 || len(interrupts) > 1 {
		t.Fatalf("raced stages duplicated audit: %d fences, %d interrupts", len(fences), len(interrupts))
	}
	if len(fences)+len(interrupts) == 0 {
		t.Fatal("every transition recorded nothing")
	}
	// Cancelling→Cancelled convergence is only reachable after the fence,
	// so an interrupt event following a Cancelled convergence must come
	// after the fence event.
	if state == "Cancelled" && len(fences) == 1 && len(interrupts) == 1 && interrupts[0].ID < fences[0].ID {
		t.Fatalf("audit order lost causality: interrupt %d before fence %d", interrupts[0].ID, fences[0].ID)
	}
}
