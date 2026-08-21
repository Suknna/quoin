package attempt

// Recovery-semantics tests (T12, RUNTIME-TASK-005/006/007, RUNTIME-CANCEL-003)
// over the real frozen schema: loss interruption including the Cancelling
// fence exception, heartbeat lease renewal, same-boot rebind, the lease
// sweeper and the ledger replay idempotency that reconnecting workers rely
// on.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// bindAndAccept moves a seeded attempt through dispatch binding and
// acceptance on the given stream identity.
func bindAndAccept(t *testing.T, service *Service, attemptID int64, boot string, epoch uint64) {
	t.Helper()
	ctx := context.Background()
	if err := service.BindToStream(ctx, attemptID, boot, epoch, DispatchLease); err != nil {
		t.Fatal(err)
	}
	if err := service.Accept(ctx, attemptID, boot, epoch); err != nil {
		t.Fatal(err)
	}
}

// setState forcibly rewrites the attempt state for scenario setup (tests
// own the row outside the product paths; every UPDATE must still bump
// row_version exactly once — the schema trigger enforces it).
func setState(t *testing.T, db *sql.DB, attemptID int64, state string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE execution_attempts SET state=?,row_version=row_version+1 WHERE id=?`, state, attemptID); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptConvergesActiveAndRespectsCancelFence(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()

	// Assigned and Running attempts interrupt with the loss reason.
	assignedID, _ := seedAttempt(t, db)
	if err := service.BindToStream(ctx, assignedID, "boot-a", 1, DispatchLease); err != nil {
		t.Fatal(err)
	}
	runningID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, runningID, "boot-a", 1)

	if final, err := service.Interrupt(ctx, assignedID, "lease_expired"); err != nil || final != "Interrupted" {
		t.Fatalf("assigned interrupt: final=%q err=%v", final, err)
	}
	if final, err := service.Interrupt(ctx, runningID, "replaced"); err != nil || final != "Interrupted" {
		t.Fatalf("running interrupt: final=%q err=%v", final, err)
	}
	var reason string
	if err := db.QueryRow(`SELECT termination_reason FROM execution_attempts WHERE id=?`, runningID).Scan(&reason); err != nil || reason != "replaced" {
		t.Fatalf("running interrupt reason=%q err=%v", reason, err)
	}
	// Terminal attempts are idempotent no-ops.
	if final, err := service.Interrupt(ctx, runningID, "lease_expired"); err != nil || final != "Interrupted" {
		t.Fatalf("terminal re-interrupt: final=%q err=%v", final, err)
	}
	// The cancellation fence wins over interruption: Cancelling converges
	// to Cancelled (RUNTIME-TASK-006 fence exception).
	cancellingID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, cancellingID, "boot-a", 1)
	if _, err := service.CancelFence(ctx, cancellingID); err != nil {
		t.Fatal(err)
	}
	if final, err := service.Interrupt(ctx, cancellingID, "lease_expired"); err != nil || final != "Cancelled" {
		t.Fatalf("cancelling interrupt: final=%q err=%v", final, err)
	}
	// Unknown reasons are refused before any write.
	if _, err := service.Interrupt(ctx, assignedID, "provider_unavailable"); err == nil {
		t.Fatal("unclosed loss reason accepted")
	}
}

func TestActiveOfSlotListsBoundAttempts(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	boundID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, boundID, "boot-a", 3)
	queuedID, _ := seedAttempt(t, db)
	_ = queuedID

	views, err := service.ActiveOfSlot(ctx, "plinth")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected one active bound attempt, got %d", len(views))
	}
	view := views[0]
	if view.ID != boundID || view.BootID == nil || *view.BootID != "boot-a" || view.ConnectionEpoch == nil || *view.ConnectionEpoch != 3 || view.LeaseUntil == nil {
		t.Fatalf("binding projection wrong: %+v", view)
	}
}

func TestRenewLeaseForBootExtendsOnlyLiveLeases(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	liveID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, liveID, "boot-a", 1)
	expiredID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, expiredID, "boot-a", 1)
	// Burn the second lease into the past without touching state.
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), expiredID); err != nil {
		t.Fatal(err)
	}

	if err := service.RenewLeaseForBoot(ctx, "plinth", "boot-a", DispatchLease); err != nil {
		t.Fatal(err)
	}
	var renewedUntil string
	if err := db.QueryRow(`SELECT lease_until FROM execution_attempts WHERE id=?`, liveID).Scan(&renewedUntil); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, renewedUntil)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Before(time.Now().UTC().Add(DispatchLease - 30*time.Second)) {
		t.Fatalf("live lease not extended: %s", renewedUntil)
	}
	var expiredUntil string
	if err := db.QueryRow(`SELECT lease_until FROM execution_attempts WHERE id=?`, expiredID).Scan(&expiredUntil); err != nil {
		t.Fatal(err)
	}
	if expiredParsed, _ := time.Parse(time.RFC3339Nano, expiredUntil); !expiredParsed.Before(time.Now().UTC()) {
		t.Fatalf("expired lease resurrected by renewal: %s", expiredUntil)
	}
	// Renewal bumps row_version exactly once per update.
	var version int64
	if err := db.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, liveID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := service.RenewLeaseForBoot(ctx, "plinth", "boot-a", DispatchLease); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err := db.QueryRow(`SELECT row_version FROM execution_attempts WHERE id=?`, liveID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != version+1 {
		t.Fatalf("renewal must bump row_version exactly once: %d -> %d", version, next)
	}
}

func TestAcceptAcrossSameBootEpochs(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	// Dispatched on epoch 1; the stream dropped before the accept.
	if err := service.BindToStream(ctx, attemptID, "boot-a", 1, DispatchLease); err != nil {
		t.Fatal(err)
	}
	// After the same-boot reconnect the accept arrives on the newer epoch's
	// stream (envelope fence proved currency); the frozen binding epoch is
	// immutable, so the fence matches the boot (RUNTIME-TASK-005).
	if err := service.Accept(ctx, attemptID, "boot-a", 2); err != nil {
		t.Fatal(err)
	}
	var state string
	var epoch int64
	if err := db.QueryRow(`SELECT state,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &epoch); err != nil || state != "Running" || epoch != 1 {
		t.Fatalf("accept across epochs: state=%q epoch=%d err=%v", state, epoch, err)
	}
	// A different boot still cannot accept.
	secondID, _ := seedAttempt(t, db)
	if err := service.BindToStream(ctx, secondID, "boot-a", 1, DispatchLease); err != nil {
		t.Fatal(err)
	}
	if err := service.Accept(ctx, secondID, "boot-b", 1); err == nil {
		t.Fatal("foreign boot accept accepted")
	}
}

