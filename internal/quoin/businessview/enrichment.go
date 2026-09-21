// 富化规则域（ADR-0012）：管理员维护的"命中即叠加 outputs"声明式富化配置。
// 与业务视图同一模块（范围声明语义一致：精确 label=value 条件 + 显式告警源
// key 约束），但拥有独立的表、命令与对象类型。写命令通过共享执行器
// execution 执行（ADR-0006）：会话复核、幂等重放、业务修改、命令台账与审计
// 事件同一事务提交；读路径走可信只读面。无删除：key 退役 = enabled=0
// （退役不复用，由 schema 触发器强制 key 不可改写、行不可删除）。
package businessview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	// CommandEnrichmentCreate/CommandEnrichmentUpdate 是命令台账的稳定命令
	// 类型，同时是审计 action。
	CommandEnrichmentCreate = "enrichment_rule.create"
	CommandEnrichmentUpdate = "enrichment_rule.update"
	// ObjectEnrichmentRule 是审计与命令结果的领域对象类型。
	ObjectEnrichmentRule = "enrichment_rule"
	// defaultEnrichmentPriority 是未显式指定 priority 时的缺省叠加序。
	defaultEnrichmentPriority = 100
)

// EnrichmentService 拥有富化规则聚合的写命令与读投影；装配约定与
// businessview.Service 完全一致（共享执行器注册表 + 可信只读面）。
type EnrichmentService struct {
	reader  execution.Reader
	runner  *execution.Runner
	create  *execution.Operation
	update  *execution.Operation
	nowFunc func() time.Time
}

// NewEnrichmentService 是组合层初始装配（应用构造在 configureReadOnly 之前）：
// 操作注册进私有执行器，读路径保持零值可信只读面——全部读取失败关闭。
func NewEnrichmentService(db *sql.DB) *EnrichmentService {
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := newEnrichmentService(runner.Reader(), runner)
	if err != nil {
		panic("businessview: compose default enrichment service: " + err.Error())
	}
	return service
}

// NewEnrichmentServiceWithReader 装配富化规则模块：reader 经共享执行器验证
// （仅接受 execution.OpenReadOnly 产生的可信类型），runner 是组合层共享执行器。
func NewEnrichmentServiceWithReader(reader audit.Reader, runner *execution.Runner) (*EnrichmentService, error) {
	if runner == nil {
		return nil, errors.New("businessview: enrichment command runner is required")
	}
	if err := runner.SetReader(reader); err != nil {
		return nil, fmt.Errorf("businessview: enrichment: %w", err)
	}
	return newEnrichmentService(runner.Reader(), runner)
}

// SetReader 把真实只读能力转发给共享执行器验证并采纳验证后的读面。
func (s *EnrichmentService) SetReader(reader audit.Reader) error {
	if err := s.runner.SetReader(reader); err != nil {
		return fmt.Errorf("businessview: enrichment: %w", err)
	}
	s.reader = s.runner.Reader()
	return nil
}

func newEnrichmentService(reader execution.Reader, runner *execution.Runner) (*EnrichmentService, error) {
	if runner == nil {
		return nil, errors.New("businessview: enrichment command runner is required")
	}
	create, err := runner.Register(execution.Operation{
		Name:       CommandEnrichmentCreate,
		Class:      execution.ClassWrite,
		ObjectType: ObjectEnrichmentRule,
		Authorize:  authorizeViewManager,
	})
	if err != nil {
		return nil, fmt.Errorf("businessview: register %s: %w", CommandEnrichmentCreate, err)
	}
	update, err := runner.Register(execution.Operation{
		Name:       CommandEnrichmentUpdate,
		Class:      execution.ClassWrite,
		ObjectType: ObjectEnrichmentRule,
		Authorize:  authorizeViewManager,
	})
	if err != nil {
		return nil, fmt.Errorf("businessview: register %s: %w", CommandEnrichmentUpdate, err)
	}
	return &EnrichmentService{reader: reader, runner: runner, create: create, update: update, nowFunc: time.Now}, nil
}

func (s *EnrichmentService) nowText() string { return s.nowFunc().UTC().Format(time.RFC3339Nano) }

