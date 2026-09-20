package stele

// 本地状态库的行为验证：入队幂等、批量取件、重试退避、死信迁移与限流
// 计数累计。全部使用临时目录里的真实 SQLite 文件。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestQueue(t *testing.T) *Queue {
	t.Helper()
	queue, err := OpenQueue(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return queue
}

func sampleEvent(id string) QueuedEvent {
	return QueuedEvent{
		ID: id, SourceKind: "alertmanager", SourceID: 7, CredentialID: 9,
		CredentialSnapshotVersion: 3, EventType: "alerts.batch",
		ReceivedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		Payload:    []byte(`{"status":"firing"}`),
	}
}

func deadLetterReasons(t *testing.T, queue *Queue) map[string]string {
	t.Helper()
	rows, err := queue.db.Query(`SELECT id, reason FROM dead_letters`)
	if err != nil {
		t.Fatalf("query dead letters: %v", err)
	}
	defer rows.Close()
	reasons := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			t.Fatalf("scan dead letter: %v", err)
		}
		reasons[id] = reason
	}
	return reasons
}

func TestQueueEnqueueFetchAndIdempotentPrimaryKey(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{
		sampleEvent("evt-1"), sampleEvent("evt-2"), sampleEvent("evt-3"),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 3 {
		t.Fatalf("depth after enqueue = %d, want 3", depth)
	}
	// 同一 event_id 重复插入必须失败：主键就是幂等边界。
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-1")}); err == nil {
		t.Fatal("duplicate event id must be rejected by the primary key")
	}
	batch, err := queue.FetchDueBatch(ctx, 2, time.Now().UTC())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("batch size = %d, want 2", len(batch))
	}
	if batch[0].ID != "evt-1" || batch[1].ID != "evt-2" {
		t.Fatalf("batch order = %s,%s, want evt-1,evt-2", batch[0].ID, batch[1].ID)
	}
	event := batch[0]
	if event.SourceKind != "alertmanager" || event.SourceID != 7 || event.CredentialID != 9 ||
		event.CredentialSnapshotVersion != 3 || event.EventType != "alerts.batch" ||
		string(event.Payload) != `{"status":"firing"}` {
		t.Fatalf("round-tripped event lost fields: %+v", event)
	}
}

func TestQueueMarkAcceptedDeletes(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-1")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dead, err := queue.MarkResult(ctx, "evt-1", 1 /* ACCEPTED */, time.Now().UTC())
	if err != nil || dead {
		t.Fatalf("mark accepted: dead=%v err=%v", dead, err)
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("depth after accept = %d, want 0", depth)
	}
	if reasons := deadLetterReasons(t, queue); len(reasons) != 0 {
		t.Fatalf("accepted event must not dead-letter: %v", reasons)
	}
}

func TestQueueMarkRejectedMovesToDeadLetter(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-1")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dead, err := queue.MarkResult(ctx, "evt-1", 2 /* REJECTED */, time.Now().UTC())
	if err != nil || !dead {
		t.Fatalf("mark rejected: dead=%v err=%v", dead, err)
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("depth after reject = %d, want 0", depth)
	}
	if reason := deadLetterReasons(t, queue)["evt-1"]; reason != "rejected" {
		t.Fatalf("dead letter reason = %q, want rejected", reason)
	}
}

func TestQueueUnavailableBackoffAndExhaustion(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-1")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dead, err := queue.MarkResult(ctx, "evt-1", 3 /* UNAVAILABLE */, now)
	if err != nil || dead {
		t.Fatalf("first unavailable: dead=%v err=%v", dead, err)
	}
	// 第一次失败：attempts=1，next_retry = now + 1s。
	if pending, err := queue.FetchDueBatch(ctx, 1, now.Add(500*time.Millisecond)); err != nil || len(pending) != 0 {
		t.Fatalf("before backoff expiry pending=%d err=%v, want 0", len(pending), err)
	}
	pending, err := queue.FetchDueBatch(ctx, 1, now.Add(1100*time.Millisecond))
	if err != nil || len(pending) != 1 {
		t.Fatalf("after backoff expiry pending=%d err=%v, want 1", len(pending), err)
	}
	if pending[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", pending[0].Attempts)
	}
	// 退避按 1s*2^(attempts-1) 指数增长：attempts=2 后要等 2s。
	dead, err = queue.MarkResult(ctx, "evt-1", 3, now.Add(2*time.Second))
	if err != nil || dead {
		t.Fatalf("second unavailable: dead=%v err=%v", dead, err)
	}
	if pending, err := queue.FetchDueBatch(ctx, 1, now.Add(3*time.Second)); err != nil || len(pending) != 0 {
		t.Fatalf("mid-backoff pending=%d err=%v, want 0", len(pending), err)
	}
	if pending, err := queue.FetchDueBatch(ctx, 1, now.Add(4*time.Second)); err != nil || len(pending) != 1 {
		t.Fatalf("after second backoff pending=%d err=%v, want 1", len(pending), err)
	}
	// 连续失败到第 10 次尝试：迁入死信 reason='exhausted'。
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := queue.MarkResult(ctx, "evt-1", 3, now.Add(time.Duration(attempt+3)*time.Minute)); err != nil {
			t.Fatalf("unavailable attempt %d: %v", attempt+3, err)
		}
	}
	if reason := deadLetterReasons(t, queue)["evt-1"]; reason != "exhausted" {
		t.Fatalf("dead letter reason = %q, want exhausted", reason)
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("depth after exhaustion = %d, want 0", depth)
	}
}

func TestQueueUnavailableAgeExhaustion(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-old")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 把 created_at 直接改到 25h 前：单次 UNAVAILABLE 即超龄死信。
	if _, err := queue.db.ExecContext(ctx,
		`UPDATE events_outbox SET created_at=? WHERE id='evt-old'`,
		formatStateTime(time.Now().UTC().Add(-25*time.Hour))); err != nil {
		t.Fatalf("age the event: %v", err)
	}
	dead, err := queue.MarkResult(ctx, "evt-old", 3, time.Now().UTC())
	if err != nil || !dead {
		t.Fatalf("aged unavailable: dead=%v err=%v", dead, err)
	}
	if reason := deadLetterReasons(t, queue)["evt-old"]; reason != "exhausted" {
		t.Fatalf("dead letter reason = %q, want exhausted", reason)
	}
}

func TestQueueRateCounterAccumulation(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.AddRateCounters(ctx, 42, 3, 1); err != nil {
		t.Fatalf("first accumulate: %v", err)
	}
	if err := queue.AddRateCounters(ctx, 42, 2, 0); err != nil {
		t.Fatalf("second accumulate: %v", err)
	}
	allowed, denied, err := queue.RateCounter(ctx, 42)
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if allowed != 5 || denied != 1 {
		t.Fatalf("counters = %d/%d, want 5/1", allowed, denied)
	}
	// 未出现过的连接读回零值。
	allowed, denied, err = queue.RateCounter(ctx, 999)
	if err != nil || allowed != 0 || denied != 0 {
		t.Fatalf("unknown connection counters = %d/%d err=%v, want 0/0", allowed, denied, err)
	}
}

func TestQueueFilePermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "state")
	queue, err := OpenQueue(root)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	defer queue.Close()
	main, err := os.Stat(filepath.Join(root, "stele.db"))
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if main.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %o, want 600", main.Mode().Perm())
	}
	// 目录以 0700 创建（umask 可能收紧，但不能更宽）。
	dir, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat data directory: %v", err)
	}
	if dir.Mode().Perm()&0o077 != 0 {
		t.Fatalf("data directory mode = %o, want group/other bits clear", dir.Mode().Perm())
	}
}
