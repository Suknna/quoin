// Package analysis owns the Initial Analysis aggregate (DATA-ANALYSIS-001/
// 002): creation against one alert occurrence, the one-active invariant,
// attempt fan-out, technical-failure retry, the cancellation fence and the
// atomic seal of the first legal result. The package is the only product
// write path for initial_analyses and its attempt fan-out; the HTTP surface
// and the runtime control stream both call through here.
package analysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// SchemaKind is the frozen input schema identifier for initial-analysis
// attempt snapshots (DATA-ATTEMPT-001: attempt_type + "_v1").
const SchemaKind = "initial_analysis_v1"

// OutputSchemaKind is the frozen result payload schema identifier
// (RUNTIME-TASK-012).
const OutputSchemaKind = "initial_analysis_output_v1"

// RendererVersion identifies the input renderer generation both sides
// agree on (ARCH-CONTEXT-006).
// Renderer v5 is the ADR-0012 alert-normalization shape: the occurrence
// carries the frozen unified semantics (severity/title/annotations/resource),
// first-observation enrichment fields, view correlations and a related-alert
// window (same correlated views or same source, 24h before first
// observation); the business_systems declaration context is gone with the
// retired domain. Renderer v6 是知识接入代：消息形状不变，冻结工具目录内容
// 新增知识检索工具（输入正文随目录内容演进，digest 覆盖）。
// 首发无历史 attempt：没有旧 renderer 分叉。
const RendererVersion = "initial-analysis-renderer-v6"

// Errors the HTTP surface maps onto the frozen status codes.
var (
	ErrNotFound              = errors.New("initial analysis not found")
	ErrModelProviderMissing  = errors.New("no enabled qualified model provider")
	ErrActiveConflict        = errors.New("initial analysis is not retryable or the fence lost the race")
	ErrCommandReplayMismatch = errors.New("client command id is already bound to a different analysis operation or target")
	ErrNoOutput              = errors.New("initial analysis has no sealed output")
	ErrLateResult            = errors.New("result proposal lost the commit-order race")
	ErrOutputSealed          = errors.New("initial analysis already sealed an output")
)

// RowVersionError reports a stale expected_row_version fence miss.
type RowVersionError struct {
	Current int64
}

func (err *RowVersionError) Error() string {
	return fmt.Sprintf("analysis row version is %d", err.Current)
}

// Service is the analysis authority.
type Service struct {
	db       *sql.DB
	attempts *attempt.Service
	evidence *evidence.Service
	now      func() time.Time
	// ProjectTerminalOutcome receives the terminal execution sequence while
	// the runner's guarded transaction is still open. It projects independent
	// platform facts atomically with the authoritative attempt transition.
	ProjectTerminalOutcome func(ctx context.Context, tx TxWriter, commitSequence int64, succeeded bool, termination string) error
	// runner 是本族共享的执行器：操作注册进私有注册表，命令写入只经执行器
	// 的受守卫事务（台账+审计同事务提交），业务代码拿不到提交权。纯读统一走
	// runner.Reader()：组合层未注入只读池时全部失败关闭（绝不回退写池）。
	opCreate      *execution.Operation
	opRetry       *execution.Operation
	opCancel      *execution.Operation
	opAccepted    *execution.Operation
	opCancelAck   *execution.Operation
	opInterrupted *execution.Operation
	opCancelled   *execution.Operation
	opSucceeded   *execution.Operation
	opFailed      *execution.Operation
	runner        *execution.Runner
}

// TxWriter is the exported structural transaction surface for composition
// hooks (the platform-fault projection) invoked on the runner's guarded
// transaction. *execution.Tx and *sql.Conn both satisfy it, so the app
// layer's hook can forward to the alerts projector without either package
// importing the other.
type TxWriter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Stable operation identities. The user-command names double as the audit
// actions the runner persists automatically, matching the previous manual
// audit vocabulary; the ledger command_type matches them, so a replayed
// command always resolves to the same operation.
const (
	CommandCreate = "initial_analysis.create"
	CommandRetry  = "initial_analysis.retry"
	CommandCancel = "initial_analysis.cancel"

	// Lifecycle facts: each durable transition is one audited system
	// operation executed by the task executor; bounded by the state machine.
	commandAccepted  = "initial_analysis.accepted"
	commandCancelAck = "initial_analysis.cancel_ack"
	commandInterrupt = "initial_analysis.interrupted"
	commandCancelled = "initial_analysis.cancelled"
	commandSucceeded = "initial_analysis.succeeded"
	commandFailed    = "initial_analysis.failed"

	// ObjectAnalysis is the audit/ledger domain object type.
	ObjectAnalysis = "initial_analysis"
)

