package stele

// 本地状态库的行为验证：入队幂等、批量取件、重试退避、死信迁移与限流
// 计数累计。全部使用临时目录里的真实 SQLite 文件。

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
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
		CredentialSnapshotVersion: 3, EventType: "alerts.batch", PayloadVersion: 1,
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

// 死信管理面（ADR-0011「保留原文可重放」）：列表投影携带凭据与原文，
// 重放按原凭据或显式新凭据重新入队（attempts 复位、立即到期），
// event_id 主键使重复重放必然冲突。
func TestDeadLetterListAndReplay(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("evt-dead-1")}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 被拒绝 → 死信（reason=rejected），凭据随行保存。
	if _, err := queue.MarkResult(ctx, "evt-dead-1", 2, time.Now().UTC()); err != nil { // EVENT_DELIVERY_STATUS_REJECTED
		t.Fatalf("mark rejected: %v", err)
	}
	letters, err := queue.ListDeadLetters(ctx, DeadLetterFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(letters) != 1 || letters[0].ID != "evt-dead-1" || letters[0].Reason != "rejected" {
		t.Fatalf("letters=%+v", letters)
	}
	if letters[0].CredentialID != 9 || letters[0].CredentialSnapshotVersion != 3 {
		t.Fatalf("dead letter lost its credential binding: %+v", letters[0])
	}
	if string(letters[0].Payload) != `{"status":"firing"}` {
		t.Fatalf("payload=%s", letters[0].Payload)
	}
	// 过滤：source/reason 命中与落空。
	if hit, _ := queue.ListDeadLetters(ctx, DeadLetterFilter{SourceKind: "alertmanager", Reason: "rejected"}); len(hit) != 1 {
		t.Fatalf("filtered list=%d, want 1", len(hit))
	}
	if miss, _ := queue.ListDeadLetters(ctx, DeadLetterFilter{Reason: "exhausted"}); len(miss) != 0 {
		t.Fatalf("filtered list=%d, want 0", len(miss))
	}

	// 按原凭据重放：回到 outbox、attempts 复位、立即到期可取。
	replayed, err := queue.ReplayDeadLetters(ctx, []string{"evt-dead-1"}, 0, 0)
	if err != nil || replayed != 1 {
		t.Fatalf("replay=(%d,%v), want (1,nil)", replayed, err)
	}
	if remaining, _ := queue.ListDeadLetters(ctx, DeadLetterFilter{}); len(remaining) != 0 {
		t.Fatalf("dead letters after replay=%d, want 0", len(remaining))
	}
	batch, err := queue.FetchDueBatch(ctx, 5, time.Now().UTC())
	if err != nil || len(batch) != 1 || batch[0].ID != "evt-dead-1" {
		t.Fatalf("replayed batch=%+v err=%v", batch, err)
	}
	if batch[0].Attempts != 0 || batch[0].CredentialID != 9 {
		t.Fatalf("replayed event=(attempts=%d,credential=%d), want (0,9)", batch[0].Attempts, batch[0].CredentialID)
	}

	// 再次死信后用显式新凭据重放（轮换场景：原凭据已吊销）。
	if _, err := queue.MarkResult(ctx, "evt-dead-1", 2, time.Now().UTC()); err != nil {
		t.Fatalf("re-mark rejected: %v", err)
	}
	replayed, err = queue.ReplayDeadLetters(ctx, []string{"evt-dead-1"}, 42, 7)
	if err != nil || replayed != 1 {
		t.Fatalf("replay with new credential=(%d,%v)", replayed, err)
	}
	batch, _ = queue.FetchDueBatch(ctx, 5, time.Now().UTC())
	if len(batch) != 1 || batch[0].CredentialID != 42 || batch[0].CredentialSnapshotVersion != 7 {
		t.Fatalf("replay must ride the explicit credential: %+v", batch)
	}

	// 不存在的 id：整体回滚——错误明确、计数为 0、已处理部分不留痕。
	replayed, err = queue.ReplayDeadLetters(ctx, []string{"evt-missing"}, 0, 0)
	if err == nil || replayed != 0 {
		t.Fatalf("replaying a missing id must fail with count 0, got (%d,%v)", replayed, err)
	}
}

