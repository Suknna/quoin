// Package inspection owns manual Inspection Run creation, run_check child
// Attempts, PromQL ResultProposal closure, and Run convergence over the frozen
// inspection contracts (CFG-INSPECTRUN-001). Browser children freeze the real
// inspection_collection_v1 journey input; admission/dispatch wiring is added
// by the runtime slices.
//
// 计划/运行/报告命令（ADR-0006）通过共享执行器 execution 执行：会话复核、
// 幂等重放、业务修改、命令台账与审计事件由执行器在同一事务统一提交。模块不再
// 自管事务、不写 audit INSERT、不持有拒绝记录路径；缺失合法执行上下文时命令
// 失败关闭。调度与 Runtime 结果提交在入口显式建立/恢复系统任务上下文，任务
// 关联（attempt 持久关联）由此保留。读路径走窄化的 audit.Reader 查询面，
// 组合层通过 SetReader 注入真实只读能力（bootstrap.Database.Reader）。
package inspection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

var (
	ErrNotFound      = errors.New("inspection run source not found")
	ErrCommandReused = errors.New("client command id reused with a different request")
)

// errResultReplayed marks an identical redelivery of an already-committed
// Runtime result. The durable fact stands from the first commit; the replay
// transaction rolls back (nothing new is recorded — no duplicate success
// audit, ADR-0006) and the public entry observes success.
var errResultReplayed = errors.New("inspection result was already committed")

// Command identities: the durable client_commands.command_type values and the
// automatic audit actions. Historic ledger rows keep replaying because the
// names and request digests are unchanged.
const (
	CommandCreatePlan    = "inspection_plan.create"
	CommandUpdatePlan    = "inspection_plan.update"
	CommandCreateRun     = "inspection_run.create"
	CommandCancelRun     = "inspection_run.cancel"
	CommandRerunRun      = "inspection_run.rerun"
	CommandReanalyzeRun  = "inspection_run.reanalyze"
	CommandScheduleRun   = "inspection_run.schedule"
	commandPluginResult  = "inspection_run.result.plugin"
	commandPromqlResult  = "inspection_run.result.promql"
	commandReportResult  = "inspection_run.result.report"
	commandDefaultPlan   = "inspection_plan.default.ensure"
	commandPromqlGap     = "inspection_run.gap.promql"
	commandCollectionGap = "inspection_run.gap.collection"
)

// Domain object types for the command ledger and the automatic audit.
const (
	ObjectInspectionPlan = "inspection_plan"
	ObjectInspectionRun  = "inspection_run"
)

// inspectionCommandRole is the only role whose verified session may execute
// interactive inspection commands (the admission layer enforces the same
// boundary; this re-check closes the race inside the runner transaction).
const inspectionCommandRole = "admin"

// RejectionError carries a deterministic, command-ledger-recorded rejection.
// It remains this family's public rejection shape; the runner's deterministic
// rejections are translated back to it so HTTP problem mapping is unchanged.
type RejectionError struct {
	Code, Detail, SystemKey string
	ObjectID                int64
}

func (e *RejectionError) Error() string { return e.Detail }

type Service struct {
	db *sql.DB
	// reader 是读路径的窄查询面（audit.Reader）；真实只读能力由组合层通过
	// SetReader 注入，注入前退化为执行器所在的连接池（兼容默认）。
	reader audit.Reader
	now    func() time.Time
	// runner 是本族共享的执行器：操作注册进私有注册表，写入只经执行器的受守卫
	// 事务（台账+审计同事务提交），业务代码拿不到提交权。
	runner *execution.Runner
	// audit 是 runner 的审计写入器（时钟与本族一致）；模块自身不再直接写审计。
	audit          *audit.Writer
	artifactWriter func(context.Context, execution.Executor, int64, []byte) (int64, error)

	createPlan    *execution.Operation
	updatePlan    *execution.Operation
	createRun     *execution.Operation
	scheduleRun   *execution.Operation
	cancelRun     *execution.Operation
	rerunRun      *execution.Operation
	reanalyzeRun  *execution.Operation
	promqlResult  *execution.Operation
	pluginResult  *execution.Operation
	reportResult  *execution.Operation
	promqlGap     *execution.Operation
	collectionGap *execution.Operation
	opDefaultPlan *execution.Operation
}

