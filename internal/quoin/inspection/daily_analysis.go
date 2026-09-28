package inspection

// 日报 Agent 分析路径（ADR-0014）：封存后的日报交 Agent 总结。程序机械地把
// 上一个已完成本地日的既有结构化巡检 Run/Check 结果收敛进不可变日报（封存
// 路径已实现），本文件只补齐其后的分析路径：
//
//   - 分析 Attempt 以 'inspection_daily_analysis'/'daily_report' 身份排队，
//     冻结输入只携带报告身份与有界事实索引（来源/检查/缺口/观测时间），
//     绝不内嵌封存正文——模型经 Quoin 只读工具 daily_report_get 按
//     「创建时冻结的精确定位符」取回封存版本文档，不做任何实时平台查询。
//   - 复用既有 Attempt/工具目录/授权机制：per-attempt 冻结目录、chat grant、
//     输入快照 digest 与重建逐字节一致、ResultProposal 账本围栏提交。
//   - 模型结论经 inspection_daily_analysis_result_v1 提案重裁决后，由冻结
//     SQL 闭包在同一事务写入版本化分析行与 Attempt 成功终态；日报事实文档
//     （config/report/version）绝不被分析改写，人工重分析/重跑只追加新版本。
//   - 缺模型时确定性事实保留：创建静默跳过，模型恢复后由周期扫描重试。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const dailyAnalysisInputKind = "inspection_daily_analysis_v1"
const dailyAnalysisResultKind = "inspection_daily_analysis_result_v1"

// dailyAnalysisRendererVersion 是日报分析输入快照的 renderer 代：v2 起
// 渲染携带分页续读指令与窗口级告警上下文工具调用（首代 v1 只携带一体
// daily_report_get 取数指令）。
const dailyAnalysisRendererVersion = "v2"

// Command identities: the durable client_commands.command_type values for the
// system-side daily analysis work (creation retry and result adjudication).
const (
	commandDailyAnalysisEnsure = "inspection_daily_report.analysis.ensure"
	commandDailyAnalysisResult = "inspection_daily_report.result.analysis"
)

// dailyAnalysisInput is the frozen canonical input of one daily report
// analysis. The provenance index (Sources/Totals) is derived from the sealed
// version's stored document — never re-aggregated at creation time — so the
// rebuild reproduces the frozen bytes exactly and the prompt index can never
// disagree with what daily_report_get returns.
type dailyAnalysisInput struct {
	SchemaKind     string              `json:"schemaKind"`
	AttemptID      int64               `json:"attemptId"`
	DailyReportID  int64               `json:"dailyReportId"`
	ConfigKey      string              `json:"configKey"`
	LocalDate      string              `json:"localDate"`
	ReportVersion  int64               `json:"reportVersion"`
	Timezone       string              `json:"timezone"`
	WindowStartUTC string              `json:"windowStartUtc"`
	WindowEndUTC   string              `json:"windowEndUtc"`
	ModelContract  reportModelContract `json:"modelContract"`
	// ToolCatalog 是随本次 Attempt 冻结的工具目录（与 attempt_input_snapshots
	// .tool_catalog_json 同一文档内嵌，digest 覆盖）。
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
	// 有界事实索引：每个事实从哪里来（Run/检查/观测时间/缺口）。正文不在此处。
	Sources []dailySourceReport `json:"sources"`
	Totals  dailyTotals         `json:"totals"`
	// ExpectedOutput 是该版本冻结的人类期望输出（管理员撰写，有界纯文本）；
	// 空串表示使用渲染器内置的安全默认。它只是期望说明，绝不扩展工具/授权。
	ExpectedOutput string `json:"expectedOutput,omitempty"`
}

// EnsureDueDailyReportAnalyses queues the analysis attempt of every sealed
// daily report whose latest sealed version has none yet. It is the system
// retry path (ADR-0014: 缺模型保留事实并稍后重试): a missing model provider
// skips silently and the periodic sweep retries after recovery. A terminal
// failed attempt is NOT retried here — deterministic facts stay untouched and
// a fresh analysis needs a fresh sealed version (人工重跑/重分析) — while
// dispatch-level interrupts converge through the shared lease reconciler.
func (s *Service) EnsureDueDailyReportAnalyses(ctx context.Context) error {
	commandCtx, err := s.taskContext(ctx)
	if err != nil {
		return err
	}
	reportIDs, err := s.dueDailyAnalysisReportIDs(ctx)
	if err != nil {
		return err
	}
	var ensureErrors []error
	for _, reportID := range reportIDs {
		if _, err := execution.Execute(commandCtx, s.runner, s.ensureDailyAnalysis, func(tx *execution.Tx) (int64, error) {
			return s.ensureDailyReportAnalysisOn(commandCtx, tx, reportID, s.nowText())
		}, func(reportID int64) int64 { return reportID }); err != nil {
			ensureErrors = append(ensureErrors, fmt.Errorf("ensure daily analysis %d: %w", reportID, err))
		}
	}
	return errors.Join(ensureErrors...)
}

