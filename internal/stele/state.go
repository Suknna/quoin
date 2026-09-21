package stele

// Stele 本地状态存储（ADR-0011）：SQLite 承载出站事件队列（outbox）、死信表、
// 出向限流累计计数，以及动态凭证/拉取游标两张预留表。这里只做网关自身的
// 可靠性簿记——入队即 ACK 之后，转发循环、指数退避与死信迁移全部以此库为
// 唯一事实；它不是业务库，bootstrap 仅用 user_version + CREATE IF NOT EXISTS。

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	_ "modernc.org/sqlite"
)

// queueSchemaVersion 是本地库结构的唯一版本号（PRAGMA user_version）。
// v2：dead_letters 增加 credential_id/credential_snapshot_version 两列
// （重放需要把事件按原凭据重新入队；v1 库经 ALTER TABLE 原地迁移）。
const queueSchemaVersion = 2

// 固定纳秒宽度的 UTC 时间布局：等宽文本使字典序与时间序一致，next_retry_at
// 的字符串比较才可靠。
const stateTimeLayout = "2006-01-02T15:04:05.000000000Z"

// 退避与死信策略（转发循环语义）：UNAVAILABLE 后按 1s*2^attempts 指数退避，
// 上限 5m；累计尝试 >= maxDeliveryAttempts 或事件年龄 >= maxEventAge 后
// 迁入死信表。
const (
	retryBackoffBase    = 1 * time.Second
	retryBackoffCap     = 5 * time.Minute
	maxDeliveryAttempts = 10
	maxEventAge         = 24 * time.Hour
)

func formatStateTime(value time.Time) string {
	return value.UTC().Format(stateTimeLayout)
}

// QueuedEvent 是 outbox 中一条待转发事件的内存形态。
type QueuedEvent struct {
	ID                        string
	SourceKind                string
	SourceID                  int64
	CredentialID              int64
	CredentialSnapshotVersion uint64
	EventType                 string
	ReceivedAt                time.Time
	Payload                   []byte
	Attempts                  int
	CreatedAt                 time.Time
}

// RelayEvent 把内存事件投影为 DeliverEvents 的线上形态。
func (event QueuedEvent) RelayEvent() *runtimev1.RelayEvent {
	return &runtimev1.RelayEvent{
		EventId:                   event.ID,
		SourceKind:                event.SourceKind,
		SourceId:                  event.SourceID,
		CredentialId:              event.CredentialID,
		CredentialSnapshotVersion: event.CredentialSnapshotVersion,
		EventType:                 event.EventType,
		ReceivedAt:                timestampProto(event.ReceivedAt),
		Payload:                   event.Payload,
	}
}

// Queue 是 Stele 的本地状态库句柄。单写连接（SetMaxOpenConns(1)）串行化全部
// 访问：WAL + synchronous(NORMAL) 足够，入向 webhook 与转发循环无需更重的
// 并发控制。
type Queue struct {
	db *sql.DB
}

