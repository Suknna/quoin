// Immutable Inspection Report closure (CFG-INSPECTRUN-002, RUNTIME-TASK-013):
// once a Run's collection has closed, Quoin creates exactly one
// inspection_analysis attempt whose frozen input carries the structured
// preallocated Report version, the Run locator, every check result and every
// complete Evidence. The model's ResultProposal is re-adjudicated against the
// frozen facts and canonical digests, and the single ledger INSERT lets the
// frozen SQL closure create the immutable Report atomically.
package inspection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const reportInputKind = "inspection_analysis_v1"
const reportResultKind = "inspection_report_result_v1"

// reportRendererVersion 是巡检分析输入快照的 renderer 代：v2（知识接入代）
// 起在 canonical 输入内嵌冻结工具目录；旧 Attempt 的快照仍记录 v1，重建只读
// 存储文档，不回填目录，字节保持不变。
const reportRendererVersion = "v2"

type reportModelContract struct {
	ModelID             string `json:"modelId"`
	ContextBudgetTokens int64  `json:"contextBudgetTokens"`
	MaxOutputTokens     int64  `json:"maxOutputTokens"`
}

// reportCheckItem 是分析输入携带的单检查项结构化清单：检查身份、冻结语义
// （名称/说明/单位）、冻结查询形状（表达式与范围）、真实执行窗口与结果对应
// 关系。gap 是显式事实：没有数据绝不当 0，没有阈值绝不判断健康。
type reportCheckItem struct {
	CheckKey    string `json:"checkKey"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
	EvidenceID  *int64 `json:"evidenceId,omitempty"`
	ArtifactID  *int64 `json:"artifactId,omitempty"`
	// 冻结的查询形状（来自 Run 冻结参数）。
	Expression   string `json:"expression,omitempty"`
	RangeSeconds *int64 `json:"rangeSeconds,omitempty"`
	StepSeconds  *int64 `json:"stepSeconds,omitempty"`
	// 真实执行事实：observedAt 与（范围查询的）实际窗口/步长；缺口检查没有
	// 执行事实，只携带 gapReason。
	ObservedAt          string   `json:"observedAt,omitempty"`
	WindowStartAt       string   `json:"windowStartAt,omitempty"`
	WindowEndAt         string   `json:"windowEndAt,omitempty"`
	ExecutedStepSeconds *int64   `json:"executedStepSeconds,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
	GapReason           *string  `json:"gapReason,omitempty"`
}

