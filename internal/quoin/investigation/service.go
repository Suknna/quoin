// Package investigation owns the Investigation aggregate (DATA-INVEST-001..005):
// the atomic first-message creation, head-fenced sends, the single active
// attempt invariant, assistant-message commit adjudication, the frozen
// investigation_v1 input projection and the transient model-delta feed that
// the ui-message-stream HTTP surface consumes. The package is the only
// product write path for investigations and their messages/attempts.
//
// 用户命令（创建/发送/停止/重试/撤销/附件暂存）通过共享执行器 execution 执行
// （ADR-0006）：会话复核、幂等重放、业务修改、命令台账与审计事件由执行器在
// 同一事务统一提交；client_commands 持久台账取代旧的进程内 replay 表。生命周期
// 非用户变更（结果裁决、接受、取消确认）走 Execute 显式任务作用域，关联从
// attempt 行的持久关联恢复（attempt.LoadCorrelation），不伪造身份。模块不自管
// 命令事务、不写 audit INSERT；读路径走组合层注入的窄化 reader（SetReader）。
package investigation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// SchemaKind is the frozen input schema identifier for investigation
// attempt snapshots (DATA-ATTEMPT-001: attempt_type + "_v1").
const SchemaKind = "investigation_v1"

// OutputSchemaKind is the frozen result payload schema identifier
// (RUNTIME-TASK-012).
const OutputSchemaKind = "investigation_output_v1"

// RendererVersion identifies the investigation input renderer generation
// (ARCH-CONTEXT-006). v2 renders the frozen integrations for blank-key
// (source-level) attempts (ADR-0004); v3 additionally renders Quoin's own
// recent alert history (frozen lineage); v5 is the ADR-0012 alert-
// normalization shape — the history is scoped to the correlated views of
// the session's occurrence sources (same 24h window as initial_analysis),
// occurrence sources render the frozen normalized semantics, and the
// business_systems context channel is gone with the retired domain.
// 首发无历史 attempt：没有旧 renderer 分叉。
const RendererVersion = "investigation-renderer-v5"

// AgentVersion is the frozen investigation agent generation recorded on
// the attempt row; the worker binary pins its own copy equal to this.
// The v3 executor identity is unchanged by the ADR-0012 input reshaping:
// 工具目录与执行语义不变，仅输入快照形状随 renderer-v5 演进。
const AgentVersion = "investigation-v3"

// Stable operation identities. The user-command names double as the audit
// actions the runner persists automatically, matching the previous manual
// audit vocabulary; the ledger command_type matches them, so a replayed
// command always resolves to the same operation.
const (
	CommandCreate     = "investigation.create"
	CommandSend       = "investigation.message.send"
	CommandStop       = "investigation.attempt.stop"
	CommandRetry      = "investigation.attempt.retry"
	CommandUndo       = "investigation.message.undo"
	CommandAttachment = "investigation.attachment.stage"

	// Lifecycle facts: each durable transition is one audited system
	// operation executed by the task executor; bounded by the state machine.
	commandAssistantCommitted = "investigation.assistant_committed"
	commandAttemptFailed      = "investigation.attempt_failed"
	commandRecoveryLoss       = "investigation.recovery_loss_committed"
	commandAttemptAccepted    = "investigation.attempt.accepted"
	commandAttemptCancelAck   = "investigation.attempt.cancel_ack"

	// ObjectInvestigation is the audit/ledger domain object type.
	ObjectInvestigation = "investigation"
	// ObjectAttachment is the staging command's domain object type.
	ObjectAttachment = "text_attachment"
)

// Errors the HTTP surface maps onto the frozen status codes.
var (
	ErrNotFound             = errors.New("investigation not found")
	ErrModelProviderMissing = errors.New("no enabled qualified model provider")
	ErrCommandReused        = errors.New("client command id reused with a different request")
	ErrActiveAttempt        = errors.New("an active attempt already owns the investigation")
	ErrLateResult           = errors.New("result lost the commit-order race")
	ErrSourceNotFound       = errors.New("investigation source not found")
	ErrInvalidSource        = errors.New("investigation source invalid")
	ErrMessageInvalid       = errors.New("message content invalid")
)

// HeadConflictError reports a stale expected_head_message_id fence miss
// (DATA-INVEST-001); the HTTP surface maps it to the frozen HeadConflict
// envelope (code=head_conflict).
type HeadConflictError struct {
	CurrentHead *int64 // nil = no head (all messages withdrawn)
}

func (err *HeadConflictError) Error() string {
	if err.CurrentHead == nil {
		return "investigation head is null"
	}
	return "investigation head is " + strconv.FormatInt(*err.CurrentHead, 10)
}

// SourceInput is one immutable provenance reference carried by the create
// command (HTTP: InvestigationSourceInput).
type SourceInput struct {
	Type     string // occurrence | initial_analysis | evidence | inspection_report
	SourceID int64
}