// NewService assembles the module over db: the pool backs the runner's owned
// transactions. Reads have NO fallback to the write pool — the composition
// layer must inject the real read-only reader via SetReader before any read
// path runs; unset reads fail closed.
func NewService(db *sql.DB) *Service {
	service := &Service{db: db, now: time.Now}
	service.audit = audit.NewWriterWithClock(service.clock)
	service.runner = execution.NewRunnerWithClock(db, execution.NewRegistry(), service.audit, service.clock)
	service.registerOperations()
	return service
}

// readReader returns the injected read-only reader. There is deliberately no
// write-pool fallback: an unset reader is a composition error, not a license
// for reads to carry write capability.
func (s *Service) readReader() (audit.Reader, error) {
	if s.reader != nil {
		return s.reader, nil
	}
	return nil, errors.New("inspection: read-only reader is not configured; wire SetReader before serving reads")
}

// clock keeps ledger and audit timestamps on the service's (deterministic in
// tests) clock even when the field is reassigned after construction.
func (s *Service) clock() time.Time { return s.now() }

// registerOperations declares every active inspection mutation. There is no
// bypass list: an undeclared operation cannot execute, and each write
// operation carries its authorization callback, re-verified inside the
// runner-owned transaction before replay lookup and business execution.
// Registration failures are declaration conflicts — programming errors that
// must surface at startup, so they panic here like the other migrated
// families.
func (s *Service) registerOperations() {
	register := func(op execution.Operation) *execution.Operation {
		declared, err := s.runner.Register(op)
		if err != nil {
			panic("inspection: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	s.createPlan = register(execution.Operation{Name: CommandCreatePlan, Class: execution.ClassWrite, ObjectType: ObjectInspectionPlan, Authorize: authorizeInspectionAdmin})
	s.updatePlan = register(execution.Operation{Name: CommandUpdatePlan, Class: execution.ClassWrite, ObjectType: ObjectInspectionPlan, Authorize: authorizeInspectionAdmin})
	s.createRun = register(execution.Operation{Name: CommandCreateRun, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: authorizeInspectionAdmin})
	s.cancelRun = register(execution.Operation{Name: CommandCancelRun, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: authorizeInspectionAdmin})
	s.rerunRun = register(execution.Operation{Name: CommandRerunRun, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: authorizeInspectionAdmin})
	s.reanalyzeRun = register(execution.Operation{Name: CommandReanalyzeRun, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: authorizeInspectionAdmin})
	// 调度与 Runtime 结果提交是系统工作，绝不允许来自客户端通道，也没有会话
	// 回退可伪造。
	s.scheduleRun = register(execution.Operation{Name: CommandScheduleRun, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSchedulerSource})
	s.promqlResult = register(execution.Operation{Name: commandPromqlResult, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSystemResultWork})
	s.pluginResult = register(execution.Operation{Name: commandPluginResult, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSystemResultWork})
	s.reportResult = register(execution.Operation{Name: commandReportResult, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSystemResultWork})
	s.promqlGap = register(execution.Operation{Name: commandPromqlGap, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSystemResultWork})
	s.collectionGap = register(execution.Operation{Name: commandCollectionGap, Class: execution.ClassWrite, ObjectType: ObjectInspectionRun, Authorize: requireSystemResultWork})
	s.opDefaultPlan = register(execution.Operation{Name: commandDefaultPlan, Class: execution.ClassWrite, ObjectType: ObjectInspectionPlan, Authorize: authorizeInspectionAdmin})
}

// SetReader injects the composition layer's real read-only query surface
// (execution.OpenReadOnly / bootstrap Database.Reader). When the reader is
// the real *sql.DB pool it is verified with the runner's PRAGMA query_only
// probe, so a writable pool can never masquerade as the read seam. Runner
// transactions keep their own guarded writer handle.
func (s *Service) SetReader(reader audit.Reader) error {
	if reader == nil {
		return errors.New("inspection: read-only reader is required")
	}
	if err := s.runner.SetReader(reader); err != nil {
		return err
	}
	s.reader = reader
	return nil
}

// SetArtifactWriter injects the content-addressed Evidence materializer used
// to make frozen report inputs readable without exposing Quoin storage paths.
func (s *Service) SetArtifactWriter(writer func(context.Context, execution.Executor, int64, []byte) (int64, error)) {
	s.artifactWriter = writer
}

// Reader serves the app layer's read-only routing queries through the
// injected bootstrap read-only pool.
func (s *Service) Reader() audit.Reader { return s.reader }

func (s *Service) nowText() string { return s.now().UTC().Format(time.RFC3339Nano) }

// authorizeInspectionAdmin 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，要求 admin 角色）：会话存在、未撤销、未过期、仍处
// 签发时的 auth_revision，且主体是启用的管理员。证明缺失或已失效返回
// ErrActorChanged——干净回滚、不持久化任何记录。
func authorizeInspectionAdmin(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, inspectionCommandRole)
}

// requireSchedulerSource confines scheduled Run creation to the system
// principal arriving through the scheduler entry. The bypass of the user
// session callback is valid only for this declared system operation — a user
// context can never pass, and there is no fallback that lets the system
// principal pose as an admin session.
func requireSchedulerSource(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("inspection: scheduled run requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind != execution.SourceScheduler {
		return fmt.Errorf("inspection: scheduled run requires the %q source, got %q", execution.SourceScheduler, meta.Source.Kind)
	}
	return nil
}

// requireSystemResultWork confines Runtime result adjudication (PromQL,
// plugin, report) to the system task executor arriving through a trusted
// background source. A user or service context — in particular anything from
// a client channel — can never drive result closure, and there is no session
// fallback to fake.
func requireSystemResultWork(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("inspection: result work requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("inspection: result work cannot arrive from the %s channel", meta.Source.Kind)
	}
	return nil
}

// resultContext resolves the task scope for one Runtime result delivery:
// inherited when the caller already carries execution metadata, restored from
// the attempt row's persisted correlation (ADR-0006: the reply joins the
// attempt by id and never trusts a runtime-supplied correlation) when a
// restart lost the request scope, or established fresh for a legacy
// correlation-less row — a deliberate recovery operation under its own task
// identity, never a fabricated link.
func (s *Service) resultContext(ctx context.Context, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, s.db, attemptID)
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
	return execution.ReplaceMetadata(ctx, execution.Metadata{CorrelationID: correlationID, Actor: actor, Source: source})
}

// schedulerContext establishes the system scheduler scope at the entry (each
// due boundary is a new operation with its own correlation); a context that
// already carries metadata is passed through unchanged — the scheduler owns
// no business child that may re-root it.
func (s *Service) schedulerContext(ctx context.Context) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceScheduler},
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

