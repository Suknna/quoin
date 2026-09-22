package knowledge

// Knowledge domain (T27): Knowledge Candidates created or returned from
// the three immutable diagnosis sources, revisioned drafts, single
// confirmation producing the first immutable KnowledgeVersion, exclusion,
// and the read projections for the knowledge workbench. The candidate
// state machine, immutability and concurrency fences are the frozen
// schema's; this service only performs the deterministic transitions
// (DATA-KNOWLEDGE-001..008).
//
// 用户命令（创建/编辑/确认/排除/修订/停用复用/导入/批次）通过共享执行器
// execution 执行（ADR-0006）：会话复核（auth.VerifyExecutionSession，任意
// 已认证角色）、幂等重放、业务修改、命令台账与审计由执行器在同一事务统一
// 提交。模块不再自管事务、不写 audit INSERT、不持有拒绝记录路径；缺失合法
// 执行上下文或会话证明时失败关闭。读路径走窄化的 audit.Reader 查询面。
// 抽取结果等后台 Attempt 生命周期走 Execute（系统主体），关联从持久化
// Attempt 恢复（attempt.LoadCorrelation），不伪造身份。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/knowledge/embedding"
	"github.com/Suknna/quoin/internal/quoin/knowledge/retrieval"
)

// MutationActor is the legacy write-parameter shape kept for call
// compatibility (app handlers and cross-package tests). ID names the acting
// principal and must equal the context actor (the runner enforces this);
// AuthRevision is superseded by the context's session proof reference —
// in-transaction re-authorization verifies the session, not this field.
type MutationActor struct {
	ID           int64
	AuthRevision int64
}

// queryer 是只读查询面：*sql.DB、*sql.Conn、*sql.Tx 与执行器事务都满足，
// 使同一扫描实现既可跑在执行器事务内，也可跑在只读读源上。
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// writer 是只写执行面：执行器事务与普通连接都满足。
// writer is the sealed mutation-helper surface: only the runner's guarded
// transaction (or a composition writer that provably implements the full
// guarded Executor) may mutate through it.
type writer = execution.Executor

// Candidate states (knowledge_candidates.state CHECK).
const (
	StateAwaiting      = "AwaitingConfirmation"
	StateConfirmed     = "Confirmed"
	StateExcluded      = "Excluded"
	StateSuperseded    = "Superseded"
	StateSourceInvalid = "SourceInvalid"
)

// Candidate source types (knowledge_candidates.source_type CHECK); the
// import-batch and revision flows add 'source_material' / 'knowledge_version'.
const (
	SourceAnalysisOutput = "initial_analysis_output"
	SourceReport         = "inspection_report"
	SourceMessage        = "investigation_message"
)

// 执行器操作（审计 action 与命令台账 command_type）。
const (
	opCreate        = "knowledge_candidate.create"
	opEdit          = "knowledge_candidate.draft_edit"
	opConfirm       = "knowledge_candidate.confirm"
	opExclude       = "knowledge_candidate.exclude"
	opCreateRevison = "knowledge_revision.create"
	opStopReuse     = "knowledge_version.stop_reuse"
	opImport        = "knowledge_import.create"
	opBatchConfirm  = "knowledge_import.confirm"
	opBatchCancel   = "knowledge_import.cancel"

	opExtractionCommit    = "knowledge_extraction.commit"
	opExtractionFail      = "knowledge_extraction.fail"
	opExtractionReject    = "knowledge_extraction.reject"
	opExtractionInterrupt = "knowledge_extraction.interrupt"
	opSearchRebuild       = "knowledge_search.rebuild"
)

// 执行器确定性拒绝 code（持久化于台账拒绝载荷，重放时映射回模块类型化错误）。
const (
	codeRevisionConflict   = "revision_conflict"
	codeRowVersionConflict = "row_version_conflict"
	codeStateConflict      = "state_conflict"
	codeSourceRejected     = "source_rejected"
	codeSourceShape        = "source_shape"
	codeNotFound           = "not_found"
	codeEmptyEdit          = "empty_edit"
	codePartialBatch       = "partial_batch_confirm"
)

