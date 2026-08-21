package investigation

// Transient delta feed tests (RUNTIME-AGENT-004, HTTP-STREAM-006): ordered
// fan-out, non-monotonic and post-terminal drops, the exactly-once terminal
// view per subscriber and the committed terminal projection.

import (
	"context"
	"testing"
	"time"
)

func TestFeedDeliversOrderedDeltasAndTerminalOnce(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-feed-1", "请回答", nil)
	if err != nil {
		t.Fatal(err)
	}
	channel, release, err := service.Subscribe(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	service.DeliverDelta(created.AttemptID, 7, 1, "第")
	service.DeliverDelta(created.AttemptID, 7, 2, "一句")
	// Non-monotonic sequences drop (RUNTIME-AGENT-004).
	service.DeliverDelta(created.AttemptID, 7, 2, "重复")
	service.DeliverDelta(created.AttemptID, 7, 1, "过期")

	first := <-channel
	if first.Delta == nil || first.Delta.Text != "第" {
		t.Fatalf("first delta=%+v", first.Delta)
	}
	second := <-channel
	if second.Delta == nil || second.Delta.Text != "一句" {
		t.Fatalf("second delta=%+v", second.Delta)
	}
	// A new model call restarts the per-call sequence.
	service.DeliverDelta(created.AttemptID, 8, 1, "第三")
	third := <-channel
	if third.Delta == nil || third.Delta.Text != "第三" {
		t.Fatalf("third delta=%+v", third.Delta)
	}

	// Terminal delivery carries the committed view exactly once; late
	// deltas drop.
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"流式结束文本"`
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: sha256Sum([]byte(content))[:],
	}); err != nil {
		t.Fatal(err)
	}
	service.DeliverDelta(created.AttemptID, 8, 2, "终态后必须丢弃")
	select {
	case event := <-channel:
		if event.Terminal == nil {
			t.Fatalf("expected terminal, got delta %+v", event.Delta)
		}
		if event.Terminal.State != "Succeeded" || event.Terminal.Content != "流式结束文本" {
			t.Fatalf("terminal view=%+v", event.Terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal event never arrived")
	}
	select {
	case event := <-channel:
		t.Fatalf("unexpected extra event %+v", event)
	default:
	}
}

func TestFeedTerminalViewAndReplay(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	ctx := context.Background()
	principalID := seedUser(t, db)
	seedProviderChain(t, db)
	created, err := service.Create(ctx, principalID, "cmd-view-1", "请回答", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Active attempts have no terminal view yet.
	if view, err := service.TerminalViewFor(ctx, created.AttemptID); err != nil || view != nil {
		t.Fatalf("active terminal view=%+v err=%v", view, err)
	}
	if err := bindRunning(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := seedModelCall(t, db, created.AttemptID); err != nil {
		t.Fatal(err)
	}
	content := `"已提交"`
	if err := service.CommitResult(ctx, Result{
		AttemptID: created.AttemptID, BootID: "boot-t", Epoch: 1, Succeeded: true,
		SchemaKind: OutputSchemaKind, Canonical: []byte(content), Digest: sha256Sum([]byte(content))[:],
	}); err != nil {
		t.Fatal(err)
	}
	view, err := service.TerminalViewFor(ctx, created.AttemptID)
	if err != nil || view == nil {
		t.Fatalf("terminal view=%+v err=%v", view, err)
	}
	if view.State != "Succeeded" || view.Content != "已提交" {
		t.Fatalf("terminal view wrong: %+v", view)
	}
	if view.InputTokens != 4 || view.OutputTokens != 2 {
		t.Fatalf("usage wrong: %+v", view)
	}
	// A subscriber attaching after the terminal state receives the stored
	// terminal view immediately (stream replay path).
	channel, release, err := service.Subscribe(ctx, created.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case event := <-channel:
		if event.Terminal == nil || event.Terminal.Content != "已提交" {
			t.Fatalf("late subscribe view=%+v", event.Terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late subscriber never received the terminal view")
	}
}

func TestFeedDropsWithoutObserver(t *testing.T) {
	db := newTestDB(t)
	service := NewService(db)
	// Unknown attempts carry no feed; delivery must never panic or create
	// state.
	service.DeliverDelta(999999, 1, 1, "无观察者")
	service.NotifyTerminal(context.Background(), 999999)
	// Empty deltas drop silently.
	service.DeliverDelta(999999, 1, 2, "")
}
