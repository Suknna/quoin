package inspection

// 独立计划的巡检 Run（ADR-0004）：Run 创建时确定性展开并冻结目标、模板、
// 查询窗口、接入修订与授权；执行中不扩大目标，重新采证必须创建新 Run。
// 采证子 Attempt 与既有 run_check 派发/收口机制完全复用。人工与定时触发
// 共享同一事务核心；重叠定时周期 SkippedOverlap 且不补跑。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

const pluginExecutionSchemaKind = "inspection_plugin_execution_v1"

type planObjectRef struct {
	ObjectType  string `json:"objectType,omitempty"`
	IdentityKey string `json:"identityKey,omitempty"`
	// LabelConditions：business_view 范围冻结的视图 exact-match 标签条件。
	LabelConditions map[string]string `json:"labelConditions,omitempty"`

}

// pluginCollectionInput 是 supervisor 收到的冻结派发输入正文。
type pluginCollectionInput struct {
	SchemaKind      string         `json:"schemaKind"`
	AttemptID       int64          `json:"attemptId"`
	InspectionRunID int64          `json:"inspectionRunId"`
	CheckKey        string         `json:"checkKey"`
	PluginID        string         `json:"pluginId"`
	TemplateID      string         `json:"templateId"`
	TemplateVersion string         `json:"templateVersion"`
	Params          map[string]any `json:"params"`
	EvidenceAt      string         `json:"evidenceAt"`
	GrantID         int64          `json:"grantId"`
	// ScopeKind 是 wire 作用域类型（integration|businessView|objects）：
	// collector 据此区分「显式全接入（labels 为空合法）」与「业务视图缺条件」
	// 等历史/异常输入，绝不把缺字段的 business_view 静默当 integration。
	ScopeKind string         `json:"scopeKind,omitempty"`
	Target    *planObjectRef `json:"target,omitempty"`
}

