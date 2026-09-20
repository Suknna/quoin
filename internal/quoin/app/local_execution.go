package app

// 本地执行器（ADR-0011 收官）：connection_probe、observation_run 与 run_check
// 的 inspection_collection 三类非 Agent attempt 不再派发 Plinth——Quoin 在本进
// 程内绑定、执行并收口。执行体复用插件内部工具（metrics_probe /
// metrics_discover / metrics_collect），平台 I/O 一律经 Stele 网关的
// PlatformCaller（凭证注入、限流、传输都在 Stele），结果映射回与原 Plinth
// ResultProposal 完全相同的 canonical payload 形状，走既有 commit 事务收口。
//
// 状态机复用：本地执行沿用冻结 schema 的 Assigned→Running 绑定路径（Running
// 状态要求 runtime_slot/boot/epoch/lease/accepted 完整），但绑定身份是本地常
// 量（quoin-local/epoch 1），绝不出现在任何 Control 流帧上；lease 仅作为进程
// 崩溃时的扫尾围栏（RunLeaseSweeper 按 lease_until 过期收敛），不复用
// DispatchLease 的心跳续期语义——本地执行以秒计，无需续期。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/connections"
	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/Suknna/quoin/internal/quoin/observation"
)

const (
	// localExecutionBootID 是本地执行的派发绑定身份：与 Plinth 的真实 boot id
	// 永不冲突（运行时生成随机 boot），取消/对账路径据此识别本地绑定并不再向
	// Plinth 发帧。
	localExecutionBootID = "quoin-local"
	// localExecutionEpoch 是本地绑定的连接纪元（schema 要求 >= 1）。
	localExecutionEpoch = uint64(1)
	// localExecutionLease 是本地执行的一次性围栏窗口：覆盖最长工具超时（60s）
	// 的宽裕余量；进程崩溃后由租约清扫按过期收敛，无心跳续期。
	localExecutionLease = 5 * time.Minute
	// localExecutionScanInterval 是扫描循环周期；启动即先跑一轮。
	localExecutionScanInterval = 2 * time.Second
	// localExecutionWorkers 限制并发执行的 attempt 数（平台 I/O 经网关限流，
	// 这里只保护本进程的并发面）。
	localExecutionWorkers = 4
	// localExecutionDefaultTimeout 是内部工具执行的超时上限（ToolEntry 声明的
	// 更短超时生效）。
	localExecutionDefaultTimeout = 60 * time.Second
)

// localMetricsToolEntry 解析一个内部工具的执行入口。变量形态仅为测试可注入。
var localMetricsToolEntry = func(name string) (plugins.ToolEntry, bool) {
	entry, _, ok := plugins.Default().ToolEntryByName(name)
	return entry, ok
}

// isLocalExecutionBinding 识别本地执行的派发绑定（取消、对账与 Plinth 重连
// 路径据此把这三类 attempt 排除出帧协议）。
func isLocalExecutionBinding(boot sql.NullString) bool {
	return boot.Valid && boot.String == localExecutionBootID
}

// sha256DigestOf 计算探测结果的密封摘要（schema kind 与 canonical 正文一起
// 入摘要，与原 ResultProposal 收口的口径一致）。
func sha256DigestOf(schemaKind string, canonical []byte) string {
	sum := sha256.Sum256(append([]byte(schemaKind+"\n"), canonical...))
	return hex.EncodeToString(sum[:])
}

// digestMatches 比较 canonical 正文与帧携带摘要（hex）。
func digestMatches(canonical []byte, digest []byte) bool {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]) == hex.EncodeToString(digest)
}

// RunLocalExecutionLoop 在进程生命周期内扫描并执行三类本地 attempt。
func (service *RuntimeService) RunLocalExecutionLoop(ctx context.Context) {
	service.runLocalExecutionPass(ctx)
	ticker := time.NewTicker(localExecutionScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.runLocalExecutionPassLocked(ctx)
		}
	}
}

// kickLocalExecution 在调度准入或收口后异步触发一轮扫描（降低消费延迟）；
// 与周期扫描互斥，重入静默跳过。
func (service *RuntimeService) kickLocalExecution(context.Context) {
	go service.runLocalExecutionPassLocked(context.Background())
}

// runLocalExecutionPassLocked 串行化扫描轮次：两个并发轮次只会重复竞争同
// 一批 Queued 行（状态围栏保证安全），互斥让日志与执行面保持确定。
func (service *RuntimeService) runLocalExecutionPassLocked(ctx context.Context) {
	service.localExecutionPass.Lock()
	defer service.localExecutionPass.Unlock()
	service.runLocalExecutionPass(ctx)
}

