package app

// quoin_routed 工具执行编排（ADR-0011）：BeginToolCall 完成围栏/授权后，
// 工具若属于 quoin_routed 执行模式，则执行本身不再回到 Plinth——Quoin 在
// 本进程内运行 Tool Handler（插件工具经 catalogs.Handlers 派发；平台
// artifact 工具走本文件的内部执行器），平台 I/O 经 Stele 网关（凭证注入/
// 限流/传输都在 Stele），长正文溢出为 tool_result Artifact。封存复用
// CompleteToolCall 的同款落库路径（tool_call 终态 + evidence + committed
// payload），随后把 ExternalToolResult 帧推给 Plinth，由 supervisor 转发
// worker 继续 agent 循环。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// routedToolDefaultTimeout 是 quoin_routed 执行的默认与上限时长：ToolEntry
// 声明的更短超时生效，更长或零值收敛到该上限。
const routedToolDefaultTimeout = 60 * time.Second

// routedErrorDetailBound 是封存进 error_detail 的非秘密说明上限（与
// preflight detail 的既有界一致）。
const routedErrorDetailBound = 1024

// routedToolContext 是一次出向执行装载的冻结上下文（围栏/授权已完成）。
type routedToolContext struct {
	attemptID   int64
	toolCallID  int64
	toolName    string
	failureMode string
	// resultSchemaKind 来自 attempt 冻结目录；结构化失败与成功载荷共用。
	resultSchemaKind string
	// arguments 是 Quoin 已改写的权威执行参数（tool_call_execution_inputs
	// 优先，与 CompleteModelCallAck 下发的语义一致），缺省回退原始
	// arguments_json。
	arguments json.RawMessage
	// conn 是冻结的连接绑定（无 grant 的 quoin_routed 工具为零值）。
	conn plugins.Connection
	// bootID/epoch 是 attempt 的派发绑定：Artifact 提交围栏与
	// ExternalToolResult 下发围栏都以它为准。
	bootID string
	epoch  uint64
}

// toolCallSeal 是封存请求的内部形态（CompleteToolCall 帧与异步编排共用）。
type toolCallSeal struct {
	attemptID   int64
	toolCallID  int64
	outcome     string // succeeded | failed | cancelled
	schemaKind  string
	canonical   []byte
	artifactID  int64
	errorCode   string
	errorDetail string
}

// sealReceipt 是封存完成后的回执（ack 与 ExternalToolResult 共用）。
type sealReceipt struct {
	evidenceIDs      []int64
	committedPayload *runtimev1.ResultPayload
	artifactRef      *runtimev1.ArtifactRef
}

// sealToolCall 是 handleCompleteToolCallRouted 抽出的可内部调用的封存
// 路径：schema 契约校验（期望 schema 来自 attempt 冻结目录 + 编译实现表）
// 后经 attempts.CompleteToolCall 落 tool_call 终态、确定性 Evidence 与
// committed payload（ARCH-TOOL-003/005）。canonical 的内容摘要在这里统一
// 计算，帧路径在调用前先核对过调用方携带的摘要。
func (service *RuntimeService) sealToolCall(ctx context.Context, attempts *attempt.Service, seal toolCallSeal) (sealReceipt, error) {
	if seal.schemaKind == "" {
		return sealReceipt{}, errors.New("tool result payload schema kind is required")
	}
	expectedSchema, err := attempts.ExpectedToolResultSchema(ctx, seal.toolCallID)
	if err != nil {
		return sealReceipt{}, fmt.Errorf("tool result schema lookup failed: %w", err)
	}
	if seal.schemaKind != expectedSchema {
		return sealReceipt{}, fmt.Errorf("tool result schema kind %q does not match the fixed tool definition %q", seal.schemaKind, expectedSchema)
	}
	if len(seal.canonical) > 0 {
		if err := attempt.ValidateToolResultPayload(attempts.Catalogs.Implementations, expectedSchema, seal.canonical); err != nil {
			return sealReceipt{}, fmt.Errorf("tool result payload violates the fixed schema: %w", err)
		}
	}
	evidenceIDs, err := attempts.CompleteToolCall(ctx, attempt.ToolResult{
		AttemptID: seal.attemptID, ToolCallID: seal.toolCallID,
		Outcome: seal.outcome, ResultJSON: string(seal.canonical), ArtifactID: seal.artifactID,
		ErrorCode: seal.errorCode, ErrorDetail: seal.errorDetail,
	})
	if err != nil {
		return sealReceipt{}, err
	}
	receipt := sealReceipt{evidenceIDs: evidenceIDs}
	receipt.committedPayload = &runtimev1.ResultPayload{SchemaKind: seal.schemaKind, CanonicalJson: seal.canonical}
	if len(seal.canonical) > 0 {
		digest := sha256.Sum256(seal.canonical)
		receipt.committedPayload.ContentDigest = digest[:]
	}
	if seal.artifactID != 0 {
		if service.Artifacts == nil {
			return sealReceipt{}, errors.New("artifact ref requested but the artifact store is not wired")
		}
		ref, refErr := service.Artifacts.RefFor(ctx, seal.attemptID, seal.artifactID)
		if refErr != nil {
			return sealReceipt{}, refErr
		}
		receipt.artifactRef = &runtimev1.ArtifactRef{
			ArtifactId: ref.ArtifactID, Role: "tool_result", MediaType: ref.MediaType,
			SizeBytes: uint64(ref.SizeBytes), Sha256: ref.SHA256, BodyExpired: ref.BodyExpired,
		}
	}
	return receipt, nil
}

