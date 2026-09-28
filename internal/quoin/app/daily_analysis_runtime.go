package app

// 日报 Agent 分析的 runtime 切片（ADR-0014）：封存日报的 Queued 分析
// Attempt 绑定到活跃 Plinth 流、结果提案裁决，以及 Quoin 只读工具
// daily_report_get 的执行器。模型取数的权威是创建时冻结的报告定位符：
// 执行器把请求定位符与 Attempt 冻结身份逐一比对，越界读取确定性失败，
// 绝不产生任何实时平台查询。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dailyReportToolBodyBound 是 daily_report_get 返回正文的有界上限；超长封存
// 文档降级为截断预览并标记 truncated（与 knowledge_get 既有结果截断语义
// 一致），模型按标记知悉文档不完整。
const dailyReportToolBodyBound = 64 * 1024

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
}

// parseDailyReportGetArguments validates the proposed arguments against the
// fixed schema: exactly the three keys, non-empty strings, YYYY-MM-DD local
// date, positive integer version.
func parseDailyReportGetArguments(raw json.RawMessage) (dailyReportGetArguments, error) {
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return dailyReportGetArguments{}, fmt.Errorf("arguments unparseable: %w", err)
	}
	if len(arguments) != 3 {
		return dailyReportGetArguments{}, fmt.Errorf("arguments must carry exactly configKey, localDate and version")
	}
	configKey, _ := arguments["configKey"].(string)
	localDate, _ := arguments["localDate"].(string)
	versionNumber, _ := arguments["version"].(float64)
	// 日期形状在执行器侧复核（与工具声明的固定 schema 同形）：绕过入口校验
	// 的参数在这里确定性拒绝。
	if configKey == "" || localDate == "" || !dailyToolLocalDatePattern.MatchString(localDate) ||
		versionNumber < 1 || versionNumber != float64(int64(versionNumber)) {
		return dailyReportGetArguments{}, fmt.Errorf("arguments must carry a non-empty configKey, a YYYY-MM-DD localDate and a positive integer version")
	}
	return dailyReportGetArguments{ConfigKey: configKey, LocalDate: localDate, Version: int64(versionNumber)}, nil
}

// dailyToolLocalDatePattern 是 daily_report_get 执行器的本地日期形状。
var dailyToolLocalDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// invokeDailyReportGetTool 执行平台工具 daily_report_get（ADR-0014：读 Quoin
// 自有封存日报 = 平台工具）。授权是双重的：工具只出现在日报总结代的冻结
// 目录里，且请求定位符必须与 Attempt 创建时冻结的报告身份逐字一致——任何
// 不一致都是确定性失败，模型看到结构化错误而不是别的报告。执行只读已提交
// 的不可变版本行，绝不触发平台查询。
func (service *RuntimeService) invokeDailyReportGetTool(ctx context.Context, attempts *attempt.Service, loaded *routedToolContext) toolCallSeal {
	arguments, err := parseDailyReportGetArguments(loaded.arguments)
	if err != nil {
		return routedFailureSeal(loaded, "invalid_arguments", err.Error())
	}
	reader := attempts.Reader()
	var reportID, frozenVersion int64
	err = reader.QueryRowContext(ctx, `
		SELECT a.scope_id, s.inspection_report_version
		FROM execution_attempts a JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='inspection_daily_analysis' AND a.scope_type='daily_report'`, loaded.attemptID).
		Scan(&reportID, &frozenVersion)
	if err != nil {
		return routedFailureSeal(loaded, "not_daily_analysis", "attempt does not carry a frozen daily report identity: "+err.Error())
	}
	var configKey, localDate string
	if err := reader.QueryRowContext(ctx, `
		SELECT config_key,local_date FROM inspection_daily_reports WHERE id=?`, reportID).Scan(&configKey, &localDate); err != nil {
		return routedFailureSeal(loaded, "report_unavailable", "frozen daily report lookup failed: "+err.Error())
	}
	if arguments.ConfigKey != configKey || arguments.LocalDate != localDate || arguments.Version != frozenVersion {
		return routedFailureSeal(loaded, "forbidden_locator",
			"requested locator does not match the attempt's frozen daily report identity (only the frozen configKey/localDate/version is readable)")
	}
	var content string
	err = reader.QueryRowContext(ctx, `
		SELECT content FROM inspection_daily_report_versions WHERE report_id=? AND version=?`, reportID, frozenVersion).Scan(&content)
	if err == sql.ErrNoRows {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report version is missing")
	}
	if err != nil {
		return routedFailureSeal(loaded, "report_unavailable", "sealed daily report read failed: "+err.Error())
	}
	if len(content) > dailyReportToolBodyBound {
		preview := map[string]any{
			"configKey": configKey, "localDate": localDate, "version": frozenVersion,
			"reportPreview":   content[:dailyReportToolBodyBound],
			"truncated":       true,
			"contentSizeByte": len(content),
		}
		return routedSuccessSeal(loaded, preview)
	}
	payload := map[string]any{
		"configKey": configKey, "localDate": localDate, "version": frozenVersion,
		"report":    json.RawMessage(content),
		"truncated": false,
	}
	return routedSuccessSeal(loaded, payload)
}
