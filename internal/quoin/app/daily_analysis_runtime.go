package app

// 日报 Agent 分析的 runtime 切片（ADR-0014）：封存日报的 Queued 分析
// Attempt 绑定到活跃 Plinth 流、结果提案裁决，以及 Quoin 只读工具
// daily_report_get 的执行器。模型取数的权威是创建时冻结的报告定位符：
// 执行器把请求定位符与 Attempt 冻结身份逐一比对，越界读取确定性失败，
// 绝不产生任何实时平台查询。

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dailyReportToolPageBound 是 daily_report_get 单页载荷的有界上限：超长封存
// 文档不再降级为截断预览，而是按确定性位置分页（cursor/nextCursor），模型
// 逐页取回直至 nextCursor 为 null——完整遍历永远可能，任何单页都有界。
const dailyReportToolPageBound = 48 * 1024

// dailyReportCheckWindowBytes 是单个超界检查项（其封存 JSON 自己就超过页界，
// 例如超长指标标签值/样本值）的确定性字节窗口目标：窗口尽量落在 JSON token
// 边界，单 token 超窗时退化为 rune 对齐的原始窗口。取 6 KiB 是为 JSON 字符串
// 转义的最坏 6 倍膨胀（控制字符 → \u00XX）留足裕量：6 KiB × 6 = 36 KiB，加
// 页信封仍严格小于页界——任何输入下单个分片页都不可能溢出。
const dailyReportCheckWindowBytes = 6 * 1024

// dailyReportCursorPattern 是分页游标的封闭形状：s<来源序号>:c<检查序号>，
// 或超界检查项分片续读的 s<来源序号>:c<检查序号>:f<分片序号>。
var dailyReportCursorPattern = regexp.MustCompile(`^s(\d+):c(\d+)(?::f(\d+))?$`)

// dailyReportPageCursor 定位封存文档内的一个确定性分页位置：Source 是
// sources 数组序号，Check 是该 source 内 checks 数组的序号（0 = 整个 source
// 从头开始），Fragment 非零时表示该检查项封存 JSON 的分片续读序号。游标只
// 在这份冻结文档内移动，与报告定位符共同构成不可变续读坐标。
type dailyReportPageCursor struct {
	Source   int64
	Check    int64
	Fragment int64
}

func parseDailyReportCursor(raw string) (dailyReportPageCursor, error) {
	if raw == "" {
		return dailyReportPageCursor{}, nil
	}
	match := dailyReportCursorPattern.FindStringSubmatch(raw)
	if match == nil {
		return dailyReportPageCursor{}, fmt.Errorf("cursor must be an exact nextCursor value like s3:c12 or s3:c12:f2")
	}
	source, _ := strconv.ParseInt(match[1], 10, 64)
	check, _ := strconv.ParseInt(match[2], 10, 64)
	var fragment int64
	if match[3] != "" {
		fragment, _ = strconv.ParseInt(match[3], 10, 64)
	}
	return dailyReportPageCursor{Source: source, Check: check, Fragment: fragment}, nil
}

func (cursor dailyReportPageCursor) String() string {
	if cursor.Fragment == 0 {
		return fmt.Sprintf("s%d:c%d", cursor.Source, cursor.Check)
	}
	return fmt.Sprintf("s%d:c%d:f%d", cursor.Source, cursor.Check, cursor.Fragment)
}

// dailyReportSealedDocument 只解封存文档的外层信封：sources 保留原始字节，
// 分页时逐字复用，绝不重新序列化漂移。
type dailyReportSealedDocument struct {
	SchemaKind     string            `json:"schemaKind"`
	ConfigKey      string            `json:"configKey"`
	LocalDate      string            `json:"localDate"`
	Timezone       string            `json:"timezone"`
	WindowStartUTC string            `json:"windowStartUtc"`
	WindowEndUTC   string            `json:"windowEndUtc"`
	SealedAt       string            `json:"sealedAt"`
	Sources        []json.RawMessage `json:"sources"`
	Totals         json.RawMessage   `json:"totals"`
}

// dailyReportSealedSource 是按检查项切片一个 source 时的封存形状（与
// agentcontext.DailySourceReport 同形；checks 保留原始字节）。
type dailyReportSealedSource struct {
	PlanKey         string            `json:"planKey"`
	DisplayName     string            `json:"displayName,omitempty"`
	ConnectionName  string            `json:"connectionName,omitempty"`
	PluginID        string            `json:"pluginId,omitempty"`
	TemplateID      string            `json:"templateId,omitempty"`
	TemplateVersion string            `json:"templateVersion,omitempty"`
	Enabled         bool              `json:"enabled"`
	SourceEnabled   bool              `json:"sourceEnabled"`
	Missing         bool              `json:"missing,omitempty"`
	Status          string            `json:"status"`
	GapReasons      []string          `json:"gapReasons,omitempty"`
	Checks          []json.RawMessage `json:"checks,omitempty"`
}

