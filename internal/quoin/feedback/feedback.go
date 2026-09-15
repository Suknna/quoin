package feedback

// Diagnosis feedback ledger (T27, DATA-KNOWLEDGE-008): an append-only
// timeline bound to exactly the three immutable diagnosis outputs —
// Initial Analysis outputs, Inspection Report versions and assistant
// Investigation messages. Every event carries a closed value and an
// optional bounded note; the read side returns the full paginated history
// plus a latestValue projection derived from the last committed event.
// A `rejected` event applies DATA-TX-011 in the same transaction: the
// source's still-operable candidates become SourceInvalid and every
// KnowledgeVersion produced from that source exits retrieval (with the
// FTS5 projection row deleted by the schema trigger).
//
// 追加命令通过共享执行器 execution 执行（ADR-0006）：会话复核（任意已认证
// 角色）、幂等重放、业务修改、命令台账与审计事件由执行器在同一事务统一
// 提交。模块不再自管事务、不写 audit INSERT、不持有拒绝记录路径；缺失合法
// 执行上下文或会话证明时命令失败关闭。读路径走 execution.OpenReadOnly 产生
// 的可信只读面（audit.Reader 查询形状）；组合层未接线时读全部失败关闭，
// 可写连接从不充当读源。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/knowledge/invalidation"
)

// Target types (diagnosis_feedback.target_type CHECK).
const (
	TargetAnalysisOutput = "initial_analysis_output"
	TargetReport         = "inspection_report"
	TargetMessage        = "investigation_message"
)

// Feedback values (diagnosis_feedback.value CHECK).
const (
	ValueAdopted           = "adopted"
	ValueExecuted          = "executed"
	ValueVerifiedEffective = "verified_effective"
	ValueRejected          = "rejected"
)

const (
	CommandAppend = "diagnosis_feedback.append"

	// ObjectFeedbackEvent 是审计与命令结果的领域对象类型：追加命令自己的
	// 产物（反馈事件行），与命令台账 result_object 引用一致。
	ObjectFeedbackEvent = "diagnosis_feedback"

	// 确定性拒绝 code（持久化于台账拒绝载荷，重放时映射回对外哨兵错误）。
	rejectionTargetMissing = "feedback_target_missing"
	rejectionTargetInvalid = "feedback_target_invalid"
)

const noteLimit = 4096

var (
	// ErrNotFound maps to 404: the target object does not exist.
	ErrNotFound = errors.New("feedback target not found")
	// ErrInvalidTarget maps to 422: the shape is not one of the three
	// immutable diagnosis outputs (for example a user message).
	ErrInvalidTarget = errors.New("feedback target must be an immutable diagnosis output or assistant message")
	// ErrInvalidValue maps to 422: a value outside the closed set.
	ErrInvalidValue = errors.New("feedback value is not one of the closed set")
	// ErrCommandReused maps to 409 command_id_reused (HTTP-COMMAND-003).
	ErrCommandReused = errors.New("client command id reused with a different request")
	// ErrNoteTooLong maps to 422.
	ErrNoteTooLong = errors.New("feedback note exceeds 4096 characters")
)

// Target is one immutable diagnosis output.
type Target struct {
	Type string
	ID   int64
}