// 重放的全有或全无：任一 id 失败即整体回滚，返回计数必须为 0（回滚后
// 事务内已走的插入/删除全部撤销），outbox 与死信表保持原状。
func TestDeadLetterReplayRollsBackEverythingOnMissingID(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	for _, id := range []string{"evt-roll-a", "evt-roll-b"} {
		if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent(id)}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
		if _, err := queue.MarkResult(ctx, id, 2 /* REJECTED */, time.Now().UTC()); err != nil {
			t.Fatalf("reject %s: %v", id, err)
		}
	}
	replayed, err := queue.ReplayDeadLetters(ctx, []string{"evt-roll-a", "evt-roll-missing"}, 0, 0)
	if err == nil {
		t.Fatal("missing id must fail the replay")
	}
	if replayed != 0 {
		t.Fatalf("rolled-back replay count = %d, want 0", replayed)
	}
	if letters, _ := queue.ListDeadLetters(ctx, DeadLetterFilter{}); len(letters) != 2 {
		t.Fatalf("rollback must restore both dead letters, got %d", len(letters))
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("rollback must not leave events in the outbox, depth = %d", depth)
	}
}

// 重放入参校验：空/空白 id、负凭据、凭据与快照失配、超出 int64 的快照
// 都在开事务前拒绝，不触碰任何行。
func TestDeadLetterReplayValidatesInput(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	if _, err := queue.ReplayDeadLetters(ctx, nil, 0, 0); err == nil {
		t.Fatal("empty id list must be rejected")
	}
	if _, err := queue.ReplayDeadLetters(ctx, []string{"evt-1", "  "}, 0, 0); err == nil {
		t.Fatal("blank ids must be rejected")
	}
	if _, err := queue.ReplayDeadLetters(ctx, []string{"evt-1"}, -1, 0); err == nil {
		t.Fatal("negative credential id must be rejected")
	}
	if _, err := queue.ReplayDeadLetters(ctx, []string{"evt-1"}, 0, 3); err == nil {
		t.Fatal("snapshot version without a credential id must be rejected")
	}
	if _, err := queue.ReplayDeadLetters(ctx, []string{"evt-1"}, 1, math.MaxUint64); err == nil {
		t.Fatal("snapshot version beyond int64 must be rejected")
	}
	if depth, _ := queue.QueueDepth(ctx); depth != 0 {
		t.Fatalf("rejected input must not touch the outbox, depth = %d", depth)
	}
	if letters, _ := queue.ListDeadLetters(ctx, DeadLetterFilter{}); len(letters) != 0 {
		t.Fatalf("rejected input must not touch dead letters, got %d", len(letters))
	}
}

func TestQueueRejectsUnknownPriorLayoutWithoutMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	queue, err := OpenQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := queue.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("old queue version=%d err=%v", version, err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenQueue(root); err == nil {
		reopened.Close()
		t.Fatal("unknown old queue must not be silently migrated or interpreted as canonical v1")
	}
	if readOnly, err := OpenQueueReadOnly(root); err == nil {
		readOnly.Close()
		t.Fatal("read-only queue must not guess the old layout's payload versions")
	}
}

func TestQueuePreservesPayloadVersionThroughRetryDeadletterAndReplay(t *testing.T) {
	ctx := context.Background()
	queue := openTestQueue(t)
	bad := sampleEvent("zero-version")
	bad.PayloadVersion = 0
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{sampleEvent("rolled-back"), bad}); err == nil {
		t.Fatal("versionless event must not enter the accepted queue")
	}
	if depth, err := queue.QueueDepth(ctx); err != nil || depth != 0 {
		t.Fatalf("a batch with one versionless item left accepted events queued: depth=%d err=%v", depth, err)
	}
	event := sampleEvent("versioned")
	event.PayloadVersion = 2
	if err := queue.EnqueueEvents(ctx, []QueuedEvent{event}); err != nil {
		t.Fatal(err)
	}
	fetched, err := queue.FetchDueBatch(ctx, 10, time.Now().UTC())
	if err != nil || len(fetched) != 1 || fetched[0].PayloadVersion != 2 || fetched[0].RelayEvent().GetPayloadVersion() != 2 {
		t.Fatalf("due versioned event=%+v err=%v", fetched, err)
	}
	if _, err := queue.MarkResult(ctx, event.ID, runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	letters, err := queue.ListDeadLetters(ctx, DeadLetterFilter{})
	if err != nil || len(letters) != 1 || letters[0].PayloadVersion != 2 {
		t.Fatalf("dead letter version=%+v err=%v", letters, err)
	}
	if _, err := queue.ReplayDeadLetters(ctx, []string{event.ID}, 0, 0); err != nil {
		t.Fatal(err)
	}
	fetched, err = queue.FetchDueBatch(ctx, 10, time.Now().UTC())
	if err != nil || len(fetched) != 1 || fetched[0].PayloadVersion != 2 {
		t.Fatalf("replayed versioned event=%+v err=%v", fetched, err)
	}
}