// dispatchInspectionDailyAnalysis binds one Queued inspection_daily_analysis
// attempt to the live Plinth stream as an agent attempt.
func (service *RuntimeService) dispatchInspectionDailyAnalysis(ctx context.Context, attemptID int64) error {
	if service.Inspections == nil {
		return fmt.Errorf("inspections are not wired")
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil {
		return err
	}
	if !view.Connected || view.ConnectionEpoch == nil {
		return fmt.Errorf("plinth is not connected")
	}
	attempts := service.Inspections.Attempts()
	if err := attempts.BindToStream(ctx, attemptID, view.BootID, *view.ConnectionEpoch, attempt.DispatchLease, view.ReleaseVersion); err != nil {
		return err
	}
	input, err := attempts.DispatchInputFor(ctx, attemptID)
	if err != nil {
		return err
	}
	var scopeID int64
	if err := service.Inspections.Reader().QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	operationCorrelationID, err := dispatchOperationCorrelation(ctx, service.Inspections.Reader(), attemptID)
	if err != nil {
		return err
	}
	return service.sendEnvelope(qruntime.SlotPlinth, &runtimev1.ControlEnvelope{
		ConnectionEpoch: *view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{DispatchAttempt: &runtimev1.DispatchAttempt{
			AttemptId: attemptID, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_DAILY_ANALYSIS,
			ScopeType: runtimev1.ScopeType_SCOPE_TYPE_DAILY_REPORT, ScopeId: scopeID, OperationCorrelationId: operationCorrelationID,
			LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
			Input:         &runtimev1.AttemptInputSnapshot{SchemaKind: input.SchemaKind, CanonicalJson: input.CanonicalJSON, ContentDigest: input.ContentDigest, AgentVersion: input.AgentVersion},
		}},
	})
}

// dispatchQueuedDailyAnalyses sweeps the queued daily report analyses.
func (service *RuntimeService) dispatchQueuedDailyAnalyses(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	view, err := service.Slots.View(ctx, qruntime.SlotPlinth)
	if err != nil || !view.Connected || view.ConnectionEpoch == nil {
		return
	}
	analysisIDs, scanErr := service.Inspections.Attempts().QueuedAgentAttempts(ctx, "inspection_daily_analysis")
	if scanErr != nil {
		sharedops.LogEvent("quoin", "error", "inspection.daily_analysis_queue_scan", scanErr.Error())
		return
	}
	for _, id := range analysisIDs {
		if dispatchErr := service.dispatchInspectionDailyAnalysis(ctx, id); dispatchErr != nil {
			sharedops.LogEvent("quoin", "error", "inspection.daily_analysis_queue_dispatch", dispatchErr.Error())
		}
	}
}

// ensureDueDailyReportAnalyses runs the system creation retry (ADR-0014:
// 缺模型保留事实并稍后重试). It is connectivity-independent: Queued attempts
// dispatch when the Plinth stream attaches.
func (service *RuntimeService) ensureDueDailyReportAnalyses(ctx context.Context) {
	if service.Inspections == nil {
		return
	}
	if err := service.Inspections.EnsureDueDailyReportAnalyses(ctx); err != nil {
		sharedops.LogEvent("quoin", "error", "inspection.daily_analysis_ensure", err.Error())
	}
}

// handleInspectionDailyResultProposal adjudicates
// inspection_daily_analysis_result_v1 against the frozen sealed facts.
func (service *RuntimeService) handleInspectionDailyResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(), CorrelationId: envelope.GetCorrelationId(), BootId: envelope.GetBootId(),
		Msg: &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted, ack.GetResultAck().Detail = false, reason
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		sharedops.LogEvent("quoin", "error", "inspection.daily_report_rejected", fmt.Sprintf("attempt=%d reason=%s", proposal.GetAttemptId(), reason))
	}
	if service.Inspections == nil {
		reject("inspections are not wired")
		return
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() != "inspection_daily_analysis_result_v1" || len(payload.GetCanonicalJson()) == 0 {
		reject("expected inspection_daily_analysis_result_v1 payload")
		return
	}
	digest := sha256.Sum256(payload.GetCanonicalJson())
	if hex.EncodeToString(digest[:]) != hex.EncodeToString(payload.GetContentDigest()) {
		reject("content digest mismatch")
		return
	}
	if proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED {
		termination := terminationReasonOf(proposal.GetTerminationReason())
		if termination == "" {
			reject("failed outcome requires a termination reason")
			return
		}
		if err := service.Inspections.Attempts().CommitResult(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), false, termination); err != nil {
			reject(err.Error())
			return
		}
		ack.GetResultAck().Accepted = true
		_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
		return
	}
	if proposal.GetOutcome() != runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		reject("unsupported daily analysis outcome")
		return
	}
	if err := service.Inspections.CommitDailyAnalysisProposal(ctx, proposal.GetAttemptId(), proposal.GetBootId(), proposal.GetConnectionEpoch(), payload.GetCanonicalJson()); err != nil {
		reject(err.Error())
		return
	}
	ack.GetResultAck().Accepted = true
	_ = service.sendEnvelope(qruntime.SlotPlinth, ack)
}

// dailyReportGetArguments is the strict argument shape of daily_report_get.
type dailyReportGetArguments struct {
	ConfigKey string
	LocalDate string
	Version   int64
	// Cursor 是可选续读位置（上一页返回的 nextCursor 原文）；缺省 = 首页。
	Cursor string
}