var (
	// ErrNotFound maps to 404.
	ErrNotFound = errors.New("knowledge object not found")
	// ErrCommandReused maps to 409 command_id_reused.
	ErrCommandReused = errors.New("client command id reused with a different request")
	// ErrActorRevoked marks a session proof that no longer verifies inside the
	// runner transaction (revoked, expired, drifted revision or lost role).
	ErrActorRevoked = errors.New("acting user is no longer authorized")
	// ErrSourceRejected maps to 409: the source has a rejected feedback
	// event and can no longer create candidates (DATA-KNOWLEDGE-006).
	ErrSourceRejected = errors.New("rejected diagnosis source cannot create a knowledge candidate")
	// ErrSourceShape maps to 422: the source reference is not an immutable
	// active assistant output of the named scope.
	ErrSourceShape = errors.New("knowledge candidate source must be an immutable diagnosis output")
)

// RevisionConflict reports a stale expected draft revision (409) with the
// authoritative current revision for the conflict envelope.
type RevisionConflict struct {
	Current int64
}

func (err *RevisionConflict) Error() string {
	return "candidate draft revision is stale"
}

// RowVersionConflict reports a stale expected row version (409).
type RowVersionConflict struct {
	Current int64
}

func (err *RowVersionConflict) Error() string {
	return "candidate row version is stale"
}

// StateConflict reports an operation against a candidate whose state no
// longer permits it (409).
type StateConflict struct {
	State string
}

func (err *StateConflict) Error() string {
	return "candidate state no longer permits this operation"
}

// CandidateSummary is the wire projection of one candidate.
type CandidateSummary struct {
	ID                   string          `json:"id"`
	SourceType           string          `json:"sourceType"`
	SourceID             string          `json:"sourceId"`
	State                string          `json:"state"`
	RowVersion           int64           `json:"rowVersion"`
	Generation           int64           `json:"generation"`
	DraftRevision        int64           `json:"draftRevision"`
	DraftTitle           string          `json:"draftTitle,omitempty"`
	DraftBody            string          `json:"draftBody,omitempty"`
	DraftScope           json.RawMessage `json:"draftScope,omitempty"`
	TargetKnowledgeID    string          `json:"targetKnowledgeId,omitempty"`
	ConfirmedKnowledgeID string          `json:"confirmedKnowledgeId,omitempty"`
	// BatchState 是导入批次候选的批次围栏投影：终态批次（取消/完成）的
	// 候选不能再编辑或确认——与写路径的 SQL 谓词同一事实。非批次候选为空。
	BatchState string `json:"batchState,omitempty"`
}

// CandidateDetail adds the immutable original model suggestion.
type CandidateDetail struct {
	CandidateSummary
	OriginalSuggestion json.RawMessage `json:"originalSuggestion"`
}

// KnowledgeSummary is the browse/detail projection of one Reusable
// Knowledge aggregate.
type KnowledgeSummary struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	CurrentVersionID  string `json:"currentVersionId"`
	CurrentVersionSeq int64  `json:"currentVersionSeq"`
	Eligible          bool   `json:"eligible"`
	RowVersion        int64  `json:"rowVersion"`
}

// KnowledgeDetail is KnowledgeSummary plus the bounded version count.
type KnowledgeDetail struct {
	KnowledgeSummary
	VersionCount int64 `json:"versionCount"`
}

// VersionSummary is one immutable version row.
type VersionSummary struct {
	ID                       string `json:"id"`
	VersionSeq               int64  `json:"versionSeq"`
	Title                    string `json:"title"`
	SourceCandidateID        string `json:"sourceCandidateId"`
	EmbeddingState           string `json:"embeddingState"`
	CreatedAt                string `json:"createdAt"`
	Eligible                 bool   `json:"eligible"`
	RetrievalStateRowVersion int64  `json:"retrievalStateRowVersion"`
}

// VersionDetail is the full immutable version body.
type VersionDetail struct {
	ID                       string          `json:"id"`
	VersionSeq               int64           `json:"versionSeq"`
	Title                    string          `json:"title"`
	Body                     string          `json:"body"`
	Scope                    json.RawMessage `json:"scope,omitempty"`
	Conditions               json.RawMessage `json:"conditions,omitempty"`
	Limitations              json.RawMessage `json:"limitations,omitempty"`
	SourceCandidateID        string          `json:"sourceCandidateId"`
	CreatedAt                string          `json:"createdAt"`
	Eligible                 bool            `json:"eligible"`
	RetrievalStateRowVersion int64           `json:"retrievalStateRowVersion"`
	EmbeddingState           string          `json:"embeddingState"`
	ExitedAt                 string          `json:"exitedAt,omitempty"`
	ExitReason               string          `json:"exitReason,omitempty"`
}