// translateCommandError maps the shared runner's outcomes back to this
// family's stable exported errors: recorded rejections replay as their
// original *RejectionError, and ledger key conflicts keep the historic
// identity. Every other error (missing context, failed session recheck,
// infrastructure) surfaces verbatim — it has no persisted result to cite.
func translateCommandError(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		return &RejectionError{Code: rejection.Code, Detail: rejection.Detail, ObjectID: rejection.ObjectID}
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return ErrCommandReused
	}
	return err
}

// CheckResult is the frozen CheckResultSummary wire union: ok carries only
// the Evidence locator; error/gap carries only the reason.
type CheckResult struct {
	CheckKey   string  `json:"checkKey"`
	Status     string  `json:"status"`
	EvidenceID *string `json:"evidenceId,omitempty"`
	GapReason  *string `json:"gapReason,omitempty"`
}

type RunDetail struct {
	RunID int64  `json:"-"`
	ID    string `json:"id"`
	// BusinessSystemKey 仅历史 Run（旧业务声明计划）携带；计划 Run 为空。
	BusinessSystemKey *string                  `json:"businessSystemKey,omitempty"`
	ConnectionName    *string                  `json:"connectionName,omitempty"`
	PlanKey           string                   `json:"planKey"`
	State             string                   `json:"state"`
	RowVersion        int64                    `json:"rowVersion"`
	TriggerKind       string                   `json:"triggerKind"`
	ScheduledFor      *string                  `json:"scheduledFor,omitempty"`
	EvidenceAt        *string                  `json:"evidenceAt,omitempty"`
	CreatedAt         string                   `json:"createdAt"`
	Checks            []CheckResult            `json:"checks"`
	ReportCount       int                      `json:"reportCount"`
	AnalysisActive    bool                     `json:"analysisActive"`
	LatestAnalysis    *InspectionAttemptStatus `json:"latestAnalysis,omitempty"`
	// FrozenConfig 是 Run 创建时冻结的分析语义投影（名称/检查说明/单位/初始
	// 报告要求）；仅计划 Run 携带，历史声明 Run 不产生该字段。
	FrozenConfig *RunFrozenConfig `json:"frozenConfig,omitempty"`
}