// parseDailyReportGetArguments validates the proposed arguments against the
// fixed schema: exactly the three locator keys plus the optional cursor,
// non-empty strings, YYYY-MM-DD local date, positive integer version.
func parseDailyReportGetArguments(raw json.RawMessage) (dailyReportGetArguments, error) {
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return dailyReportGetArguments{}, fmt.Errorf("arguments unparseable: %w", err)
	}
	for key := range arguments {
		switch key {
		case "configKey", "localDate", "version", "cursor":
		default:
			return dailyReportGetArguments{}, fmt.Errorf("argument %q is not part of the fixed schema", key)
		}
	}
	if len(arguments) != 3 && len(arguments) != 4 {
		return dailyReportGetArguments{}, fmt.Errorf("arguments must carry exactly configKey, localDate, version and the optional cursor")
	}
	configKey, _ := arguments["configKey"].(string)
	localDate, _ := arguments["localDate"].(string)
	cursor, _ := arguments["cursor"].(string)
	versionNumber, _ := arguments["version"].(float64)
	if _, exists := arguments["cursor"]; exists && cursor == "" {
		return dailyReportGetArguments{}, fmt.Errorf("argument %q must be a non-empty nextCursor value", "cursor")
	}
	// 日期形状在执行器侧复核（与工具声明的固定 schema 同形）：绕过入口校验
	// 的参数在这里确定性拒绝。
	if configKey == "" || localDate == "" || !dailyToolLocalDatePattern.MatchString(localDate) ||
		versionNumber < 1 || versionNumber != float64(int64(versionNumber)) {
		return dailyReportGetArguments{}, fmt.Errorf("arguments must carry a non-empty configKey, a YYYY-MM-DD localDate and a positive integer version")
	}
	return dailyReportGetArguments{ConfigKey: configKey, LocalDate: localDate, Version: int64(versionNumber), Cursor: cursor}, nil
}

// dailyToolLocalDatePattern 是 daily_report_get 执行器的本地日期形状。
var dailyToolLocalDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// dailyReportFrozenIdentity 是一次日报只读工具调用的冻结事实源：Attempt
// 创建时冻结的报告身份（报告定位符）加上封存行内的窗口/采证截止。两个
// 日报工具共用同一解析与授权路径。
type dailyReportFrozenIdentity struct {
	ReportID    int64
	Version     int64
	ConfigKey   string
	LocalDate   string
	WindowStart string
	WindowEnd   string
	CutoffAt    string
}

// resolveDailyReportAttempt 把 Attempt 绑定到其冻结的报告事实源；非日报
// Attempt 与行缺失分别返回稳定错误码与模型可见说明。
func resolveDailyReportAttempt(ctx context.Context, attempts *attempt.Service, attemptID int64) (dailyReportFrozenIdentity, string, string) {
	reader := attempts.Reader()
	var identity dailyReportFrozenIdentity
	err := reader.QueryRowContext(ctx, `
		SELECT a.scope_id, s.inspection_report_version
		FROM execution_attempts a JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='inspection_daily_analysis' AND a.scope_type='daily_report'`, attemptID).
		Scan(&identity.ReportID, &identity.Version)
	if err != nil {
		return identity, "not_daily_analysis", "attempt does not carry a frozen daily report identity: " + err.Error()
	}
	err = reader.QueryRowContext(ctx, `
		SELECT config_key,local_date,window_start_utc,window_end_utc,cutoff_at
		FROM inspection_daily_reports WHERE id=?`, identity.ReportID).
		Scan(&identity.ConfigKey, &identity.LocalDate, &identity.WindowStart, &identity.WindowEnd, &identity.CutoffAt)
	if err != nil {
		return identity, "report_unavailable", "frozen daily report lookup failed: " + err.Error()
	}
	return identity, "", ""
}

// invokeDailyReportGetTool 执行平台工具 daily_report_get（ADR-0014：读 Quoin
// 自有封存日报 = 平台工具）。授权是双重的：工具只出现在日报总结代的冻结
// 目录里，且请求定位符必须与 Attempt 创建时冻结的报告身份逐字一致——任何
// 不一致都是确定性失败，模型看到结构化错误而不是别的报告。封存文档按
// 确定性位置分页返回：每页携带精确定位符与 nextCursor 续读坐标，逐页取回
// 直至 nextCursor 为 null（完整遍历永远可能），任何单页载荷有界；单个检查
// 项的封存 JSON 自己超界时，按确定性字节窗口分片返回（checkFragments，
// 逐字节无损），绝不静默截断。执行只读已提交的不可变版本行，绝不触发
// 平台查询。
func (service *RuntimeService) invokeDailyReportGetTool(ctx context.Context, attempts *attempt.Service, loaded *routedToolContext) toolCallSeal {
	arguments, err := parseDailyReportGetArguments(loaded.arguments)
	if err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", err.Error())
	}
	cursor, err := parseDailyReportCursor(arguments.Cursor)
	if err != nil {
		return routedFailureSeal(loaded, "invalid_cursor", err.Error())
	}
	identity, errorCode, errorDetail := resolveDailyReportAttempt(ctx, attempts, loaded.attemptID)
	if errorCode != "" {
		return routedFailureSeal(loaded, errorCode, errorDetail)
	}
	if arguments.ConfigKey != identity.ConfigKey || arguments.LocalDate != identity.LocalDate || arguments.Version != identity.Version {
		return routedFailureSeal(loaded, "forbidden_locator",
			"requested locator does not match the attempt's frozen daily report identity (only the frozen configKey/localDate/version is readable)")
	}
	var content string
	err = attempts.Reader().QueryRowContext(ctx, `
		SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=?`, identity.ReportID, identity.Version).Scan(&content)
	if err == sql.ErrNoRows {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report version is missing")
	}
	if err != nil {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report read failed: "+err.Error())
	}
	var document dailyReportSealedDocument
	if err := json.Unmarshal([]byte(content), &document); err != nil {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report document is malformed: "+err.Error())
	}
	if document.ConfigKey != identity.ConfigKey || document.LocalDate != identity.LocalDate {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report document identity disagrees with its frozen report row")
	}
	page, err := buildDailyReportPage(&document, cursor)
	if err != nil {
		return routedFailureSeal(loaded, "invalid_cursor", err.Error())
	}
	payload := map[string]any{
		// 精确不可变定位符与窗口/汇总随每一页重复：任何一页都自描述且可审计。
		"configKey": identity.ConfigKey, "localDate": identity.LocalDate, "version": identity.Version,
		"schemaKind": document.SchemaKind, "timezone": document.Timezone,
		"windowStartUtc": document.WindowStartUTC, "windowEndUtc": document.WindowEndUTC,
		"sealedAt": document.SealedAt, "totals": json.RawMessage(document.Totals),
		"cursor":  cursor.String(),
		"sources": page.Sources,
	}
	if len(page.CheckFragments) > 0 {
		payload["checkFragments"] = page.CheckFragments
	}
	if page.Next != nil {
		payload["nextCursor"] = page.Next.String()
	}
	return routedSuccessSeal(loaded, payload)
}