// dueDailyAnalysisReportIDs lists sealed reports whose latest sealed version
// lacks an analysis attempt while no analysis attempt is active for the
// report (read-only projection; the mutations re-verify inside their
// transactions).
func (s *Service) dueDailyAnalysisReportIDs(ctx context.Context) ([]int64, error) {
	reader, err := s.readReader()
	if err != nil {
		return nil, err
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT r.id FROM inspection_daily_reports r
		WHERE r.state='Sealed'
		  AND EXISTS (SELECT 1 FROM inspection_daily_report_versions v WHERE v.report_id=r.id)
		  AND NOT EXISTS (
		    SELECT 1 FROM execution_attempts a
		    WHERE a.attempt_type='inspection_daily_analysis' AND a.scope_type='daily_report' AND a.scope_id=r.id
		      AND a.state IN ('Queued','Assigned','Running','Cancelling'))
		  AND NOT EXISTS (
		    SELECT 1 FROM execution_attempts a
		    JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		    WHERE a.attempt_type='inspection_daily_analysis' AND a.scope_type='daily_report' AND a.scope_id=r.id
		      AND s.inspection_report_version=(SELECT MAX(v.version) FROM inspection_daily_report_versions v WHERE v.report_id=r.id))
		ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ensureDailyReportAnalysisOn freezes one daily analysis attempt inside the
// caller's transaction. It re-reads the report and the latest sealed version
// under the transaction so the frozen identity is the authoritative sealed
// fact; every idempotence guard re-runs here for the race window.
func (s *Service) ensureDailyReportAnalysisOn(ctx context.Context, tx execution.Executor, reportID int64, now string) (int64, error) {
	var configKey, localDate, timezone, windowStartUTC, windowEndUTC, state string
	err := tx.QueryRowContext(ctx, `
		SELECT config_key,local_date,timezone,window_start_utc,window_end_utc,state
		FROM inspection_daily_reports WHERE id=?`, reportID).
		Scan(&configKey, &localDate, &timezone, &windowStartUTC, &windowEndUTC, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if state != "Sealed" {
		return 0, nil
	}
	var reportVersion int64
	var content string
	var expectedOutput sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT version,content,expected_output FROM inspection_daily_report_versions
		WHERE report_id=? ORDER BY version DESC LIMIT 1`, reportID).Scan(&reportVersion, &content, &expectedOutput)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// 该报告的最新版本已有分析 Attempt（含终态）→ 绝不重复创建；失败恢复走
	// 人工重跑的新版本。任何活跃 Attempt 都阻塞新的创建（一个版本至多一个
	// 存活生产者）。
	var existing int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_attempts a
		JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.attempt_type='inspection_daily_analysis' AND a.scope_type='daily_report' AND a.scope_id=?
		  AND s.inspection_report_version=?`, reportID, reportVersion).Scan(&existing); err != nil {
		return 0, err
	}
	if existing != 0 {
		return 0, nil
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_attempts
		WHERE attempt_type='inspection_daily_analysis' AND scope_type='daily_report' AND scope_id=?
		  AND state IN ('Queued','Assigned','Running','Cancelling')`, reportID).Scan(&existing); err != nil {
		return 0, err
	}
	if existing != 0 {
		return 0, nil
	}
	provider, err := selectReportModelProvider(ctx, tx)
	if err != nil {
		if errors.Is(err, ErrModelProviderMissing) {
			// 缺模型：确定性事实已封存保留；静默跳过，恢复后由周期扫描重试。
			return 0, nil
		}
		return 0, err
	}
	// 事实索引从封存文档本身推导（创建与重建同一解析路径，字节一致）。
	sealed := dailyReportContent{}
	if err := json.Unmarshal([]byte(content), &sealed); err != nil {
		return 0, fmt.Errorf("daily report %d version %d content malformed: %w", reportID, reportVersion, err)
	}
	analysisID, err := attempt.CreateOn(ctx, tx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('inspection_daily_analysis','daily_report',?,'Queued',?,?,?)`, reportID, attempt.ReleaseVersion(), attempt.InspectionDailyAgentVersion, now)
	if err != nil {
		return 0, err
	}
	// 知识接入代起的标准形态：本次 Attempt 冻结工具目录（同一文档内嵌进
	// canonical 输入，digest 覆盖）。
	catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(s.Attempts().Catalogs, attempt.InspectionDailyAgentVersion)
	if err != nil {
		return 0, err
	}
	input := dailyAnalysisInput{
		SchemaKind: dailyAnalysisInputKind, AttemptID: analysisID, DailyReportID: reportID,
		ConfigKey: configKey, LocalDate: localDate, ReportVersion: reportVersion,
		Timezone: timezone, WindowStartUTC: windowStartUTC, WindowEndUTC: windowEndUTC,
		ModelContract: reportModelContract{ModelID: provider.ChatModelID, ContextBudgetTokens: provider.ContextBudget, MaxOutputTokens: provider.MaxOutput},
		ToolCatalog:   catalog,
		Sources:       sealed.Sources, Totals: sealed.Totals,
		ExpectedOutput: expectedOutput.String,
	}
	if input.Sources == nil {
		input.Sources = []dailySourceReport{}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(body)
	snapshot, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,inspection_report_version,created_at)
		VALUES(?,?,?,?,?,?,?)`, analysisID, dailyAnalysisInputKind, dailyAnalysisRendererVersion, hex.EncodeToString(digest[:]), string(catalogDocument), reportVersion, now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return 0, err
	}
	versionDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-daily-report-version:%d:%d", reportID, reportVersion)))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_daily_report_id)
		VALUES(?,1,'daily_report',?,?)`, snapshotID, hex.EncodeToString(versionDigest[:]), reportID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?, 'chat_model', ?,?,?,?,?)`,
		analysisID, provider.ConnectionID, provider.RevisionID, provider.CredentialGen, provider.ProbeResultID, now); err != nil {
		return 0, err
	}
	return analysisID, nil
}

// dailyAnalysisProposal is the typed ResultProposal of one daily analysis.
type dailyAnalysisProposal struct {
	SchemaKind    string `json:"schemaKind"`
	AttemptID     int64  `json:"attemptId"`
	DailyReportID int64  `json:"dailyReportId"`
	ConfigKey     string `json:"configKey"`
	LocalDate     string `json:"localDate"`
	ReportVersion int64  `json:"reportVersion"`
	ModelCallID   int64  `json:"modelCallId"`
	Outcome       string `json:"outcome"`
	Content       string `json:"content"`
	ResultDigest  string `json:"resultDigest"`
	PromptDigest  string `json:"promptDigest"`
}

// CommitDailyAnalysisProposal re-adjudicates the model's typed ResultProposal
// against the frozen sealed facts and commits the single ledger row; the
// frozen SQL closure appends the immutable analysis version and the Attempt's
// Succeeded terminal state in the same statement. An identical redelivery
// replays silently — the first commit's audit stands.
func (s *Service) CommitDailyAnalysisProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal dailyAnalysisProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("daily analysis result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != dailyAnalysisResultKind || proposal.AttemptID != attemptID || proposal.DailyReportID < 1 ||
		proposal.ConfigKey == "" || proposal.LocalDate == "" || proposal.ReportVersion < 1 ||
		proposal.ModelCallID < 1 || proposal.Outcome != "success" || proposal.Content == "" {
		return fmt.Errorf("daily analysis result has an invalid identity envelope")
	}
	// 传输完整性：摘要按提案自声明字段重算，只证明 wire 载荷未被损坏。
	canonical := fmt.Sprintf("%s|%d|%d|%d|success|%s|%s",
		dailyAnalysisResultKind, attemptID, proposal.DailyReportID, proposal.ModelCallID,
		proposal.Content, proposal.PromptDigest)
	sum := sha256.Sum256([]byte(canonical))
	if proposal.ResultDigest != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("daily analysis result digest does not match its canonical payload")
	}
	commandCtx, err := s.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(commandCtx, s.runner, s.dailyAnalysisResult, func(tx *execution.Tx) (struct{}, error) {
		var reportID int64
		var state string
		if err := tx.QueryRowContext(commandCtx, `
			SELECT scope_id, state FROM execution_attempts
			WHERE id=? AND attempt_type='inspection_daily_analysis' AND scope_type='daily_report'`, attemptID).
			Scan(&reportID, &state); err != nil {
			return struct{}{}, err
		}
		if reportID != proposal.DailyReportID {
			return struct{}{}, fmt.Errorf("daily analysis result identity does not match attempt")
		}
		// 提案携带的报告身份必须与冻结报告逐字一致：定位符是创建时冻结的。
		var configKey, localDate string
		if err := tx.QueryRowContext(commandCtx, `
			SELECT config_key,local_date FROM inspection_daily_reports WHERE id=?`, reportID).
			Scan(&configKey, &localDate); err != nil {
			return struct{}{}, err
		}
		if configKey != proposal.ConfigKey || localDate != proposal.LocalDate {
			return struct{}{}, fmt.Errorf("daily analysis result locator does not match the frozen report identity")
		}
		// Idempotent replay: an already-committed ledger row accepts only the
		// identical payload (its digest was derived from this exact body).
		var existingDigest []byte
		replayErr := tx.QueryRowContext(commandCtx, `SELECT result_digest FROM inspection_daily_analysis_ledgers WHERE attempt_id=?`, attemptID).Scan(&existingDigest)
		if replayErr == nil {
			if hex.EncodeToString(existingDigest) == proposal.ResultDigest {
				return struct{}{}, errResultReplayed
			}
			return struct{}{}, fmt.Errorf("daily analysis replay digest conflicts")
		}
		if replayErr != sql.ErrNoRows {
			return struct{}{}, replayErr
		}
		// The model call must be the attempt's own succeeded call with the
		// exact prompt provenance.
		var callState string
		var callPrompt string
		if err := tx.QueryRowContext(commandCtx, `
			SELECT status, prompt_digest FROM model_calls WHERE id=? AND attempt_id=?`, proposal.ModelCallID, attemptID).
			Scan(&callState, &callPrompt); err != nil {
			return struct{}{}, fmt.Errorf("daily analysis model call does not belong to the attempt: %w", err)
		}
		if callState != "succeeded" || callPrompt != proposal.PromptDigest {
			return struct{}{}, fmt.Errorf("daily analysis model call is not the succeeded prompt provenance")
		}
		// Boot/epoch fence: the ledger closure performs the terminal transition.
		var bound int
		if err := tx.QueryRowContext(commandCtx, `
			SELECT 1 FROM execution_attempts
			WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return struct{}{}, attempt.ErrLateResult
			}
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(commandCtx, `
			INSERT INTO inspection_daily_analysis_ledgers(
				attempt_id, daily_report_id, report_version, model_call_id, result_digest,
				content, prompt_digest, created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			attemptID, reportID, proposal.ReportVersion, proposal.ModelCallID, sum[:],
			proposal.Content, proposal.PromptDigest, s.nowText()); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, func(struct{}) int64 { return proposal.DailyReportID })
	if err != nil {
		if errors.Is(err, errResultReplayed) {
			return nil
		}
		return err
	}
	return nil
}

// DailyReportAnalysisSummary is one immutable analysis version entry.
type DailyReportAnalysisSummary struct {
	AnalysisVersion int64  `json:"analysisVersion"`
	ID              string `json:"id"`
	AttemptState    string `json:"attemptState"`
	ReportVersion   int64  `json:"reportVersion"`
	ModelID         string `json:"modelId"`
	CreatedAt       string `json:"createdAt"`
}

// DailyReportAnalysisDetail is one analysis version's read model.
type DailyReportAnalysisDetail struct {
	DailyReportAnalysisSummary
	Content string `json:"content"`
}

// ListDailyReportAnalyses returns a report's analysis versions newest first
// through the read-only reader.
func (s *Service) ListDailyReportAnalyses(ctx context.Context, configKey, localDate string) ([]DailyReportAnalysisSummary, error) {
	reader, err := s.readReader()
	if err != nil {
		return nil, err
	}
	rows, err := reader.QueryContext(ctx, `
		SELECT x.analysis_version,x.attempt_id,a.state,x.report_version,x.model_id,x.created_at
		FROM inspection_daily_report_analyses x
		JOIN inspection_daily_reports r ON r.id=x.report_id
		JOIN execution_attempts a ON a.id=x.attempt_id
		WHERE r.config_key=? AND r.local_date=? ORDER BY x.analysis_version DESC`, configKey, localDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []DailyReportAnalysisSummary{}
	for rows.Next() {
		var item DailyReportAnalysisSummary
		var attemptID int64
		if err := rows.Scan(&item.AnalysisVersion, &attemptID, &item.AttemptState, &item.ReportVersion, &item.ModelID, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.ID = locatorID(attemptID)
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetDailyReportAnalysis returns one immutable analysis version.
func (s *Service) GetDailyReportAnalysis(ctx context.Context, configKey, localDate string, analysisVersion int64) (DailyReportAnalysisDetail, error) {
	var detail DailyReportAnalysisDetail
	reader, err := s.readReader()
	if err != nil {
		return DailyReportAnalysisDetail{}, err
	}
	var attemptID int64
	err = reader.QueryRowContext(ctx, `
		SELECT x.attempt_id,a.state,x.report_version,x.model_id,x.content,x.created_at
		FROM inspection_daily_report_analyses x
		JOIN inspection_daily_reports r ON r.id=x.report_id
		JOIN execution_attempts a ON a.id=x.attempt_id
		WHERE r.config_key=? AND r.local_date=? AND x.analysis_version=?`, configKey, localDate, analysisVersion).
		Scan(&attemptID, &detail.AttemptState, &detail.ReportVersion, &detail.ModelID, &detail.Content, &detail.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DailyReportAnalysisDetail{}, ErrNotFound
	}
	if err != nil {
		return DailyReportAnalysisDetail{}, err
	}
	detail.ID = locatorID(attemptID)
	return detail, nil
}