// CreatePlanRun 启动独立计划的一次人工 Run。每个展开检查在同一事务成为
// run_check 子 Attempt：插件采集子冻结 inspection_plugin_execution_v1 与其
// config_thanos_query grant。
func (s *Service) CreatePlanRun(ctx context.Context, principalID int64, clientCommandID, planKey string) (RunDetail, error) {
	const command = "inspection_run.create"
	digest := auth.DigestCommand(command, map[string]any{"planKey": planKey})
	if d, replayed, err := s.replay(ctx, principalID, clientCommandID, digest); replayed || err != nil {
		return d, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if d, replayed, err := s.replayOn(ctx, conn, principalID, clientCommandID, digest); replayed || err != nil {
		if replayed {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return RunDetail{}, err
			}
			committed = true
		}
		return d, err
	}
	detail, rejection, err := s.createPlanRunOn(ctx, conn, planRunRequest{planKey: planKey, triggerKind: "manual", availability: RuntimeAvailability{Plinth: true}, now: s.nowText()})
	if err != nil {
		return RunDetail{}, err
	}
	if rejection != nil {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, rejection, &committed)
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, detail.RunID, s.nowText()); err != nil {
		return RunDetail{}, err
	}
	if err = s.recordCommand(ctx, conn, principalID, clientCommandID, command, digest, detail.RunID, detail); err != nil {
		return RunDetail{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	committed = true
	return detail, nil
}

// ScheduledPlan 是调度器消费的计划投影；不可变 inspection_runs 行是重复权威。
type ScheduledPlan struct {
	PlanID   int64
	PlanKey  string
	Cron     string
	Timezone string
}

// ScheduledPlans 列出启用的定时计划（接入同样启用）。
func (s *Service) ScheduledPlans(ctx context.Context) ([]ScheduledPlan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id,p.plan_key,p.cron,p.timezone
		FROM inspection_plans p JOIN connections c ON c.id=p.connection_id
		WHERE p.enabled=1 AND c.enabled=1 AND p.cron IS NOT NULL
		  AND EXISTS (SELECT 1 FROM inspection_plans p2 WHERE p2.id=p.id)
		ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []ScheduledPlan{}
	for rows.Next() {
		var plan ScheduledPlan
		if err := rows.Scan(&plan.PlanID, &plan.PlanKey, &plan.Cron, &plan.Timezone); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// CreateScheduledPlanRun 提交一个到期的 UTC 计划 Run。计划不可再调度（被停
// 用或接入停用）不是错误：调度器读到过期快照时静默跳过，绝不把旧定义变成
// Run。重叠定时周期 SkippedOverlap 且不补跑。
func (s *Service) CreateScheduledPlanRun(ctx context.Context, plan ScheduledPlan, scheduledFor time.Time, availability RuntimeAvailability) (RunDetail, error) {
	scheduledFor = scheduledFor.UTC()
	if scheduledFor.Nanosecond() != 0 || scheduledFor.Second() != 0 {
		return RunDetail{}, fmt.Errorf("scheduled_for must be a minute boundary")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return RunDetail{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RunDetail{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	scheduledText := scheduledFor.Format(time.RFC3339Nano)
	var existingID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM inspection_runs WHERE plan_id=? AND scheduled_for=?`, plan.PlanID, scheduledText).Scan(&existingID)
	if err == nil {
		detail, detailErr := s.detailOn(ctx, conn, existingID)
		if detailErr != nil {
			return RunDetail{}, detailErr
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RunDetail{}, err
		}
		committed = true
		return detail, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, err
	}
	detail, rejection, err := s.createPlanRunOn(ctx, conn, planRunRequest{planKey: plan.PlanKey, triggerKind: "schedule", scheduledFor: &scheduledText, availability: availability, now: s.nowText()})
	if err != nil {
		return RunDetail{}, err
	}
	if rejection != nil {
		// 计划被停用或存在过期的只读快照：这不是调度错误，静默跳过。
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return RunDetail{}, err
		}
		committed = true
		return RunDetail{}, nil
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return RunDetail{}, err
	}
	committed = true
	return detail, nil
}

// createPlanRunOn 是人工/定时共享的事务核心。rejection 非 nil 表示确定性
// 拒绝（调用方决定是否写入命令台账）。
// planRunRequest 携带一次计划 Run 创建的全部边界事实。
type planRunRequest struct {
	planKey      string
	triggerKind  string
	scheduledFor *string
	rerunOf      int64
	availability RuntimeAvailability
	now          string
}

func (s *Service) createPlanRunOn(ctx context.Context, conn *sql.Conn, request planRunRequest) (RunDetail, *RejectionError, error) {
	planKey, triggerKind, scheduledFor, availability, now := request.planKey, request.triggerKind, request.scheduledFor, request.availability, request.now
	var planID, connectionID int64
	var planEnabled, connectionEnabled int
	var pluginID, templateID, paramsJSON, scopeJSON, scopeKind string
	var templateVersion sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT p.id,p.connection_id,p.enabled,p.plugin_id,p.template_id,p.template_version,p.params_json,p.scope_json,p.scope_kind,c.enabled
		FROM inspection_plans p JOIN connections c ON c.id=p.connection_id
		WHERE p.plan_key=?`, planKey).
		Scan(&planID, &connectionID, &planEnabled, &pluginID, &templateID, &templateVersion, &paramsJSON, &scopeJSON, &scopeKind, &connectionEnabled)
	if err == sql.ErrNoRows {
		return RunDetail{}, &RejectionError{Code: "not_found", Detail: "找不到该巡检计划"}, nil
	}
	if err != nil {
		return RunDetail{}, nil, err
	}
	if planEnabled == 0 || connectionEnabled == 0 {
		return RunDetail{}, &RejectionError{Code: "plan_disabled", Detail: "计划或其来源接入未启用，不能运行巡检"}, nil
	}
	template, ok := TemplateFor(pluginID, templateID)
	if !ok {
		return RunDetail{}, &RejectionError{Code: "unknown_template", Detail: "计划引用的模板不再可用"}, nil
	}
	frozenVersion := template.Version
	if templateVersion.Valid {
		frozenVersion = templateVersion.String
	}
	// 展开发生在 INSERT 之前：业务视图的标签条件与对象已观测身份标签在 Run
	// 创建时确定性冻结进 frozen_scope_json（origin 触发器随后使其不可变），
	// 视图/观测的后续变化不影响已创建 Run。
	targets, frozenScopeJSON, wireKind, expansionErr := s.expandPlanScope(ctx, conn, connectionID, scopeKind, scopeJSON)
	if expansionErr != nil {
		return RunDetail{}, nil, expansionErr
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,scheduled_for,rerun_of_id,state,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?, 'Queued',?)`,
		planID, planKey, connectionID, pluginID, templateID, frozenVersion, paramsJSON, frozenScopeJSON, triggerKind, scheduledFor, nullableInt64(request.rerunOf), now)
	if err != nil {
		// 活动唯一索引是提交顺序上的重叠裁决：只有真正活动的 Run 才把定时
		// 到期转换成 SkippedOverlap，绝不把其它数据库故障伪装成调度决策。
		if scheduledFor != nil && isActiveConflict(ctx, conn, planID) {
			var activeID int64
			_ = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE plan_id=? AND state IN ('Queued','Running')`, planID).Scan(&activeID)
			skip, err := conn.ExecContext(ctx, `
				INSERT INTO inspection_runs(plan_id,plan_key,connection_id,plugin_id,template_id,template_version,frozen_params_json,frozen_scope_json,trigger_kind,scheduled_for,rerun_of_id,state,created_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?, 'SkippedOverlap',?)`,
				planID, planKey, connectionID, pluginID, templateID, frozenVersion, paramsJSON, frozenScopeJSON, triggerKind, scheduledFor, nullableInt64(request.rerunOf), now)
			if err != nil {
				return RunDetail{}, nil, err
			}
			skipID, err := skip.LastInsertId()
			if err != nil {
				return RunDetail{}, nil, err
			}
			detail, err := s.detailOn(ctx, conn, skipID)
			return detail, nil, err
		}
		var active int64
		_ = conn.QueryRowContext(ctx, `SELECT id FROM inspection_runs WHERE plan_id=? AND state IN ('Queued','Running')`, planID).Scan(&active)
		return RunDetail{}, &RejectionError{Code: "active_conflict", Detail: "该巡检计划已有进行中的 Run", ObjectID: active}, nil
	}
	runID, err := insert.LastInsertId()
	if err != nil {
		return RunDetail{}, nil, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE inspection_runs SET state='Running',evidence_at=?,row_version=row_version+1 WHERE id=? AND state='Queued'`, now, runID); err != nil {
		return RunDetail{}, nil, err
	}
	for i, target := range targets {
		checkKey := templateID
		if len(targets) > 1 || target.ObjectType != "" {
			checkKey = fmt.Sprintf("%s-%04d", templateID, i+1)
		}
		displayName := template.DisplayName
		var targetJSON any
		if target.ObjectType != "" || target.LabelConditions != nil {
			encoded, marshalErr := json.Marshal(target)
			if marshalErr != nil {
				return RunDetail{}, nil, marshalErr
			}
			targetJSON = string(encoded)
			switch {
			case target.ObjectType != "":
				displayName = fmt.Sprintf("%s（%s/%s）", template.DisplayName, target.ObjectType, target.IdentityKey)
			case len(target.LabelConditions) != 0:
				displayName = fmt.Sprintf("%s（视图条件 %d 项）", template.DisplayName, len(target.LabelConditions))
			}
		}
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO inspection_run_checks(run_id,check_key,display_name,plugin_id,template_id,template_version,params_json,target_json,created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, runID, checkKey, displayName, pluginID, templateID, frozenVersion, paramsJSON, targetJSON, now); err != nil {
			return RunDetail{}, nil, err
		}
		if !availability.Plinth {
			if err = s.runtimeUnavailableChild(ctx, conn, runID, checkKey, now); err != nil {
				return RunDetail{}, nil, err
			}
			continue
		}
		if err = s.pluginChild(ctx, conn, runID, connectionID, pluginCollectionCheck{
			checkKey: checkKey, pluginID: pluginID, templateID: templateID, templateVersion: frozenVersion,
			paramsJSON: paramsJSON, target: target, evidenceAt: now, scopeKind: wireKind,
		}); err != nil {
			if errors.Is(err, thanos.ErrThanosUnavailable) || errors.Is(err, thanos.ErrGrantNotCurrent) {
				return RunDetail{}, nil, fmt.Errorf("%w: %s", err, "尚无可用的指标连接，请先创建并启用连接后重试")
			}
			return RunDetail{}, nil, err
		}
	}
	if err = s.convergeOn(ctx, conn, runID); err != nil {
		return RunDetail{}, nil, err
	}
	detail, err := s.detailOn(ctx, conn, runID)
	if err != nil {
		return RunDetail{}, nil, err
	}
	return detail, nil, nil
}

// expandPlanScope 在 Run 创建事务内把计划范围确定性展开为冻结目标集，并
// 返回 wire 作用域类型（integration|businessView|objects，随派发输入下发）：
//   - integration：显式全接入意图，单次采集，目标无约束（nil conditions）；
//   - business_view：冻结视图当前 label_conditions（空条件等同全源查询，
//     fail closed 拒绝而非静默放大），快照同时写入 frozen_scope_json；
//   - objects：显式对象集合，逐对象冻结其已观测身份标签（取
//     observed_source_objects.labels_json，不从 canonical 字符串硬编码解析）。
func (s *Service) expandPlanScope(ctx context.Context, conn *sql.Conn, connectionID int64, scopeKind, scopeJSON string) ([]planObjectRef, string, string, error) {
	switch scopeKind {
	case "integration":
		return []planObjectRef{{}}, scopeJSON, "integration", nil
	case "business_view":
		var viewScope struct {
			BusinessViewKey string `json:"businessViewKey"`
		}
		if err := json.Unmarshal([]byte(scopeJSON), &viewScope); err != nil {
			return nil, "", "", err
		}
		var conditionsJSON string
		var viewConnection sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT label_conditions_json, connection_id FROM business_views WHERE view_key=?`, viewScope.BusinessViewKey).Scan(&conditionsJSON, &viewConnection); err != nil {
			if err == sql.ErrNoRows {
				return nil, "", "", &RejectionError{Code: "unknown_business_view", Detail: "业务视图已不存在，不能按其范围采证"}
			}
			return nil, "", "", err
		}
		// Run 创建时重验接入相交：视图保存后其来源可被改到另一接入，与计划
		// 接入不再相交的范围定义必须拒绝，而不是沿旧接入按 labels 查询。
		if viewConnection.Valid && viewConnection.Int64 != connectionID {
			return nil, "", "", &RejectionError{Code: "scope_conflict", Detail: "业务视图来源接入与计划接入不一致，请调整视图或计划"}
		}
		conditions := map[string]string{}
		if err := json.Unmarshal([]byte(conditionsJSON), &conditions); err != nil {
			return nil, "", "", err
		}
		if len(conditions) == 0 {
			return nil, "", "", &RejectionError{Code: "scope_empty", Detail: "业务视图没有标签条件，等同全源查询被拒绝；如需全接入巡检请改用 integration 范围"}
		}
		augmented, err := json.Marshal(map[string]any{
			"kind": "businessView", "businessViewKey": viewScope.BusinessViewKey, "labelConditions": conditions,
		})
		if err != nil {
			return nil, "", "", err
		}
		return []planObjectRef{{LabelConditions: conditions}}, string(augmented), "businessView", nil
	case "objects":
		var scope struct {
			Objects []planObjectRef `json:"objects"`
		}
		if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
			return nil, "", "", err
		}
		if len(scope.Objects) == 0 {
			return nil, "", "", &RejectionError{Code: "malformed_scope", Detail: "objects 范围为空，等同无界查询被拒绝"}
		}
		for i := range scope.Objects {
			var labelsJSON string
			err := conn.QueryRowContext(ctx, `
				SELECT labels_json FROM observed_source_objects
				WHERE connection_id=? AND object_type=? AND identity_key=?`,
				connectionID, scope.Objects[i].ObjectType, scope.Objects[i].IdentityKey).Scan(&labelsJSON)
			if err != nil {
				if err == sql.ErrNoRows {
					return nil, "", "", &RejectionError{Code: "unknown_object", Detail: fmt.Sprintf("对象 %s/%s 未被观测到，不能冻结其身份约束", scope.Objects[i].ObjectType, scope.Objects[i].IdentityKey)}
				}
				return nil, "", "", err
			}
			labels := map[string]string{}
			if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
				return nil, "", "", err
			}
			if len(labels) == 0 {
				return nil, "", "", &RejectionError{Code: "scope_empty", Detail: fmt.Sprintf("对象 %s/%s 没有已观测身份标签，等同无界查询被拒绝", scope.Objects[i].ObjectType, scope.Objects[i].IdentityKey)}
			}
			// 已观测身份标签直接冻结为 LabelConditions（查询约束）：不新增第二
			// 字段，也绝不从 canonical 身份字符串解析标签（其真实格式是按名排序
			// 的 \x1f 连接，不是可逆的键值编码）。
			scope.Objects[i].LabelConditions = labels
		}
		return scope.Objects, scopeJSON, "objects", nil
	}
	return nil, "", "", &RejectionError{Code: "malformed_scope", Detail: "未知的计划范围类型，拒绝展开"}
}