// quoinRoutedToolCall 判定一次 tool call 是否属于 quoin_routed 执行模式，
// 并返回其工具名。执行模式以 attempt 冻结目录为权威（BeginToolCall 已验证
// 冻结条目与安装实现一致）。
func (service *RuntimeService) quoinRoutedToolCall(ctx context.Context, attempts *attempt.Service, attemptID, toolCallID int64) (string, bool, error) {
	var toolName string
	if err := attempts.Reader().QueryRowContext(ctx, `SELECT tool_name FROM tool_calls WHERE id=? AND attempt_id=?`, toolCallID, attemptID).Scan(&toolName); err != nil {
		return "", false, err
	}
	catalog, err := attempts.FrozenToolCatalog(ctx, attemptID)
	if err != nil {
		return "", false, err
	}
	frozen, known := catalog.Lookup(toolName)
	if !known {
		return toolName, false, nil
	}
	return toolName, frozen.ExecutionMode == plugins.ModeQuoinRouted, nil
}

// executeRoutedToolCall 是 quoin_routed 工具的异步执行体：装载冻结上下文
// -> 派发（插件 handler 或平台 artifact 执行器）-> 封存 -> ExternalToolResult。
// 任何阶段失败都以确定性错误封存为 failed tool call（遵守 FailureMode 的
// 语义由 attempts.CompleteToolCall 的既有路径裁决：return_to_model 需要模型
// 可见结果，fail_attempt 只需错误码）。
func (service *RuntimeService) executeRoutedToolCall(ctx context.Context, attempts *attempt.Service, attemptID, toolCallID int64, toolName string) {
	loaded, err := service.loadRoutedToolContext(ctx, attempts, attemptID, toolCallID, toolName)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "toolcall.routed_load_failed", fmt.Sprintf("attempt=%d tool_call=%d: %v", attemptID, toolCallID, err))
		return
	}
	timeout := routedToolDefaultTimeout
	if entry, known := attempts.Catalogs.Handlers[toolName]; known && entry.Timeout > 0 && entry.Timeout < timeout {
		timeout = entry.Timeout
	}
	execCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var seal toolCallSeal
	if entry, known := attempts.Catalogs.Handlers[toolName]; known {
		seal = service.invokePluginTool(execCtx, attempts, loaded, entry)
	} else if toolName == "alerts_recent" {
		seal = service.invokeAlertsRecentTool(execCtx, attempts, loaded)
	} else if toolName == "artifact_read" || toolName == "artifact_grep" {
		seal = service.invokeArtifactTool(execCtx, loaded)
	} else {
		// 冻结目录接受了该工具但进程内没有可派发的执行器：确定性失败。
		seal = routedFailureSeal(loaded, "no_executor", "quoin_routed tool "+toolName+" has no installed executor")
	}
	receipt, sealErr := service.sealToolCall(ctx, attempts, seal)
	if sealErr != nil {
		// 封存失败（围栏拒绝/库错误）：终态以 ledger 为权威，这里只能审计。
		sharedops.LogEvent("quoin", "error", "toolcall.routed_seal_failed",
			fmt.Sprintf("attempt=%d tool_call=%d outcome=%s: %v", attemptID, toolCallID, seal.outcome, sealErr))
		return
	}
	service.sendExternalToolResult(ctx, loaded, seal, receipt)
}