// checkItemQuerier 抽象冻结路径（执行器事务）与重建路径（只读池）共用的查询面。
type checkItemQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// reportCheckItemsOn 推导一个 Run 的逐检查项结构化清单。冻结与重建使用同一
// SQL 与同一解析路径，输入只来自不可变行（run_checks / check_results 的冻结
// 元数据 / evidence / artifacts 的 evidence 归属关联），因此两次推导字节一致。
// 元数据来源：新检查结果行自带 meta_json（observedAt/warnings/窗口，gap 亦有）；
// 历史行（meta_json NULL）显式回退 Evidence 形状。malformed 的冻结 JSON 是
// 数据完整性故障，带 run/check 身份返回错误，绝不静默吞掉。
func reportCheckItemsOn(ctx context.Context, q checkItemQuerier, runID int64) ([]reportCheckItem, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT c.check_key, c.display_name, c.params_json,
		       x.status, x.gap_reason, x.evidence_id, x.meta_json,
		       e.observed_at, e.params_json, e.warnings_json,
		       a.id
		FROM inspection_run_checks c
		LEFT JOIN inspection_check_results x ON x.run_id=c.run_id AND x.check_key=c.check_key
		LEFT JOIN evidence e ON e.id=x.evidence_id
		LEFT JOIN artifacts a ON a.owner_type='evidence' AND a.owner_id=x.evidence_id AND a.kind='report_file'
		WHERE c.run_id=? ORDER BY c.check_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []reportCheckItem{}
	for rows.Next() {
		var item reportCheckItem
		var checkParams, resultMeta, observedAt, evidenceParams, warningsJSON sql.NullString
		var gapReason sql.NullString
		var evidenceID, artifactID sql.NullInt64
		if err := rows.Scan(&item.CheckKey, &item.DisplayName, &checkParams,
			&item.Status, &gapReason, &evidenceID, &resultMeta,
			&observedAt, &evidenceParams, &warningsJSON, &artifactID); err != nil {
			return nil, err
		}
		if gapReason.Valid {
			item.GapReason = &gapReason.String
		}
		if checkParams.Valid {
			var params struct {
				Expression   string `json:"expression"`
				RangeSeconds *int64 `json:"rangeSeconds"`
				StepSeconds  *int64 `json:"stepSeconds"`
			}
			if err := json.Unmarshal([]byte(checkParams.String), &params); err != nil {
				return nil, fmt.Errorf("inspection checklist run=%d check=%s params_json malformed: %w", runID, item.CheckKey, err)
			}
			item.Expression = params.Expression
			item.RangeSeconds = params.RangeSeconds
			item.StepSeconds = params.StepSeconds
		}
		if evidenceID.Valid {
			item.EvidenceID = &evidenceID.Int64
		}
		if artifactID.Valid {
			item.ArtifactID = &artifactID.Int64
		}
		// 执行元数据：新检查结果行优先（gap 亦有观察时间/warnings/窗口事实）；
		// 历史行显式回退 Evidence 形状。两个来源都是提交时冻结的不可变事实。
		if resultMeta.Valid {
			var meta struct {
				ObservedAt      string   `json:"observedAt"`
				Warnings        []string `json:"warnings"`
				ExecutionWindow *struct {
					StartAt     string `json:"startAt"`
					EndAt       string `json:"endAt"`
					StepSeconds int64  `json:"stepSeconds"`
				} `json:"executionWindow"`
			}
			if err := json.Unmarshal([]byte(resultMeta.String), &meta); err != nil {
				return nil, fmt.Errorf("inspection checklist run=%d check=%s result meta_json malformed: %w", runID, item.CheckKey, err)
			}
			item.ObservedAt = meta.ObservedAt
			item.Warnings = meta.Warnings
			if meta.ExecutionWindow != nil {
				item.WindowStartAt = meta.ExecutionWindow.StartAt
				item.WindowEndAt = meta.ExecutionWindow.EndAt
				if meta.ExecutionWindow.StepSeconds > 0 {
					step := meta.ExecutionWindow.StepSeconds
					item.ExecutedStepSeconds = &step
				}
			}
		} else {
			if observedAt.Valid {
				item.ObservedAt = observedAt.String
			}
			if evidenceParams.Valid {
				// 真实执行窗口由采集闭包冻结进 evidence params；缺省（旧证据或
				// 即时查询）保持缺失，绝不从样本时间戳推断。
				var params struct {
					ExecutionWindow *struct {
						StartAt     string `json:"startAt"`
						EndAt       string `json:"endAt"`
						StepSeconds int64  `json:"stepSeconds"`
					} `json:"executionWindow"`
				}
				if err := json.Unmarshal([]byte(evidenceParams.String), &params); err != nil {
					return nil, fmt.Errorf("inspection checklist run=%d check=%s evidence params_json malformed: %w", runID, item.CheckKey, err)
				}
				if params.ExecutionWindow != nil {
					item.WindowStartAt = params.ExecutionWindow.StartAt
					item.WindowEndAt = params.ExecutionWindow.EndAt
					if params.ExecutionWindow.StepSeconds > 0 {
						step := params.ExecutionWindow.StepSeconds
						item.ExecutedStepSeconds = &step
					}
				}
			}
			if warningsJSON.Valid {
				if err := json.Unmarshal([]byte(warningsJSON.String), &item.Warnings); err != nil {
					return nil, fmt.Errorf("inspection checklist run=%d check=%s evidence warnings_json malformed: %w", runID, item.CheckKey, err)
				}
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type reportInput struct {
	SchemaKind         string              `json:"schemaKind"`
	AttemptID          int64               `json:"attemptId"`
	InspectionRunID    int64               `json:"inspectionRunId"`
	ReportVersion      int64               `json:"reportVersion"`
	PlanKey            string              `json:"planKey"`
	EvidenceIDs        []int64             `json:"evidenceIds"`
	ArtifactIDs        []int64             `json:"artifactIds"`
	KnowledgeVersionID []int64             `json:"knowledgeVersionIds"`
	ModelContract      reportModelContract `json:"modelContract"`
	// ToolCatalog 是随本次 Attempt 冻结的工具目录（inspection-analysis-v4 起）：
	// 与 attempt_input_snapshots.tool_catalog_json 同一文档内嵌进 canonical
	// 输入（digest 覆盖）；旧 Attempt（无目录）重建时保持缺失以维持字节不变。
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
	// Run 冻结的计划绑定上下文（ADR-0004 独立计划 Run）。
	Plan            *planReportContext `json:"plan,omitempty"`
	ConnectionName  string             `json:"connectionName,omitempty"`
	TemplateID      string             `json:"templateId,omitempty"`
	TemplateVersion string             `json:"templateVersion,omitempty"`
	// Run 的逐检查项结构化清单（冻结语义 + 真实执行事实 + 缺口）；旧形状
	// Attempt（无 requirements 行）保持缺失以维持重建字节。
	Checks []reportCheckItem `json:"checks,omitempty"`
	// 仅本次重分析的报告要求覆盖；缺省表示沿用 Run 冻结的初始报告要求。覆盖
	// 只改本次报告的指令文本，绝不改写旧证据、冻结语义或检查项含义。
	ReportInstructionsOverride *string `json:"reportInstructionsOverride,omitempty"`
}

// planReportContext 是模型可见的计划事实摘要（非秘密、非正文）。检查说明、
// 单位与初始报告要求来自 Run 冻结列，不读计划当前定义。
type planReportContext struct {
	Key    string         `json:"key"`
	Params map[string]any `json:"params"`
	Scope  map[string]any `json:"scope"`
	// Run 冻结的分析语义（可选；旧 Run 为 NULL 时保持缺失以维持重建字节）。
	CheckDescription   *string `json:"checkDescription,omitempty"`
	MetricUnit         *string `json:"metricUnit,omitempty"`
	ReportInstructions *string `json:"reportInstructions,omitempty"`
}

// modelProviderSelection mirrors the single enabled model provider resolution
// owned by internal/quoin/analysis (DATA-CONN-003): one enabled provider with
// a current explicit qualification.
type modelProviderSelection struct {
	ConnectionID  int64
	RevisionID    int64
	CredentialGen int64
	ProbeResultID int64
	ChatModelID   string
	ContextBudget int64
	MaxOutput     int64
}

var ErrModelProviderMissing = errors.New("no enabled qualified model provider")

func selectReportModelProvider(ctx context.Context, tx execution.Executor) (modelProviderSelection, error) {
	var selected modelProviderSelection
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
		return selected, ErrModelProviderMissing
	}
	if err != nil {
		return selected, err
	}
	if qualificationRowVersion != connectionRowVersion || probeOutcome != "passed" {
		return selected, ErrModelProviderMissing
	}
	var streaming, nativeToolCalling bool
	if err = tx.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens, streaming_supported, native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput, &streaming, &nativeToolCalling); err != nil {
		return selected, err
	}
	if selected.ChatModelID == "" || !nativeToolCalling {
		return selected, ErrModelProviderMissing
	}
	return selected, nil
}

// startReportAnalysisOn creates the first analysis attempt after collection
// closes. A missing model provider is recoverable here: the reconciler retries
// later instead of creating a placeholder report. 自动分析只使用 Run 冻结值。
func (s *Service) startReportAnalysisOn(ctx context.Context, tx execution.Executor, runID int64, now string) error {
	_, err := s.createReportAnalysisOn(ctx, tx, runID, now, false, nil)
	if errors.Is(err, ErrModelProviderMissing) {
		return nil
	}
	return err
}

// createReportAnalysisOn freezes one report attempt. allowPrior is used only
// by the explicit re-analysis command: it permits prior terminal attempts but
// never an additional concurrent attempt, so every report version has exactly
// one live producer. reportInstructionsOverride 仅由显式重分析提供（nil = 沿用
// Run 冻结的初始报告要求）；实际生效的全部分析要求冻结进本次 Attempt 的输入
// 快照与 inspection_analysis_requirements 行，重建摘要因此稳定。
func (s *Service) createReportAnalysisOn(ctx context.Context, tx execution.Executor, runID int64, now string, allowPrior bool, reportInstructionsOverride *string) (int64, error) {
	var runState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM inspection_runs WHERE id=?`, runID).Scan(&runState); err != nil {
		return 0, err
	}
	if runState != "Completed" && runState != "CompletedWithGaps" {
		return 0, nil
	}
	if !allowPrior {
		var existing int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, runID).
			Scan(&existing); err != nil {
			return 0, err
		}
		if existing != 0 {
			return 0, nil
		}
	}
	provider, err := selectReportModelProvider(ctx, tx)
	if err != nil {
		return 0, err
	}
	var connectionID sql.NullInt64
	var planKey string
	if err = tx.QueryRowContext(ctx, `
		SELECT plan_key, connection_id FROM inspection_runs WHERE id=?`, runID).
		Scan(&planKey, &connectionID); err != nil {
		return 0, err
	}
	// 计划 Run：模型上下文携带 Run 冻结的计划绑定（连接与模板）与冻结的分析
	// 语义（检查说明/单位/初始报告要求）；在任何快照写入前读取完毕，快照行保
	// 持 append-only。
	var planContext *planReportContext
	var planConnectionName, planTemplateID, planTemplateVersion string
	if connectionID.Valid {
		if err = tx.QueryRowContext(ctx, `SELECT name FROM connections WHERE id=?`, connectionID.Int64).Scan(&planConnectionName); err != nil {
			return 0, err
		}
	}
	var paramsRaw, scopeRaw string
	var checkDescription, metricUnit, reportInstructions sql.NullString
	if err = tx.QueryRowContext(ctx, `
		SELECT frozen_params_json, frozen_scope_json, template_id, template_version,
		       frozen_check_description, frozen_metric_unit, frozen_report_instructions
		FROM inspection_runs WHERE id=?`, runID).
		Scan(&paramsRaw, &scopeRaw, &planTemplateID, &planTemplateVersion, &checkDescription, &metricUnit, &reportInstructions); err != nil {
		return 0, err
	}
	params := map[string]any{}
	_ = json.Unmarshal([]byte(paramsRaw), &params)
	scope := map[string]any{}
	_ = json.Unmarshal([]byte(scopeRaw), &scope)
	planContext = &planReportContext{Key: planKey, Params: params, Scope: scope}
	if checkDescription.Valid {
		planContext.CheckDescription = &checkDescription.String
	}
	if metricUnit.Valid {
		planContext.MetricUnit = &metricUnit.String
	}
	if reportInstructions.Valid {
		planContext.ReportInstructions = &reportInstructions.String
	}
	var reportVersion int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, runID).Scan(&reportVersion); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT x.id, x.evidence_id, e.result_json, e.artifact_id
		FROM inspection_check_results x LEFT JOIN evidence e ON e.id=x.evidence_id
		WHERE x.run_id=? ORDER BY x.check_key`, runID)
	if err != nil {
		return 0, err
	}
	type settledCheck struct {
		resultID   int64
		evidence   sql.NullInt64
		resultJSON sql.NullString
		artifactID sql.NullInt64
	}
	checks := []settledCheck{}
	for rows.Next() {
		var check settledCheck
		if err = rows.Scan(&check.resultID, &check.evidence, &check.resultJSON, &check.artifactID); err != nil {
			rows.Close()
			return 0, err
		}
		checks = append(checks, check)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	evidenceIDs := []int64{}
	artifactIDs := []int64{}
	for _, check := range checks {
		if !check.evidence.Valid {
			continue
		}
		evidenceIDs = append(evidenceIDs, check.evidence.Int64)
		if check.artifactID.Valid {
			artifactIDs = append(artifactIDs, check.artifactID.Int64)
			continue
		}
		if !check.resultJSON.Valid {
			return 0, fmt.Errorf("inspection evidence %d has no readable result", check.evidence.Int64)
		}
		// In production app wiring injects the artifact store. Isolated domain
		// tests deliberately exercise the report ledger without a filesystem.
		if s.artifactWriter == nil {
			continue
		}
		artifactID, materializeErr := s.artifactWriter(ctx, tx, check.evidence.Int64, []byte(check.resultJSON.String))
		if materializeErr != nil {
			return 0, materializeErr
		}
		artifactIDs = append(artifactIDs, artifactID)
	}
	// CreateOn centrally persists the operation correlation onto the new
	// analysis attempt in this same transaction (ADR-0006); a context without
	// execution metadata fails closed.
	analysisID, err := attempt.CreateOn(ctx, tx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('inspection_analysis','run',?,'Queued',?,?,?)`, runID, attempt.ReleaseVersion(), attempt.InspectionAgentVersion, now)
	if err != nil {
		return 0, err
	}
	// 实际生效的分析要求冻结（本次 Attempt 专属）：覆盖仅影响本次报告指令。
	var overrideValue any
	if reportInstructionsOverride != nil {
		overrideValue = *reportInstructionsOverride
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO inspection_analysis_requirements(attempt_id,report_instructions_override,created_at) VALUES(?,?,?)`,
		analysisID, overrideValue, now); err != nil {
		return 0, err
	}
	// 计划 Run 冻结的逐检查项结构化清单：与重建路径同一推导（只读不可变行）。
	checkItems, err := reportCheckItemsOn(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	// 知识接入代起冻结本次 Attempt 的工具目录：与派发字节同文档内嵌进
	// canonical 输入（digest 覆盖），同一文档写入 tool_catalog_json。
	catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(s.Attempts().Catalogs, attempt.InspectionAgentVersion)
	if err != nil {
		return 0, err
	}
	input := reportInput{
		SchemaKind: reportInputKind, AttemptID: analysisID, InspectionRunID: runID,
		ReportVersion: int64(reportVersion + 1), PlanKey: planKey,
		EvidenceIDs: evidenceIDs, ArtifactIDs: artifactIDs, KnowledgeVersionID: []int64{},
		ModelContract: reportModelContract{ModelID: provider.ChatModelID, ContextBudgetTokens: provider.ContextBudget, MaxOutputTokens: provider.MaxOutput},
		Plan:          planContext, ConnectionName: planConnectionName, TemplateID: planTemplateID, TemplateVersion: planTemplateVersion,
		Checks: checkItems, ReportInstructionsOverride: reportInstructionsOverride, ToolCatalog: catalog,
	}
	body, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(body)
	snapshot, err := tx.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,inspection_report_version,created_at)
		VALUES(?,?,?,?,?,?,?)`, analysisID, reportInputKind, reportRendererVersion, hex.EncodeToString(digest[:]), string(catalogDocument), input.ReportVersion, now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return 0, err
	}
	runDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-run:%d", runID)))
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_run_id)
		VALUES(?,1,'inspection_run',?,?)`, snapshotID, hex.EncodeToString(runDigest[:]), runID); err != nil {
		return 0, err
	}
	itemSeq := 1
	for _, check := range checks {
		itemSeq++
		checkDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-check-result:%d", check.resultID)))
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_check_result_id)
			VALUES(?,?,'inspection_check_result',?,?)`, snapshotID, itemSeq, hex.EncodeToString(checkDigest[:]), check.resultID); err != nil {
			return 0, err
		}
	}
	for _, evidenceID := range evidenceIDs {
		itemSeq++
		evidenceDigest := sha256.Sum256([]byte(fmt.Sprintf("evidence:%d", evidenceID)))
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,evidence_id)
			VALUES(?,?,'inspection_evidence',?,?)`, snapshotID, itemSeq, hex.EncodeToString(evidenceDigest[:]), evidenceID); err != nil {
			return 0, err
		}
	}
	for _, artifactID := range artifactIDs {
		itemSeq++
		artifactDigest := sha256.Sum256([]byte(fmt.Sprintf("artifact:%d", artifactID)))
		if _, err = tx.ExecContext(ctx, `INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,artifact_id) VALUES(?,?,'inspection_artifact',?,?)`, snapshotID, itemSeq, hex.EncodeToString(artifactDigest[:]), artifactID); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at) VALUES(?,?,'input_snapshot',?,?)`, analysisID, artifactID, snapshotID, now); err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?, 'chat_model', ?,?,?,?,?)`,
		analysisID, provider.ConnectionID, provider.RevisionID, provider.CredentialGen, provider.ProbeResultID, now)
	if err != nil {
		return 0, err
	}
	return analysisID, nil
}