func isActiveConflict(ctx context.Context, conn *sql.Conn, planID int64) bool {
	var active int
	return conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_runs WHERE plan_id=? AND state IN ('Queued','Running')`, planID).Scan(&active) == nil && active > 0
}

// pluginChildForRerun 为重采证 Run 的已复制检查目录冻结子 Attempt：检查行、
// 冻结绑定与 wire 作用域类型全部来自源 Run 的复制（视图当前内容绝不参与），
// 仅接入授权按当前启用修订重新解析。
func (s *Service) pluginChildForRerun(ctx context.Context, conn *sql.Conn, runID, sourceRunID int64, checkKey, now string) error {
	var connectionID int64
	var pluginID, templateID, templateVersion, paramsJSON, frozenScopeJSON string
	var targetJSON sql.NullString
	if err := conn.QueryRowContext(ctx, `
		SELECT r.connection_id, c.plugin_id, c.template_id, c.template_version, c.params_json, c.target_json, r.frozen_scope_json
		FROM inspection_runs r JOIN inspection_run_checks c ON c.run_id=r.id
		WHERE r.id=? AND c.check_key=?`, runID, checkKey).Scan(&connectionID, &pluginID, &templateID, &templateVersion, &paramsJSON, &targetJSON, &frozenScopeJSON); err != nil {
		return err
	}
	target := planObjectRef{}
	if targetJSON.Valid {
		if err := json.Unmarshal([]byte(targetJSON.String), &target); err != nil {
			return err
		}
	}
	frozen := map[string]any{}
	_ = json.Unmarshal([]byte(frozenScopeJSON), &frozen)
	wireKind, _ := frozen["kind"].(string)
	return s.pluginChild(ctx, conn, runID, connectionID, pluginCollectionCheck{
		checkKey: checkKey, pluginID: pluginID, templateID: templateID, templateVersion: templateVersion,
		paramsJSON: paramsJSON, target: target, evidenceAt: now, scopeKind: wireKind,
	})
}

// pluginCollectionCheck 携带一个展开检查的冻结事实。
type pluginCollectionCheck struct {
	checkKey        string
	pluginID        string
	templateID      string
	templateVersion string
	paramsJSON      string
	target          planObjectRef
	evidenceAt      string
	scopeKind       string // wire 值：integration|businessView|objects
}

// pluginChild 冻结一个 run_check 插件采集子 Attempt：config_thanos_query
// grant 与版本化输入正文（与既有 PromQL 子 Attempt 的授权边界相同）。
func (s *Service) pluginChild(ctx context.Context, conn *sql.Conn, runID, connectionID int64, check pluginCollectionCheck) error {
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,check_key,state,quoin_release_version,created_at)
		VALUES('inspection_collection','run_check',?,?,'Queued',?,?)`, runID, check.checkKey, attempt.ReleaseVersion(), s.nowText())
	if err != nil {
		return err
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	grant, err := thanos.ResolveConfigGrantForConnection(ctx, conn, attemptID, connectionID)
	if err != nil {
		return err
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(check.paramsJSON), &params); err != nil {
		return err
	}
	var target *planObjectRef
	if check.target.ObjectType != "" || check.target.LabelConditions != nil {
		target = &planObjectRef{ObjectType: check.target.ObjectType, IdentityKey: check.target.IdentityKey, LabelConditions: check.target.LabelConditions}
	}
	body := pluginCollectionInput{
		SchemaKind: pluginExecutionSchemaKind, AttemptID: attemptID, InspectionRunID: runID,
		CheckKey: check.checkKey, PluginID: check.pluginID, TemplateID: check.templateID,
		TemplateVersion: check.templateVersion, Params: params, EvidenceAt: check.evidenceAt,
		GrantID: grant.GrantID, Target: target, ScopeKind: check.scopeKind,
	}
	canonical, err := json.Marshal(body)
	if err != nil {
		return err
	}
	inputDigest := sha256.Sum256(canonical)
	snapshot, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?, 'v1',?,?)`, attemptID, pluginExecutionSchemaKind, hex.EncodeToString(inputDigest[:]), s.nowText())
	if err != nil {
		return err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return err
	}
	runDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-run:%d", runID)))
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_run_id)
		VALUES(?,1,'inspection_run',?,?)`, snapshotID, hex.EncodeToString(runDigest[:]), runID); err != nil {
		return err
	}
	// 冻结实际使用的接入修订作为来源谱系；归属由 input item 闭合触发器精确校验。
	revisionDigest := sha256.Sum256([]byte(fmt.Sprintf("connection-revision:%d", grant.ConnectionRevisionID)))
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
		VALUES(?,2,'connection_revision',?,?)`, snapshotID, hex.EncodeToString(revisionDigest[:]), grant.ConnectionRevisionID); err != nil {
		return err
	}
	return nil
}