// RunFrozenConfig 是 UI 展示用的 Run 冻结分析语义；与重分析弹框的默认要求
// 同源（Run 冻结列），计划后续修改不改写。
type RunFrozenConfig struct {
	DisplayName        *string `json:"displayName,omitempty"`
	CheckDescription   *string `json:"checkDescription,omitempty"`
	MetricUnit         *string `json:"metricUnit,omitempty"`
	ReportInstructions *string `json:"reportInstructions,omitempty"`
}

// InspectionAttemptStatus is the safe, read-only lifecycle projection for the
// most recent report analysis. It makes failures and cancellations recoverable
// in the Run UI without exposing model input or report content.
type InspectionAttemptStatus struct {
	ID                string  `json:"id"`
	State             string  `json:"state"`
	TerminationReason *string `json:"terminationReason,omitempty"`
}

type ReportSummaryItem struct {
	Version   int64  `json:"version"`
	ModelID   string `json:"modelId"`
	CreatedAt string `json:"createdAt"`
}

type ReportDetail struct {
	// ID is the immutable report locator consumed by diagnosis feedback, unlike
	// the human-facing (runId, version) route locator.
	ID             string   `json:"id"`
	RunID          string   `json:"runId"`
	Version        int64    `json:"version"`
	EvidenceDigest string   `json:"evidenceDigest"`
	EvidenceIDs    []string `json:"evidenceIds"`
	ModelID        string   `json:"modelId"`
	Content        string   `json:"content"`
	CreatedAt      string   `json:"createdAt"`
	// ReportInstructions 是该版本分析实际生效的报告要求投影（Attempt 不可变
	// requirements 优先，Run 冻结初始要求回退）；两者都缺失时不出现。
	ReportInstructions *string `json:"reportInstructions,omitempty"`
}

func locatorID(id int64) string { return strconv.FormatInt(id, 10) }

// mustLocator parses a decimal locator back into its typed id. Ledger replays
// restore the dropped typed id from the persisted human locator; a payload
// that lost its locator surfaces the zero id instead of fabricating a value.
func mustLocator(id string) int64 {
	value, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func freezeInput(ctx context.Context, conn execution.Executor, attemptID int64, kind string, body []byte, versionID, contractID int64, now string) error {
	digest := sha256.Sum256(body)
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?, 'v1',?,?)`, attemptID, kind, hex.EncodeToString(digest[:]), now)
	if err != nil {
		return err
	}
	snapshotID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	versionDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", versionID)))
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
		VALUES(?,1,'config_version',?,?)`, snapshotID, hex.EncodeToString(versionDigest[:]), versionID); err != nil {
		return err
	}
	// New inspection children carry only config-version lineage. contractID stays
	// in the function signature while historical run rows still reference it.
	return nil
}