// EnrichmentRule 是规则的可读投影；labelConditions/alertSourceKeys/outputs 是
// 解析后的 wire 形状。
type EnrichmentRule struct {
	RuleKey         string            `json:"ruleKey"`
	DisplayName     string            `json:"displayName"`
	Description     string            `json:"description"`
	Enabled         bool              `json:"enabled"`
	LabelConditions map[string]string `json:"labelConditions"`
	AlertSourceKeys []string          `json:"alertSourceKeys"`
	Outputs         map[string]string `json:"outputs"`
	Priority        int               `json:"priority"`
	RowVersion      int64             `json:"rowVersion"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
	ruleID          int64
}

// EnrichmentRuleInput 是创建/更新命令载荷（wire 命名）。
type EnrichmentRuleInput struct {
	RuleKey         string
	DisplayName     string
	Description     string
	Enabled         bool
	LabelConditions map[string]string
	AlertSourceKeys []string
	Outputs         map[string]string
	Priority        int
}

// CreateEnrichmentRule 在执行器的一个事务中校验并创建规则；台账与审计由
// 执行器自动持久化。
func (s *EnrichmentService) CreateEnrichmentRule(ctx context.Context, principalID int64, clientCommandID string, input EnrichmentRuleInput) (EnrichmentRule, error) {
	digest := auth.DigestCommand(CommandEnrichmentCreate, map[string]any{"ruleKey": input.RuleKey, "displayName": input.DisplayName, "alertSourceKeys": input.AlertSourceKeys, "outputs": input.Outputs})
	outcome, err := execution.Run(ctx, s.runner, s.create, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (EnrichmentRule, execution.Change, error) {
		normalized, err := validateEnrichmentInput(ctx, tx, input, true)
		if err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM enrichment_rules WHERE rule_key=?`, input.RuleKey).Scan(&exists); err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		if exists != 0 {
			return EnrichmentRule{}, execution.Changed, &execution.Rejection{Code: "rule_exists", Detail: "同名富化规则已存在，key 退役后不可复用"}
		}
		now := s.nowText()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO enrichment_rules(rule_key,display_name,description,enabled,label_conditions_json,alert_source_keys_json,outputs_json,priority,row_version,created_by,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,1,?,?,?)`,
			input.RuleKey, input.DisplayName, input.Description, boolInt(normalized.enabled), normalized.conditions, normalized.alertSourceKeys, normalized.outputs, normalized.priority, principalID, now, now); err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		rule, err := enrichmentRuleOn(ctx, tx, input.RuleKey)
		if err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		return rule, execution.Changed, nil
	}, func(rule EnrichmentRule) int64 { return rule.ruleID })
	if err != nil {
		return EnrichmentRule{}, translateRunnerError(err)
	}
	return outcome.Result, nil
}

// UpdateEnrichmentRule 以整体提交方式更新规则的可变内容（显示名/描述/启停/
// 条件/来源约束/输出/叠加序）；expectedRowVersion 是并发前提。rule_key 不可
// 改写（schema 触发器强制），规则永不删除——退役 = enabled=0 后 key 不复用。
// 更新不回写任何已冻结的首观测富化文档（ADR-0012 冻结语义）。
func (s *EnrichmentService) UpdateEnrichmentRule(ctx context.Context, principalID int64, clientCommandID string, input EnrichmentRuleInput, expectedRowVersion int64) (EnrichmentRule, error) {
	digest := auth.DigestCommand(CommandEnrichmentUpdate, map[string]any{"ruleKey": input.RuleKey, "expectedRowVersion": expectedRowVersion, "displayName": input.DisplayName, "alertSourceKeys": input.AlertSourceKeys, "outputs": input.Outputs})
	outcome, err := execution.Run(ctx, s.runner, s.update, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (EnrichmentRule, execution.Change, error) {
		normalized, err := validateEnrichmentInput(ctx, tx, input, false)
		if err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE enrichment_rules SET display_name=?,description=?,enabled=?,label_conditions_json=?,alert_source_keys_json=?,outputs_json=?,priority=?,
			  row_version=row_version+1,updated_at=?
			WHERE rule_key=? AND row_version=?`,
			input.DisplayName, input.Description, boolInt(normalized.enabled), normalized.conditions, normalized.alertSourceKeys, normalized.outputs, normalized.priority, s.nowText(),
			input.RuleKey, expectedRowVersion)
		if err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return EnrichmentRule{}, execution.Changed, &execution.Rejection{Code: "row_version_conflict", Detail: "富化规则已变化，请刷新后重试"}
		}
		rule, err := enrichmentRuleOn(ctx, tx, input.RuleKey)
		if err != nil {
			return EnrichmentRule{}, execution.Changed, err
		}
		return rule, execution.Changed, nil
	}, func(rule EnrichmentRule) int64 { return rule.ruleID })
	if err != nil {
		return EnrichmentRule{}, translateRunnerError(err)
	}
	return outcome.Result, nil
}

