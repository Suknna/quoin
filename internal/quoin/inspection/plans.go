package inspection

// 独立巡检计划（ADR-0004）：计划直接绑定一个来源接入与一个插件模板，不再内嵌
// 于业务声明。本文件拥有计划的命令面（创建/更新/读取）、插件模板的静态校验
// 目录、以及接入启用时幂等创建的默认基础计划。运行只消费 Run 创建时冻结的
// 绑定（inspection_runs.plan_* 列），计划后续修改不改写任何已存在 Run。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/robfig/cron/v3"

	"github.com/Suknna/quoin/internal/plugins/builtin"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// planKeyPattern 与业务声明稳定 key 共用同一封闭词表：小写字母开头，仅小写
// 字母/数字/连字符，退役不复用。
var planKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// planCronParser 只接受标准五字段 cron，与既有调度语义一致。
var planCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Template 描述一个插件贡献的确定性采证模板。执行位置固定为 Plinth
// supervisor 的类型化出站调用；参数 Schema 是封闭对象。
type Template struct {
	ID          string
	PluginID    string
	Version     string
	DisplayName string
	Description string
	// Validate 静态校验一份类型化参数；不做任何 I/O。
	Validate func(params map[string]any) error
}

// paramValidators 是模板参数形状的静态校验表；模板身份与版本的存在性权威
// 是共享插件描述目录（builtin.Descriptors → plugins.Registry），执行
// 绑定在 Plinth supervisor。二者以 (pluginID, templateID, version) 对齐。
var paramValidators = map[string]func(map[string]any) error{
	"promql_instant": validateInstantParams,
	"promql_range":   validateRangeParams,
}

// TemplateFor 返回 (pluginID, templateID) 的已注册模板；目录来自构建期
// 描述符（共享 builtin 声明 → plugins.Registry），绝不声明不存在的模板。
func TemplateFor(pluginID, templateID string) (Template, bool) {
	for _, descriptor := range builtin.Descriptors() {
		if descriptor.ID != pluginID {
			continue
		}
		for _, template := range descriptor.InspectionTemplates {
			if template.ID == templateID {
				validate, known := paramValidators[template.ID]
				if !known {
					validate = func(map[string]any) error { return nil }
				}
				return Template{
					ID: template.ID, PluginID: pluginID, Version: template.Version,
					DisplayName: template.Title, Description: template.Description,
					Validate: validate,
				}, true
			}
		}
	}
	return Template{}, false
}

// pluginForConnectionKind 把来源接入平台类型映射到执行插件。浏览器与模型
// 接入不参与巡检采证。
func pluginForConnectionKind(kind string) (string, bool) {
	switch kind {
	case "prometheus":
		return "prometheus", true
	case "thanos":
		return "thanos", true
	}
	return "", false
}

// validateExpression 用锁定的上游 AST 解析表达式；不使用正则或字符串改写。
// 巡检表达式允许 offset/子查询（与声明检查一致）。
func validateExpression(expression string) error {
	if expression == "" {
		return fmt.Errorf("表达式不能为空")
	}
	if _, err := promQLParser.ParseExpr(expression); err != nil {
		return fmt.Errorf("PromQL 解析失败: %s", firstLine(err.Error()))
	}
	return nil
}

// promQLParser 与声明配置共用同一冻结的上游解析器选项。
var promQLParser = parser.NewParser(parser.Options{})

func firstLine(message string) string {
	for i, r := range message {
		if r == '\n' {
			return message[:i]
		}
	}
	return message
}

func validateInstantParams(params map[string]any) error {
	expression, _ := params["expression"].(string)
	if err := validateExpression(expression); err != nil {
		return err
	}
	if len(params) != 1 {
		return fmt.Errorf("即时查询只接受 expression 参数")
	}
	return nil
}