// NewService builds the analysis service on the product database and
// wires the deterministic input rebuilder and the tool observation hooks
// (grant resolution/validation for thanos_query, deterministic Evidence)
// into the shared attempt machine. The runner owns the writable database;
// every pure read goes through the runner's fail-closed read seam until the
// composition layer injects a real read-only pool via SetReader — reads
// never fall back to the writable pool.
func NewService(db *sql.DB) *Service {
	now := func() time.Time { return time.Now().UTC() }
	service := &Service{
		db:       db,
		attempts: attempt.NewService(db),
		now:      now,
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
			return attempt.ToolResolution{}, fmt.Errorf("tool %s has no grant resolver", tool.Name)
		}
	}
	service.attempts.ToolGrantValidator = func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool attempt.ToolDef) error {
		switch tool.Name {
		case thanos.QueryToolName:
			return thanos.ValidateGrantForExecution(ctx, conn, attemptID, toolCallID)
		default:
			return fmt.Errorf("tool %s has no grant validator", tool.Name)
		}
	}
	service.attempts.EvidenceWriter = service.evidence.WriteForToolCall
	service.registerOperations(execution.NewRunnerWithClock(db, execution.NewRegistry(), audit.NewWriterWithClock(now), now))
	return service
}

// registerOperations declares every active analysis mutation. There is no
// bypass list: an undeclared operation cannot execute, and each write
// operation carries its authorization callback, re-verified inside the
// runner-owned transaction before replay lookup and business execution.
func (service *Service) registerOperations(runner *execution.Runner) {
	service.runner = runner
	register := func(op execution.Operation) *execution.Operation {
		declared, err := runner.Register(op)
		if err != nil {
			// A declaration conflict is a programming error that must
			// surface at startup, never at first use.
			panic("analysis: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	user := execution.Operation{Class: execution.ClassWrite, ObjectType: ObjectAnalysis, Authorize: authorizeAnalysisUser}
	service.opCreate = register(execution.Operation{Name: CommandCreate, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opRetry = register(execution.Operation{Name: CommandRetry, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	service.opCancel = register(execution.Operation{Name: CommandCancel, Class: user.Class, ObjectType: user.ObjectType, Authorize: user.Authorize})
	lifecycle := execution.Operation{Class: execution.ClassWrite, ObjectType: ObjectAnalysis, Authorize: requireSystemLifecycle}
	service.opAccepted = register(execution.Operation{Name: commandAccepted, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
	service.opCancelAck = register(execution.Operation{Name: commandCancelAck, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
	service.opInterrupted = register(execution.Operation{Name: commandInterrupt, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
	service.opCancelled = register(execution.Operation{Name: commandCancelled, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
	service.opSucceeded = register(execution.Operation{Name: commandSucceeded, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
	service.opFailed = register(execution.Operation{Name: commandFailed, Class: lifecycle.Class, ObjectType: lifecycle.ObjectType, Authorize: lifecycle.Authorize})
}

// SetReader installs the composition layer's real read-only query surface
// (execution.OpenReadOnly / Database.Reader). Validation and ownership live
// in the runner: it probes PRAGMA query_only and refuses the writable pool,
// so a wiring gap fails closed instead of silently reading (and contending)
// on the writer. The same reader is forwarded to every child capability of
// this family — the shared attempt machine and the evidence authority — so
// their standalone reads share the one read-only capability.
func (service *Service) SetReader(reader audit.Reader) error {
	if err := service.runner.SetReader(reader); err != nil {
		return fmt.Errorf("analysis: install read-only reader: %w", err)
	}
	if err := service.attempts.SetReader(reader); err != nil {
		return fmt.Errorf("analysis: forward read-only reader to attempts: %w", err)
	}
	return service.evidence.SetReader(reader)
}

// authorizeAnalysisUser 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，任意已认证已初始化用户）：会话存在、未撤销、未过期、
// 仍处签发时的 auth_revision 且主体启用。证明缺失或已失效返回
// ErrActorChanged——干净回滚、不持久化任何记录。
func authorizeAnalysisUser(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "")
}

// requireSystemLifecycle confines lifecycle mutations (accept, cancel ack,
// loss convergence) to the system task executor arriving through a trusted
// background source. A user or service context — in particular anything from
// a client channel — can never drive the lifecycle, and there is no session
// fallback to fake.
func requireSystemLifecycle(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("analysis: lifecycle operation requires the system principal")
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return errors.New("analysis: lifecycle operation cannot arrive from the http channel")
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

// Attempts exposes the shared attempt state machine to the runtime slice.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// Evidence exposes the evidence authority to the app layer (read paths
// and the deterministic projector registry).
func (service *Service) Evidence() *evidence.Service { return service.evidence }

// Reader serves the app layer's read-only routing queries (attempt type
// lookups etc.) through the injected bootstrap read-only pool; unwired it
// fails closed.
func (service *Service) Reader() audit.Reader { return service.runner.Reader() }

func (service *Service) nowText() string { return service.now().Format(time.RFC3339Nano) }

// writer is the sealed write surface the mutation helpers compose on: an
// alias of execution.Executor, so only the runner's guarded *execution.Tx
// satisfies it. A raw database or connection can never compose a write;
// pure reads take audit.Reader instead.
type writer = execution.Executor

// Input is the rendered, immutable input of one analysis. The frozen
// integrations are the attempt's source-level read-only authority
// (ADR-0004); the occurrence carries the ADR-0012 normalized semantics.
type Input struct {
	Occurrence    OccurrenceContext     `json:"occurrence"`
	Integrations  []RenderedIntegration `json:"integrations,omitempty"`
	ModelContract ModelContract         `json:"modelContract"`
	// ToolCatalog is the attempt's frozen model tool catalog (ADR-0004);
	// the snapshot digest covers it via this embedding.
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
}

// RenderedIntegration is one admin-enabled integration frozen into the
// attempt input as its source-level read-only authority. Kind is
// "metrics" (Prometheus/Thanos); credentials never appear.
type RenderedIntegration struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// OccurrenceContext is the frozen alert context the model receives
// (ADR-0012): the unified semantics (severity/title/resource), the frozen
// canonical annotations, the first-observation enrichment fields, the view
// correlations and the related-alert window are all first-observation
// frozen facts; state stays a live read like the historical shape.
type OccurrenceContext struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	Severity        string            `json:"severity"`
	Title           string            `json:"title"`
	Resource        string            `json:"resource,omitempty"`
	FirstSeenAt     string            `json:"firstSeenAt"`
	LastStateChange string            `json:"lastStateChangeAt"`
	ResolvedAt      *string           `json:"resolvedAt,omitempty"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	// Enrichment 是首观测富化终值（命中规则叠加后的 fields）。
	Enrichment map[string]string `json:"enrichment,omitempty"`
	// Correlations 是首观测命中的业务视图快照（多命中全记录）。
	Correlations []RenderedCorrelation `json:"correlations,omitempty"`
	// RelatedAlerts 是相关告警窗口：同关联视图或同 source、首观测前 24h
	// 内的最近 10 条其它 occurrence（创建时冻结为谱系项）。
	RelatedAlerts []RenderedRelatedAlert `json:"relatedAlerts,omitempty"`
}

// RenderedCorrelation is one frozen view-correlation snapshot of the
// occurrence (ADR-0012 Correlate 段：视图改名/退役后不漂移).
type RenderedCorrelation struct {
	ViewKey     string `json:"viewKey"`
	DisplayName string `json:"displayName"`
}

// RenderedRelatedAlert is one related-alert window entry (immutable
// projection facts; state is a live read like the main occurrence).
type RenderedRelatedAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	State    string `json:"state"`
	StartsAt string `json:"startsAt"`
}

// ModelContract is the frozen chat contract of the attempt
// (ARCH-AGENT-003): the worker renders these values, never selects them.
type ModelContract struct {
	ModelID             string `json:"modelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}

// Summary is the list projection (InitialAnalysisSummary).
type Summary struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	RowVersion int64  `json:"rowVersion"`
	CreatedAt  string `json:"createdAt"`
}

// Output is the sealed first successful result (AnalysisOutput).
type Output struct {
	ID          string   `json:"id"`
	ModelID     string   `json:"modelId"`
	Content     string   `json:"content"`
	EvidenceIDs []string `json:"evidenceIds"`
	CreatedAt   string   `json:"createdAt"`
}

// Detail is the get/list create/retry/cancel response projection
// (InitialAnalysisDetail).
type Detail struct {
	Summary
	AttemptCount int64   `json:"attemptCount"`
	Output       *Output `json:"output,omitempty"`
}

// AttemptItem is the listInitialAnalysisAttempts projection (AttemptSummary).
type AttemptItem struct {
	ID                string  `json:"id"`
	Type              string  `json:"type"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	StartedAt         *string `json:"startedAt,omitempty"`
	EndedAt           *string `json:"endedAt,omitempty"`
	TerminationReason *string `json:"terminationReason,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// CreateResult carries the durable ids of one created (or replayed)
// analysis.
type CreateResult struct {
	AnalysisID int64
	AttemptID  int64
}

// Create renders the input snapshot, creates the analysis and its first
// attempt and freezes the chat_model grant in one transaction. A second
// create for the same occurrence returns the active analysis (the unique
// partial index is the authority; DATA-ANALYSIS-001), and a replayed
// client command returns its original record.
func (service *Service) Create(ctx context.Context, occurrenceID, principalID int64, clientCommandID string) (CreateResult, error) {
	return service.create(ctx, occurrenceID, principalID, clientCommandID, "create", 0)
}

// create performs a create or recovery-retry as one durable, replayable
// command: the runner transaction holds the occurrence admission, the active
// invariant, the analysis and attempt creation, the frozen input snapshot and
// the command ledger row plus the automatic audit event. A replayed client
// command returns the stored result; a reused command id for a different
// operation or target carries a different digest and conflicts
// (HTTP-COMMAND-003) as ErrCommandReplayMismatch.
func (service *Service) create(ctx context.Context, occurrenceID, principalID int64, clientCommandID, operation string, targetAnalysisID int64) (CreateResult, error) {
	digest := auth.DigestCommand(operation, map[string]any{
		"occurrenceId":     occurrenceID,
		"targetAnalysisId": targetAnalysisID,
	})
	op := service.opCreate
	if operation == "retry" {
		op = service.opRetry
	}
	outcome, err := execution.Run(ctx, service.runner, op, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (CreateResult, execution.Change, error) {
		var occurrenceIDRow int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&occurrenceIDRow); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return CreateResult{}, execution.Unchanged, ErrNotFound
			}
			return CreateResult{}, execution.Unchanged, err
		}
		var activeID int64
		activeErr := tx.QueryRowContext(ctx, `SELECT id FROM initial_analyses WHERE occurrence_id=? AND state IN ('Queued','Running')`, occurrenceID).Scan(&activeID)
		if activeErr == nil {
			var attemptID int64
			if attemptErr := tx.QueryRowContext(ctx, `SELECT id FROM execution_attempts WHERE scope_type='analysis' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, activeID).Scan(&attemptID); attemptErr != nil {
				return CreateResult{}, execution.Unchanged, attemptErr
			}
			// The one-active invariant answered the command durably before:
			// the existing analysis is the result, and this command created
			// nothing of its own.
			return CreateResult{AnalysisID: activeID, AttemptID: attemptID}, execution.Unchanged, nil
		}
		if !errors.Is(activeErr, sql.ErrNoRows) {
			return CreateResult{}, execution.Unchanged, activeErr
		}
		input, _, selected, err := service.renderInput(ctx, tx, occurrenceID)
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		// Freeze THIS attempt's tool catalog at creation: the identical document
		// travels in the digested input and in attempt_input_snapshots.
		catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(service.attempts.Catalogs, attempt.AgentVersion)
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		input.ToolCatalog = catalog
		canonical, err := json.Marshal(input)
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		digest := sha256.Sum256(canonical)
		digestHex := hex.EncodeToString(digest[:])
		now := service.nowText()
		analysisInsert, err := tx.ExecContext(ctx, `
			INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_by,created_at)
			VALUES(?,?,?,?,?)`, occurrenceID, "Queued", digestHex, principalID, now)
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		analysisID, err := analysisInsert.LastInsertId()
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		attemptID, err := insertAttempt(ctx, tx, analysisID, digestHex, input, selected, now, string(catalogDocument))
		if err != nil {
			return CreateResult{}, execution.Unchanged, err
		}
		return CreateResult{AnalysisID: analysisID, AttemptID: attemptID}, execution.Changed, nil
	}, func(result CreateResult) int64 { return result.AnalysisID })
	if err != nil {
		if errors.Is(err, execution.ErrCommandReused) {
			return CreateResult{}, ErrCommandReplayMismatch
		}
		return CreateResult{}, err
	}
	return outcome.Result, nil
}

// provider is the resolved enabled model provider contract for one attempt.
type provider struct {
	ConnectionID      int64
	RevisionID        int64
	CredentialGen     int64
	ProbeResultID     int64
	ChatModelID       string
	ContextBudget     int
	MaxOutput         int
	Streaming         bool
	NativeToolCalling bool
}

// renderInput loads the occurrence context (ADR-0012 normalized semantics,
// enrichment, correlations and the related-alert window) and resolves the
// current enabled model provider (ARCH-AGENT-003). No enabled provider is a
// deterministic 503, not a stored analysis.
func (service *Service) renderInput(ctx context.Context, tx audit.Reader, occurrenceID int64) (Input, ModelContract, provider, error) {
	var input Input
	if err := loadOccurrenceContext(ctx, tx, occurrenceID, &input.Occurrence); err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	related, err := selectRelatedAlerts(ctx, tx, &input.Occurrence)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	input.Occurrence.RelatedAlerts = related
	// The enabled integrations are ALWAYS the attempt's source-level
	// authority (ADR-0004); the retired business_systems declaration context
	// no longer participates in the input.
	integrations, err := enabledIntegrations(ctx, tx)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	input.Integrations = integrations
	selected, err := selectModelProvider(ctx, tx)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	contract := ModelContract{
		ModelID:             selected.ChatModelID,
		ContextBudgetTokens: selected.ContextBudget,
		MaxOutputTokens:     selected.MaxOutput,
	}
	input.ModelContract = contract
	return input, contract, selected, nil
}

// enabledIntegrations lists the admin-enabled observation integrations in
// deterministic name order. Each entry freezes the connection's current
// revision when the attempt items are written, so the model-visible source
// authority is exactly the grant-eligible set.
func enabledIntegrations(ctx context.Context, tx audit.Reader) ([]RenderedIntegration, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, type FROM connections
		WHERE type IN ('thanos','prometheus') AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var name, connectionType string
		if err := rows.Scan(&name, &connectionType); err != nil {
			return nil, err
		}
		integrations = append(integrations, RenderedIntegration{Kind: "metrics", Name: name})
	}
	return integrations, rows.Err()
}

// selectModelProvider resolves the single enabled model provider and its
// qualification (DATA-CONN-003: one enabled provider; the explicit
// qualification must close onto the current pair).
func selectModelProvider(ctx context.Context, tx audit.Reader) (provider, error) {
	var selected provider
	var qualificationRowVersion, connectionRowVersion int64
	var probeOutcome string
	err := tx.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       q.probe_result_id, q.enabled_row_version, c.row_version, p.outcome
		FROM connections c
		JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0
		ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen,
			&selected.ProbeResultID, &qualificationRowVersion, &connectionRowVersion, &probeOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return provider{}, ErrModelProviderMissing
	}
	if err != nil {
		return provider{}, err
	}
	if qualificationRowVersion != connectionRowVersion || probeOutcome != "passed" {
		return provider{}, ErrModelProviderMissing
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens,
		       streaming_supported, native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`,
		selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput,
			&selected.Streaming, &selected.NativeToolCalling); err != nil {
		return provider{}, err
	}
	if selected.ChatModelID == "" || !selected.NativeToolCalling {
		return provider{}, ErrModelProviderMissing
	}
	return selected, nil
}

// insertAttempt persists one Queued attempt with its frozen input snapshot,
// input items and chat_model grant (DATA-ATTEMPT-001/002).
func insertAttempt(ctx context.Context, tx writer, analysisID int64, digestHex string, input Input, selected provider, now, toolCatalogJSON string) (int64, error) {
	// CreateOn centrally persists the command's correlation metadata onto
	// the new attempt in this same transaction (ADR-0006); a context
	// without execution metadata fails the creation.
	attemptID, err := attempt.CreateOn(ctx, tx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('initial_analysis','analysis',?,'Queued',?,?,?)`, analysisID, attempt.ReleaseVersion(), attempt.AgentVersion, now)
	if err != nil {
		return 0, err
	}
	snapshotInsert, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at)
		VALUES(?,?,?,?,?,?)`, attemptID, SchemaKind, RendererVersion, digestHex, toolCatalogJSON, now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshotInsert.LastInsertId()
	if err != nil {
		return 0, err
	}
	occurrenceID, err := strconv.ParseInt(input.Occurrence.ID, 10, 64)
	if err != nil {
		return 0, err
	}
	occurrenceDigest := sha256.Sum256([]byte("occurrence:" + input.Occurrence.ID))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id)
		VALUES(?,1,'occurrence',?,?)`, snapshotID, hex.EncodeToString(occurrenceDigest[:]), occurrenceID); err != nil {
		return 0, err
	}
	// ADR-0012：相关告警窗口冻结为谱系项（item_seq 序即窗口顺序），重建按
	// 谱系回读——后续新告警不改变已冻结窗口的集合。
	itemCount := int64(1)
	for _, related := range input.Occurrence.RelatedAlerts {
		itemCount++
		relatedID, parseErr := strconv.ParseInt(related.ID, 10, 64)
		if parseErr != nil {
			return 0, parseErr
		}
		relatedDigest := sha256.Sum256([]byte("occurrence:" + related.ID))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id)
			VALUES(?,?,'related_occurrence',?,?)`, snapshotID, itemCount, hex.EncodeToString(relatedDigest[:]), relatedID); err != nil {
			return 0, err
		}
	}
	// Every attempt freezes the enabled integrations at their current
	// revisions — the authoritative grant-eligible set (ADR-0004).
	if err := insertSourceLineageItems(ctx, tx, snapshotID, itemCount+1, input.Integrations); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		attemptID, "chat_model", selected.ConnectionID, selected.RevisionID, selected.CredentialGen, selected.ProbeResultID, now); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// insertSourceLineageItems freezes one input item per enabled integration at
// its current revision. The revision pointer is the frozen fact: later
// rotations create new revisions, so the attempt's authorized set stays
// reconstructible byte-for-byte (and later grants must match these items).
func insertSourceLineageItems(ctx context.Context, tx writer, snapshotID, firstSeq int64, integrations []RenderedIntegration) error {
	if len(integrations) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT name, current_revision_id FROM connections
		WHERE type IN ('thanos','prometheus','kubernetes') AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`)
	if err != nil {
		return err
	}
	type frozen struct {
		name       string
		revisionID int64
	}
	var frozenIntegrations []frozen
	for rows.Next() {
		var item frozen
		if err := rows.Scan(&item.name, &item.revisionID); err != nil {
			rows.Close()
			return err
		}
		frozenIntegrations = append(frozenIntegrations, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The integration list rendered into the digest and the frozen items must
	// be the same set; a concurrent enablement inside this IMMEDIATE
	// transaction is impossible, so a size mismatch is a programming error.
	if len(frozenIntegrations) != len(integrations) {
		return fmt.Errorf("integration snapshot drift: %d rendered vs %d frozen", len(integrations), len(frozenIntegrations))
	}
	for index, item := range frozenIntegrations {
		role := "metrics_source"
		if integrations[index].Kind == "kubernetes" {
			role = "kubernetes_source"
		}
		digest := sha256.Sum256([]byte("connection-revision:" + strconv.FormatInt(item.revisionID, 10)))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
			VALUES(?,?,?, ?,?)`, snapshotID, firstSeq+int64(index), role, hex.EncodeToString(digest[:]), item.revisionID); err != nil {
			return err
		}
	}
	return nil
}