// dailyAlertsGetArguments is the strict argument shape of daily_alerts_get:
// the frozen locator plus the optional bounded page window into the
// deterministic result order.
type dailyAlertsGetArguments struct {
	ConfigKey string
	LocalDate string
	Version   int64
	Offset    int64
	Limit     int64
}

// parseDailyAlertsGetArguments validates the proposed arguments: exactly the
// three locator keys plus optional integer offset (≥0) and limit (1..50).
func parseDailyAlertsGetArguments(raw json.RawMessage) (dailyAlertsGetArguments, error) {
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return dailyAlertsGetArguments{}, fmt.Errorf("arguments unparseable: %w", err)
	}
	for key := range arguments {
		switch key {
		case "configKey", "localDate", "version", "offset", "limit":
		default:
			return dailyAlertsGetArguments{}, fmt.Errorf("argument %q is not part of the fixed schema", key)
		}
	}
	if len(arguments) < 3 || len(arguments) > 5 {
		return dailyAlertsGetArguments{}, fmt.Errorf("arguments must carry exactly configKey, localDate, version and the optional offset/limit")
	}
	configKey, _ := arguments["configKey"].(string)
	localDate, _ := arguments["localDate"].(string)
	versionNumber, _ := arguments["version"].(float64)
	offset, limit := int64(0), int64(20)
	if value, exists := arguments["offset"]; exists && value != nil {
		number, ok := value.(float64)
		if !ok || number < 0 || number != float64(int64(number)) {
			return dailyAlertsGetArguments{}, fmt.Errorf("argument %q must be a non-negative integer", "offset")
		}
		offset = int64(number)
	}
	if value, exists := arguments["limit"]; exists && value != nil {
		number, ok := value.(float64)
		if !ok || number < 1 || number > 50 || number != float64(int64(number)) {
			return dailyAlertsGetArguments{}, fmt.Errorf("argument %q must be an integer between 1 and 50", "limit")
		}
		limit = int64(number)
	}
	if configKey == "" || localDate == "" || !dailyToolLocalDatePattern.MatchString(localDate) ||
		versionNumber < 1 || versionNumber != float64(int64(versionNumber)) {
		return dailyAlertsGetArguments{}, fmt.Errorf("arguments must carry a non-empty configKey, a YYYY-MM-DD localDate and a positive integer version")
	}
	return dailyAlertsGetArguments{ConfigKey: configKey, LocalDate: localDate, Version: int64(versionNumber), Offset: offset, Limit: limit}, nil
}

// dailyAlertRow is one normalized window alert observation's bounded
// projection (identity columns only — never labels/annotations bodies or
// credentials). startedTime is the parsed ordering key and never marshals.
type dailyAlertRow struct {
	OccurrenceID int64
	SourceKey    string
	Severity     string
	Title        string
	State        string
	StartedAt    string
	Resource     string
	FirstSeenAt  string
	startedTime  time.Time
	ViewKeys     []string
}