// loadRoutedToolContext 从持久化行重建一次出向执行的冻结上下文。
func (service *RuntimeService) loadRoutedToolContext(ctx context.Context, attempts *attempt.Service, attemptID, toolCallID int64, toolName string) (*routedToolContext, error) {
	loaded := &routedToolContext{attemptID: attemptID, toolCallID: toolCallID, toolName: toolName}
	reader := attempts.Reader()
	var boot sql.NullString
	var epoch sql.NullInt64
	if err := reader.QueryRowContext(ctx, `SELECT boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&boot, &epoch); err != nil {
		return nil, err
	}
	if !boot.Valid || !epoch.Valid {
		return nil, fmt.Errorf("attempt %d has no runtime binding for quoin_routed execution", attemptID)
	}
	loaded.bootID = boot.String
	loaded.epoch = uint64(epoch.Int64)
	catalog, err := attempts.FrozenToolCatalog(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	frozen, known := catalog.Lookup(toolName)
	if !known || frozen.ResultSchemaKind == "" {
		return nil, fmt.Errorf("tool %q has no fixed result schema in the attempt's frozen catalog", toolName)
	}
	loaded.resultSchemaKind = frozen.ResultSchemaKind
	// 权威执行参数：改写后的 execution_inputs 优先（与 CompleteModelCallAck
	// 下发的语义一致），无改写的工具回退原始 arguments_json。
	var arguments string
	err = reader.QueryRowContext(ctx, `SELECT arguments_json FROM tool_call_execution_inputs WHERE tool_call_id=?`, toolCallID).Scan(&arguments)
	switch {
	case err == nil:
		loaded.arguments = json.RawMessage(arguments)
	case errors.Is(err, sql.ErrNoRows):
		if err := reader.QueryRowContext(ctx, `SELECT arguments_json FROM tool_calls WHERE id=?`, toolCallID).Scan(&arguments); err != nil {
			return nil, err
		}
		loaded.arguments = json.RawMessage(arguments)
	default:
		return nil, err
	}
	// 冻结连接绑定（授权事务内落表）；无 grant 的 quoin_routed 工具保持
	// 零值 Conn。
	var connectionID, revisionID int64
	var connectionType, configJSON string
	err = reader.QueryRowContext(ctx, `
		SELECT c.id, c.type, ag.connection_revision_id, r.config_json
		FROM tool_call_connection_grants tcg
		JOIN attempt_connection_grants ag ON ag.id = tcg.connection_grant_id
		JOIN connections c ON c.id = ag.connection_id
		JOIN connection_revisions r ON r.id = ag.connection_revision_id
		WHERE tcg.tool_call_id = ? ORDER BY tcg.ordinal LIMIT 1`, toolCallID).
		Scan(&connectionID, &connectionType, &revisionID, &configJSON)
	switch {
	case err == nil:
		loaded.conn = plugins.Connection{ID: connectionID, RevisionID: revisionID, Type: connectionType, Settings: json.RawMessage(configJSON)}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	return loaded, nil
}

// invokePluginTool 派发一次插件工具执行并组装封存请求。Invoke 的返回字节
// 就是密封结果载荷；非 nil error 是执行级失败，按哨兵分类为稳定错误码。
func (service *RuntimeService) invokePluginTool(ctx context.Context, attempts *attempt.Service, loaded *routedToolContext, entry plugins.ToolEntry) toolCallSeal {
	caller := &gatewayPlatformCaller{gateway: service.SteleGateway, conn: loaded.conn}
	spill := service.routedSpillFunc(loaded)
	result, err := entry.Invoke(ctx, plugins.ToolExecution{
		Arguments: loaded.arguments, Conn: loaded.conn, Platform: caller, Spill: spill,
	})
	if err != nil {
		return routedFailureSeal(loaded, routedToolErrorCode(err), boundedRoutedDetail(err.Error()))
	}
	if len(result) == 0 || !routedJSONValid(result) {
		return routedFailureSeal(loaded, "tool_error", "tool handler returned a non-object result payload")
	}
	return toolCallSeal{
		attemptID: loaded.attemptID, toolCallID: loaded.toolCallID,
		outcome: "succeeded", schemaKind: loaded.resultSchemaKind, canonical: result,
	}
}

// alertsRecentRow 是一次 alerts_recent 查询结果的单行投影（ADR-0012 归一化
// 语义：severity/title/resource 均来自 occurrence 首观测冻结列）。
type alertsRecentRow struct {
	OccurrenceID int64
	Severity     string
	Title        string
	State        string
	StartedAt    string
	Resource     string
	ViewKeys     []string
}

// invokeAlertsRecentTool 执行平台工具 alerts_recent（ADR-0012：读 Quoin 自有
// 告警库 = 平台工具）。只读查询走 attempt 机器注入的只读 reader（与 artifact
// 工具同一围栏/封存路径；不包含平台故障——那只查 alert_occurrences）。
// severity 词表排序在 SQL 里用 CASE 投影为可比较序数；viewKey 过滤按首观测
// 冻结的关联快照命中，不回读当前视图配置。
func (service *RuntimeService) invokeAlertsRecentTool(ctx context.Context, attempts *attempt.Service, loaded *routedToolContext) toolCallSeal {
	var arguments map[string]any
	if err := json.Unmarshal(loaded.arguments, &arguments); err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", "arguments unparseable: "+err.Error())
	}
	viewKey, _ := arguments["viewKey"].(string)
	severityMin := "info"
	if value, exists := arguments["severityMin"].(string); exists && value != "" {
		severityMin = value
	}
	hours := 24.0
	if value, ok := arguments["hours"].(float64); ok && value >= 1 && value <= 168 {
		hours = value
	}
	limit := 10.0
	if value, ok := arguments["limit"].(float64); ok && value >= 1 && value <= 50 {
		limit = value
	}
	reader := attempts.Reader()
	now := time.Now().UTC()
	windowStart := now.Add(-time.Duration(hours * float64(time.Hour)))
	rows, err := reader.QueryContext(ctx, `
		SELECT o.id, o.severity, o.title, o.state, o.starts_at, o.resource
		FROM alert_occurrences o
		WHERE o.starts_at >= ?
		  AND CASE o.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END
		      >= CASE ? WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END
		  AND (? = '' OR EXISTS (
		    SELECT 1 FROM alert_occurrence_correlations c
		    WHERE c.occurrence_id = o.id AND c.view_key = ?))
		ORDER BY o.starts_at DESC, o.id DESC
		LIMIT ?`, windowStart.Format(time.RFC3339Nano), severityMin, viewKey, viewKey, int64(limit))
	if err != nil {
		return routedFailureSeal(loaded, "alerts_recent_failed", boundedRoutedDetail(err.Error()))
	}
	results := make([]alertsRecentRow, 0, int(limit))
	for rows.Next() {
		var row alertsRecentRow
		if err := rows.Scan(&row.OccurrenceID, &row.Severity, &row.Title, &row.State, &row.StartedAt, &row.Resource); err != nil {
			rows.Close()
			return routedFailureSeal(loaded, "alerts_recent_failed", boundedRoutedDetail(err.Error()))
		}
		results = append(results, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return routedFailureSeal(loaded, "alerts_recent_failed", boundedRoutedDetail(err.Error()))
	}
	// total 是同过滤条件下的全量命中数（不受本次 limit 截断影响）。
	total := 0
	if err := reader.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM alert_occurrences o
		WHERE o.starts_at >= ?
		  AND CASE o.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END
		      >= CASE ? WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END
		  AND (? = '' OR EXISTS (
		    SELECT 1 FROM alert_occurrence_correlations c
		    WHERE c.occurrence_id = o.id AND c.view_key = ?))`,
		windowStart.Format(time.RFC3339Nano), severityMin, viewKey, viewKey).Scan(&total); err != nil {
		return routedFailureSeal(loaded, "alerts_recent_failed", boundedRoutedDetail(err.Error()))
	}
	if err := attachAlertsRecentViewKeys(ctx, reader, results); err != nil {
		return routedFailureSeal(loaded, "alerts_recent_failed", boundedRoutedDetail(err.Error()))
	}
	alerts := make([]map[string]any, 0, len(results))
	for _, row := range results {
		alerts = append(alerts, map[string]any{
			"occurrenceId": strconv.FormatInt(row.OccurrenceID, 10),
			"severity":     row.Severity,
			"title":        row.Title,
			"state":        row.State,
			"startedAt":    row.StartedAt,
			"resource":     row.Resource,
			"viewKeys":     row.ViewKeys,
		})
	}
	payload := map[string]any{
		"alerts": alerts,
		"total":  total,
		"window": windowStart.Format(time.RFC3339Nano) + "/" + now.Format(time.RFC3339Nano),
	}
	return routedSuccessSeal(loaded, payload)
}