func TestSweepExpiredInterruptsAndConvergesCancelling(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	expiredID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, expiredID, "boot-a", 1)
	cancelledID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, cancelledID, "boot-a", 1)
	if _, err := service.CancelFence(ctx, cancelledID); err != nil {
		t.Fatal(err)
	}
	liveID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, liveID, "boot-a", 1)
	// Burn the first two, keep the third alive.
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE execution_attempts SET lease_until=?,row_version=row_version+1 WHERE id IN (?,?)`, past, expiredID, cancelledID); err != nil {
		t.Fatal(err)
	}

	swept, err := service.SweepExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 2 {
		t.Fatalf("expected two swept attempts, got %d: %+v", len(swept), swept)
	}
	byFinal := map[int64]string{}
	for _, item := range swept {
		byFinal[item.AttemptID] = item.Final
	}
	if byFinal[expiredID] != "Interrupted" || byFinal[cancelledID] != "Cancelled" {
		t.Fatalf("sweep outcomes wrong: %+v", swept)
	}
	var reason string
	if err := db.QueryRow(`SELECT termination_reason FROM execution_attempts WHERE id=?`, expiredID).Scan(&reason); err != nil || reason != "lease_expired" {
		t.Fatalf("sweep reason=%q err=%v", reason, err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, liveID).Scan(&state); err != nil || state != "Running" {
		t.Fatalf("live attempt swept: state=%q err=%v", state, err)
	}
	// The sweep carries the scope identity for parent closure routing.
	for _, item := range swept {
		if item.ScopeType != "analysis" || item.ScopeID == 0 {
			t.Fatalf("sweep lost scope identity: %+v", item)
		}
	}
}

func TestBeginModelCallReplayReturnsOriginalRow(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, attemptID, "boot-a", 1)
	toolsDigest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	items := []ModelInputItem{
		{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
		{Sequence: 2, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
	}

	first, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: items, ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same physical call replays to its original row (lost ack).
	replayed, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: items, ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first {
		t.Fatalf("replay returned %d, original %d", replayed, first)
	}
	// A divergent digest conflicts instead of aliasing.
	if _, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("9", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: items, ContextBudget: 4096, MaxOutput: 1024,
	}); err == nil {
		t.Fatal("divergent replay accepted")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND call_seq=1 AND retry_seq=0`, attemptID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay created a second physical row: count=%d err=%v", count, err)
	}
}