// Service owns the knowledge domain writes and reads. 用户命令只经 runner
// （Run），定位只读查询经 Reader()（组合层注入的真实只读池）；写权威仅作
// 构造期组合依赖保留（子聚合绑定），不再对外再暴露。
type Service struct {
	reader     audit.Reader
	writer     *sql.DB
	runner     *execution.Runner
	now        func() time.Time
	attempts   *attempt.Service
	embeddings *embedding.Service
	semantic   *retrieval.Service

	create          *execution.Operation
	edit            *execution.Operation
	confirm         *execution.Operation
	exclude         *execution.Operation
	createRevision  *execution.Operation
	stopReuse       *execution.Operation
	startImport     *execution.Operation
	confirmBatch    *execution.Operation
	cancelBatch     *execution.Operation
	extractionDone  *execution.Operation
	extractionFail  *execution.Operation
	extractionDrop  *execution.Operation
	extractionBreak *execution.Operation
	searchRebuild   *execution.Operation
}

// NewService builds the knowledge domain service. Knowledge extraction owns a
// typed Plinth attempt, so its rebuilder belongs to this aggregate rather than
// a catch-all runtime switch; embedding attempts share the same attempt
// machine with their own typed rebuilder branch. 组合层迁移前的兼容构造：
// reader 与执行器共用同一个可写数据库句柄。
func NewService(db *sql.DB) *Service {
	// The compose-time default is a DECLARED domain with no reader: pure
	// reads fail closed (zero-value execution.Reader, never the writer)
	// until composition calls SetReader with the trusted pool. This path
	// must never panic at app construction.
	service, err := newKnowledgeService(execution.Reader{}, db, execution.NewRunner(db, execution.NewRegistry(), nil), false)
	if err != nil {
		// Unreachable with wireChild=false (no child validation runs); kept
		// total for the declared error contract.
		panic("knowledge: compose default service: " + err.Error())
	}
	return service
}

// NewServiceWithReader 装配模块：reader 是读路径查询面（组合层的真实只读
// 能力），writer 是子聚合（attempt/embedding/retrieval）构造所需的写权威
// 组合依赖——它只流入构造，不再经公共访问器外泄；runner 是组合层共享的执
// 行器（操作注册进其注册表，同名重复注册即失败）。
func NewServiceWithReader(reader audit.Reader, writer *sql.DB, runner *execution.Runner) (*Service, error) {
	if reader == nil {
		return nil, errors.New("knowledge: read capability is required")
	}
	if writer == nil {
		return nil, errors.New("knowledge: composition writer is required")
	}
	if runner == nil {
		return nil, errors.New("knowledge: command runner is required")
	}
	// Validate the trusted pool through the runner's own check first: an
	// untrusted or zero reader is rejected before any child is constructed.
	if err := runner.SetReader(reader); err != nil {
		return nil, err
	}
	return newKnowledgeService(reader, writer, runner, true)
}