// Event is one append-only feedback record (FeedbackSummary). Locator ids
// serialize as the frozen string LocatorId shape.
type Event struct {
	ID         string `json:"id"`
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId"`
	Value      string `json:"value"`
	Note       string `json:"note,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// Cursor is the keyset cursor for the timeline (createdAt DESC, id DESC).
type Cursor struct {
	CreatedAt string
	ID        int64
}

// Timeline is the read projection: full history page plus the latestValue
// derived from the last committed event (never a second write source).
type Timeline struct {
	LatestValue string
	Items       []Event
	Next        *Cursor
}

// Service owns the diagnosis feedback ledger.
type Service struct {
	// reader 是执行器验证后的可信只读面（execution.Reader）：组合层未接线时
	// 保持零值，全部读取失败关闭——可写连接从不充当读源。
	reader execution.Reader
	// runner 是组合层共享的执行器；操作注册进共享注册表。
	runner *execution.Runner
	append *execution.Operation
	now    func() time.Time
}

// NewService 是组合层初始装配（应用构造在 configureReadOnly 之前）：操作
// 注册进私有执行器，读路径保持零值可信只读面——所有读取失败关闭，直到
// 组合层通过 NewServiceWithReader 或 SetReader 注入真实只读能力。db 绝不
// 充当读源。
func NewService(db *sql.DB) *Service {
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := newService(runner.Reader(), runner)
	if err != nil {
		// 新注册表上的首次声明不会失败；此分支只为编程错误兜底。
		panic("feedback: compose default service: " + err.Error())
	}
	return service
}

// NewServiceWithReader 装配模块：reader 经共享执行器的 SetReader 原地验证
// ——仅接受 execution.OpenReadOnly 产生的可信类型，随后采纳执行器的
// runner.Reader()；任意 audit.Reader 适配器（包括裸可写 db）连同 nil 一起
// 被拒绝。runner 是组合层共享的执行器（操作注册进其注册表，同名重复注册
// 即失败）。
func NewServiceWithReader(reader audit.Reader, runner *execution.Runner) (*Service, error) {
	if runner == nil {
		return nil, errors.New("feedback: command runner is required")
	}
	if err := runner.SetReader(reader); err != nil {
		return nil, fmt.Errorf("feedback: %w", err)
	}
	return newService(runner.Reader(), runner)
}

// SetReader 把真实只读能力转发给共享执行器验证，并采纳验证后的读面：默认
// 构造的服务由此从失败关闭转为可读。仅接受 execution.OpenReadOnly 产生的
// 可信类型，不存在可写兼容适配器。
func (service *Service) SetReader(reader audit.Reader) error {
	if err := service.runner.SetReader(reader); err != nil {
		return fmt.Errorf("feedback: %w", err)
	}
	service.reader = service.runner.Reader()
	return nil
}

// newService 是两个公开构造共享的私有装配：把模块自有操作注册进 runner 的
// 共享注册表并完成字段装配，reader 由调用方先行验证为可信只读面。
func newService(reader execution.Reader, runner *execution.Runner) (*Service, error) {
	if runner == nil {
		return nil, errors.New("feedback: command runner is required")
	}
	appendOp, err := runner.Register(execution.Operation{
		Name:       CommandAppend,
		Class:      execution.ClassWrite,
		ObjectType: ObjectFeedbackEvent,
		Authorize:  authorizeFeedbackWriter,
	})
	if err != nil {
		return nil, fmt.Errorf("feedback: register %s: %w", CommandAppend, err)
	}
	return &Service{reader: reader, runner: runner, append: appendOp, now: time.Now}, nil
}

func (service *Service) nowText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

// authorizeFeedbackWriter 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，任意已认证角色）：会话未撤销、未过期、仍处签发时
// 的 auth_revision 且主体启用。证明缺失或已失效返回 ErrActorChanged——
// 干净回滚、不持久化任何记录。
func authorizeFeedbackWriter(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "")
}

func validTargetType(targetType string) bool {
	switch targetType {
	case TargetAnalysisOutput, TargetReport, TargetMessage:
		return true
	}
	return false
}

func validValue(value string) bool {
	switch value {
	case ValueAdopted, ValueExecuted, ValueVerifiedEffective, ValueRejected:
		return true
	}
	return false
}

// targetExists resolves the closed target shape: "missing" (404) for an
// absent row and "invalid" (422) for a user message (DATA-KNOWLEDGE-008).
func targetExists(ctx context.Context, q audit.Reader, target Target) (string, error) {
	switch target.Type {
	case TargetAnalysisOutput:
		found, err := exists(ctx, q, `SELECT 1 FROM initial_analysis_outputs WHERE id=?`, target.ID)
		return presence(found), err
	case TargetReport:
		found, err := exists(ctx, q, `SELECT 1 FROM inspection_reports WHERE id=?`, target.ID)
		return presence(found), err
	case TargetMessage:
		row := q.QueryRowContext(ctx, `SELECT role FROM investigation_messages WHERE id=?`, target.ID)
		var role string
		if err := row.Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "missing", nil
			}
			return "", err
		}
		if role != "assistant" {
			return "invalid", nil
		}
		return "present", nil
	}
	return "invalid", nil
}

func presence(found bool) string {
	if found {
		return "present"
	}
	return "missing"
}

func exists(ctx context.Context, q audit.Reader, query string, id int64) (bool, error) {
	row := q.QueryRowContext(ctx, query, id)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Append adds one event to the immutable timeline (HTTP-COMMAND-003
// replay through the shared command runner). A rejected event applies
// the DATA-TX-011 source invalidation inside the same transaction.
func (service *Service) Append(ctx context.Context, principalID int64, commandID string, target Target, value, note string) (Event, error) {
	if !validTargetType(target.Type) {
		return Event{}, ErrInvalidTarget
	}
	if !validValue(value) {
		return Event{}, ErrInvalidValue
	}
	if utf8.RuneCountInString(note) > noteLimit {
		return Event{}, ErrNoteTooLong
	}
	digest := auth.DigestCommand(CommandAppend, map[string]any{
		"targetType": target.Type,
		"targetId":   target.ID,
		"value":      value,
		"note":       note,
	})
	outcome, err := execution.Run(ctx, service.runner, service.append, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: commandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Event, execution.Change, error) {
		state, err := targetExists(ctx, tx, target)
		if err != nil {
			return Event{}, execution.Changed, err
		}
		if state != "present" {
			if state == "invalid" {
				return Event{}, execution.Changed, &execution.Rejection{Code: rejectionTargetInvalid, Detail: "反馈目标必须是不可变的诊断输出或助手消息", ObjectID: target.ID}
			}
			return Event{}, execution.Changed, &execution.Rejection{Code: rejectionTargetMissing, Detail: "反馈目标不存在", ObjectID: target.ID}
		}
		now := service.nowText()
		result, err := tx.ExecContext(ctx, `INSERT INTO diagnosis_feedback(target_type,target_id,value,note,created_by,created_at) VALUES(?,?,?,?,?,?)`,
			target.Type, target.ID, value, nullableNote(note), principalID, now)
		if err != nil {
			return Event{}, execution.Changed, err
		}
		eventID, err := result.LastInsertId()
		if err != nil {
			return Event{}, execution.Changed, err
		}
		if value == ValueRejected {
			// DATA-TX-011 in the same runner transaction: operable
			// candidates of the source become SourceInvalid and every
			// version the source produced exits retrieval permanently.
			if _, err := invalidation.Apply(ctx, tx, target.Type, []int64{target.ID}, now); err != nil {
				return Event{}, execution.Changed, err
			}
		}
		// The durable ledger carries the original result payload so a replay
		// returns the committed outcome even after later appends (HTTP-COMMAND-003).
		row := tx.QueryRowContext(ctx, `
			SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
			FROM diagnosis_feedback WHERE id=?`, eventID)
		event, err := scanEvent(row.Scan)
		if err != nil {
			return Event{}, execution.Changed, err
		}
		return event, execution.Changed, nil
	}, func(event Event) int64 { return parseID(event.ID) })
	if err != nil {
		return Event{}, translateRunnerError(err)
	}
	return outcome.Result, nil
}

// translateRunnerError 把执行器的确定性结果映射回模块对外哨兵错误，保持
// knowledge/http.go 的既有 errors.Is 问题映射不变。旧台账的拒绝载荷只记录
// 404 事实（无 code），统一按 ErrNotFound 重放。
func translateRunnerError(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		if rejection.Code == rejectionTargetInvalid {
			return ErrInvalidTarget
		}
		return ErrNotFound
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	return err
}

func nullableNote(note string) any {
	if note == "" {
		return nil
	}
	return note
}

func scanEvent(scan func(dest ...any) error) (Event, error) {
	var id, targetID, createdBy int64
	var event Event
	if err := scan(&id, &event.TargetType, &targetID, &event.Value, &event.Note, &createdBy, &event.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Event{}, ErrNotFound
		}
		return Event{}, err
	}
	event.ID = strconv.FormatInt(id, 10)
	event.TargetID = strconv.FormatInt(targetID, 10)
	if createdBy != 0 {
		event.CreatedBy = strconv.FormatInt(createdBy, 10)
	}
	return event, nil
}

// List returns one page of the append-only history (createdAt DESC, id
// DESC keyset) and the latestValue projection of the last committed event.
func (service *Service) List(ctx context.Context, target Target, after *Cursor, limit int) (Timeline, error) {
	if !validTargetType(target.Type) {
		return Timeline{}, ErrInvalidTarget
	}
	var (
		rows *sql.Rows
		err  error
	)
	nextLimit := limit + 1
	if after == nil {
		rows, err = service.reader.QueryContext(ctx, `
			SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
			FROM diagnosis_feedback WHERE target_type=? AND target_id=?
			ORDER BY created_at DESC, id DESC LIMIT ?`, target.Type, target.ID, nextLimit)
	} else {
		rows, err = service.reader.QueryContext(ctx, `
			SELECT id,target_type,target_id,value,COALESCE(note,''),COALESCE(created_by,0),created_at
			FROM diagnosis_feedback WHERE target_type=? AND target_id=?
			AND (created_at < ? OR (created_at = ? AND id < ?))
			ORDER BY created_at DESC, id DESC LIMIT ?`,
			target.Type, target.ID, after.CreatedAt, after.CreatedAt, after.ID, nextLimit)
	}
	if err != nil {
		return Timeline{}, err
	}
	defer rows.Close()
	items := make([]Event, 0, limit)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TargetType, &event.TargetID, &event.Value, &event.Note, &event.CreatedBy, &event.CreatedAt); err != nil {
			return Timeline{}, err
		}
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		return Timeline{}, err
	}
	var next *Cursor
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = &Cursor{CreatedAt: last.CreatedAt, ID: parseID(last.ID)}
	}
	var latestValue string
	if len(items) > 0 && after == nil {
		// The first page's newest item is the last committed event for
		// this target (the keyset order equals commit order).
		latestValue = items[0].Value
	}
	return Timeline{LatestValue: latestValue, Items: items, Next: next}, nil
}

func parseID(value string) int64 {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// TrimNote is a defensive bound used by the HTTP layer before Append
// (characters, matching the schema's length() semantics).
func TrimNote(note string) string {
	note = strings.TrimSpace(note)
	if count := utf8.RuneCountInString(note); count > noteLimit {
		return string([]rune(note)[:noteLimit])
	}
	return note
}