// CreateResult carries the durable ids of one created investigation turn.
type CreateResult struct {
	InvestigationID int64
	MessageID       int64
	AttemptID       int64
}

// SendResult carries the durable ids of one appended turn.
type SendResult struct {
	MessageID int64
	AttemptID int64
}

// Service is the investigation authority.
type Service struct {
	db       *sql.DB
	attempts *attempt.Service
	evidence *evidence.Service
	now      func() time.Time
	// runner 是本族共享的执行器：操作注册进私有注册表，命令写入只经执行器
	// 的受守卫事务（台账+审计同事务提交），业务代码拿不到提交权。纯读统一走
	// runner.Reader()：组合层未注入只读池时全部失败关闭（绝不回退写池）。
	runner               *execution.Runner
	opCreate             *execution.Operation
	opSend               *execution.Operation
	opStop               *execution.Operation
	opRetry              *execution.Operation
	opUndo               *execution.Operation
	opStage              *execution.Operation
	opAssistantCommitted *execution.Operation
	opAttemptFailed      *execution.Operation
	opRecoveryLoss       *execution.Operation
	opAttemptAccepted    *execution.Operation
	opAttemptCancelAck   *execution.Operation

	// streamMu guards the transient delta feeds (one per attempt while an
	// observer exists).
	streamMu sync.Mutex
	streams  map[int64]*feed
	// attachmentMu guards the staging dependency and the message-level
	// attachment boundary (wired once at startup by the app layer).
	attachmentMu sync.Mutex
	attachments  *artifact.Store
	// wiredReader remembers the reader accepted by SetReader purely so a
	// capability wired later (SetAttachmentStore) can share it; it is never
	// a read fallback — all reads go through runner.Reader().
	wiredReader     audit.Reader
	attachmentLimit int64
	// stagedMu guards the bounded in-process idempotency ledger of staging
	// commands ((principal, client_command_id) → attachment id). Staging
	// stays in-memory until the transient staged-body handle/seal flow is
	// redesigned for the durable ledger — its reveal handle must never enter
	// a persisted command payload.
	stagedMu sync.Mutex
	staged   map[string]stagedReplay
}

// NewService builds the investigation service and wires the deterministic
// input rebuilder plus the tool observation hooks (grant
// resolution/validation for thanos_query, deterministic Evidence) into the
// shared attempt machine. The runner owns the writable database; reads go
// through the narrow reader seam until the composition layer injects a real
// read-only reader via SetReader (the pool is the compatible default).
func NewService(db *sql.DB) *Service {
	now := func() time.Time { return time.Now().UTC() }
	service := &Service{
		db:              db,
		attempts:        attempt.NewService(db),
		now:             now,
		streams:         map[int64]*feed{},
		staged:          map[string]stagedReplay{},
		attachmentLimit: DefaultAttachmentLimitBytes,
	}
	service.attempts.SnapshotRebuilder = service.RebuildInput
	service.evidence = evidence.NewService(db)
	service.evidence.RegisterProjector(thanos.QueryToolName, thanos.EvidenceFor)
	service.attempts.ToolGrantResolver = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool attempt.ToolDef) (attempt.ToolResolution, error) {
		switch tool.Name {
		case thanos.QueryToolName:
			// ResolveQueryGrant returns the full resolution (grants + preflight).
			return thanos.ResolveQueryGrant(ctx, conn, attemptID, toolCallID)
		default:
			return attempt.ToolResolution{}, errors.New("tool " + tool.Name + " has no grant resolver")
		}
	}
	service.attempts.ToolGrantValidator = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool attempt.ToolDef) error {
		switch tool.Name {
		case thanos.QueryToolName:
			return thanos.ValidateGrantForExecution(ctx, conn, attemptID, toolCallID)
		default:
			return errors.New("tool " + tool.Name + " has no grant validator")
		}
	}
	service.attempts.EvidenceWriter = service.evidence.WriteForToolCall
	service.runner = execution.NewRunnerWithClock(db, execution.NewRegistry(), audit.NewWriterWithClock(now), now)
	service.registerOperations()
	return service
}