// newKnowledgeService is the single private builder. wireChild forwards the
// reader to the attempt child only when the pool is trusted; the unwired
// compose path constructs the domain fail-closed instead of panicking.
func newKnowledgeService(reader audit.Reader, writer *sql.DB, runner *execution.Runner, wireChild bool) (*Service, error) {
	service := &Service{reader: reader, writer: writer, runner: runner, now: time.Now}
	service.attempts = attempt.NewService(writer)
	if wireChild {
		if err := service.attempts.SetReader(reader); err != nil {
			return nil, err
		}
	}
	service.attempts.SnapshotRebuilder = service.rebuildAttemptInput
	embeddings, err := embedding.NewServiceWithReader(reader, writer, runner)
	if err != nil {
		return nil, err
	}
	service.embeddings = embeddings
	service.semantic = retrieval.NewService(writer)
	// targetVersion 在同一事务内重读对象行版本，令审计目标引用钉住适用版本
	// （DATA-AUDIT-001）。
	candidateVersion := func(ctx context.Context, tx *execution.Tx, objectID int64) (int64, bool) {
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT row_version FROM knowledge_candidates WHERE id=?`, objectID).Scan(&version); err != nil {
			return 0, false
		}
		return version, true
	}
	batchVersion := func(ctx context.Context, tx *execution.Tx, objectID int64) (int64, bool) {
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT row_version FROM knowledge_import_batches WHERE id=?`, objectID).Scan(&version); err != nil {
			return 0, false
		}
		return version, true
	}
	retrievalVersion := func(ctx context.Context, tx *execution.Tx, objectID int64) (int64, bool) {
		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT row_version FROM knowledge_version_retrieval_state WHERE knowledge_version_id=?`, objectID).Scan(&version); err != nil {
			return 0, false
		}
		return version, true
	}
	register := func(name, objectType string, authorize func(context.Context, *execution.Tx) error, targetVersion func(context.Context, *execution.Tx, int64) (int64, bool)) *execution.Operation {
		op, err := runner.Register(execution.Operation{Name: name, Class: execution.ClassWrite, ObjectType: objectType, Authorize: authorize, TargetVersion: targetVersion})
		if err != nil {
			panic("knowledge: register " + name + ": " + err.Error())
		}
		return op
	}
	user := func(ctx context.Context, tx *execution.Tx) error { return authorizeKnowledgeWriter(ctx, tx) }
	worker := func(ctx context.Context, tx *execution.Tx) error { return authorizeExtractionWorker(ctx, tx) }
	maintenance := func(ctx context.Context, tx *execution.Tx) error { return authorizeKnowledgeMaintenance(ctx, tx) }
	service.create = register(opCreate, "knowledge_candidate", user, candidateVersion)
	service.edit = register(opEdit, "knowledge_candidate", user, candidateVersion)
	service.confirm = register(opConfirm, "knowledge_candidate", user, candidateVersion)
	service.exclude = register(opExclude, "knowledge_candidate", user, candidateVersion)
	service.createRevision = register(opCreateRevison, "knowledge_candidate", user, candidateVersion)
	service.stopReuse = register(opStopReuse, "knowledge_version", user, retrievalVersion)
	service.startImport = register(opImport, "knowledge_import_batch", user, batchVersion)
	service.confirmBatch = register(opBatchConfirm, "knowledge_import_batch", user, batchVersion)
	service.cancelBatch = register(opBatchCancel, "knowledge_import_batch", user, batchVersion)
	service.extractionDone = register(opExtractionCommit, "knowledge_import_batch", worker, batchVersion)
	service.extractionFail = register(opExtractionFail, "knowledge_import_batch", worker, batchVersion)
	service.extractionDrop = register(opExtractionReject, "knowledge_import_batch", worker, batchVersion)
	service.extractionBreak = register(opExtractionInterrupt, "knowledge_import_batch", worker, batchVersion)
	service.searchRebuild = register(opSearchRebuild, "knowledge_search_docs", maintenance, nil)
	return service, nil
}

// authorizeKnowledgeWriter 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，任意已认证角色——管理员与操作员都可驱动知识
// 工作台）。证明缺失或已失效返回 ErrActorChanged，干净回滚且不留任何痕迹；
// 绝不退化为仅查 users 行。
func authorizeKnowledgeWriter(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "")
}

// authorizeExtractionWorker 复核抽取结果应用步骤的系统主体身份；权威裁决
// 是业务阶段内的 attempt 冻结身份（attempt id、boot、epoch）。
func authorizeExtractionWorker(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem {
		return fmt.Errorf("knowledge: actor kind %q may not apply extraction results", meta.Actor.Kind)
	}
	return nil
}

// authorizeKnowledgeMaintenance 允许系统主体执行派生投影维护；若未来由
// 管理端触发，用户主体必须持有有效的会话证明。
func authorizeKnowledgeMaintenance(ctx context.Context, tx *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind == execution.PrincipalSystem {
		return nil
	}
	return auth.VerifyExecutionSession(ctx, tx, "")
}

// Embeddings exposes the semantic index projection service (sweeps, query
// embeds and state resolution).
func (service *Service) Embeddings() *embedding.Service {
	return service.embeddings
}

// Semantic exposes the semantic search channel reader.
func (service *Service) Semantic() *retrieval.Service {
	return service.semantic
}

// rebuildAttemptInput routes the frozen input rebuild by attempt type:
// knowledge_extraction rebuilds the import snapshot, embedding rebuilds the
// embedding_v1 envelope.
func (service *Service) rebuildAttemptInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var attemptType string
	if err := service.reader.QueryRowContext(ctx, `SELECT attempt_type FROM execution_attempts WHERE id=?`, attemptID).Scan(&attemptType); err != nil {
		return nil, err
	}
	if attemptType == "embedding" {
		return service.embeddings.RebuildInput(ctx, attemptID)
	}
	return service.RebuildImportInput(ctx, attemptID)
}

// Attempts exposes the typed import attempt state machine to the runtime.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// Reader is the runtime's read-only locator authority (frozen dispatch
// locators, correlation dispatch): it serves the composition-injected
// read-only pool. Knowledge domain reads use the same seam; no writer is
// re-exposed.
func (service *Service) Reader() audit.Reader { return service.reader }

// SetReader upgrades an unwired domain to the trusted read-only pool and
// forwards it to the attempt child. Accepts only the trusted execution.Reader.
func (service *Service) SetReader(reader execution.Reader) error {
	service.reader = reader
	return service.attempts.SetReader(reader)
}

func (service *Service) nowText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

func validSourceType(sourceType string) bool {
	switch sourceType {
	case SourceAnalysisOutput, SourceReport, SourceMessage:
		return true
	}
	return false
}

// commandDigest wraps the durable ledger digest helper.
func commandDigest(commandType string, fields map[string]any) string {
	return auth.DigestCommand(commandType, fields)
}

// createReplayPayload carries the original create outcome (the summary
// plus whether the command created the candidate) for exact replay. The
// field names are the frozen ledger payload shape.
type createReplayPayload struct {
	Summary CandidateSummary `json:"summary"`
	Created bool             `json:"created"`
}

// translateCommandError 把执行器的确定性结果映射回模块的类型化错误，保持
// HTTP 问题映射与既有调用方输出不变。revision/row-version 冲突的权威当前
// 值从对象行重读——重放返回同一 409 类别与最新权威版本。统一执行器之前的
// 旧拒绝行携带域自己的载荷形状（kind/current/state/description），从
// Rejection.Raw 解码并保留同样的已知类别，绝不静默错判为命令冲突。
func (service *Service) translateCommandError(ctx context.Context, err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		switch rejection.Code {
		case codeRevisionConflict:
			current, _ := service.currentDraftRevision(ctx, rejection.ObjectID)
			return &RevisionConflict{Current: current}
		case codeRowVersionConflict:
			current, _ := service.currentRowVersion(ctx, rejection.ObjectID)
			return &RowVersionConflict{Current: current}
		case codeStateConflict:
			return &StateConflict{State: rejection.Detail}
		case codeSourceRejected:
			return ErrSourceRejected
		case codeSourceShape:
			return ErrSourceShape
		case codeNotFound:
			return ErrNotFound
		case codeEmptyEdit:
			return ErrEmptyEdit
		case codePartialBatch:
			return ErrPartialBatchConfirm
		default:
			if legacy := decodeLegacyRejection(rejection.Raw); legacy != nil {
				return legacy
			}
			return ErrCommandReused
		}
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	return err
}

// legacyRejectionPayload 是统一执行器之前本模块持久化的拒绝载荷形状。
type legacyRejectionPayload struct {
	Kind        string `json:"kind"`
	Current     int64  `json:"current,omitempty"`
	State       string `json:"state,omitempty"`
	Status      int    `json:"status"`
	Description string `json:"description"`
}

// decodeLegacyRejection 解码旧拒绝载荷为同样的类型化错误；kind 未知的旧
// 记录如实返回其记录描述， nil 表示载荷根本不是旧形状。
func decodeLegacyRejection(raw string) error {
	if raw == "" {
		return nil
	}
	var recorded legacyRejectionPayload
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil || recorded.Kind == "" {
		return nil
	}
	switch recorded.Kind {
	case codeRevisionConflict:
		return &RevisionConflict{Current: recorded.Current}
	case codeRowVersionConflict:
		return &RowVersionConflict{Current: recorded.Current}
	case codeStateConflict:
		return &StateConflict{State: recorded.State}
	case codeSourceRejected:
		return ErrSourceRejected
	case codeSourceShape:
		return ErrSourceShape
	case codeNotFound:
		return ErrNotFound
	case codeEmptyEdit:
		return ErrEmptyEdit
	default:
		return fmt.Errorf("recorded rejection: %s", recorded.Description)
	}
}

func (service *Service) currentDraftRevision(ctx context.Context, candidateID int64) (int64, error) {
	var revision int64
	err := service.reader.QueryRowContext(ctx, `SELECT draft_revision FROM knowledge_candidates WHERE id=?`, candidateID).Scan(&revision)
	return revision, err
}

func (service *Service) currentRowVersion(ctx context.Context, objectID int64) (int64, error) {
	var rowVersion int64
	err := service.reader.QueryRowContext(ctx, `SELECT row_version FROM knowledge_candidates WHERE id=?`, objectID).Scan(&rowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.reader.QueryRowContext(ctx, `SELECT row_version FROM knowledge_import_batches WHERE id=?`, objectID).Scan(&rowVersion)
	}
	return rowVersion, err
}