// localExecutionCandidate 是一轮扫描发现的一个待执行 attempt。
type localExecutionCandidate struct {
	ID        int64
	Type      string
	ScopeType string
}

// runLocalExecutionPass 扫描 Queued 的三类 attempt 并有界并发执行。各类型的
// 队列扫描沿用既有 service 查询（观察/巡检 join 根 Running，探测按类型），
// Queued 状态本身保证未绑定任何 runtime（schema CHECK）。整个轮次（含扫描）
// 运行在脱离调用方的上下文上：一次已取消的触发不得截断已开始的收敛。
func (service *RuntimeService) runLocalExecutionPass(ctx context.Context) {
	// 执行体使用独立后台上下文：调度 kick 携带的请求作用域可能先于执行结束。
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	candidates := service.localExecutionCandidates(execCtx)
	if len(candidates) == 0 {
		return
	}
	semaphore := make(chan struct{}, localExecutionWorkers)
	var wait sync.WaitGroup
	for _, candidate := range candidates {
		wait.Add(1)
		semaphore <- struct{}{}
		go func(candidate localExecutionCandidate) {
			defer wait.Done()
			defer func() { <-semaphore }()
			service.executeLocalAttempt(execCtx, candidate)
		}(candidate)
	}
	wait.Wait()
}

// localExecutionCandidates 汇聚三类扫描（每类服务未装配时静默跳过——测试可
// 只装配其中一个切片）。
func (service *RuntimeService) localExecutionCandidates(ctx context.Context) []localExecutionCandidate {
	var candidates []localExecutionCandidate
	appendIDs := func(attemptType, scopeType string, ids []int64, err error) {
		if err != nil {
			sharedops.LogEvent("quoin", "error", "local_execution.queue_scan", attemptType+": "+err.Error())
			return
		}
		for _, id := range ids {
			candidates = append(candidates, localExecutionCandidate{ID: id, Type: attemptType, ScopeType: scopeType})
		}
	}
	if service.Connections != nil {
		ids, err := service.Connections.QueuedProbeAttempts(ctx)
		appendIDs("connection_probe", "connection", ids, err)
	}
	if service.Observations != nil {
		ids, err := service.Observations.QueuedObservationAttempts(ctx)
		appendIDs("inspection_collection", "observation_run", ids, err)
	}
	if service.Inspections != nil {
		ids, err := service.Inspections.QueuedPromQLAttempts(ctx)
		appendIDs("inspection_collection", "run_check", ids, err)
	}
	return candidates
}

// executeLocalAttempt 按类型路由一次本地执行。
func (service *RuntimeService) executeLocalAttempt(ctx context.Context, candidate localExecutionCandidate) {
	var err error
	switch {
	case candidate.Type == "connection_probe":
		err = service.executeLocalProbe(ctx, candidate.ID)
	case candidate.Type == "inspection_collection" && candidate.ScopeType == "observation_run":
		err = service.executeLocalObservation(ctx, candidate.ID)
	case candidate.Type == "inspection_collection" && candidate.ScopeType == "run_check":
		err = service.executeLocalInspectionCollection(ctx, candidate.ID)
	default:
		return
	}
	if err != nil {
		sharedops.LogEvent("quoin", "error", "local_execution.execute_failed",
			fmt.Sprintf("attempt=%d type=%s scope=%s: %v", candidate.ID, candidate.Type, candidate.ScopeType, err))
	}
}

// bindLocalAttempt 把一个 Queued attempt 绑定到本地执行身份并置 Running，复用
// 既有派发绑定/接受事务（审计、围栏与触发器全部一致）。
func (service *RuntimeService) bindLocalAttempt(ctx context.Context, attempts *attempt.Service, attemptID int64) error {
	if err := attempts.BindToStream(ctx, attemptID, localExecutionBootID, localExecutionEpoch, localExecutionLease, attempt.ReleaseVersion()); err != nil {
		return err
	}
	return attempts.Accept(ctx, attemptID, localExecutionBootID, localExecutionEpoch)
}

// localConnection 是一次本地执行解析出的非秘密连接材料（秘密由 Stele 网关
// 自行注入，Quoin 只携带 revision 的 config_json）。
type localConnection struct {
	plugins.Connection
}