// invokeDailyAlertsGetTool 执行平台工具 daily_alerts_get（用户故事 11：窗口
// 级告警上下文）。窗口与采证截止都来自 Attempt 冻结的报告行，绝不来自模型
// 参数：SQL 先用加宽的保守边界取候选，再在 Go 侧按 RFC3339Nano 精确解析
// 过滤（starts_at ∈ [windowStartUtc, windowEndUtc) 且 first_seen_at ≤
// cutoff_at）——窗口外与采证截止后提交的观测绝不出现，同一报告版本的每
// 次分析取回同一事实集。结果按 (starts_at, id) 确定性排序后分页；载荷只
// 携带身份列与冻结视图关联，绝不携带原始标签/注释体或凭据。告警上下文是
// 窗口级事实，不与任何日报来源/计划建立未声明的归属关系。
func (service *RuntimeService) invokeDailyAlertsGetTool(ctx context.Context, attempts *attempt.Service, loaded *routedToolContext) toolCallSeal {
	arguments, err := parseDailyAlertsGetArguments(loaded.arguments)
	if err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", err.Error())
	}
	identity, errorCode, errorDetail := resolveDailyReportAttempt(ctx, attempts, loaded.attemptID)
	if errorCode != "" {
		return routedFailureSeal(loaded, errorCode, errorDetail)
	}
	if arguments.ConfigKey != identity.ConfigKey || arguments.LocalDate != identity.LocalDate || arguments.Version != identity.Version {
		return routedFailureSeal(loaded, "forbidden_locator",
			"requested locator does not match the attempt's frozen daily report identity (only the frozen configKey/localDate/version is readable)")
	}
	rows, err := service.dailyWindowAlertRows(ctx, attempts, identity)
	if err != nil {
		return routedFailureSeal(loaded, "daily_alerts_failed", boundedRoutedDetail(err.Error()))
	}
	total := int64(len(rows))
	page := []map[string]any{}
	if arguments.Offset < total {
		window := rows[arguments.Offset:]
		if arguments.Limit < int64(len(window)) {
			window = window[:arguments.Limit]
		}
		if err := attachDailyAlertViewKeys(ctx, attempts.Reader(), window); err != nil {
			return routedFailureSeal(loaded, "daily_alerts_failed", boundedRoutedDetail(err.Error()))
		}
		for _, row := range window {
			page = append(page, map[string]any{
				"occurrenceId": strconv.FormatInt(row.OccurrenceID, 10),
				"sourceKey":    row.SourceKey,
				"severity":     row.Severity,
				"title":        row.Title,
				"state":        row.State,
				"startsAt":     row.StartedAt,
				"resource":     row.Resource,
				"viewKeys":     row.ViewKeys,
			})
		}
	}
	payload := map[string]any{
		// 精确不可变定位符与冻结窗口/截止随每页重复。
		"configKey": identity.ConfigKey, "localDate": identity.LocalDate, "version": identity.Version,
		"windowStartUtc": identity.WindowStart, "windowEndUtc": identity.WindowEnd, "cutoffAt": identity.CutoffAt,
		"alerts": page, "total": total,
		"offset": arguments.Offset, "limit": arguments.Limit,
		"hasMore": arguments.Offset+arguments.Limit < total,
	}
	return routedSuccessSeal(loaded, payload)
}

