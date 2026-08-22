package investigation

// T15 deterministic transition/race tests (DATA-INVEST-001..003,
// HTTP-COMMAND-002/003/005, DATA-TX-005): simultaneous sends serialize on
// the head fence, command replays stay idempotent across target objects,
// Undo-vs-result and Stop-vs-result resolve by SQLite commit order, and
// Retry only re-answers a Failed attempt whose message is still the active
// branch.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// commitSuccessFor seals one running attempt with the given answer text.
func commitSuccessFor(t *testing.T, service *Service, attemptID int64, text string) error {
	t.Helper()
	content := `"` + text + `"`
	digest := sha256Sum([]byte(content))
	return service.CommitResult(context.Background(), Result{
		AttemptID: attemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: digest[:],
	})
}

func TestSimultaneousSendsConflictDeterministically(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, principalID, "cmd-race-create", "第一条", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, created.AttemptID, "第一轮回复"); err != nil {
		t.Fatal(err)
	}
	detail, err := service.Get(ctx, created.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	head, parseErr := parseLocator(detail.HeadMessageID)
	if parseErr != nil {
		t.Fatal(parseErr)
	}

	// Two sends fenced on the same expected head race: BEGIN IMMEDIATE
	// serializes the writers, so exactly one commits and the loser sees
	// the moved head (DATA-INVEST-001).
	const racers = 2
	results := make([]error, racers)
	var wait sync.WaitGroup
	for index := 0; index < racers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			_, err := service.Send(ctx, principalID, "cmd-race-send-"+string(rune('a'+slot)), created.InvestigationID, &head, "并发消息"+string(rune('a'+slot)), nil)
			results[slot] = err
		}(index)
	}
	wait.Wait()
	successes, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.As(err, new(*HeadConflictError)), errors.Is(err, ErrActiveAttempt):
			conflicts++
		default:
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d want 1/1", successes, conflicts)
	}
	var messages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=?`, created.InvestigationID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 3 {
		t.Fatalf("messages=%d want 3 (first turn + one winning send)", messages)
	}
}

func TestSendReplayIdempotentAcrossTargetAndHead(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	first, err := service.Create(ctx, principalID, "cmd-replay-create", "起点", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, first.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, first.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, first.AttemptID, "回复"); err != nil {
		t.Fatal(err)
	}
	head, err := parseLocatorDetail(t, service, first.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := service.Send(ctx, principalID, "cmd-replay-send", first.InvestigationID, &head, "第二条", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The head has moved past the send's own fence; the replay still
	// returns the original message (HTTP-COMMAND-007: the command key
	// lookup precedes any version/head premise).
	replayed, err := service.Send(ctx, principalID, "cmd-replay-send", first.InvestigationID, &head, "第二条", nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.MessageID != sent.MessageID || replayed.AttemptID != sent.AttemptID {
		t.Fatalf("replay diverged: %+v want %+v", replayed, sent)
	}
	var messages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=?`, first.InvestigationID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 3 {
		t.Fatalf("messages=%d want 3", messages)
	}

	// The same command id against a DIFFERENT investigation with identical
	// content is a different semantic request: the target participates in
	// the digest, so it must conflict instead of replaying the first
	// investigation's message (HTTP-COMMAND-002).
	other, err := service.Create(ctx, principalID, "cmd-replay-other", "起点", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherHead := int64(0)
	detail, err := service.Get(ctx, other.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.HeadMessageID != "" {
		if otherHead, err = parseLocator(detail.HeadMessageID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Send(ctx, principalID, "cmd-replay-send", other.InvestigationID, &otherHead, "第二条", nil); !errors.Is(err, ErrCommandReused) {
		t.Fatalf("cross-target replay err=%v want ErrCommandReused", err)
	}
}

func TestUndoWithdrawsLatestTurnAndCancelsQueuedAttempt(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, principalID, "cmd-undo-create", "要撤回的消息", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The attempt is still Queued (no runtime bound): the undo fence closes
	// it directly to Cancelled.
	outcome, err := service.Undo(ctx, principalID, "cmd-undo-1", created.InvestigationID, created.MessageID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if outcome.Withdrawn != 1 || outcome.AttemptID != created.AttemptID || outcome.AttemptState != "Cancelled" || outcome.DispatchRequired {
		t.Fatalf("outcome wrong: %+v", outcome)
	}
	if outcome.NewHead != nil {
		t.Fatalf("new head want nil, got %d", *outcome.NewHead)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM investigation_messages WHERE id=?`, created.MessageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "withdrawn" {
		t.Fatalf("message status=%s want withdrawn", status)
	}
	var head sql.NullInt64
	if err := db.QueryRow(`SELECT current_head_message_id FROM investigations WHERE id=?`, created.InvestigationID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head.Valid {
		t.Fatalf("head must be null after withdrawing the whole branch, got %d", head.Int64)
	}
	var attemptState, reason string
	if err := db.QueryRow(`SELECT state,termination_reason FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&attemptState, &reason); err != nil {
		t.Fatal(err)
	}
	if attemptState != "Cancelled" || reason != "cancelled" {
		t.Fatalf("attempt %s/%s want Cancelled/cancelled", attemptState, reason)
	}
	// New messages continue from the withdrawn head: the explicit null
	// fence (HTTP-COMMAND-002), and the withdrawn message never re-enters
	// the new attempt's input snapshot.
	resent, err := service.Send(ctx, principalID, "cmd-undo-resend", created.InvestigationID, nil, "重新表述的消息", nil)
	if err != nil {
		t.Fatalf("resend after full withdrawal: %v", err)
	}
	var items int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.investigation_message_id=?`, resent.AttemptID, created.MessageID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Fatalf("withdrawn message entered the new snapshot %d times", items)
	}
	// Undo replay reports the same committed state without re-fencing.
	replayed, err := service.Undo(ctx, principalID, "cmd-undo-1", created.InvestigationID, created.MessageID)
	if err != nil {
		t.Fatalf("undo replay: %v", err)
	}
	if replayed.Withdrawn != 0 || replayed.AttemptID != 0 {
		t.Fatalf("replay must be a pure state report: %+v", replayed)
	}
}

func TestUndoVersusResultCommitOrder(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	// (a) The undo fence commits first: the running attempt closes to
	// Cancelling, and the late success result is audit-only — no assistant
	// message, no head move (DATA-INVEST-002 / DATA-TX-005).
	created, err := service.Create(ctx, principalID, "cmd-undo-race-a", "撤销先到", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Undo(ctx, principalID, "cmd-undo-race-a-cmd", created.InvestigationID, created.MessageID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if outcome.AttemptState != "Cancelling" || !outcome.DispatchRequired {
		t.Fatalf("running attempt must fence to Cancelling with a dispatch: %+v", outcome)
	}
	if err := commitSuccessFor(t, service, created.AttemptID, "迟到的回复"); !errors.Is(err, ErrLateResult) {
		t.Fatalf("late result err=%v want ErrLateResult", err)
	}
	var assistant int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=? AND role='assistant'`, created.InvestigationID).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	if assistant != 0 {
		t.Fatalf("late result must not commit an assistant message")
	}
	// The runtime's cancel ack finishes the fence (RUNTIME-CANCEL-003).
	if err := service.CancelAck(ctx, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM execution_attempts WHERE id=?`, created.AttemptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "Cancelled" {
		t.Fatalf("state=%s want Cancelled", state)
	}

	// (b) The success result commits first: the undo's head fence loses —
	// the withdrawal conflicts instead of withdrawing a sealed reply.
	raced, err := service.Create(ctx, principalID, "cmd-undo-race-b", "结果先到", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, raced.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, raced.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, raced.AttemptID, "先到的回复"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Undo(ctx, principalID, "cmd-undo-race-b-cmd", raced.InvestigationID, raced.MessageID); err == nil {
		t.Fatal("undo with the stale user-message head must conflict")
	} else if !errors.As(err, new(*HeadConflictError)) {
		t.Fatalf("undo err=%v want HeadConflictError", err)
	}
	var statuses []string
	rows, err := db.Query(`SELECT status FROM investigation_messages WHERE investigation_id=? ORDER BY seq`, raced.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var status string
		_ = rows.Scan(&status)
		statuses = append(statuses, status)
	}
	rows.Close()
	if len(statuses) != 2 || statuses[0] != "active" || statuses[1] != "active" {
		t.Fatalf("statuses=%v want both active", statuses)
	}
}

func TestUndoAfterSuccessWithdrawsWholeTurn(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, principalID, "cmd-undo-success", "完整回合", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, created.AttemptID, "回合回复"); err != nil {
		t.Fatal(err)
	}
	assistantHead, err := parseLocatorDetail(t, service, created.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	// Undo fences on the CURRENT head (the sealed assistant message) and
	// withdraws the user turn together with its reply.
	outcome, err := service.Undo(ctx, principalID, "cmd-undo-success-cmd", created.InvestigationID, assistantHead)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if outcome.Withdrawn != 2 || outcome.NewHead != nil {
		t.Fatalf("outcome wrong: %+v", outcome)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=? AND status='active'`, created.InvestigationID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active messages=%d want 0", active)
	}
	// History is never deleted: both rows remain as the read-only branch.
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM investigation_messages WHERE investigation_id=?`, created.InvestigationID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("withdrawn history rows=%d want 2", total)
	}
}

func TestStopFenceCommitOrderings(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	// Success first: the stop answers the completed object, never a
	// conflict (HTTP-COMMAND-005).
	won, err := service.Create(ctx, principalID, "cmd-stop-won", "结果先提交", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, won.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, won.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, won.AttemptID, "成功结果"); err != nil {
		t.Fatal(err)
	}
	view, err := service.AttemptView(ctx, won.InvestigationID, won.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Cancel(ctx, principalID, "cmd-stop-won-cmd", won.InvestigationID, won.AttemptID, view.RowVersion-5)
	if err != nil {
		t.Fatalf("stop after success must answer the completed object: %v", err)
	}
	if outcome.State != "Succeeded" || outcome.DispatchRequired {
		t.Fatalf("outcome wrong: %+v", outcome)
	}

	// Running: the fence moves to Cancelling and owes the runtime a
	// cancel frame; the ack finishes Cancelled (no fenced middle state).
	running, err := service.Create(ctx, principalID, "cmd-stop-running", "运行中停止", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, running.AttemptID); err != nil {
		t.Fatal(err)
	}
	runningView, err := service.AttemptView(ctx, running.InvestigationID, running.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = service.Cancel(ctx, principalID, "cmd-stop-running-cmd", running.InvestigationID, running.AttemptID, runningView.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != "Cancelling" || !outcome.DispatchRequired {
		t.Fatalf("outcome wrong: %+v", outcome)
	}
	// The replay reports the committed state and never re-dispatches.
	replay, err := service.Cancel(ctx, principalID, "cmd-stop-running-cmd", running.InvestigationID, running.AttemptID, runningView.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	if replay.State != "Cancelling" || replay.DispatchRequired {
		t.Fatalf("replay must report without dispatch: %+v", replay)
	}
	if err := service.CancelAck(ctx, running.AttemptID); err != nil {
		t.Fatal(err)
	}

	// Stale row version on a still-active attempt conflicts.
	stale, err := service.Create(ctx, principalID, "cmd-stop-stale", "版本过期", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, stale.AttemptID); err != nil {
		t.Fatal(err)
	}
	staleView, err := service.AttemptView(ctx, stale.InvestigationID, stale.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cancel(ctx, principalID, "cmd-stop-stale-cmd", stale.InvestigationID, stale.AttemptID, staleView.RowVersion-1); err == nil {
		t.Fatal("stale row version must conflict")
	} else if !errors.As(err, new(*attempt.RowVersionError)) {
		t.Fatalf("err=%v want RowVersionError", err)
	}

	// A stop against another investigation's attempt is a not-found.
	if _, err := service.Cancel(ctx, principalID, "cmd-stop-foreign", stale.InvestigationID, won.AttemptID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestRetryGuardsAndReanswer(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, principalID, "cmd-retry-create", "要重试的问题", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	// A failed attempt (no assistant message, head stays on the user turn).
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: false,
		Termination: "provider_unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	// Retry refuses while the original attempt is terminal but the message
	// is withdrawn; first prove the happy path.
	retriedID, err := service.Retry(ctx, principalID, "cmd-retry-1", created.InvestigationID, created.AttemptID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retriedID == created.AttemptID {
		t.Fatalf("retry must create a new attempt")
	}
	// The replay returns the same new attempt (HTTP-COMMAND-003).
	again, err := service.Retry(ctx, principalID, "cmd-retry-1", created.InvestigationID, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if again != retriedID {
		t.Fatalf("replay diverged: %d want %d", again, retriedID)
	}
	// The retried attempt runs and its result commits through the lineage
	// fallback (no user message row of its own): assistant message + head.
	if err := bindRunning(t, db, retriedID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, retriedID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, retriedID, "重试后的回复"); err != nil {
		t.Fatalf("retry result commit: %v", err)
	}
	var assistant int64
	if err := db.QueryRow(`SELECT id FROM investigation_messages WHERE attempt_id=? AND role='assistant'`, retriedID).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	head, err := parseLocatorDetail(t, service, created.InvestigationID)
	if err != nil {
		t.Fatal(err)
	}
	if head != assistant {
		t.Fatalf("head=%d want the retry's assistant message %d", head, assistant)
	}
	// Retrying the now-Succeeded attempt conflicts.
	if _, err := service.Retry(ctx, principalID, "cmd-retry-succeeded", created.InvestigationID, retriedID); !errors.Is(err, ErrAttemptNotFailed) {
		t.Fatalf("err=%v want ErrAttemptNotFailed", err)
	}

	// Retry with an active attempt conflicts (one active attempt per
	// investigation, DATA-INVEST-003).
	active, err := service.Send(ctx, principalID, "cmd-retry-next", created.InvestigationID, &head, "下一轮", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retry(ctx, principalID, "cmd-retry-active", created.InvestigationID, created.AttemptID); !errors.Is(err, ErrActiveAttempt) {
		t.Fatalf("err=%v want ErrActiveAttempt", err)
	}

	// A withdrawn turn never re-enters the model context: undo the active
	// turn, then retry the original failed attempt must conflict.
	if err := bindRunning(t, db, active.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, active.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := commitSuccessFor(t, service, active.AttemptID, "第二轮回复"); err != nil {
		t.Fatal(err)
	}
	// Withdraw the first turn only: undo fences on the current head, so
	// first withdraw the second turn, then the first is not withdrawable
	// (only the latest turn) — the guard is proven on the second turn's
	// failed twin instead: create a fresh investigation, fail it, undo it.
	fresh, err := service.Create(ctx, principalID, "cmd-retry-fresh", "撤回后不可重试", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, fresh.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, Result{
		AttemptID: fresh.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: false,
		Termination: "provider_unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Undo(ctx, principalID, "cmd-retry-fresh-undo", fresh.InvestigationID, fresh.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Retry(ctx, principalID, "cmd-retry-fresh-cmd", fresh.InvestigationID, fresh.AttemptID); !errors.Is(err, ErrRetryMessageWithdrawn) {
		t.Fatalf("err=%v want ErrRetryMessageWithdrawn", err)
	}
	// Attempt-scope mismatch is a not-found.
	if _, err := service.Retry(ctx, principalID, "cmd-retry-foreign", fresh.InvestigationID, created.AttemptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestMessageAttemptPrefersActiveRetryAttempt(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)

	created, err := service.Create(ctx, principalID, "cmd-attach-create", "流附着", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: false,
		Termination: "provider_unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	// No active attempt: the stream attaches to the message's own sealed
	// attempt (terminal replay).
	bound, err := service.MessageAttempt(ctx, created.InvestigationID, created.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if bound != created.AttemptID {
		t.Fatalf("bound=%d want sealed attempt %d", bound, created.AttemptID)
	}
	// With a live retry re-answering the same message, the stream must
	// observe the active execution instead.
	retriedID, err := service.Retry(ctx, principalID, "cmd-attach-retry", created.InvestigationID, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err = service.MessageAttempt(ctx, created.InvestigationID, created.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if bound != retriedID {
		t.Fatalf("bound=%d want active retry attempt %d", bound, retriedID)
	}
}

func parseLocatorDetail(t *testing.T, service *Service, investigationID int64) (int64, error) {
	t.Helper()
	detail, err := service.Get(context.Background(), investigationID)
	if err != nil {
		return 0, err
	}
	if detail.HeadMessageID == "" {
		return 0, errors.New("head is null")
	}
	return parseLocator(detail.HeadMessageID)
}

func parseLocator(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64)
}