// loadLocalConnection 从 attempt 的 config_thanos_query grant 解析连接身份与
// revision 配置（观察/巡检采集共用）。
func (service *RuntimeService) loadLocalConnection(ctx context.Context, reader audit.Reader, attemptID int64) (localConnection, error) {
	var connectionID, revisionID int64
	var connectionType, configJSON string
	err := reader.QueryRowContext(ctx, `
		SELECT c.id, c.type, g.connection_revision_id, r.config_json
		FROM attempt_connection_grants g
		JOIN connections c ON c.id = g.connection_id
		JOIN connection_revisions r ON r.id = g.connection_revision_id
		WHERE g.attempt_id=? AND g.purpose='config_thanos_query'
		ORDER BY g.id LIMIT 1`, attemptID).
		Scan(&connectionID, &connectionType, &revisionID, &configJSON)
	if err != nil {
		return localConnection{}, fmt.Errorf("resolve metrics connection for attempt %d: %w", attemptID, err)
	}
	return localConnection{plugins.Connection{
		ID: connectionID, RevisionID: revisionID, Type: connectionType, Settings: json.RawMessage(configJSON),
	}}, nil
}

// invokeLocalTool 执行一个内部工具：参数解码、平台调用与超时都由 ToolEntry
// 与网关 caller 承担；返回的字节即该工具的 canonical 结果载荷。
func (service *RuntimeService) invokeLocalTool(ctx context.Context, name string, arguments any, conn localConnection) (json.RawMessage, error) {
	entry, known := localMetricsToolEntry(name)
	if !known {
		return nil, fmt.Errorf("internal tool %q is not registered in this process", name)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	timeout := localExecutionDefaultTimeout
	if entry.Timeout > 0 && entry.Timeout < timeout {
		timeout = entry.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return entry.Invoke(execCtx, plugins.ToolExecution{
		Arguments: encoded,
		Conn:      conn.Connection,
		Platform:  &gatewayPlatformCaller{gateway: service.SteleGateway, conn: conn.Connection},
	})
}

// ---------------------------------------------------------------------------
// connection_probe：metrics_probe（vector(1) 契约）
// ---------------------------------------------------------------------------

// metricsProbeResultJSON 是 metrics_probe 的 canonical 结果形状（与
// internal/plugins/builtin/metrics.go 的 metricsProbeResult 对齐）。
type metricsProbeResultJSON struct {
	Reachable    bool   `json:"reachable"`
	LatencyMS    int64  `json:"latencyMs"`
	Kind         string `json:"kind"`
	Query        string `json:"query"`
	ResponseType string `json:"responseType,omitempty"`
	SampleCount  int    `json:"sampleCount,omitempty"`
	SampleValue  string `json:"sampleValue,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// executeLocalProbe 本地执行一次连接探测：绑定 → metrics_probe → 与原 Plinth
// supervisor 相同语义的 probeResultJSON 载荷 → CommitProbeResult 收口。
func (service *RuntimeService) executeLocalProbe(ctx context.Context, attemptID int64) error {
	if service.Connections == nil {
		return nil
	}
	_, _, _, bound, err := service.Connections.BindQueuedToStream(ctx, attemptID, localExecutionBootID, localExecutionEpoch, localExecutionLease)
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if !bound {
		// 另一个执行者赢得了 Queued→Assigned 竞争：本轮无事可做。
		return nil
	}
	if err := service.Connections.AcceptProbe(ctx, attemptID, localExecutionBootID, localExecutionEpoch); err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	startedAt := time.Now().UTC()
	var connectionID, revisionID int64
	var connectionType, configJSON string
	if err := service.Connections.Reader().QueryRowContext(ctx, `
		SELECT c.id, c.type, g.connection_revision_id, r.config_json
		FROM execution_attempts a
		JOIN connections c ON c.id = a.scope_id
		JOIN attempt_connection_grants g ON g.attempt_id = a.id AND g.connection_id = c.id
		JOIN connection_revisions r ON r.id = g.connection_revision_id
		WHERE a.id=? AND a.attempt_type='connection_probe'
		ORDER BY g.id LIMIT 1`, attemptID).
		Scan(&connectionID, &connectionType, &revisionID, &configJSON); err != nil {
		return fmt.Errorf("resolve probe connection: %w", err)
	}
	conn := plugins.Connection{ID: connectionID, RevisionID: revisionID, Type: connectionType, Settings: json.RawMessage(configJSON)}
	outcome := "passed"
	detail := map[string]any{"kind": connectionType, "query": "vector(1)"}
	switch connectionType {
	case connections.TypePrometheus, connections.TypeThanos:
		raw, invokeErr := service.invokeLocalTool(ctx, "metrics_probe", struct{}{}, localConnection{conn})
		if invokeErr != nil {
			// 工具级失败（网关不可达、参数不可解码）：与今天平台不通时相同，
			// 落 failed 结果而不是留下永远 Running 的 attempt。
			outcome = "failed"
			detail["error"] = invokeErr.Error()
			break
		}
		var probe metricsProbeResultJSON
		if err := json.Unmarshal(raw, &probe); err != nil {
			outcome = "failed"
			detail["error"] = "探测结果不可解析: " + err.Error()
			break
		}
		detail["responseType"] = probe.ResponseType
		detail["sampleCount"] = probe.SampleCount
		detail["sampleValue"] = probe.SampleValue
		if !probe.Reachable || probe.Detail != "" {
			outcome = "failed"
			if probe.Detail != "" {
				detail["error"] = probe.Detail
			}
		}
	default:
		// model_provider 资格探测需要真实的模型调用资格验证，本地指标工具集
		// 不覆盖；按确定性失败收口，保持 attempt 与探测结果历史的诚实。
		// <--Waiting for Implementation-->
		outcome = "failed"
		detail["error"] = "该连接类型暂无本地探测执行器"
	}
	finishedAt := time.Now().UTC()
	detailJSON, _ := json.Marshal(detail)
	canonical, err := json.Marshal(probeResultJSON{
		Outcome: outcome, Detail: detailJSON,
		StartedAt: startedAt.Format(time.RFC3339Nano), FinishedAt: finishedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	schemaKind := "connection_probe_" + connectionType + "_v1"
	if err := service.commitProbeResultPayload(ctx, attemptID, schemaKind, canonical, localExecutionBootID, localExecutionEpoch); err != nil {
		// 提交被取消围栏抢先（晚到结果）是正常竞态：围栏路径负责终态。
		if strings.Contains(err.Error(), "late") || strings.Contains(err.Error(), "not running") {
			return nil
		}
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// observation_run：metrics_discover（有界发现）
// ---------------------------------------------------------------------------

// localObservationInput 是冻结的 source_observation_execution_v1 输入正文。
type localObservationInput struct {
	SchemaKind       string   `json:"schemaKind"`
	AttemptID        int64    `json:"attemptId"`
	ObservationRunID int64    `json:"observationRunId"`
	PluginID         string   `json:"pluginId"`
	ObjectType       string   `json:"objectType"`
	Query            string   `json:"query"`
	IdentityLabels   []string `json:"identityLabels"`
	Limit            int      `json:"limit"`
	GrantID          int64    `json:"grantId"`
}

// executeLocalObservation 本地执行一次有界发现：重建冻结输入 → 绑定 →
// metrics_discover → source_observation_result_v1 载荷 → CommitProposal。
func (service *RuntimeService) executeLocalObservation(ctx context.Context, attemptID int64) error {
	if service.Observations == nil {
		return nil
	}
	attempts := service.Observations.Attempts()
	// 输入重建先于绑定：无法重建的冻结输入永远不会变得可执行，按技术缺口收
	// 敛（与原派发路径的收敛语义一致）。
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		if gapErr := service.Observations.ConvergeTechnicalGap(ctx, attemptID, err.Error()); gapErr != nil {
			return fmt.Errorf("converge gap after rebuild failure %v: %w", err, gapErr)
		}
		return nil
	}
	if err := service.bindLocalAttempt(ctx, attempts, attemptID); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	var frozen localObservationInput
	if err := json.Unmarshal(input.CanonicalJSON, &frozen); err != nil ||
		frozen.SchemaKind != observation.ExecutionSchemaKind || frozen.AttemptID != attemptID ||
		frozen.ObservationRunID < 1 || frozen.ObjectType == "" {
		return service.commitObservationResult(ctx, attemptID, frozen, "error", nil, []string{"invalid source observation input"}, "query_failed")
	}
	conn, connErr := service.loadLocalConnection(ctx, service.Observations.Reader(), attemptID)
	if connErr != nil {
		return service.commitObservationResult(ctx, attemptID, frozen, "error", nil, []string{connErr.Error()}, "query_failed")
	}
	raw, invokeErr := service.invokeLocalTool(ctx, "metrics_discover", struct {
		ObjectType string `json:"objectType"`
		Limit      int    `json:"limit,omitempty"`
	}{ObjectType: frozen.ObjectType, Limit: frozen.Limit}, conn)
	if invokeErr != nil {
		return service.commitObservationResult(ctx, attemptID, frozen, "error", nil, []string{invokeErr.Error()}, "query_failed")
	}
	var discovered plugins.DiscoverResult
	if err := json.Unmarshal(raw, &discovered); err != nil {
		return service.commitObservationResult(ctx, attemptID, frozen, "error", nil, []string{"discovery result unparseable: " + err.Error()}, "query_failed")
	}
	if discovered.Incomplete {
		return service.commitObservationResult(ctx, attemptID, frozen, "gap", nil, []string{"discovery pass incomplete"}, "partial_response")
	}
	targets := make([]observationTargetJSON, 0, len(discovered.Objects))
	for _, object := range discovered.Objects {
		identity := map[string]string{}
		for _, part := range strings.Split(object.CanonicalIdentity, ",") {
			if name, value, found := strings.Cut(part, "="); found {
				identity[name] = value
			}
		}
		// 与原 supervisor 适配器一致：v1 插件契约只携带声明的身份 label 集，
		// 它们同时是身份与 label 投影，不发明额外事实。
		labels := make(map[string]string, len(identity))
		for name, value := range identity {
			labels[name] = value
		}
		targets = append(targets, observationTargetJSON{Identity: identity, Labels: labels, DisplayName: object.DisplayName})
	}
	return service.commitObservationResult(ctx, attemptID, frozen, "success", targets, nil, "")
}

// observationTargetJSON 是 source_observation_result_v1 的单个对象形状。
type observationTargetJSON struct {
	Identity    map[string]string `json:"identity"`
	Labels      map[string]string `json:"labels"`
	DisplayName string            `json:"displayName,omitempty"`
}

// commitObservationResult 组装与原 Plinth marshalSourceObservationProposal 字节
// 语义一致的 canonical 载荷并经 CommitProposal 收口。非 success 的 objects 一
// 律为空数组（控制面的完整性规则从不投影部分事实）。
func (service *RuntimeService) commitObservationResult(ctx context.Context, attemptID int64, frozen localObservationInput, outcome string, targets []observationTargetJSON, messages []string, gapReason string) error {
	if outcome != "success" {
		targets = nil
	}
	if targets == nil {
		targets = []observationTargetJSON{}
	}
	warnings := []string{}
	errs := []string{}
	if outcome == "error" {
		errs = messages
	} else if len(messages) != 0 {
		warnings = messages
	}
	var gap any
	if gapReason != "" {
		gap = gapReason
	}
	canonical, err := json.Marshal(map[string]any{
		"schemaKind":       observation.ResultSchemaKind,
		"attemptId":        attemptID,
		"observationRunId": frozen.ObservationRunID,
		"objectType":       frozen.ObjectType,
		"outcome":          outcome,
		"observedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"objects":          targets,
		"warnings":         warnings,
		"errors":           errs,
		"gapReason":        gap,
	})
	if err != nil {
		return err
	}
	if err := service.Observations.CommitProposal(ctx, attemptID, localExecutionBootID, localExecutionEpoch, canonical); err != nil {
		if errors.Is(err, attempt.ErrLateResult) {
			return nil
		}
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// run_check collection：metrics_collect（声明 PromQL 与计划插件模板共用）
// ---------------------------------------------------------------------------

// localPromQLInput 是冻结的 inspection_promql_execution_v1 输入正文。
type localPromQLInput struct {
	SchemaKind      string `json:"schemaKind"`
	AttemptID       int64  `json:"attemptId"`
	InspectionRunID int64  `json:"inspectionRunId"`
	CheckKey        string `json:"checkKey"`
	EvidenceAt      string `json:"evidenceAt"`
	GrantID         int64  `json:"grantId"`
	Query           struct {
		Mode         string `json:"mode"`
		Expression   string `json:"expression"`
		RangeSeconds *int64 `json:"rangeSeconds"`
		StepSeconds  *int64 `json:"stepSeconds"`
	} `json:"query"`
}

// localPluginCollectionInput 是冻结的 inspection_plugin_execution_v1 输入正文。
type localPluginCollectionInput struct {
	SchemaKind      string              `json:"schemaKind"`
	AttemptID       int64               `json:"attemptId"`
	InspectionRunID int64               `json:"inspectionRunId"`
	CheckKey        string              `json:"checkKey"`
	PluginID        string              `json:"pluginId"`
	TemplateID      string              `json:"templateId"`
	TemplateVersion string              `json:"templateVersion"`
	Params          json.RawMessage     `json:"params"`
	EvidenceAt      string              `json:"evidenceAt"`
	GrantID         int64               `json:"grantId"`
	ScopeKind       string              `json:"scopeKind,omitempty"`
	Target          *localCollectionRef `json:"target,omitempty"`
}

// localCollectionRef 是冻结输入中的单个目标定位。
type localCollectionRef struct {
	ObjectType      string            `json:"objectType,omitempty"`
	IdentityKey     string            `json:"identityKey,omitempty"`
	LabelConditions map[string]string `json:"labelConditions,omitempty"`
}

// executeLocalInspectionCollection 本地执行一次 run_check 采集：重建冻结输入
// → 绑定 → metrics_collect → inspection_promql_result_v1 /
// inspection_plugin_result_v1 载荷 → 对应 Commit*Proposal 收口。
func (service *RuntimeService) executeLocalInspectionCollection(ctx context.Context, attemptID int64) error {
	if service.Inspections == nil {
		return nil
	}
	attempts := service.Inspections.Attempts()
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		if gapErr := service.Inspections.ConvergeCollectionGap(ctx, attemptID); gapErr != nil {
			return fmt.Errorf("converge gap after rebuild failure %v: %w", err, gapErr)
		}
		return nil
	}
	if err := service.bindLocalAttempt(ctx, attempts, attemptID); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	switch input.SchemaKind {
	case "inspection_promql_execution_v1":
		return service.executeLocalPromQLCollection(ctx, attemptID, input.CanonicalJSON)
	case "inspection_plugin_execution_v1":
		return service.executeLocalPluginCollection(ctx, attemptID, input.CanonicalJSON)
	default:
		return fmt.Errorf("attempt %d carries unsupported collection schema %q", attemptID, input.SchemaKind)
	}
}

// executeLocalPromQLCollection 执行声明 PromQL 子：模板参数由冻结输入的查询
// 形状派生（instant/range），integration 范围、无目标。
func (service *RuntimeService) executeLocalPromQLCollection(ctx context.Context, attemptID int64, canonical []byte) error {
	var frozen localPromQLInput
	if err := json.Unmarshal(canonical, &frozen); err != nil || frozen.SchemaKind != "inspection_promql_execution_v1" ||
		frozen.AttemptID != attemptID || frozen.InspectionRunID < 1 || frozen.CheckKey == "" ||
		(frozen.Query.Mode != "instant" && frozen.Query.Mode != "range") {
		return service.commitInspectionResult(ctx, attemptID, inspectionCollectionOutcome{
			schemaKind: "inspection_promql_result_v1", runID: frozen.InspectionRunID, checkKey: frozen.CheckKey,
			queryMode: frozen.Query.Mode, outcome: "error", messages: []string{"invalid inspection PromQL input"}, gapReason: "query_failed",
		})
	}
	if (frozen.Query.Mode == "instant") != (frozen.Query.RangeSeconds == nil) ||
		(frozen.Query.Mode == "range" && (frozen.Query.RangeSeconds == nil || frozen.Query.StepSeconds == nil)) {
		return service.commitInspectionResult(ctx, attemptID, inspectionCollectionOutcome{
			schemaKind: "inspection_promql_result_v1", runID: frozen.InspectionRunID, checkKey: frozen.CheckKey,
			queryMode: frozen.Query.Mode, outcome: "error", messages: []string{"inspection PromQL query shape mismatch"}, gapReason: "query_failed",
		})
	}
	templateID, params := "promql_instant", map[string]any{"expression": frozen.Query.Expression}
	if frozen.Query.Mode == "range" {
		templateID = "promql_range"
		params = map[string]any{"expression": frozen.Query.Expression, "rangeSeconds": *frozen.Query.RangeSeconds, "stepSeconds": *frozen.Query.StepSeconds}
	}
	outcome := service.runLocalCollection(ctx, attemptID, collectRequestJSON{
		TemplateID: templateID, TemplateVersion: "1", Params: params,
		EvidenceAt: frozen.EvidenceAt, ScopeKind: string(plugins.ScopeIntegration),
	})
	outcome.schemaKind = "inspection_promql_result_v1"
	outcome.runID = frozen.InspectionRunID
	outcome.checkKey = frozen.CheckKey
	outcome.queryMode = frozen.Query.Mode
	// 范围查询的窗口是冻结输入的派生事实（evidence_at 为终点），与执行结果
	// 无关；失败/gap 同样携带，收口方按请求窗口元数据复核。
	if frozen.Query.Mode == "range" && frozen.EvidenceAt != "" {
		if endAt, parseErr := time.Parse(time.RFC3339Nano, frozen.EvidenceAt); parseErr == nil {
			outcome.executionWindow = map[string]any{
				"startAt":     endAt.Add(-time.Duration(*frozen.Query.RangeSeconds) * time.Second).UTC().Format(time.RFC3339Nano),
				"endAt":       endAt.UTC().Format(time.RFC3339Nano),
				"stepSeconds": *frozen.Query.StepSeconds,
			}
		}
	}
	return service.commitInspectionResult(ctx, attemptID, outcome)
}

// executeLocalPluginCollection 执行独立计划插件采集子：模板/参数/范围/目标全
// 部来自冻结输入。
func (service *RuntimeService) executeLocalPluginCollection(ctx context.Context, attemptID int64, canonical []byte) error {
	var frozen localPluginCollectionInput
	if err := json.Unmarshal(canonical, &frozen); err != nil || frozen.SchemaKind != "inspection_plugin_execution_v1" ||
		frozen.AttemptID != attemptID || frozen.InspectionRunID < 1 || frozen.CheckKey == "" ||
		frozen.PluginID == "" || frozen.TemplateID == "" {
		return service.commitInspectionResult(ctx, attemptID, inspectionCollectionOutcome{
			schemaKind: "inspection_plugin_result_v1", runID: frozen.InspectionRunID, checkKey: frozen.CheckKey,
			outcome: "error", messages: []string{"invalid inspection plugin input"}, gapReason: "query_failed",
		})
	}
	switch plugins.CollectScopeKind(frozen.ScopeKind) {
	case plugins.ScopeIntegration, plugins.ScopeBusinessView, plugins.ScopeObjects:
	default:
		// 空/未知范围 fail closed：缺字段绝不等价于更宽的授权。
		return service.commitInspectionResult(ctx, attemptID, inspectionCollectionOutcome{
			schemaKind: "inspection_plugin_result_v1", runID: frozen.InspectionRunID, checkKey: frozen.CheckKey,
			outcome: "error", messages: []string{fmt.Sprintf("frozen inspection input carries unsupported scope kind %q", frozen.ScopeKind)}, gapReason: "query_failed",
		})
	}
	targets := []plugins.CollectTarget{}
	if frozen.Target != nil {
		targets = append(targets, plugins.CollectTarget{
			ObjectType:        frozen.Target.ObjectType,
			CanonicalIdentity: frozen.Target.IdentityKey,
			LabelConditions:   frozen.Target.LabelConditions,
		})
	}
	request := collectRequestJSON{
		TemplateID: frozen.TemplateID, TemplateVersion: frozen.TemplateVersion,
		Params: json.RawMessage(frozen.Params), EvidenceAt: frozen.EvidenceAt,
		ScopeKind: frozen.ScopeKind, Targets: targets,
	}
	outcome := service.runLocalCollection(ctx, attemptID, request)
	outcome.schemaKind = "inspection_plugin_result_v1"
	outcome.runID = frozen.InspectionRunID
	outcome.checkKey = frozen.CheckKey
	// promql_range 的窗口是冻结输入的派生事实（evidence_at + range/step），与
	// 执行结果无关；失败/gap 同样携带。
	if frozen.TemplateID == "promql_range" && frozen.EvidenceAt != "" {
		var params struct {
			RangeSeconds float64 `json:"rangeSeconds"`
			StepSeconds  float64 `json:"stepSeconds"`
		}
		if json.Unmarshal(frozen.Params, &params) == nil && params.RangeSeconds >= 1 && params.StepSeconds >= 1 {
			if endAt, parseErr := time.Parse(time.RFC3339Nano, frozen.EvidenceAt); parseErr == nil {
				outcome.executionWindow = map[string]any{
					"startAt":     endAt.Add(-time.Duration(int64(params.RangeSeconds)) * time.Second).UTC().Format(time.RFC3339Nano),
					"endAt":       endAt.UTC().Format(time.RFC3339Nano),
					"stepSeconds": int64(params.StepSeconds),
				}
			}
		}
	}
	return service.commitInspectionResult(ctx, attemptID, outcome)
}

// collectRequestJSON 是 metrics_collect 的参数形状。
type collectRequestJSON struct {
	TemplateID      string                  `json:"templateId"`
	TemplateVersion string                  `json:"templateVersion"`
	Params          any                     `json:"params"`
	EvidenceAt      string                  `json:"evidenceAt,omitempty"`
	ScopeKind       string                  `json:"scopeKind"`
	Targets         []plugins.CollectTarget `json:"targets"`
}

// inspectionCollectionOutcome 汇聚一次采集的结果映射。
type inspectionCollectionOutcome struct {
	schemaKind      string
	runID           int64
	checkKey        string
	queryMode       string
	outcome         string // success | gap | error
	result          json.RawMessage
	messages        []string
	gapReason       string
	executionWindow any
}

// runLocalCollection 解析连接并执行 metrics_collect，把工具结果映射为与原
// supervisor 相同的 outcome 分流。执行级失败的诊断进入结果的 errors 字段；
// 这里同步落一条事件日志，便于排障（载荷本身不落 errors 列）。
func (service *RuntimeService) runLocalCollection(ctx context.Context, attemptID int64, request collectRequestJSON) inspectionCollectionOutcome {
	fail := func(err error) inspectionCollectionOutcome {
		sharedops.LogEvent("quoin", "error", "local_execution.collect_failed",
			fmt.Sprintf("attempt=%d template=%s: %v", attemptID, request.TemplateID, err))
		return inspectionCollectionOutcome{outcome: "error", messages: []string{err.Error()}, gapReason: "query_failed"}
	}
	conn, connErr := service.loadLocalConnection(ctx, service.Inspections.Reader(), attemptID)
	if connErr != nil {
		return fail(connErr)
	}
	raw, invokeErr := service.invokeLocalTool(ctx, "metrics_collect", request, conn)
	if invokeErr != nil {
		return fail(invokeErr)
	}
	var collected plugins.CollectResult
	if err := json.Unmarshal(raw, &collected); err != nil {
		return fail(fmt.Errorf("collection result unparseable: %w", err))
	}
	if collected.Incomplete || len(collected.Checks) == 0 {
		// 截断/局部响应不得伪装完整结果。
		return inspectionCollectionOutcome{outcome: "gap", messages: []string{"collection pass incomplete"}, gapReason: "partial_response"}
	}
	var evidence struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(collected.Checks[0].EvidenceJSON, &evidence); err != nil || len(evidence.Result) == 0 {
		return fail(errors.New("collector evidence carried no result projection"))
	}
	return inspectionCollectionOutcome{outcome: "success", result: evidence.Result}
}

// commitInspectionResult 组装与原 Plinth proposeInspectionPromQL /
// proposeInspectionPlugin 字节语义一致的 canonical 载荷并经对应 Commit 方法收
// 口。error 的诊断进 errors；gap 的观察事实进 warnings，绝不丢弃为空数组。
func (service *RuntimeService) commitInspectionResult(ctx context.Context, attemptID int64, outcome inspectionCollectionOutcome) error {
	result := outcome.result
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	warnings := []string{}
	errorMessages := []string{}
	if outcome.outcome == "error" {
		errorMessages = outcome.messages
	} else if len(outcome.messages) != 0 {
		warnings = outcome.messages
	}
	var gap any
	if outcome.gapReason != "" {
		gap = outcome.gapReason
	}
	window := outcome.executionWindow
	if window == nil {
		window = json.RawMessage("null")
	}
	payload := map[string]any{
		"schemaKind": outcome.schemaKind, "attemptId": attemptID, "inspectionRunId": outcome.runID,
		"checkKey": outcome.checkKey, "outcome": outcome.outcome,
		"observedAt": time.Now().UTC().Format(time.RFC3339Nano), "executionWindow": window, "result": result,
		"warnings": warnings, "errors": errorMessages, "gapReason": gap,
	}
	if outcome.queryMode != "" {
		payload["queryMode"] = outcome.queryMode
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var commitErr error
	switch outcome.schemaKind {
	case "inspection_promql_result_v1":
		commitErr = service.Inspections.CommitPromQLProposal(ctx, attemptID, localExecutionBootID, localExecutionEpoch, canonical)
	case "inspection_plugin_result_v1":
		commitErr = service.Inspections.CommitPluginProposal(ctx, attemptID, localExecutionBootID, localExecutionEpoch, canonical)
	default:
		return fmt.Errorf("attempt %d carries unsupported inspection result schema %q", attemptID, outcome.schemaKind)
	}
	if commitErr != nil {
		if errors.Is(commitErr, attempt.ErrLateResult) || errors.Is(commitErr, inspection.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("commit: %w", commitErr)
	}
	// 采集收口可能闭合 Run 并创建报告分析 attempt：立即派发，不等待无关事件。
	go service.dispatchQueuedInspections(context.Background())
	return nil
}