// OpenQueue 打开（必要时创建）<dataDirectory>/stele.db。目录以 0700 创建，
// 库文件收紧为 0600；结构版本不认识时直接失败，避免静默使用未知布局。
func OpenQueue(dataDirectory string) (*Queue, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("stele: create data directory: %w", err)
	}
	databasePath := filepath.Join(dataDirectory, "stele.db")
	dsn := (&url.URL{
		Scheme: "file", Path: databasePath,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(1)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("stele: open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("stele: ping state database: %w", err)
	}
	queue := &Queue{db: db}
	if err := queue.bootstrap(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("stele: restrict state database permissions: %w", err)
	}
	return queue, nil
}

func (queue *Queue) Close() error {
	return queue.db.Close()
}

// bootstrap 建表并推进 user_version；已识别版本则跳过。
func (queue *Queue) bootstrap() error {
	var version int
	if err := queue.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("stele: read state schema version: %w", err)
	}
	if version == queueSchemaVersion {
		return nil
	}
	if version == 1 {
		// v1 → v2：dead_letters 补凭据列（重放入队需要）；既有死信行的
		// 凭据记为 0，重放时必须显式指定有效凭据（CLI 强制校验）。
		tx, err := queue.db.Begin()
		if err != nil {
			return fmt.Errorf("stele: begin v2 migration: %w", err)
		}
		if _, err := tx.Exec(`ALTER TABLE dead_letters ADD COLUMN credential_id INTEGER NOT NULL DEFAULT 0`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stele: migrate dead_letters credential_id: %w", err)
		}
		if _, err := tx.Exec(`ALTER TABLE dead_letters ADD COLUMN credential_snapshot_version INTEGER NOT NULL DEFAULT 0`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stele: migrate dead_letters credential_snapshot_version: %w", err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, queueSchemaVersion)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stele: stamp schema v2: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("stele: commit v2 migration: %w", err)
		}
		return nil
	}
	if version != 0 {
		return fmt.Errorf("stele: state database schema version %d is not supported (want %d)", version, queueSchemaVersion)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS events_outbox(
			id TEXT PRIMARY KEY,
			source_kind TEXT NOT NULL,
			source_id INTEGER NOT NULL,
			credential_id INTEGER NOT NULL,
			credential_snapshot_version INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			received_at TEXT NOT NULL,
			payload BLOB NOT NULL,
			state TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_retry_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS events_outbox_due ON events_outbox(created_at, id)`,
		`CREATE TABLE IF NOT EXISTS dead_letters(
			id TEXT PRIMARY KEY,
			source_kind TEXT NOT NULL,
			source_id INTEGER NOT NULL,
			credential_id INTEGER NOT NULL DEFAULT 0,
			credential_snapshot_version INTEGER NOT NULL DEFAULT 0,
			event_type TEXT NOT NULL,
			payload BLOB NOT NULL,
			reason TEXT NOT NULL,
			attempts INTEGER NOT NULL,
			first_received_at TEXT NOT NULL,
			dead_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS rate_counters(
			connection_id INTEGER PRIMARY KEY,
			allowed_total INTEGER NOT NULL DEFAULT 0,
			denied_total INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		// 动态凭证生命周期预留（token 刷新等）；连接材料缓存目前只在内存。
		`CREATE TABLE IF NOT EXISTS token_cache(
			cache_key TEXT PRIMARY KEY,
			value BLOB NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		// 拉取型事件源的游标预留；当前入向全部是推送 webhook。
		`CREATE TABLE IF NOT EXISTS poll_cursors(
			source_kind TEXT PRIMARY KEY,
			cursor TEXT NOT NULL
		)`,
	}
	tx, err := queue.db.Begin()
	if err != nil {
		return fmt.Errorf("stele: begin state bootstrap: %w", err)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stele: bootstrap state schema: %w", err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", queueSchemaVersion)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("stele: stamp state schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stele: commit state bootstrap: %w", err)
	}
	return nil
}

// EnqueueEvents 在单事务内批量写入 outbox；event_id 主键天然幂等（重复插入
// 报错而不是静默覆盖）。
func (queue *Queue) EnqueueEvents(ctx context.Context, events []QueuedEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := queue.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("stele: begin enqueue: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO events_outbox(
		id, source_kind, source_id, credential_id, credential_snapshot_version,
		event_type, received_at, payload, state, attempts, next_retry_at, created_at
	) VALUES (?,?,?,?,?,?,?,?,'pending',0,NULL,?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("stele: prepare enqueue: %w", err)
	}
	defer statement.Close()
	now := time.Now().UTC()
	for index, event := range events {
		receivedAt := event.ReceivedAt
		if receivedAt.IsZero() {
			receivedAt = now
		}
		createdAt := event.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		if _, err := statement.ExecContext(ctx,
			event.ID, event.SourceKind, event.SourceID, event.CredentialID,
			int64(event.CredentialSnapshotVersion), event.EventType,
			formatStateTime(receivedAt), event.Payload, formatStateTime(createdAt),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stele: enqueue event %d of %d: %w", index+1, len(events), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stele: commit enqueue: %w", err)
	}
	return nil
}

// FetchDueBatch 取一批到期事件（pending 且退避已过），按入队顺序消费。
func (queue *Queue) FetchDueBatch(ctx context.Context, batch int, now time.Time) ([]QueuedEvent, error) {
	rows, err := queue.db.QueryContext(ctx, `SELECT
		id, source_kind, source_id, credential_id, credential_snapshot_version,
		event_type, received_at, payload, attempts, created_at
		FROM events_outbox
		WHERE state='pending' AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY created_at, id LIMIT ?`, formatStateTime(now), batch)
	if err != nil {
		return nil, fmt.Errorf("stele: fetch due batch: %w", err)
	}
	defer rows.Close()
	var events []QueuedEvent
	for rows.Next() {
		var event QueuedEvent
		var receivedAt, createdAt string
		var snapshotVersion int64
		if err := rows.Scan(&event.ID, &event.SourceKind, &event.SourceID, &event.CredentialID,
			&snapshotVersion, &event.EventType, &receivedAt, &event.Payload, &event.Attempts, &createdAt); err != nil {
			return nil, fmt.Errorf("stele: scan due batch row: %w", err)
		}
		event.CredentialSnapshotVersion = uint64(snapshotVersion)
		if event.ReceivedAt, err = time.Parse(stateTimeLayout, receivedAt); err != nil {
			return nil, fmt.Errorf("stele: parse received_at: %w", err)
		}
		if event.CreatedAt, err = time.Parse(stateTimeLayout, createdAt); err != nil {
			return nil, fmt.Errorf("stele: parse created_at: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// MarkResult 按 DeliverEvents 的逐事件裁决落账，返回该事件是否被迁入死信。
//   - ACCEPTED：从 outbox 删除（Quoin 已提交，event_id 幂等）。
//   - REJECTED：删除并写死信（reason='rejected'），留待人工处置。
//   - UNAVAILABLE：attempts+1 并按指数退避推迟；重试耗尽或事件超龄则死信
//     （reason='exhausted'）。
func (queue *Queue) MarkResult(ctx context.Context, id string, result runtimev1.EventDeliveryStatus, now time.Time) (bool, error) {
	switch result {
	case runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_ACCEPTED:
		if _, err := queue.db.ExecContext(ctx, `DELETE FROM events_outbox WHERE id=?`, id); err != nil {
			return false, fmt.Errorf("stele: delete accepted event: %w", err)
		}
		return false, nil
	case runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_REJECTED:
		dead, err := queue.moveToDeadLetter(ctx, id, "rejected", now)
		if err != nil {
			return false, err
		}
		return dead, nil
	case runtimev1.EventDeliveryStatus_EVENT_DELIVERY_STATUS_UNAVAILABLE:
		return queue.markUnavailable(ctx, id, now)
	default:
		return false, fmt.Errorf("stele: unknown delivery status %d for event %s", int32(result), id)
	}
}

func (queue *Queue) markUnavailable(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := queue.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("stele: begin unavailable mark: %w", err)
	}
	var attempts int
	var createdAt string
	if err := tx.QueryRowContext(ctx, `SELECT attempts, created_at FROM events_outbox WHERE id=?`, id).Scan(&attempts, &createdAt); err != nil {
		_ = tx.Rollback()
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("stele: load event for retry: %w", err)
	}
	attempts++
	ageExhausted := false
	if created, err := time.Parse(stateTimeLayout, createdAt); err == nil {
		ageExhausted = now.Sub(created) >= maxEventAge
	}
	if attempts >= maxDeliveryAttempts || ageExhausted {
		// 行数据齐全后走死信迁移；事务里先读全列再删除。
		var event QueuedEvent
		var receivedAt string
		err := tx.QueryRowContext(ctx, `SELECT
			id, source_kind, source_id, credential_id, credential_snapshot_version, event_type, received_at, payload
			FROM events_outbox WHERE id=?`, id).Scan(
			&event.ID, &event.SourceKind, &event.SourceID, &event.CredentialID, &event.CredentialSnapshotVersion, &event.EventType, &receivedAt, &event.Payload)
		if err != nil {
			_ = tx.Rollback()
			if err == sql.ErrNoRows {
				return false, nil
			}
			return false, fmt.Errorf("stele: load exhausted event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dead_letters(
			id, source_kind, source_id, credential_id, credential_snapshot_version, event_type, payload, reason, attempts, first_received_at, dead_at
		) VALUES (?,?,?,?,?,?,?,'exhausted',?,?,?)`,
			event.ID, event.SourceKind, event.SourceID, event.CredentialID, event.CredentialSnapshotVersion, event.EventType, event.Payload,
			attempts, receivedAt, formatStateTime(now)); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("stele: dead-letter exhausted event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events_outbox WHERE id=?`, id); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("stele: delete exhausted event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("stele: commit exhausted mark: %w", err)
		}
		return true, nil
	}
	backoff := retryBackoffBase << uint(attempts-1)
	if backoff > retryBackoffCap || backoff <= 0 {
		backoff = retryBackoffCap
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events_outbox SET attempts=?, next_retry_at=? WHERE id=?`,
		attempts, formatStateTime(now.Add(backoff)), id); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("stele: schedule retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("stele: commit retry mark: %w", err)
	}
	return false, nil
}

func (queue *Queue) moveToDeadLetter(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	tx, err := queue.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("stele: begin dead-letter: %w", err)
	}
	var event QueuedEvent
	var receivedAt string
	err = tx.QueryRowContext(ctx, `SELECT
		id, source_kind, source_id, credential_id, credential_snapshot_version, event_type, received_at, payload, attempts
		FROM events_outbox WHERE id=?`, id).Scan(
		&event.ID, &event.SourceKind, &event.SourceID, &event.CredentialID, &event.CredentialSnapshotVersion, &event.EventType, &receivedAt, &event.Payload, &event.Attempts)
	if err != nil {
		_ = tx.Rollback()
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("stele: load rejected event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dead_letters(
		id, source_kind, source_id, credential_id, credential_snapshot_version, event_type, payload, reason, attempts, first_received_at, dead_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		event.ID, event.SourceKind, event.SourceID, event.CredentialID, event.CredentialSnapshotVersion, event.EventType, event.Payload,
		reason, event.Attempts, receivedAt, formatStateTime(now)); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("stele: dead-letter rejected event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events_outbox WHERE id=?`, id); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("stele: delete rejected event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("stele: commit dead-letter: %w", err)
	}
	return true, nil
}

// QueueDepth 返回当前待转发事件数（含退避中的）。
func (queue *Queue) QueueDepth(ctx context.Context) (int, error) {
	var depth int
	if err := queue.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_outbox`).Scan(&depth); err != nil {
		return 0, fmt.Errorf("stele: count queue depth: %w", err)
	}
	return depth, nil
}

// DeadLetter 是一条死信的管理面投影（ADR-0011：超限/超龄/被拒事件保留原文
// 可重放；本结构与重放出口兑现该承诺）。
type DeadLetter struct {
	ID                        string
	SourceKind                string
	SourceID                  int64
	CredentialID              int64
	CredentialSnapshotVersion uint64
	EventType                 string
	Payload                   []byte
	Reason                    string
	Attempts                  int
	FirstReceivedAt           time.Time
	DeadAt                    time.Time
}

// DeadLetterFilter 是死信列表的可选过滤（零值不过滤）。
type DeadLetterFilter struct {
	SourceKind string
	Reason     string
	Limit      int
}

// ListDeadLetters 按 dead_at 倒序列出死信（管理面只读；Limit<=0 时默认 50，
// 上限 500，避免一次性捞出全部原文）。
func (queue *Queue) ListDeadLetters(ctx context.Context, filter DeadLetterFilter) ([]DeadLetter, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query := `SELECT id, source_kind, source_id, credential_id, credential_snapshot_version,
		event_type, payload, reason, attempts, first_received_at, dead_at
		FROM dead_letters`
	var clauses []string
	var args []any
	if filter.SourceKind != "" {
		clauses = append(clauses, "source_kind = ?")
		args = append(args, filter.SourceKind)
	}
	if filter.Reason != "" {
		clauses = append(clauses, "reason = ?")
		args = append(args, filter.Reason)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY dead_at DESC, id LIMIT ?"
	args = append(args, limit)
	rows, err := queue.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("stele: list dead letters: %w", err)
	}
	defer rows.Close()
	var letters []DeadLetter
	for rows.Next() {
		var letter DeadLetter
		var firstReceivedAt, deadAt string
		if err := rows.Scan(&letter.ID, &letter.SourceKind, &letter.SourceID, &letter.CredentialID,
			&letter.CredentialSnapshotVersion, &letter.EventType, &letter.Payload, &letter.Reason,
			&letter.Attempts, &firstReceivedAt, &deadAt); err != nil {
			return nil, fmt.Errorf("stele: scan dead letter: %w", err)
		}
		if letter.FirstReceivedAt, err = time.Parse(stateTimeLayout, firstReceivedAt); err != nil {
			return nil, fmt.Errorf("stele: parse first_received_at: %w", err)
		}
		if letter.DeadAt, err = time.Parse(stateTimeLayout, deadAt); err != nil {
			return nil, fmt.Errorf("stele: parse dead_at: %w", err)
		}
		letters = append(letters, letter)
	}
	return letters, rows.Err()
}

// ReplayDeadLetters 把选中的死信在单事务内重新入队（attempts 复位、立即到
// 期）并删除死信行；event_id 主键使重复重放必然冲突而不是静默双写。
// credentialID/snapshotVersion 指定重放使用的凭据：轮换场景下原凭据已吊销，
// 操作者必须显式给出当前有效凭据（来自 Quoin 告警源详情）；传 0 则沿死信
// 行原凭据（Quoin 侧仍会按当前有效性裁决，无效会再次被拒并回到死信）。
func (queue *Queue) ReplayDeadLetters(ctx context.Context, ids []string, credentialID int64, snapshotVersion uint64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := queue.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("stele: begin dead-letter replay: %w", err)
	}
	replayed := 0
	for _, id := range ids {
		var letter DeadLetter
		var receivedAt string
		err := tx.QueryRowContext(ctx, `SELECT id, source_kind, source_id, credential_id, credential_snapshot_version,
			event_type, payload, first_received_at FROM dead_letters WHERE id=?`, id).
			Scan(&letter.ID, &letter.SourceKind, &letter.SourceID, &letter.CredentialID, &letter.CredentialSnapshotVersion,
				&letter.EventType, &letter.Payload, &receivedAt)
		if err != nil {
			_ = tx.Rollback()
			if err == sql.ErrNoRows {
				return replayed, fmt.Errorf("stele: dead letter %q not found (%d already replayed)", id, replayed)
			}
			return replayed, fmt.Errorf("stele: load dead letter: %w", err)
		}
		useCredential, useSnapshot := credentialID, snapshotVersion
		if useCredential == 0 {
			useCredential, useSnapshot = letter.CredentialID, letter.CredentialSnapshotVersion
		}
		if useCredential <= 0 {
			_ = tx.Rollback()
			return replayed, fmt.Errorf("stele: dead letter %q has no usable credential; pass an explicit valid credential id", id)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events_outbox(
			id, source_kind, source_id, credential_id, credential_snapshot_version,
			event_type, received_at, payload, state, attempts, next_retry_at, created_at
		) VALUES (?,?,?,?,?,?,?,?,'pending',0,NULL,?)`,
			letter.ID, letter.SourceKind, letter.SourceID, useCredential, useSnapshot,
			letter.EventType, receivedAt, letter.Payload, formatStateTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return replayed, fmt.Errorf("stele: re-enqueue dead letter %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dead_letters WHERE id=?`, id); err != nil {
			_ = tx.Rollback()
			return replayed, fmt.Errorf("stele: delete replayed dead letter %q: %w", id, err)
		}
		replayed++
	}
	if err := tx.Commit(); err != nil {
		return replayed, fmt.Errorf("stele: commit dead-letter replay: %w", err)
	}
	return replayed, nil
}

// AddRateCounters 把一个连接的 allowed/denied 增量累加进 rate_counters。
func (queue *Queue) AddRateCounters(ctx context.Context, connectionID int64, allowed, denied int64) error {
	if allowed == 0 && denied == 0 {
		return nil
	}
	if _, err := queue.db.ExecContext(ctx, `INSERT INTO rate_counters(connection_id, allowed_total, denied_total, updated_at)
		VALUES(?,?,?,?)
		ON CONFLICT(connection_id) DO UPDATE SET
			allowed_total=allowed_total+excluded.allowed_total,
			denied_total=denied_total+excluded.denied_total,
			updated_at=excluded.updated_at`,
		connectionID, allowed, denied, formatStateTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("stele: accumulate rate counters: %w", err)
	}
	return nil
}

// RateCounter 读回一个连接的累计计数（观测/测试用）。
func (queue *Queue) RateCounter(ctx context.Context, connectionID int64) (allowed, denied int64, err error) {
	err = queue.db.QueryRowContext(ctx,
		`SELECT allowed_total, denied_total FROM rate_counters WHERE connection_id=?`, connectionID).
		Scan(&allowed, &denied)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("stele: read rate counters: %w", err)
	}
	return allowed, denied, nil
}