// ListEnrichmentRules 返回全部规则，按 key 稳定序。
func (s *EnrichmentService) ListEnrichmentRules(ctx context.Context) ([]EnrichmentRule, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT rule_key FROM enrichment_rules ORDER BY rule_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rules := []EnrichmentRule{}
	for _, key := range keys {
		rule, err := enrichmentRuleOn(ctx, s.reader, key)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// GetEnrichmentRule 返回一个规则。
func (s *EnrichmentService) GetEnrichmentRule(ctx context.Context, ruleKey string) (EnrichmentRule, error) {
	return enrichmentRuleOn(ctx, s.reader, ruleKey)
}

// enrichmentRuleOn 读取单个规则投影；查询面是 audit.Reader（*execution.Tx 亦
// 满足），同一实现既可跑在执行器事务内，也可跑在只读读源上。
func enrichmentRuleOn(ctx context.Context, q audit.Reader, ruleKey string) (EnrichmentRule, error) {
	var rule EnrichmentRule
	var enabled int
	var conditionsJSON, sourceKeysJSON, outputsJSON string
	err := q.QueryRowContext(ctx, `
		SELECT id,rule_key,display_name,description,enabled,label_conditions_json,alert_source_keys_json,outputs_json,priority,row_version,created_at,updated_at
		FROM enrichment_rules WHERE rule_key=?`, ruleKey).
		Scan(&rule.ruleID, &rule.RuleKey, &rule.DisplayName, &rule.Description, &enabled, &conditionsJSON, &sourceKeysJSON, &outputsJSON, &rule.Priority, &rule.RowVersion, &rule.CreatedAt, &rule.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrichmentRule{}, &ConflictError{Code: "not_found", Detail: "富化规则不存在"}
	}
	if err != nil {
		return EnrichmentRule{}, err
	}
	rule.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(conditionsJSON), &rule.LabelConditions); err != nil {
		return EnrichmentRule{}, err
	}
	if rule.LabelConditions == nil {
		rule.LabelConditions = map[string]string{}
	}
	if err := json.Unmarshal([]byte(sourceKeysJSON), &rule.AlertSourceKeys); err != nil {
		return EnrichmentRule{}, err
	}
	if rule.AlertSourceKeys == nil {
		rule.AlertSourceKeys = []string{}
	}
	if err := json.Unmarshal([]byte(outputsJSON), &rule.Outputs); err != nil {
		return EnrichmentRule{}, err
	}
	if rule.Outputs == nil {
		rule.Outputs = map[string]string{}
	}
	return rule, nil
}

type normalizedEnrichmentInput struct {
	enabled         bool
	conditions      string
	alertSourceKeys string
	outputs         string
	priority        int
}

// validateEnrichmentInput 静态校验规则定义并在同一事务内解析来源引用。
// 确定性校验失败以 *execution.Rejection 返回，由执行器持久记录拒绝事实。
// 语义约束（ADR-0012）：
//   - rule_key 词表 ^[a-z][a-z0-9-]{0,62}$（仅创建路径校验，key 不可改写）；
//   - label_conditions 是非空 label=value 对（与视图同规则）；空集合合法 =
//     全局规则（命中一切，用于默认富化）；
//   - alert_source_keys 去重、稳定序且存在于启用中的 alert_sources；空 = 不限来源；
//   - outputs 非空且值为非空字符串；
//   - priority 缺省 100，恒 >= 1。
func validateEnrichmentInput(ctx context.Context, q audit.Reader, input EnrichmentRuleInput, isCreate bool) (normalizedEnrichmentInput, error) {
	var normalized normalizedEnrichmentInput
	normalized.enabled = input.Enabled
	if isCreate && !viewKeyPattern.MatchString(input.RuleKey) {
		return normalized, &execution.Rejection{Code: "malformed_key", Detail: "规则 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	if input.DisplayName == "" {
		return normalized, &execution.Rejection{Code: "malformed_rule", Detail: "规则显示名不能为空"}
	}
	if input.LabelConditions == nil {
		input.LabelConditions = map[string]string{}
	}
	for name, value := range input.LabelConditions {
		if name == "" || value == "" {
			return normalized, &execution.Rejection{Code: "malformed_conditions", Detail: "标签条件必须是非空 label=value 对"}
		}
	}
	conditions, err := json.Marshal(input.LabelConditions)
	if err != nil {
		return normalized, err
	}
	normalized.conditions = string(conditions)

	normalized.alertSourceKeys = "[]"
	if len(input.AlertSourceKeys) > 0 {
		seen := map[string]bool{}
		keys := make([]string, 0, len(input.AlertSourceKeys))
		for _, key := range input.AlertSourceKeys {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var enabled int
			if err := q.QueryRowContext(ctx, `SELECT enabled FROM alert_sources WHERE source_key=?`, key).Scan(&enabled); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return normalized, &execution.Rejection{Code: "unknown_alert_source", Detail: "告警源 " + key + " 不存在"}
				}
				return normalized, err
			}
			if enabled != 1 {
				return normalized, &execution.Rejection{Code: "unknown_alert_source", Detail: "告警源 " + key + " 未启用"}
			}
		}
		keysJSON, err := json.Marshal(keys)
		if err != nil {
			return normalized, err
		}
		normalized.alertSourceKeys = string(keysJSON)
	}

	if len(input.Outputs) == 0 {
		return normalized, &execution.Rejection{Code: "malformed_outputs", Detail: "outputs 必须至少一个非空字符串字段"}
	}
	for name, value := range input.Outputs {
		if name == "" || value == "" {
			return normalized, &execution.Rejection{Code: "malformed_outputs", Detail: "outputs 必须至少一个非空字符串字段"}
		}
	}
	outputs, err := json.Marshal(input.Outputs)
	if err != nil {
		return normalized, err
	}
	normalized.outputs = string(outputs)

	normalized.priority = input.Priority
	if normalized.priority == 0 {
		normalized.priority = defaultEnrichmentPriority
	}
	if normalized.priority < 1 {
		return normalized, &execution.Rejection{Code: "malformed_priority", Detail: "priority 必须是 >= 1 的叠加序"}
	}
	return normalized, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