// convergeOn closes the Run once every configured check has settled:
// Completed requires all-ok coverage, CompletedWithGaps at least one explicit
// gap (trg_inspection_runs_result_set_complete re-validates both). Plan runs
// count their run-frozen catalog; legacy declaration runs keep their
// config_checks join. It composes inside caller transactions (browser journey
// closure, technical gaps) and inside runner transactions alike.
func (s *Service) convergeOn(ctx context.Context, tx execution.Executor, runID int64) error {
	var pending, gaps int
	err := tx.QueryRowContext(ctx, `
		SELECT
		  CASE WHEN r.plan_id IS NOT NULL
		    THEN (SELECT COUNT(*) FROM inspection_run_checks c WHERE c.run_id=r.id)
		    ELSE (SELECT COUNT(*) FROM config_checks c JOIN config_plans p ON p.id=c.plan_id
		          WHERE p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key)
		  END
		  - (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=?),
		  (SELECT COUNT(*) FROM inspection_check_results x WHERE x.run_id=? AND x.status <> 'ok')
		FROM inspection_runs r WHERE r.id=?`, runID, runID, runID).Scan(&pending, &gaps)
	if err != nil {
		return err
	}
	if pending != 0 {
		return nil
	}
	state := "Completed"
	if gaps > 0 {
		state = "CompletedWithGaps"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inspection_runs SET state=?, row_version=row_version+1 WHERE id=? AND state='Running'`, state, runID); err != nil {
		return err
	}
	// A closed collection immediately owns its analysis attempt; the frozen
	// snapshot carries the preallocated Report version and the full locator
	// set (RUNTIME-TASK-013).
	return s.startReportAnalysisOn(ctx, tx, runID, s.nowText())
}

type RunSummary struct {
	ID                string  `json:"id"`
	BusinessSystemKey *string `json:"businessSystemKey,omitempty"`
	ConnectionName    *string `json:"connectionName,omitempty"`
	PlanKey           string  `json:"planKey"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	TriggerKind       string  `json:"triggerKind"`
	ScheduledFor      *string `json:"scheduledFor,omitempty"`
	EvidenceAt        *string `json:"evidenceAt,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// RuntimeAvailability is sampled by the scheduling runtime at the boundary
// (ADR-0011: collection executes locally through the Stele gateway). A false
// value produces a durable runtime_unavailable check gap rather than a queued
// execution that would silently run later.
type RuntimeAvailability struct {
	// Collection reports whether the local collector's platform transport
	// (the Stele gateway stream) can accept collection work at the boundary.
	Collection bool
}

// runtimeUnavailableChild records a boundary-time Runtime outage as a terminal
// technical child. The frozen result trigger requires every gap to identify an
// exact Attempt, even when no dispatch could be attempted. CreateOn centrally
// persists the operation correlation onto the new child in the same
// transaction (ADR-0006) — a context without execution metadata fails closed.
func (s *Service) runtimeUnavailableChild(ctx context.Context, tx execution.Executor, runID int64, checkKey, now string) error {
	attemptID, err := attempt.CreateOn(ctx, tx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,?,'Queued',?,?)`, runID, checkKey, attempt.ReleaseVersion(), now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_attempts SET state='Failed',ended_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, attemptID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO inspection_check_results(run_id,check_key,status,evidence_id,attempt_id,result_digest,gap_reason,created_at)
		VALUES(?,?,'gap',NULL,?,NULL,'runtime_unavailable',?)`, runID, checkKey, attemptID, now)
	return err
}