// attachAlertsRecentViewKeys 就地补齐每条命中告警的关联视图 key 列表
// （按 matched_at 稳定序；无关联为空数组）。
func attachAlertsRecentViewKeys(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, results []alertsRecentRow) error {
	if len(results) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(results))
	arguments := make([]any, 0, len(results))
	for index := range results {
		placeholders = append(placeholders, "?")
		arguments = append(arguments, results[index].OccurrenceID)
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT occurrence_id, view_key FROM alert_occurrence_correlations
		WHERE occurrence_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY occurrence_id, matched_at, id`, arguments...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64][]string, len(results))
	for rows.Next() {
		var occurrenceID int64
		var viewKey string
		if err := rows.Scan(&occurrenceID, &viewKey); err != nil {
			return err
		}
		byID[occurrenceID] = append(byID[occurrenceID], viewKey)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range results {
		results[index].ViewKeys = byID[results[index].OccurrenceID]
		if results[index].ViewKeys == nil {
			results[index].ViewKeys = []string{}
		}
	}
	return nil
}

// invokeArtifactTool 执行平台 quoin_routed 工具（artifact_read/artifact_grep）。
// 参数/结果契约与原 Plinth 执行器一致：读取直接走进程内 ArtifactService
// 背后的 artifact.Store（不经 gRPC 自调）。
func (service *RuntimeService) invokeArtifactTool(ctx context.Context, loaded *routedToolContext) toolCallSeal {	if service.Artifacts == nil {
		return routedFailureSeal(loaded, "artifact_store_unavailable", "artifact store is not wired")
	}
	var arguments map[string]any
	if err := json.Unmarshal(loaded.arguments, &arguments); err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", "arguments unparseable: "+err.Error())
	}
	switch loaded.toolName {
	case "artifact_read":
		return service.executeArtifactRead(ctx, loaded, arguments)
	case "artifact_grep":
		return service.executeArtifactGrep(ctx, loaded, arguments)
	default:
		return routedFailureSeal(loaded, "no_executor", "platform quoin_routed tool "+loaded.toolName+" is not implemented")
	}
}

// executeArtifactRead 按范围读取一个 Artifact 的文本片段（RUNTIME-ARTIFACT-
// 002/003 的进程内形态；attempt/boot/epoch/grant 围栏由 store 校验）。
func (service *RuntimeService) executeArtifactRead(ctx context.Context, loaded *routedToolContext, arguments map[string]any) toolCallSeal {
	artifactID, err := parseArtifactLocator(arguments["artifactId"])
	if err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", err.Error())
	}
	startLine := int64(1)
	maxLines := int64(2000)
	if offset, ok := arguments["offset"].(float64); ok && offset >= 1 {
		startLine = int64(offset)
	}
	if limit, ok := arguments["limit"].(float64); ok && limit >= 1 && limit <= 2000 {
		maxLines = int64(limit)
	}
	slice, readErr := service.Artifacts.ReadText(ctx, loaded.attemptID, artifactID, loaded.bootID, loaded.epoch, startLine, int(maxLines))
	if readErr != nil {
		return routedFailureSeal(loaded, "artifact_read_failed", boundedRoutedDetail(readErr.Error()))
	}
	payload := map[string]any{
		"success": true, "output": string(slice.Content),
		"startLine": slice.StartLine, "nextLine": slice.NextLine, "eof": slice.EOF,
		"totalBytes": slice.TotalSizeBytes, "totalLines": slice.TotalLines,
		"artifact": map[string]any{"id": strconv.FormatInt(slice.ArtifactID, 10), "mediaType": slice.MediaType},
	}
	return routedSuccessSeal(loaded, payload)
}

// executeArtifactGrep 在 Artifact 文本内按 RE2 正则搜索（有界匹配；参数/
// 结果契约与原 Plinth 执行器一致）。
func (service *RuntimeService) executeArtifactGrep(ctx context.Context, loaded *routedToolContext, arguments map[string]any) toolCallSeal {
	artifactID, err := parseArtifactLocator(arguments["artifactId"])
	if err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", err.Error())
	}
	pattern, _ := arguments["pattern"].(string)
	if pattern == "" {
		return routedFailureSeal(loaded, "invalid_arguments", "pattern 必须是非空字符串")
	}
	result, grepErr := service.Artifacts.GrepText(ctx, loaded.attemptID, artifactID, loaded.bootID, loaded.epoch, pattern, 200, 5)
	if grepErr != nil {
		return routedFailureSeal(loaded, "artifact_grep_failed", boundedRoutedDetail(grepErr.Error()))
	}
	lines := make([]string, 0, len(result.Matches))
	for _, match := range result.Matches {
		lines = append(lines, fmt.Sprintf("%d:%s", match.LineNumber, match.Line))
	}
	payload := map[string]any{
		"success": true, "output": strings.Join(lines, "\n"),
		"matchCount": len(lines), "truncated": result.Truncated,
		"totalBytes": result.TotalSizeBytes, "totalLines": result.TotalLines,
		"artifact": map[string]any{"id": strconv.FormatInt(result.ArtifactID, 10), "mediaType": result.MediaType},
	}
	return routedSuccessSeal(loaded, payload)
}

// gatewayPlatformCaller 把网关绑定到一次执行的冻结连接上：PlatformCaller
// 接口本身不携带连接信息，绑定发生在编排侧（模型绝不选择连接）。
type gatewayPlatformCaller struct {
	gateway *steleGateway
	conn    plugins.Connection
}

func (caller *gatewayPlatformCaller) Call(ctx context.Context, req plugins.PlatformRequest) (*plugins.PlatformResponse, error) {
	if caller.gateway == nil {
		return nil, fmt.Errorf("%w: stele gateway is not wired", plugins.ErrPlatformUnreachable)
	}
	return caller.gateway.Execute(ctx, caller.conn.ID, caller.conn.RevisionID, req)
}

// routedSpillFunc 构造本次执行的長正文溢出：走 tool_result Artifact 的既有
// 提交路径（与 gRPC 上传同一条 BeginUpload/CommitUpload 管线，attempt/
// tool_call 归属与上传围栏由 store 校验）。
func (service *RuntimeService) routedSpillFunc(loaded *routedToolContext) plugins.SpillFunc {
	return func(ctx context.Context, body []byte, mediaType string) (int64, error) {
		if service.Artifacts == nil {
			return 0, errors.New("artifact store is not wired")
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		uploadID, err := newRoutedUploadID()
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(body)
		header := artifact.UploadHeader{
			RuntimeSlot: qruntime.SlotPlinth, UploadID: uploadID, AttemptID: loaded.attemptID,
			BootID: loaded.bootID, ConnectionEpoch: loaded.epoch,
			OwnerType: "tool_call", OwnerID: loaded.toolCallID,
			Kind: "tool_result", RetentionKind: "generated", Sensitive: false,
			SizeBytes: int64(len(body)), SHA256: sum[:], MediaType: mediaType,
		}
		file, replayID, err := service.Artifacts.BeginUpload(ctx, header)
		if err != nil {
			return 0, err
		}
		if replayID != 0 {
			// 同一幂等能力的已提交重放：直接返回既有 artifact。
			return replayID, nil
		}
		if _, err := file.Write(body); err != nil {
			_ = file.Close()
			service.Artifacts.AbortUpload(uploadID)
			return 0, err
		}
		return service.Artifacts.CommitUpload(ctx, header, file)
	}
}

// sendExternalToolResult 把封存结果经 Control 流推给 Plinth（boot/epoch
// 围栏与现有下发帧一致；Plinth supervisor 转发 worker 继续 agent 循环）。
// 下发失败只审计不重试：ledger 已是权威，Plinth 侧重连/对账由既有机制
// 收敛。
func (service *RuntimeService) sendExternalToolResult(ctx context.Context, loaded *routedToolContext, seal toolCallSeal, receipt sealReceipt) {
	outcome := runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_FAILED
	switch seal.outcome {
	case "succeeded":
		outcome = runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_SUCCEEDED
	case "cancelled":
		outcome = runtimev1.ToolCallOutcome_TOOL_CALL_OUTCOME_CANCELLED
	}
	frame := &runtimev1.ExternalToolResult{
		AttemptId: loaded.attemptID, ToolCallId: loaded.toolCallID, Outcome: outcome,
		Payload: receipt.committedPayload, ArtifactRef: receipt.artifactRef,
		EvidenceIds: receipt.evidenceIDs, ErrorCode: seal.errorCode, ErrorDetail: seal.errorDetail,
	}
	if err := service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: loaded.epoch, CorrelationId: uint64(loaded.attemptID), BootId: loaded.bootID,
		Msg: &runtimev1.ControlEnvelope_ExternalToolResult{ExternalToolResult: frame},
	}); err != nil {
		sharedops.LogEvent("quoin", "error", "toolcall.external_result_send_failed",
			fmt.Sprintf("attempt=%d tool_call=%d: %v", loaded.attemptID, loaded.toolCallID, err))
	}
}

// routedFailureSeal 组装一次确定性失败封存：结构化失败载荷（success=false）
// 携带工具自身的 result schema kind——return_to_model 语义要求模型可见的
// 失败结果；fail_attempt 只需错误码，载荷同样无害。
func routedFailureSeal(loaded *routedToolContext, code, detail string) toolCallSeal {
	payload, _ := json.Marshal(map[string]any{"success": false, "errorCode": code, "errorDetail": boundedRoutedDetail(detail)})
	return toolCallSeal{
		attemptID: loaded.attemptID, toolCallID: loaded.toolCallID,
		outcome: "failed", schemaKind: loaded.resultSchemaKind, canonical: payload,
		errorCode: code, errorDetail: boundedRoutedDetail(detail),
	}
}

// routedSuccessSeal 组装一次成功封存。
func routedSuccessSeal(loaded *routedToolContext, payload map[string]any) toolCallSeal {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return routedFailureSeal(loaded, "tool_error", "result payload marshal failed: "+err.Error())
	}
	return toolCallSeal{
		attemptID: loaded.attemptID, toolCallID: loaded.toolCallID,
		outcome: "succeeded", schemaKind: loaded.resultSchemaKind, canonical: canonical,
	}
}

// routedToolErrorCode 把执行错误映射到稳定工具错误码（哨兵优先）。
func routedToolErrorCode(err error) string {
	switch {
	case errors.Is(err, plugins.ErrPlatformRateLimited):
		return "rate_limited"
	case errors.Is(err, plugins.ErrCredentialUnavailable):
		return "credential_unavailable"
	case errors.Is(err, plugins.ErrPlatformUnreachable):
		return "unreachable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "tool_error"
	}
}

// boundedRoutedDetail 截断非秘密错误说明。
func boundedRoutedDetail(detail string) string {
	if len(detail) > routedErrorDetailBound {
		return detail[:routedErrorDetailBound] + "…"
	}
	return detail
}

// routedJSONValid 断言结果载荷是 JSON 对象（attempts.CompleteToolCall 对
// 成功预览的既有约束）。
func routedJSONValid(body []byte) bool {
	var object map[string]any
	return json.Unmarshal(body, &object) == nil && object != nil
}

// parseArtifactLocator 解析十进制 artifact 定位符。
func parseArtifactLocator(value any) (int64, error) {
	text, _ := value.(string)
	id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("artifactId 必须是十进制正整数定位符")
	}
	return id, nil
}

// newRoutedUploadID 生成一次进程内溢出提交的幂等能力标识。
func newRoutedUploadID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "quoin-routed-" + hex.EncodeToString(buf), nil
}