func TestCompleteModelCallReplayRebuildsOriginalAck(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, attemptID, "boot-a", 1)
	toolsDigest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	items := []ModelInputItem{
		{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
		{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
		{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
	}

	callID, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: items, ContextBudget: 4096, MaxOutput: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolsJSON, err := CanonicalToolsJSON()
	if err != nil {
		t.Fatal(err)
	}
	_ = toolsJSON
	assistantText := "最终诊断"
	arguments := []byte(`{"command":"echo ok"}`)
	proposed := []ProposedTool{{
		ProviderIndex: 0, ProviderToolCallID: "call-1", ToolName: "bash",
		ArgumentsJSON: arguments, ArgumentsDigest: sha256HexString(arguments),
	}}
	digest, err := canonicalDigestOf(assistantText, proposed)
	if err != nil {
		t.Fatal(err)
	}
	original, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded",
		AssistantText: assistantText, ResponseDigest: digest, ResponseComplete: true,
		ProposedTools: proposed, FinishReason: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 {
		t.Fatalf("original completion authorizations: %+v", original)
	}
	// A replay of the identical completion rebuilds the same durable
	// authorizations instead of rejecting (lost CompleteModelCallAck).
	replayed, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded",
		AssistantText: assistantText, ResponseDigest: digest, ResponseComplete: true,
		ProposedTools: proposed, FinishReason: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].ToolCallID != original[0].ToolCallID {
		t.Fatalf("replayed authorizations diverge: %+v vs %+v", replayed, original)
	}
	// A divergent response conflicts.
	if _, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "succeeded",
		AssistantText: "不同的输出", ResponseDigest: digest, ResponseComplete: true,
		ProposedTools: proposed, FinishReason: "stop",
	}); err == nil {
		t.Fatal("divergent completion replay accepted")
	}
	var toolCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE model_call_id=?`, callID).Scan(&toolCount); err != nil || toolCount != 1 {
		t.Fatalf("replay duplicated tool calls: count=%d err=%v", toolCount, err)
	}
}

// canonicalDigestOf mirrors the wire digest for the canonical response.
func canonicalDigestOf(assistantText string, tools []ProposedTool) (string, error) {
	_, digest, err := CanonicalChatResponseJSON(assistantText, tools)
	return digest, err
}

var _ *sql.DB

func TestPartialFailurePersistsIncompleteOutputRow(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, attemptID, "boot-a", 1)
	toolsDigest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	callID, err := service.BeginModelCall(ctx, BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
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
	// The provider hung up after exposing partial tokens: the physical call
	// seals failed with the partial text as an incomplete output
	// (RUNTIME-AGENT-005 — audit only, never a valid seal).
	if _, err := service.CompleteModelCall(ctx, CompleteCall{
		AttemptID: attemptID, CallID: callID, Outcome: "failed",
		FailureReason: "transport_error", AssistantText: "初步诊断：中断前",
	}); err != nil {
		t.Fatal(err)
	}
	var complete int
	var body string
	if err := db.QueryRow(`SELECT complete,response_json FROM model_call_outputs WHERE model_call_id=?`, callID).Scan(&complete, &body); err != nil {
		t.Fatalf("partial output row missing: %v", err)
	}
	if complete != 0 || !strings.Contains(body, "中断前") {
		t.Fatalf("partial output wrong: complete=%d body=%s", complete, body)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM model_calls WHERE id=?`, callID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("call status=%q err=%v", status, err)
	}
}

func TestBeginModelCallLostAckRetryAliasesRunningPredecessor(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	attemptID, _ := seedAttempt(t, db)
	bindAndAccept(t, service, attemptID, "boot-a", 1)
	toolsDigest, err := CanonicalToolsDigest()
	if err != nil {
		t.Fatal(err)
	}
	items := []ModelInputItem{
		{Sequence: 1, ItemKind: "system_contract", ContentDigest: strings.Repeat("d", 64), Role: "system"},
		{Sequence: 2, ItemKind: "tool_schema", ContentDigest: strings.Repeat("e", 64), Role: "system"},
		{Sequence: 3, ItemKind: "snapshot", ContentDigest: testDigest, Role: "system"},
	}
	request := BeginCall{
		AttemptID: attemptID, CallSeq: 1, RetrySeq: 0, ModelID: "fixture-chat-1",
		PromptDigest: strings.Repeat("1", 64), ToolSchemaDigest: toolsDigest,
		InputDigest: strings.Repeat("3", 64), RenderedDigest: strings.Repeat("4", 64),
		InputItems: items, ContextBudget: 4096, MaxOutput: 1024,
	}
	first, err := service.BeginModelCall(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// The stream dropped before the ack; the worker resends with
	// retry_seq+1. The still-running predecessor with identical digests IS
	// the same physical call: the retry aliases onto it instead of being
	// rejected (a rejected resend would strand the whole retry chain).
	retry := request
	retry.RetrySeq = 1
	aliased, err := service.BeginModelCall(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if aliased != first {
		t.Fatalf("lost-ack retry returned %d, want the running row %d", aliased, first)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_calls WHERE attempt_id=? AND call_seq=1`, attemptID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("alias created a second physical row: count=%d err=%v", count, err)
	}
	// A divergent resend (different request bytes) still cannot retry.
	divergent := request
	divergent.RetrySeq = 1
	divergent.RenderedDigest = strings.Repeat("9", 64)
	if _, err := service.BeginModelCall(ctx, divergent); err == nil {
		t.Fatal("divergent retry over a running predecessor accepted")
	}
}