// registerOperations declares every active investigation mutation. There is
// no bypass list: an undeclared operation cannot execute, and each write
// operation carries its authorization callback, re-verified inside the
// runner-owned transaction before replay lookup and business execution.
// Registration failures are declaration conflicts — programming errors that
// must surface at startup, so they panic here like the compose default.
func (service *Service) registerOperations() {
	register := func(op execution.Operation) *execution.Operation {
		declared, err := service.runner.Register(op)
		if err != nil {
			panic("investigation: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	user := execution.Operation{Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: authorizeInvestigationUser}
	service.opCreate = register(execution.Operation{Name: CommandCreate, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opSend = register(execution.Operation{Name: CommandSend, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opStop = register(execution.Operation{Name: CommandStop, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opRetry = register(execution.Operation{Name: CommandRetry, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opUndo = register(execution.Operation{Name: CommandUndo, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opStage = register(execution.Operation{Name: CommandAttachment, Class: execution.ClassWrite, ObjectType: ObjectAttachment, Authorize: authorizeInvestigationUser})
	service.opAssistantCommitted = register(execution.Operation{Name: commandAssistantCommitted, Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: requireSystemLifecycle})
	service.opAttemptFailed = register(execution.Operation{Name: commandAttemptFailed, Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: requireSystemLifecycle})
	service.opRecoveryLoss = register(execution.Operation{Name: commandRecoveryLoss, Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: requireSystemLifecycle})
	service.opAttemptAccepted = register(execution.Operation{Name: commandAttemptAccepted, Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: requireSystemLifecycle})
	service.opAttemptCancelAck = register(execution.Operation{Name: commandAttemptCancelAck, Class: execution.ClassWrite, ObjectType: ObjectInvestigation, Authorize: requireSystemLifecycle})
}

// SetReader installs the composition layer's real read-only query surface
// (execution.OpenReadOnly / Database.Reader). Validation and ownership live
// in the runner: it probes PRAGMA query_only and refuses the writable pool,
// so a wiring gap fails closed instead of silently reading (and contending)
// on the writer. The same reader is forwarded to every child capability of
// this family — the shared attempt machine, the evidence authority and an
// already-wired attachment store — so their standalone reads share the one
// read-only capability. A store wired later is covered by SetAttachmentStore.
func (service *Service) SetReader(reader audit.Reader) error {
	if err := service.runner.SetReader(reader); err != nil {
		return fmt.Errorf("investigation: install read-only reader: %w", err)
	}
	if err := service.attempts.SetReader(reader); err != nil {
		return fmt.Errorf("investigation: forward read-only reader to attempts: %w", err)
	}
	if err := service.evidence.SetReader(reader); err != nil {
		return fmt.Errorf("investigation: forward read-only reader to evidence: %w", err)
	}
	service.attachmentMu.Lock()
	defer service.attachmentMu.Unlock()
	service.wiredReader = reader
	if service.attachments != nil {
		return service.attachments.SetReader(reader)
	}
	return nil
}

// Attempts exposes the shared attempt state machine to the runtime slice.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// Evidence exposes the evidence authority to the app layer (read paths).
func (service *Service) Evidence() *evidence.Service { return service.evidence }

// DB exposes the product database to the app layer for read-only routing
// queries (attempt type lookups etc.).
func (service *Service) DB() *sql.DB { return service.db }

func (service *Service) nowText() string { return service.now().Format(time.RFC3339Nano) }

// writer is the sealed write surface the mutation helpers compose on: an
// alias of execution.Executor, so only the runner's guarded *execution.Tx
// satisfies it. A raw database or connection can never compose a write;
// pure reads take audit.Reader instead.
type writer = execution.Executor

// authorizeInvestigationUser 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，任意已认证已初始化用户）：会话存在、未撤销、未过期、
// 仍处签发时的 auth_revision 且主体启用。调查归属创建者，任何角色均可发命令；
// 证明缺失或已失效返回 ErrActorChanged——干净回滚、不持久化任何记录。
func authorizeInvestigationUser(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "")
}

// requireSystemLifecycle confines lifecycle mutations (result adjudication,
// accept, cancel ack, recovery loss) to the system task executor arriving
// through a trusted background source. A user or service context — in
// particular anything from a client channel — can never drive the lifecycle,
// and there is no session fallback to fake.
func requireSystemLifecycle(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("investigation: lifecycle operation requires the system principal")
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return errors.New("investigation: lifecycle operation cannot arrive from the http channel")
	}
	return nil
}

// lifecycleContext resolves the task scope for one lifecycle mutation:
// inherited when the caller already carries execution metadata, restored from
// the attempt row's persisted correlation (ADR-0006) when a restart lost the
// request scope, or established fresh for a legacy correlation-less row — a
// deliberate recovery operation under its own task identity, never a
// fabricated link.
func (service *Service) lifecycleContext(ctx context.Context, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, service.runner.Reader(), attemptID)
	if err != nil {
		return nil, err
	}
	actor := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	source := execution.Source{Kind: execution.SourceTask}
	if found && restorableCorrelation(correlation) {
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         actor,
			Initiator: execution.Principal{
				Kind: execution.PrincipalKind(correlation.InitiatorType),
				ID:   correlation.InitiatorID,
			},
			Source: source,
		})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         actor,
		Source:        source,
	})
}

// restorableCorrelation reports whether a persisted attempt correlation is
// complete enough for a restart to reattach (a NULL legacy column set is a
// no-correlation fact that is never guessed).
func restorableCorrelation(correlation attempt.Correlation) bool {
	if correlation.OperationCorrelationID == "" {
		return false
	}
	switch execution.PrincipalKind(correlation.InitiatorType) {
	case execution.PrincipalSystem:
		return correlation.InitiatorID == 0
	case execution.PrincipalUser, execution.PrincipalService:
		return correlation.InitiatorID > 0
	default:
		return false
	}
}