// dailyWindowAlertRows loads the frozen window's committed normalized alert
// observations in deterministic (starts_at, id) order. The SQL bounds are
// deliberately widened conservative supersets; RFC3339Nano strings cannot be
// compared lexicographically at fraction boundaries, so the exact window and
// cutoff membership is decided in Go after parsing — same discipline as the
// daily seal's committed-cutoff check. A malformed committed timestamp is a
// data-integrity failure named with the occurrence id, never silently
// dropped.
func (service *RuntimeService) dailyWindowAlertRows(ctx context.Context, attempts *attempt.Service, identity dailyReportFrozenIdentity) ([]dailyAlertRow, error) {
	windowStart, err := time.Parse(time.RFC3339Nano, identity.WindowStart)
	if err != nil {
		return nil, fmt.Errorf("frozen daily report window start %q is invalid: %w", identity.WindowStart, err)
	}
	windowEnd, err := time.Parse(time.RFC3339Nano, identity.WindowEnd)
	if err != nil {
		return nil, fmt.Errorf("frozen daily report window end %q is invalid: %w", identity.WindowEnd, err)
	}
	cutoff, err := time.Parse(time.RFC3339Nano, identity.CutoffAt)
	if err != nil {
		return nil, fmt.Errorf("frozen daily report cutoff %q is invalid: %w", identity.CutoffAt, err)
	}
	// 加宽 2 秒的保守边界：任何落进窗口/截止内的 RFC3339Nano 文本都不可能
	// 被字典序边界排除（同秒内的小数陷阱由 Go 解析过滤兑底）。
	fixedNano := "2006-01-02T15:04:05.000000000Z"
	sqlStart := windowStart.Add(-2 * time.Second).UTC().Format(fixedNano)
	sqlEnd := windowEnd.Add(2 * time.Second).UTC().Format(fixedNano)
	sqlCutoff := cutoff.Add(2 * time.Second).UTC().Format(fixedNano)
	rows, err := attempts.Reader().QueryContext(ctx, `
		SELECT o.id, s.source_key, o.severity, o.title, o.state, o.starts_at, o.resource, o.first_seen_at, o.resolved_at
		FROM alert_occurrences o JOIN alert_sources s ON s.id=o.source_id
		WHERE o.starts_at >= ? AND o.starts_at < ? AND o.first_seen_at <= ?
		ORDER BY o.starts_at DESC, o.id DESC`, sqlStart, sqlEnd, sqlCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	window := []dailyAlertRow{}
	for rows.Next() {
		var row dailyAlertRow
		var resolvedAt sql.NullString
		if err := rows.Scan(&row.OccurrenceID, &row.SourceKey, &row.Severity, &row.Title, &row.State, &row.StartedAt, &row.Resource, &row.FirstSeenAt, &resolvedAt); err != nil {
			return nil, err
		}
		startedAt, err := time.Parse(time.RFC3339Nano, row.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("alert occurrence %d has an invalid starts_at %q: %w", row.OccurrenceID, row.StartedAt, err)
		}
		firstSeenAt, err := time.Parse(time.RFC3339Nano, row.FirstSeenAt)
		if err != nil {
			return nil, fmt.Errorf("alert occurrence %d has an invalid first_seen_at %q: %w", row.OccurrenceID, row.FirstSeenAt, err)
		}
		if startedAt.Before(windowStart) || !startedAt.Before(windowEnd) || firstSeenAt.After(cutoff) {
			continue
		}
		// alert_occurrences.state is mutable. Its first resolved_at is a
		// committed lifecycle fact and cannot move back to Firing, so derive
		// the state as of this report's frozen cutoff instead of leaking a
		// resolution that only arrived on a later day.
		row.State = "Firing"
		if resolvedAt.Valid {
			resolvedTime, parseErr := time.Parse(time.RFC3339Nano, resolvedAt.String)
			if parseErr != nil {
				return nil, fmt.Errorf("alert occurrence %d has invalid resolved_at %q: %w", row.OccurrenceID, resolvedAt.String, parseErr)
			}
			if !resolvedTime.After(cutoff) {
				row.State = "Resolved"
			}
		}
		row.startedTime = startedAt
		window = append(window, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 确定性排序在 Go 侧按解析后的时间完成：RFC3339Nano 字典序在同秒内
	// 带小数与整秒文本之间不可靠，绝不用作事实顺序。
	slices.SortFunc(window, func(left, right dailyAlertRow) int {
		if order := right.startedTime.Compare(left.startedTime); order != 0 {
			return order
		}
		return cmp.Compare(right.OccurrenceID, left.OccurrenceID)
	})
	return window, nil
}

// attachDailyAlertViewKeys fills each row's frozen business-view keys (same
// frozen correlation snapshot semantics as alerts_recent; ordered by
// matched_at, empty list when uncorrelated).
func attachDailyAlertViewKeys(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, rows []dailyAlertRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(rows))
	arguments := make([]any, 0, len(rows))
	for index := range rows {
		placeholders = append(placeholders, "?")
		arguments = append(arguments, rows[index].OccurrenceID)
	}
	result, err := reader.QueryContext(ctx, `
		SELECT occurrence_id, view_key FROM alert_occurrence_correlations
		WHERE occurrence_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY occurrence_id, matched_at, id`, arguments...)
	if err != nil {
		return err
	}
	defer result.Close()
	byID := map[int64][]string{}
	for result.Next() {
		var occurrenceID int64
		var viewKey string
		if err := result.Scan(&occurrenceID, &viewKey); err != nil {
			return err
		}
		byID[occurrenceID] = append(byID[occurrenceID], viewKey)
	}
	if err := result.Err(); err != nil {
		return err
	}
	for index := range rows {
		rows[index].ViewKeys = byID[rows[index].OccurrenceID]
		if rows[index].ViewKeys == nil {
			rows[index].ViewKeys = []string{}
		}
	}
	return nil
}

// dailyReportCheckFragment 是一个超界检查项的一页分片：Text 是该检查项封存
// JSON 的逐字字节窗口（UTF-8 安全），按 (sourceIndex, checkIndex, byteOffset)
// 升序拼接后即还原完整封存字节——分片只切窗口，绝不改写或省略任何字节，
// 因此完整遍历仍然无损。note 是字节恒定的固定续读说明。
type dailyReportCheckFragment struct {
	SourceIndex int64  `json:"sourceIndex"`
	CheckIndex  int64  `json:"checkIndex"`
	ByteOffset  int64  `json:"byteOffset"`
	ByteLength  int64  `json:"byteLength"`
	TotalBytes  int64  `json:"totalBytes"`
	Text        string `json:"text"`
	Note        string `json:"note"`
}

// dailyReportFragmentNote 是每个分片页携带的固定协议说明（字节恒定）：
// 告诉模型分片如何重组、为什么不得按单片直接下结论。
const dailyReportFragmentNote = "该检查项的封存 JSON 超过单页上限，已按确定性字节窗口分片：同一定位符的连续分片页按 byteOffset 升序拼接 text 即为该检查项的完整封存字节；必须逐片取回（直到 nextCursor 为 null）后才可引用该检查项的事实，不得按单个分片段落直接下结论。"

// dailyReportPageResult 是一页的确定性产物：sources 页（整 source 或按检查
// 项切片的 source 前缀，逐字节复用封存原文）或一个超界检查项的分片组。
type dailyReportPageResult struct {
	Sources        []json.RawMessage
	CheckFragments []dailyReportCheckFragment
	Next           *dailyReportPageCursor
}

// buildDailyReportPage packs the sealed document's sources into one bounded
// page starting at the cursor position. Whole sources are reused byte-for-
// byte; a source that does not fit (or a mid-source continuation) is split
// at its check array, re-emitting the source envelope with a bounded slice
// of verbatim checks. A single check whose own sealed JSON exceeds the page
// bound (huge metric label values) is emitted as a deterministic sequence of
// byte-window fragment pages instead — never truncated, never unbounded. The
// walk is deterministic: the same frozen document and cursor always yield the
// same page and nextCursor. Next is nil once every source has been emitted
// (complete traversal).
func buildDailyReportPage(document *dailyReportSealedDocument, cursor dailyReportPageCursor) (dailyReportPageResult, error) {
	total := int64(len(document.Sources))
	if cursor.Source > total || (cursor.Source == total && (cursor.Check > 0 || cursor.Fragment > 0)) {
		return dailyReportPageResult{}, fmt.Errorf("cursor %s is past the end of the sealed document (%d sources)", cursor, total)
	}
	if cursor.Fragment > 0 {
		return buildDailyCheckFragmentPage(document, cursor)
	}
	page := []json.RawMessage{}
	// pageSize 是当前页已选 sources 的精确字节数：每次候选追加都真实重新
	// 序列化，杜绝估算漂移（页信封与分片页共用 ≤512 字节裕量，与页界测试
	// 同一约定）。
	pageSize := func(fragments []json.RawMessage) int {
		encoded, err := json.Marshal(map[string]any{"sources": fragments})
		if err != nil {
			return int(^uint(0) >> 1) // 不可达：payload 仅含 JSON 字节
		}
		return len(encoded)
	}
	si, ci := cursor.Source, cursor.Check
	for si < total {
		if ci == 0 {
			candidate := append(append([]json.RawMessage{}, page...), document.Sources[si])
			if pageSize(candidate) <= dailyReportToolPageBound {
				page = candidate
				si, ci = si+1, 0
				continue
			}
			if len(page) > 0 {
				return dailyReportPageResult{Sources: page, Next: &dailyReportPageCursor{Source: si}}, nil
			}
			// 空页且整个 source 超界：按检查项切片该 source。
		}
		var sealed dailyReportSealedSource
		if err := json.Unmarshal(document.Sources[si], &sealed); err != nil {
			return dailyReportPageResult{}, fmt.Errorf("sealed daily report source %d is malformed: %w", si, err)
		}
		if ci >= int64(len(sealed.Checks)) {
			// 合法游标永远不会指向已耗尽的检查序号：分片页取完检查项后直接
			// 生成下一个检查项/下一个 source 的游标。这里是确定性拒绝伪造游标。
			return dailyReportPageResult{}, fmt.Errorf("cursor %s points past the %d checks of source %d", cursor, len(sealed.Checks), si)
		}
		partial := sealed
		prior := append([]json.RawMessage{}, page...)
		kept := int64(0)
		oversized := false
		for kept < int64(len(sealed.Checks))-ci {
			partial.Checks = sealed.Checks[ci : ci+kept+1]
			// 候选页始终是「此前完整 sources + 本 source 的当前片段」：片段随
			// kept 增长而替换，绝不与前缀叠加重复。
			candidate := append(append([]json.RawMessage{}, prior...), mustMarshalJSON(partial))
			if pageSize(candidate) > dailyReportToolPageBound {
				if len(prior) > 0 || kept > 0 {
					break // 页面已有内容：在本 source 前收页，游标原地等待下一页。
				}
				// 空页的首个检查项自己就超界：不再无条件整块塞入（旧守卫的
				// 无界漏洞），改走检查项分片页——逐字节无损且有界。
				oversized = true
				break
			}
			page = candidate
			kept++
		}
		if oversized {
			return buildDailyCheckFragmentPage(document, dailyReportPageCursor{Source: si, Check: ci + kept})
		}
		if kept == 0 {
			if len(page) == 0 {
				return dailyReportPageResult{}, fmt.Errorf("sealed daily report source %d cannot make progress at cursor %s", si, cursor)
			}
			// 页面已有内容且本 source 首片就超界：收页，下一页从原地重试
			//（下一页为空页，必然至少取一个检查项或进入分片页）。
			return dailyReportPageResult{Sources: page, Next: &dailyReportPageCursor{Source: si, Check: ci}}, nil
		}
		if consumed := ci + kept; consumed < int64(len(sealed.Checks)) {
			return dailyReportPageResult{Sources: page, Next: &dailyReportPageCursor{Source: si, Check: consumed}}, nil
		}
		si, ci = si+1, 0
	}
	return dailyReportPageResult{Sources: page}, nil
}

// buildDailyCheckFragmentPage 续读一个超界检查项的确定性字节窗口分片：
// 页内按序装入尽可能多的分片（每次真实重新序列化度量），信封与精确续读
// 坐标随每页重复。单个分片页按构造必然有界（6 KiB 窗口 × 最坏 6 倍 JSON
// 转义 < 页界），此处仍以真实度量兑底：越界即确定性失败，绝不静默截断。
// 分片全部取完后，游标推进到该 source 的下一个检查项（或下一个 source）。
func buildDailyCheckFragmentPage(document *dailyReportSealedDocument, cursor dailyReportPageCursor) (dailyReportPageResult, error) {
	if cursor.Source >= int64(len(document.Sources)) {
		return dailyReportPageResult{}, fmt.Errorf("cursor %s is past the end of the sealed document", cursor)
	}
	var sealed dailyReportSealedSource
	if err := json.Unmarshal(document.Sources[cursor.Source], &sealed); err != nil {
		return dailyReportPageResult{}, fmt.Errorf("sealed daily report source %d is malformed: %w", cursor.Source, err)
	}
	if cursor.Check >= int64(len(sealed.Checks)) {
		return dailyReportPageResult{}, fmt.Errorf("cursor %s points past the %d checks of source %d", cursor, len(sealed.Checks), cursor.Source)
	}
	check := sealed.Checks[cursor.Check]
	fragments := dailyCheckJSONFragments(check, dailyReportCheckWindowBytes)
	if cursor.Fragment >= int64(len(fragments)) {
		return dailyReportPageResult{}, fmt.Errorf("cursor %s points past the %d fragments of source %d check %d", cursor, len(fragments), cursor.Source, cursor.Check)
	}
	candidates := make([]dailyReportCheckFragment, 0, len(fragments)-int(cursor.Fragment))
	offset := int64(0)
	for index := int64(0); index < cursor.Fragment; index++ {
		offset += int64(len(fragments[index]))
	}
	for index := cursor.Fragment; index < int64(len(fragments)); index++ {
		window := fragments[index]
		candidates = append(candidates, dailyReportCheckFragment{
			SourceIndex: cursor.Source, CheckIndex: cursor.Check,
			ByteOffset: offset, ByteLength: int64(len(window)), TotalBytes: int64(len(check)),
			Text: string(window), Note: dailyReportFragmentNote,
		})
		offset += int64(len(window))
	}
	page := []dailyReportCheckFragment{}
	for index := 0; index < len(candidates); index++ {
		candidate := append(append([]dailyReportCheckFragment{}, page...), candidates[index])
		if len(page) > 0 {
			if encoded, err := json.Marshal(map[string]any{"checkFragments": candidate}); err != nil || len(encoded) > dailyReportToolPageBound {
				break // 不可达的 marshal 错误与页界溢出同判：停止装入，剩余分片下一页。
			}
		}
		page = candidate
	}
	if len(page) == 0 {
		// 构造上不可达（6 KiB 窗口的最坏转义膨胀仍在页界内）；兑底确定性
		// 失败而不是发出任何超界页。
		return dailyReportPageResult{}, fmt.Errorf("sealed check fragment of source %d check %d exceeds the page bound", cursor.Source, cursor.Check)
	}
	result := dailyReportPageResult{Sources: []json.RawMessage{}, CheckFragments: page}
	consumed := cursor.Fragment + int64(len(page))
	var next dailyReportPageCursor
	if consumed < int64(len(fragments)) {
		next = dailyReportPageCursor{Source: cursor.Source, Check: cursor.Check, Fragment: consumed}
	} else if cursor.Check+1 < int64(len(sealed.Checks)) {
		next = dailyReportPageCursor{Source: cursor.Source, Check: cursor.Check + 1}
	} else {
		next = dailyReportPageCursor{Source: cursor.Source + 1}
	}
	if next.Source < int64(len(document.Sources)) {
		result.Next = &next
	}
	return result, nil
}

// dailyCheckJSONFragments deterministically splits one sealed check's JSON
// bytes into bounded UTF-8-safe windows. Cuts land on JSON token boundaries
// when the bytes parse; a single token larger than the window target (a huge
// label value) is further split at rune-aligned raw windows. The same bytes
// always yield the same fragment sequence, and concatenating every fragment
// in order reproduces the input bytes exactly — the split is a pure
// geometry, never a rewrite.
func dailyCheckJSONFragments(check []byte, target int) [][]byte {
	if len(check) <= target {
		return [][]byte{check}
	}
	cuts := dailyJSONTokenCuts(check, target)
	parts := [][]byte{}
	for index := 0; index+1 < len(cuts); index++ {
		start, end := cuts[index], cuts[index+1]
		for start < end {
			limit := end
			if limit-start > target {
				limit = dailyRuneAlignedLimit(check, start, start+target)
			}
			parts = append(parts, check[start:limit])
			start = limit
		}
	}
	return parts
}

// dailyJSONTokenCuts walks the check's JSON tokens and emits cut points so
// that consecutive cuts span at least one window target; a window may exceed
// the target only by carrying one whole oversized token, which the caller
// then sub-splits at rune boundaries. Malformed sealed bytes (a data-
// integrity fault the seal path should never produce) deterministically fall
// back to pure rune-aligned windows instead of panicking or drifting.
func dailyJSONTokenCuts(check []byte, target int) []int {
	cuts := []int{0}
	decoder := json.NewDecoder(bytes.NewReader(check))
	for {
		offset := decoder.InputOffset()
		if offset >= int64(len(check)) {
			break
		}
		if int(offset) >= cuts[len(cuts)-1]+target {
			cuts = append(cuts, int(offset))
			continue
		}
		if _, err := decoder.Token(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return dailyRawWindowCuts(check, target)
		}
	}
	if cuts[len(cuts)-1] != len(check) {
		cuts = append(cuts, len(check))
	}
	return cuts
}

// dailyRawWindowCuts is the rune-aligned fallback tiling for bytes that do
// not parse as JSON: fixed-size windows whose boundaries never split a
// multi-byte rune.
func dailyRawWindowCuts(check []byte, target int) []int {
	cuts := []int{0}
	for start := 0; start < len(check); {
		limit := start + target
		if limit >= len(check) {
			cuts = append(cuts, len(check))
			break
		}
		limit = dailyRuneAlignedLimit(check, start, limit)
		cuts = append(cuts, limit)
		start = limit
	}
	return cuts
}

// dailyRuneAlignedLimit walks limit back (at most three bytes, the maximum
// continuation tail of a 4-byte rune) so that check[limit] starts a rune —
// every emitted window is therefore valid UTF-8 on its own.
func dailyRuneAlignedLimit(check []byte, start, limit int) int {
	for limit > start+3 && isUTF8Continuation(check[limit]) {
		limit--
	}
	return limit
}

func isUTF8Continuation(byteValue byte) bool { return byteValue&0xC0 == 0x80 }

// mustMarshalJSON marshals an in-memory sealed-source projection; the shape
// is our own sealed vocabulary, so a marshal failure is unreachable.
func mustMarshalJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("sealed source projection marshal failed: %v", err))
	}
	return encoded
}