func validateRangeParams(params map[string]any) error {
	expression, _ := params["expression"].(string)
	if err := validateExpression(expression); err != nil {
		return err
	}
	rangeSeconds, ok := params["rangeSeconds"].(float64)
	if !ok || rangeSeconds < 1 || rangeSeconds != float64(int64(rangeSeconds)) {
		return fmt.Errorf("rangeSeconds 必须是正整数秒")
	}
	stepSeconds, ok := params["stepSeconds"].(float64)
	if !ok || stepSeconds < 1 || stepSeconds != float64(int64(stepSeconds)) {
		return fmt.Errorf("stepSeconds 必须是正整数秒")
	}
	if len(params) != 3 {
		return fmt.Errorf("范围查询只接受 expression/rangeSeconds/stepSeconds 参数")
	}
	return nil
}

// Plan 是计划的可读投影；params/scope 保持创建时的类型化结构。
type Plan struct {
	PlanKey         string  `json:"planKey"`
	DisplayName     string  `json:"displayName"`
	Enabled         bool    `json:"enabled"`
	ConnectionName  string  `json:"connectionName"`
	PluginID        string  `json:"pluginId"`
	TemplateID      string  `json:"templateId"`
	TemplateVersion *string `json:"templateVersion,omitempty"`
	Params          any     `json:"params"`
	Scope           any     `json:"scope"`
	Cron            *string `json:"cron,omitempty"`
	Timezone        string  `json:"timezone"`
	RowVersion      int64   `json:"rowVersion"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
	planID          int64
	connectionID    int64
	paramsJSON      string
	scopeJSON       string
	scopeKind       string
}

// PlanInput 是创建/更新命令的载荷。Scope 是已按 DTO 形状反序列化的结构：
// Kind 决定其余字段（integration: 空；business_view: businessViewKey；
// objects: objects[]）。
type PlanInput struct {
	PlanKey         string
	DisplayName     string
	Enabled         bool
	ConnectionName  string
	PluginID        string
	TemplateID      string
	TemplateVersion *string
	Params          map[string]any
	ScopeKind       string
	BusinessViewKey string
	Objects         []PlanObject
	Cron            *string
	Timezone        string
}

// wireScopeKind 把 DB 存储值映射为 wire 命名（camelCase）；integration 与
// objects 两个命名在两侧一致。
func wireScopeKind(scopeKind string) string {
	if scopeKind == "business_view" {
		return "businessView"
	}
	return scopeKind
}

// dbScopeKind 接受 wire 命名并归一为 DB 存储值；未知命名绝不默认为
// integration（那会悄悄扩大范围）。
func dbScopeKind(wire string) (string, error) {
	switch wire {
	case "integration", "objects":
		return wire, nil
	case "businessView":
		return "business_view", nil
	}
	return "", &PlanConflictError{Code: "malformed_scope", Detail: "scope.kind 必须是 integration、businessView 或 objects"}
}

// PlanObject 是 objects 范围中的一个来源对象定位（与资源 DTO 的
// objectType+identityKey 完全一致，UI 不重算身份）。
type PlanObject struct {
	ObjectType  string `json:"objectType"`
	IdentityKey string `json:"identityKey"`
}

func (input PlanInput) scopeShape() map[string]any {
	switch input.ScopeKind {
	case "business_view":
		return map[string]any{"kind": "business_view", "businessViewKey": input.BusinessViewKey}
	case "objects":
		if input.Objects == nil {
			input.Objects = []PlanObject{}
		}
		return map[string]any{"kind": "objects", "objects": input.Objects}
	default:
		return map[string]any{"kind": "integration"}
	}
}

// PlanConflictError 是命令台账记录的确定性拒绝。
type PlanConflictError struct {
	Code, Detail string
}

func (e *PlanConflictError) Error() string { return e.Detail }

// planRejection adapts the module's deterministic plan validation failures to
// the runner's recorded rejection shape; any other error passes through
// unchanged (clean rollback, no durable trace).
func planRejection(err error) error {
	var conflict *PlanConflictError
	if errors.As(err, &conflict) {
		return &execution.Rejection{Code: conflict.Code, Detail: conflict.Detail}
	}
	return err
}

// translatePlanError maps the runner's recorded plan rejections back to the
// public PlanConflictError shape — the HTTP problem mapping depends on Code —
// and keeps the historic command_reused identity for ledger key conflicts.
func translatePlanError(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		return &PlanConflictError{Code: rejection.Code, Detail: rejection.Detail}
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return &PlanConflictError{Code: "command_reused", Detail: "命令标识已用于其它请求，请更换后重试"}
	}
	return err
}

// CreatePlan 在执行器的一个事务中校验并创建计划：会话复核、幂等重放、业务
// 修改、命令台账与审计事件同事务提交。计划不改变任何运行中的对象；只有随后
// 创建的 Run 消费它。
func (s *Service) CreatePlan(ctx context.Context, principalID int64, clientCommandID string, input PlanInput) (Plan, error) {
	if err := validatePlanKey(input.PlanKey); err != nil {
		return Plan{}, err
	}
	digest := auth.DigestCommand(CommandCreatePlan, map[string]any{"planKey": input.PlanKey, "connectionName": input.ConnectionName, "displayName": input.DisplayName, "enabled": input.Enabled})
	outcome, err := execution.Run(ctx, s.runner, s.createPlan, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Plan, execution.Change, error) {
		frozen, err := s.validatePlanInput(ctx, tx, input)
		if err != nil {
			return Plan{}, execution.Unchanged, planRejection(err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, input.PlanKey).Scan(&exists); err != nil {
			return Plan{}, execution.Unchanged, err
		}
		if exists != 0 {
			return Plan{}, execution.Unchanged, &execution.Rejection{Code: "plan_exists", Detail: "同名巡检计划已存在，key 退役后不可复用"}
		}
		now := s.nowText()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)`,
			input.PlanKey, input.DisplayName, boolInt(input.Enabled), frozen.connectionID, input.PluginID, input.TemplateID, input.TemplateVersion,
			frozen.params, frozen.scope, frozen.scopeKind, nullableText(input.Cron), input.Timezone, principalID, now, now); err != nil {
			return Plan{}, execution.Unchanged, err
		}
		plan, err := s.planOn(ctx, tx, input.PlanKey)
		if err != nil {
			return Plan{}, execution.Unchanged, err
		}
		return plan, execution.Changed, nil
	}, func(plan Plan) int64 { return plan.planID })
	if err != nil {
		return Plan{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

// UpdatePlan 以整体提交方式更新计划定义；expectedRowVersion 是并发前提。更新
// 创建新的定义事实，但不改写任何已存在 Run 的冻结绑定。
func (s *Service) UpdatePlan(ctx context.Context, principalID int64, clientCommandID string, input PlanInput, expectedRowVersion int64) (Plan, error) {
	digest := auth.DigestCommand(CommandUpdatePlan, map[string]any{"planKey": input.PlanKey, "expectedRowVersion": expectedRowVersion, "displayName": input.DisplayName, "enabled": input.Enabled})
	outcome, err := execution.Run(ctx, s.runner, s.updatePlan, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (Plan, execution.Change, error) {
		frozen, err := s.validatePlanInput(ctx, tx, input)
		if err != nil {
			return Plan{}, execution.Unchanged, planRejection(err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE inspection_plans SET display_name=?,enabled=?,connection_id=?,plugin_id=?,template_id=?,template_version=?,
			  params_json=?,scope_json=?,scope_kind=?,cron=?,timezone=?,row_version=row_version+1,updated_at=?
			WHERE plan_key=? AND row_version=?`,
			input.DisplayName, boolInt(input.Enabled), frozen.connectionID, input.PluginID, input.TemplateID, input.TemplateVersion,
			frozen.params, frozen.scope, frozen.scopeKind, nullableText(input.Cron), input.Timezone, s.nowText(),
			input.PlanKey, expectedRowVersion)
		if err != nil {
			return Plan{}, execution.Unchanged, err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return Plan{}, execution.Unchanged, &execution.Rejection{Code: "row_version_conflict", Detail: "巡检计划已变化，请刷新后重试"}
		}
		plan, err := s.planOn(ctx, tx, input.PlanKey)
		if err != nil {
			return Plan{}, execution.Unchanged, err
		}
		return plan, execution.Changed, nil
	}, func(plan Plan) int64 { return plan.planID })
	if err != nil {
		return Plan{}, translatePlanError(err)
	}
	return outcome.Result, nil
}

// ListPlans 返回全部计划，按 key 稳定序（只读 reader）。
func (s *Service) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT plan_key FROM inspection_plans ORDER BY plan_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	plans := []Plan{}
	for _, key := range keys {
		plan, err := s.planOn(ctx, s.reader, key)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// GetPlan 返回一个计划（只读 reader）。
func (s *Service) GetPlan(ctx context.Context, planKey string) (Plan, error) {
	return s.planOn(ctx, s.reader, planKey)
}

func (s *Service) planOn(ctx context.Context, q audit.Reader, planKey string) (Plan, error) {
	var plan Plan
	var enabled int
	var templateVersion, cron sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT p.id,p.plan_key,p.display_name,p.enabled,c.name,p.plugin_id,p.template_id,p.template_version,
		       p.params_json,p.scope_json,p.scope_kind,p.cron,p.timezone,p.row_version,p.created_at,p.updated_at
		FROM inspection_plans p JOIN connections c ON c.id=p.connection_id
		WHERE p.plan_key=?`, planKey).
		Scan(&plan.planID, &plan.PlanKey, &plan.DisplayName, &enabled, &plan.ConnectionName, &plan.PluginID, &plan.TemplateID, &templateVersion,
			&plan.paramsJSON, &plan.scopeJSON, &plan.scopeKind, &cron, &plan.Timezone, &plan.RowVersion, &plan.CreatedAt, &plan.UpdatedAt)
	if err == sql.ErrNoRows {
		return Plan{}, &PlanConflictError{Code: "not_found", Detail: "巡检计划不存在"}
	}
	if err != nil {
		return Plan{}, err
	}
	plan.Enabled = enabled != 0
	if templateVersion.Valid {
		plan.TemplateVersion = &templateVersion.String
	}
	if cron.Valid {
		plan.Cron = &cron.String
	}
	if err := json.Unmarshal([]byte(plan.paramsJSON), &plan.Params); err != nil {
		return Plan{}, err
	}
	scopeShape := map[string]any{}
	if err := json.Unmarshal([]byte(plan.scopeJSON), &scopeShape); err != nil {
		return Plan{}, err
	}
	scopeShape["kind"] = wireScopeKind(plan.scopeKind)
	plan.Scope = scopeShape
	return plan, nil
}

// frozenPlanBinding 是 Run 创建时从计划复制的不可变绑定。
type frozenPlanBinding struct {
	connectionID int64
	params       string
	scope        string
	scopeKind    string // 归一后的 DB 存储值
}

// validatePlanInput 静态校验计划定义并解析接入/视图/对象引用。缺省模板版本
// 在 Run 创建时冻结当时版本；params 按模板 Schema 校验；scope 引用必须存在
// 且视图接入与计划接入固定相交（绝不全源查询）。
func (s *Service) validatePlanInput(ctx context.Context, q execution.Executor, input PlanInput) (frozenPlanBinding, error) {
	var frozen frozenPlanBinding
	normalizedKind, err := dbScopeKind(input.ScopeKind)
	if err != nil {
		return frozen, err
	}
	input.ScopeKind = normalizedKind
	if err != nil {
		return frozen, err
	}
	if !planKeyPattern.MatchString(input.PlanKey) {
		return frozen, &PlanConflictError{Code: "malformed_key", Detail: "计划 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	if input.DisplayName == "" {
		return frozen, &PlanConflictError{Code: "malformed_plan", Detail: "计划显示名不能为空"}
	}
	if input.Timezone == "" {
		input.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil {
		return frozen, &PlanConflictError{Code: "malformed_plan", Detail: "时区必须是合法 IANA 名称"}
	}
	if input.Cron != nil && *input.Cron != "" {
		if _, err := planCronParser.Parse(*input.Cron); err != nil {
			return frozen, &PlanConflictError{Code: "malformed_cron", Detail: "cron 必须是标准五字段表达式"}
		}
	}
	template, ok := TemplateFor(input.PluginID, input.TemplateID)
	if !ok {
		return frozen, &PlanConflictError{Code: "unknown_template", Detail: "插件未提供该巡检模板"}
	}
	if input.TemplateVersion != nil && *input.TemplateVersion != template.Version {
		return frozen, &PlanConflictError{Code: "unknown_template", Detail: "模板版本不可用"}
	}
	if input.Params == nil {
		input.Params = map[string]any{}
	}
	if err := template.Validate(input.Params); err != nil {
		return frozen, &PlanConflictError{Code: "invalid_params", Detail: err.Error()}
	}
	params, err := json.Marshal(input.Params)
	if err != nil {
		return frozen, err
	}
	scope, err := json.Marshal(input.scopeShape())
	if err != nil {
		return frozen, err
	}
	frozen.params = string(params)
	frozen.scope = string(scope)
	frozen.scopeKind = input.ScopeKind
	if err := s.resolvePlanReferences(ctx, q, input, &frozen); err != nil {
		return frozen, err
	}
	return frozen, nil
}

// resolvePlanReferences 校验接入、业务视图与对象引用。计划接入必须固定：
// business_view 范围只允许跨来源候选视图或与计划接入一致的视图。
func (s *Service) resolvePlanReferences(ctx context.Context, q execution.Executor, input PlanInput, frozen *frozenPlanBinding) error {
	var kind string
	err := q.QueryRowContext(ctx, `SELECT type FROM connections WHERE name=?`, input.ConnectionName).Scan(&kind)
	if err == sql.ErrNoRows {
		return &PlanConflictError{Code: "unknown_connection", Detail: "来源接入不存在"}
	}
	if err != nil {
		return err
	}
	pluginID, ok := pluginForConnectionKind(kind)
	if !ok {
		return &PlanConflictError{Code: "unsupported_connection", Detail: "该接入类型不支持巡检采证"}
	}
	if pluginID != input.PluginID {
		return &PlanConflictError{Code: "plugin_mismatch", Detail: "插件与接入平台类型不匹配"}
	}
	if err := q.QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, input.ConnectionName).Scan(&frozen.connectionID); err != nil {
		return err
	}
	switch input.ScopeKind {
	case "integration":
	case "business_view":
		if input.BusinessViewKey == "" {
			return &PlanConflictError{Code: "malformed_scope", Detail: "business_view 范围必须携带 businessViewKey"}
		}
		var viewConnection sql.NullInt64
		err := q.QueryRowContext(ctx, `SELECT connection_id FROM business_views WHERE view_key=?`, input.BusinessViewKey).Scan(&viewConnection)
		if err == sql.ErrNoRows {
			return &PlanConflictError{Code: "unknown_business_view", Detail: "业务视图不存在"}
		}
		if err != nil {
			return err
		}
		if viewConnection.Valid && viewConnection.Int64 != frozen.connectionID {
			return &PlanConflictError{Code: "scope_conflict", Detail: "业务视图绑定其它接入；计划接入必须与视图固定相交"}
		}
	case "objects":
		if len(input.Objects) == 0 {
			return &PlanConflictError{Code: "malformed_scope", Detail: "objects 范围至少显式写一个对象"}
		}
		for _, object := range input.Objects {
			if object.ObjectType == "" || object.IdentityKey == "" {
				return &PlanConflictError{Code: "malformed_scope", Detail: "对象必须携带 objectType 与 identityKey"}
			}
			var exists int
			if err := q.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM observed_source_objects
				WHERE connection_id=? AND object_type=? AND identity_key=?`,
				frozen.connectionID, object.ObjectType, object.IdentityKey).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return &PlanConflictError{Code: "unknown_object", Detail: fmt.Sprintf("对象 %s/%s 未被观测到", object.ObjectType, object.IdentityKey)}
			}
		}
	default:
		return &PlanConflictError{Code: "malformed_scope", Detail: "scope.kind 必须是 integration、business_view 或 objects"}
	}
	return nil
}

// DefaultPlanKey 返回接入的确定性默认计划 key：合法且不超长时直接派生，
// 否则用完整 SHA-256 前缀保证无碰撞，绝不截断原名。
func DefaultPlanKey(connectionName string) string {
	candidate := "basic-" + connectionName
	if planKeyPattern.MatchString(connectionName) && len(candidate) <= 63 {
		return candidate
	}
	sum := sha256.Sum256([]byte(connectionName))
	return "basic-" + hex.EncodeToString(sum[:])[:32]
}

// EnsureDefaultPlan 以独立执行器命令幂等创建默认基础计划（无外部组合方的
// 直连入口）；与接入启用同事务的组合路径仍走 EnsureDefaultPlanOn。
func (s *Service) EnsureDefaultPlan(ctx context.Context, connectionID int64, connectionName string) error {
	if s.opDefaultPlan == nil {
		return errors.New("inspection: default plan operation is not registered")
	}
	_, err := execution.Execute(ctx, s.runner, s.opDefaultPlan,
		func(tx *execution.Tx) (struct{}, error) {
			return struct{}{}, s.EnsureDefaultPlanOn(ctx, tx, connectionID, connectionName)
		},
		func(struct{}) int64 { return int64(connectionID) })
	return err
}

// EnsureDefaultPlanOn 在接入启用事务内幂等创建默认基础计划：用户点“立即
// 巡检”无需先填表。计划仅人工运行（cron NULL，不产生定时模型费用）；已存在
// 同名计划（含用户自建）时原样保留。非指标接入不创建。conn 是连接启用方的
// 受守卫事务句柄（execution.Executor），使默认计划的创建与启用保持同事务
// 原子提交；*sql.Conn 等同签名查询面同样满足该接口。
func (s *Service) EnsureDefaultPlanOn(ctx context.Context, conn execution.Executor, connectionID int64, connectionName string) error {
	var kind string
	if err := conn.QueryRowContext(ctx, `SELECT type FROM connections WHERE id=?`, connectionID).Scan(&kind); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	pluginID, ok := pluginForConnectionKind(kind)
	if !ok {
		return nil
	}
	key := DefaultPlanKey(connectionName)
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_plans WHERE plan_key=?`, key).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	now := s.nowText()
	params, _ := json.Marshal(map[string]any{"expression": "up"})
	scope, _ := json.Marshal(map[string]any{"kind": "integration"})
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,template_version,params_json,scope_json,scope_kind,cron,timezone,row_version,created_by,created_at,updated_at)
		VALUES(?,?,1,?,?,?,NULL,?,?,'integration',NULL,'UTC',1,NULL,?,?)`,
		key, connectionName+" 基础巡检", connectionID, pluginID, "promql_instant", string(params), string(scope), now, now); err != nil {
		return err
	}
	return nil
}

func validatePlanKey(key string) error {
	if !planKeyPattern.MatchString(key) {
		return &PlanConflictError{Code: "malformed_key", Detail: "计划 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	return nil
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableText(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}