type reportProposal struct {
	SchemaKind          string  `json:"schemaKind"`
	AttemptID           int64   `json:"attemptId"`
	InspectionRunID     int64   `json:"inspectionRunId"`
	ModelCallID         int64   `json:"modelCallId"`
	Outcome             string  `json:"outcome"`
	Content             string  `json:"content"`
	EvidenceIDs         []int64 `json:"evidenceIds"`
	ArtifactIDs         []int64 `json:"artifactIds"`
	KnowledgeVersionIDs []int64 `json:"knowledgeVersionIds"`
	ResultDigest        string  `json:"resultDigest"`
	EvidenceDigest      string  `json:"evidenceDigest"`
	PromptDigest        string  `json:"promptDigest"`
}

// CommitReportProposal re-adjudicates the model's typed ResultProposal against
// the frozen input and commits the single ledger row; the frozen SQL closure
// creates the immutable Report, its ordered references, and the Attempt's
// Succeeded terminal state in the same statement. The commit runs on the
// shared execution runner (ADR-0006): the system task scope is restored from
// the attempt's persisted correlation, the boot/epoch fence is preserved, and
// the automatic audit row commits atomically with the ledger INSERT. An
// identical redelivery replays silently — the first commit's audit stands.
//
// 知识引用权威化：ledger 的 knowledge_version_ids 不采纳提案声明的列表，而是
// 在事务内从本 Attempt 的实际消费记录推导——knowledge_get 执行成功，且其封存
// 结果进入了报告所依据的那次模型调用（proposal.ModelCallID）的输入谱系
// （model_call_input_items）；result_digest 同步按权威列表重算，存储值与冻结
// SQL 闭包的校验一致。提案携带的列表只参与传输完整性摘要校验（防 wire 损
// 坏），不进入 ledger。
func (s *Service) CommitReportProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal reportProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("inspection report result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != reportResultKind || proposal.AttemptID != attemptID || proposal.InspectionRunID < 1 ||
		proposal.ModelCallID < 1 || proposal.Outcome != "success" || proposal.Content == "" {
		return fmt.Errorf("inspection report result has an invalid identity envelope")
	}
	evidenceJSON, err := canonicalIDArray(proposal.EvidenceIDs)
	if err != nil {
		return err
	}
	artifactJSON, err := canonicalIDArray(proposal.ArtifactIDs)
	if err != nil {
		return err
	}
	declaredKnowledgeJSON, err := canonicalIDArray(proposal.KnowledgeVersionIDs)
	if err != nil {
		return err
	}
	evidenceSum := sha256.Sum256([]byte(evidenceJSON))
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	if proposal.EvidenceDigest != evidenceDigest {
		return fmt.Errorf("inspection report evidence digest does not match its locators")
	}
	// 传输完整性：摘要按提案自声明字段重算，只证明 wire 载荷未被损坏；知识
	// 引用的权威裁决在事务内按消费记录另行进行。
	declaredCanonical := fmt.Sprintf("%s|%d|%d|%d|%s|%s|%s|%s|%s|%s|%s",
		reportResultKind, attemptID, proposal.InspectionRunID, proposal.ModelCallID, "success",
		proposal.Content, evidenceJSON, artifactJSON, declaredKnowledgeJSON, evidenceDigest, proposal.PromptDigest)
	declaredSum := sha256.Sum256([]byte(declaredCanonical))
	if proposal.ResultDigest != hex.EncodeToString(declaredSum[:]) {
		return fmt.Errorf("inspection report result digest does not match its canonical payload")
	}

	commandCtx, err := s.resultContext(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(commandCtx, s.runner, s.reportResult, func(tx *execution.Tx) (struct{}, error) {
		var runID int64
		var state string
		if err := tx.QueryRowContext(commandCtx, `
			SELECT scope_id, state FROM execution_attempts
			WHERE id=? AND attempt_type='inspection_analysis' AND scope_type='run'`, attemptID).
			Scan(&runID, &state); err != nil {
			return struct{}{}, err
		}
		if runID != proposal.InspectionRunID {
			return struct{}{}, fmt.Errorf("inspection report result identity does not match attempt")
		}
		// 权威知识引用：从本 Attempt 成功且被报告模型调用实际消费的
		// knowledge_get 调用按首次读取顺序去重推导，不信任提案声明。
		consumed, err := consumedKnowledgeVersionsOn(commandCtx, tx, attemptID, proposal.ModelCallID)
		if err != nil {
			return struct{}{}, err
		}
		knowledgeJSON, err := canonicalIDArray(consumed)
		if err != nil {
			return struct{}{}, err
		}
		authoritativeCanonical := fmt.Sprintf("%s|%d|%d|%d|%s|%s|%s|%s|%s|%s|%s",
			reportResultKind, attemptID, proposal.InspectionRunID, proposal.ModelCallID, "success",
			proposal.Content, evidenceJSON, artifactJSON, knowledgeJSON, evidenceDigest, proposal.PromptDigest)
		authoritativeSum := sha256.Sum256([]byte(authoritativeCanonical))
		authoritativeDigest := hex.EncodeToString(authoritativeSum[:])
		// Idempotent replay: an already-committed ledger row accepts only the
		// identical payload (its digest was derived from this exact body).
		var existingDigest []byte
		replayErr := tx.QueryRowContext(commandCtx, `SELECT result_digest FROM inspection_report_result_ledgers WHERE attempt_id=?`, attemptID).Scan(&existingDigest)
		if replayErr == nil {
			if hex.EncodeToString(existingDigest) == authoritativeDigest {
				return struct{}{}, errResultReplayed
			}
			return struct{}{}, fmt.Errorf("inspection report replay digest conflicts")
		}
		if replayErr != sql.ErrNoRows {
			return struct{}{}, replayErr
		}
		// The model call must be the attempt's own succeeded call with the exact
		// prompt provenance.
		var callState string
		var callPrompt string
		err = tx.QueryRowContext(commandCtx, `
			SELECT status, prompt_digest FROM model_calls WHERE id=? AND attempt_id=?`, proposal.ModelCallID, attemptID).
			Scan(&callState, &callPrompt)
		if err != nil {
			return struct{}{}, fmt.Errorf("inspection report model call does not belong to the attempt: %w", err)
		}
		if callState != "succeeded" || callPrompt != proposal.PromptDigest {
			return struct{}{}, fmt.Errorf("inspection report model call is not the succeeded prompt provenance")
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
		var reportVersion int64
		if err := tx.QueryRowContext(commandCtx, `
			SELECT inspection_report_version FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&reportVersion); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.ExecContext(commandCtx, `
			INSERT INTO inspection_report_result_ledgers(
				attempt_id, inspection_run_id, report_version, model_call_id, result_digest, evidence_digest,
				content, prompt_digest, evidence_ids_json, artifact_ids_json, knowledge_version_ids_json, created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			attemptID, runID, reportVersion, proposal.ModelCallID, authoritativeSum[:], evidenceDigest,
			proposal.Content, proposal.PromptDigest, evidenceJSON, artifactJSON, knowledgeJSON, s.nowText()); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	}, func(struct{}) int64 { return proposal.InspectionRunID })
	if err != nil {
		if errors.Is(err, errResultReplayed) {
			return nil
		}
		return err
	}
	return nil
}

// consumedKnowledgeVersionsOn derives the authoritative knowledge-version
// citation list of one inspection analysis attempt from its ACTUAL
// consumption records: a knowledge_get tool call counts only when BOTH its
// execution succeeded AND its sealed result entered the report-producing
// model call's input lineage (model_call_input_items, item_role='tool',
// model_call_id = the report's own model call). 执行成功但结果从未被后续
// 模型调用消费（如被上下文淘汰）的读取不构成引用依据——报告正文由
// proposal 的那次模型调用产生，只有它实际看到的工具结果才是报告的知识
// 依据。Versions are cited in first-read order (tool_calls.id ASC),
// deduplicated. A version the model never actually read can never be cited; a
// hand-forged proposal list never reaches the ledger. Malformed arguments are
// impossible through the validated ingress but are skipped defensively rather
// than aborting the commit.
func consumedKnowledgeVersionsOn(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, attemptID, reportModelCallID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT t.arguments_json FROM tool_calls t
		WHERE t.attempt_id=? AND t.tool_name='knowledge_get' AND t.status='succeeded'
		  AND EXISTS (SELECT 1 FROM model_call_input_items i
		              WHERE i.tool_call_id=t.id AND i.item_role='tool' AND i.model_call_id=?)
		ORDER BY t.id`, attemptID, reportModelCallID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[int64]bool{}
	consumed := []int64{}
	for rows.Next() {
		var arguments string
		if err := rows.Scan(&arguments); err != nil {
			return nil, err
		}
		var parsed struct {
			VersionID float64 `json:"versionId"`
		}
		if err := json.Unmarshal([]byte(arguments), &parsed); err != nil || parsed.VersionID < 1 || parsed.VersionID != math.Trunc(parsed.VersionID) {
			continue
		}
		id := int64(parsed.VersionID)
		if seen[id] {
			continue
		}
		seen[id] = true
		consumed = append(consumed, id)
	}
	return consumed, rows.Err()
}

// canonicalIDArray renders the locator array exactly as the frozen SQL json()
// projection: minified integers, no spaces.
func canonicalIDArray(ids []int64) (string, error) {
	if ids == nil {
		ids = []int64{}
	}
	for _, id := range ids {
		if id < 1 {
			return "", fmt.Errorf("inspection report locator ids must be positive")
		}
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
