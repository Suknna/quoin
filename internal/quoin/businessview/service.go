// Package businessview owns the optional Business View aggregate (ADR-0004):
// a versioned-by-row-version organisation over source integration scope and
// explicit label conditions. A view owns no resource identity, no credentials
// and no additional authority — it only narrows candidates that its consumers
// (currently inspection plans) freeze at their own creation time.
//
// 写命令（创建/更新）通过共享执行器 execution 执行（ADR-0006）：会话复核、
// 幂等重放、业务修改、命令台账与审计事件由执行器在同一事务统一提交。模块
// 不再自管事务、不写 audit INSERT、不持有拒绝记录路径；缺失合法执行上下文
// （actor/session 证明/correlation 元数据）时命令失败关闭，不默认任何身份。
// 读路径走 execution.OpenReadOnly 产生的可信只读面（audit.Reader 查询形状）；
// 组合层未接线时读全部失败关闭，可写连接从不充当读源。
package businessview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// viewKeyPattern 与业务声明稳定 key 共用同一封闭词表；退役不复用。
var viewKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

const (
	// CommandCreate/CommandUpdate 是命令台账的稳定命令类型，同时是审计
	// action；与既有台账行保持一致，旧记录跨迁移仍可重放。
	CommandCreate = "business_view.create"
	CommandUpdate = "business_view.update"
	// ObjectBusinessView 是审计与命令结果的领域对象类型。
	ObjectBusinessView = "business_view"
)

// ConflictError 是模块对外的确定性拒绝形状（HTTP 问题映射依赖其 Code）。
// 执行器把业务阶段返回的 *execution.Rejection 翻译回此类型，保持既有调用
// 方的错误输出不变。
type ConflictError struct {
	Code, Detail string
}

func (e *ConflictError) Error() string { return e.Detail }

type Service struct {
	// reader 是执行器验证后的可信只读面（execution.Reader）：组合层未接线时
	// 保持零值，全部读取失败关闭——可写连接从不充当读源。
	reader execution.Reader
	// runner 是组合层共享的执行器；模块把自有操作注册进共享注册表，
	// 避免业务私有注册表与全局操作覆盖检查脱节。
	runner *execution.Runner
	create *execution.Operation
	update *execution.Operation
	now    func() time.Time
}

// NewService 是组合层初始装配（应用构造在 configureReadOnly 之前）：操作
// 注册进私有执行器，读路径保持零值可信只读面——所有读取失败关闭，直到
// 组合层通过 NewServiceWithReader 或 SetReader 注入真实只读能力。db 绝不
// 充当读源。
func NewService(db *sql.DB) *Service {
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := newService(runner.Reader(), runner)
	if err != nil {
		// 新注册表上的首次声明不会失败；此分支只为编程错误兜底。
		panic("businessview: compose default service: " + err.Error())
	}
	return service
}

// NewServiceWithReader 装配模块：reader 经共享执行器的 SetReader 原地验证
// ——仅接受 execution.OpenReadOnly 产生的可信类型，随后采纳执行器的
// runner.Reader()；任意 audit.Reader 适配器（包括裸可写 db）连同 nil 一起
// 被拒绝。runner 是组合层共享的执行器（操作注册进其注册表，同名重复注册
// 即失败）。
func NewServiceWithReader(reader audit.Reader, runner *execution.Runner) (*Service, error) {
	if runner == nil {
		return nil, errors.New("businessview: command runner is required")
	}
	if err := runner.SetReader(reader); err != nil {
		return nil, fmt.Errorf("businessview: %w", err)
	}
	return newService(runner.Reader(), runner)
}

// SetReader 把真实只读能力转发给共享执行器验证，并采纳验证后的读面：默认
// 构造的服务由此从失败关闭转为可读。仅接受 execution.OpenReadOnly 产生的
// 可信类型，不存在可写兼容适配器。
func (s *Service) SetReader(reader audit.Reader) error {
	if err := s.runner.SetReader(reader); err != nil {
		return fmt.Errorf("businessview: %w", err)
	}
	s.reader = s.runner.Reader()
	return nil
}

// newService 是两个公开构造共享的私有装配：把模块自有操作注册进 runner 的
// 共享注册表并完成字段装配，reader 由调用方先行验证为可信只读面。注册失败
// 只能是声明冲突的编程错误，在装配时立即暴露。
func newService(reader execution.Reader, runner *execution.Runner) (*Service, error) {
	if runner == nil {
		return nil, errors.New("businessview: command runner is required")
	}
	create, err := runner.Register(execution.Operation{
		Name:       CommandCreate,
		Class:      execution.ClassWrite,
		ObjectType: ObjectBusinessView,
		Authorize:  authorizeViewManager,
	})
	if err != nil {
		return nil, fmt.Errorf("businessview: register %s: %w", CommandCreate, err)
	}
	update, err := runner.Register(execution.Operation{
		Name:       CommandUpdate,
		Class:      execution.ClassWrite,
		ObjectType: ObjectBusinessView,
		Authorize:  authorizeViewManager,
	})
	if err != nil {
		return nil, fmt.Errorf("businessview: register %s: %w", CommandUpdate, err)
	}
	return &Service{reader: reader, runner: runner, create: create, update: update, now: time.Now}, nil
}

func (s *Service) nowText() string { return s.now().UTC().Format(time.RFC3339Nano) }

// authorizeViewManager 在执行器事务内复核会话证明引用（auth.
// VerifyExecutionSession，要求 admin 角色）：会话存在、未撤销、未过期、
// 仍处签发时的 auth_revision，且主体是启用的管理员。证明缺失或已失效返回
// ErrActorChanged——干净回滚、不持久化任何记录；绝不退化为仅查 users 行，
// 那会漏掉准入之后会话被撤销或口令轮换的竞态。
func authorizeViewManager(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "admin")
}

// View 是视图的可读投影；scope.connectionName 为空表示跨来源候选集合。
// 计划消费视图时仍必须固定自己的接入（固定相交，绝不全源查询）。
type View struct {
	ViewKey      string    `json:"viewKey"`
	DisplayName  string    `json:"displayName"`
	Description  string    `json:"description"`
	Scope        ViewScope `json:"scope"`
	RowVersion   int64     `json:"rowVersion"`
	CreatedAt    string    `json:"createdAt"`
	UpdatedAt    string    `json:"updatedAt"`
	viewID       int64
	conditionsDB string
}

// ViewScope 的 wire 形状：connectionName 可空；labelConditions 是精确
// label=value 条件集合。
type ViewScope struct {
	ConnectionName  string            `json:"connectionName,omitempty"`
	LabelConditions map[string]string `json:"labelConditions,omitempty"`
}

// ViewInput 是创建/更新命令载荷（wire 命名）。
type ViewInput struct {
	ViewKey         string
	DisplayName     string
	Description     string
	ConnectionName  string
	LabelConditions map[string]string
}

// CreateView 在执行器的一个事务中校验并创建视图；台账与成功/拒绝审计由
// 执行器自动持久化。
func (s *Service) CreateView(ctx context.Context, principalID int64, clientCommandID string, input ViewInput) (View, error) {
	digest := auth.DigestCommand(CommandCreate, map[string]any{"viewKey": input.ViewKey, "displayName": input.DisplayName, "connectionName": input.ConnectionName})
	outcome, err := execution.Run(ctx, s.runner, s.create, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (View, execution.Change, error) {
		normalized, err := validateViewInput(ctx, tx, input)
		if err != nil {
			return View{}, execution.Changed, err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_views WHERE view_key=?`, input.ViewKey).Scan(&exists); err != nil {
			return View{}, execution.Changed, err
		}
		if exists != 0 {
			return View{}, execution.Changed, &execution.Rejection{Code: "view_exists", Detail: "同名业务视图已存在，key 退役后不可复用"}
		}
		now := s.nowText()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at)
			VALUES(?,?,?,?,?,1,?,?,?)`,
			input.ViewKey, input.DisplayName, input.Description, nullableInt64(normalized.connectionID), normalized.conditions, principalID, now, now); err != nil {
			return View{}, execution.Changed, err
		}
		view, err := viewOn(ctx, tx, input.ViewKey)
		if err != nil {
			return View{}, execution.Changed, err
		}
		return view, execution.Changed, nil
	}, func(view View) int64 { return view.viewID })
	if err != nil {
		return View{}, translateRunnerError(err)
	}
	return outcome.Result, nil
}

// UpdateView 以整体提交方式更新视图；expectedRowVersion 是并发前提。更新不
// 改写任何已冻结该视图内容的计划或 Run。
func (s *Service) UpdateView(ctx context.Context, principalID int64, clientCommandID string, input ViewInput, expectedRowVersion int64) (View, error) {
	digest := auth.DigestCommand(CommandUpdate, map[string]any{"viewKey": input.ViewKey, "expectedRowVersion": expectedRowVersion, "displayName": input.DisplayName})
	outcome, err := execution.Run(ctx, s.runner, s.update, execution.Command{
		PrincipalType:   string(execution.PrincipalUser),
		PrincipalID:     principalID,
		ClientCommandID: clientCommandID,
		Digest:          digest,
	}, func(tx *execution.Tx) (View, execution.Change, error) {
		normalized, err := validateViewInput(ctx, tx, input)
		if err != nil {
			return View{}, execution.Changed, err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE business_views SET display_name=?,description=?,connection_id=?,label_conditions_json=?,
			  row_version=row_version+1,updated_at=?
			WHERE view_key=? AND row_version=?`,
			input.DisplayName, input.Description, nullableInt64(normalized.connectionID), normalized.conditions, s.nowText(),
			input.ViewKey, expectedRowVersion)
		if err != nil {
			return View{}, execution.Changed, err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return View{}, execution.Changed, &execution.Rejection{Code: "row_version_conflict", Detail: "业务视图已变化，请刷新后重试"}
		}
		view, err := viewOn(ctx, tx, input.ViewKey)
		if err != nil {
			return View{}, execution.Changed, err
		}
		return view, execution.Changed, nil
	}, func(view View) int64 { return view.viewID })
	if err != nil {
		return View{}, translateRunnerError(err)
	}
	return outcome.Result, nil
}

// ListViews 返回全部视图，按 key 稳定序。
func (s *Service) ListViews(ctx context.Context) ([]View, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT view_key FROM business_views ORDER BY view_key`)
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
	views := []View{}
	for _, key := range keys {
		view, err := viewOn(ctx, s.reader, key)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// GetView 返回一个视图。
func (s *Service) GetView(ctx context.Context, viewKey string) (View, error) {
	return viewOn(ctx, s.reader, viewKey)
}

// viewOn 读取单个视图投影；查询面是 audit.Reader（*execution.Tx 亦满足），
// 同一实现既可跑在执行器事务内，也可跑在只读读源上。
func viewOn(ctx context.Context, q audit.Reader, viewKey string) (View, error) {
	var view View
	var connectionID sql.NullInt64
	var connectionName sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT v.id,v.view_key,v.display_name,v.description,v.connection_id,c.name,
		       v.label_conditions_json,v.row_version,v.created_at,v.updated_at
		FROM business_views v LEFT JOIN connections c ON c.id=v.connection_id
		WHERE v.view_key=?`, viewKey).
		Scan(&view.viewID, &view.ViewKey, &view.DisplayName, &view.Description, &connectionID, &connectionName,
			&view.conditionsDB, &view.RowVersion, &view.CreatedAt, &view.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, &ConflictError{Code: "not_found", Detail: "业务视图不存在"}
	}
	if err != nil {
		return View{}, err
	}
	view.Scope.ConnectionName = connectionName.String
	if err := json.Unmarshal([]byte(view.conditionsDB), &view.Scope.LabelConditions); err != nil {
		return View{}, err
	}
	if view.Scope.LabelConditions == nil {
		view.Scope.LabelConditions = map[string]string{}
	}
	return view, nil
}

type normalizedInput struct {
	connectionID int64
	conditions   string
}

// validateViewInput 静态校验视图定义并在同一事务内解析接入引用。确定性校验
// 失败以 *execution.Rejection 返回，由执行器在干净事务中持久记录拒绝事实。
func validateViewInput(ctx context.Context, q audit.Reader, input ViewInput) (normalizedInput, error) {
	var normalized normalizedInput
	if !viewKeyPattern.MatchString(input.ViewKey) {
		return normalized, &execution.Rejection{Code: "malformed_key", Detail: "视图 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	if input.DisplayName == "" {
		return normalized, &execution.Rejection{Code: "malformed_view", Detail: "视图显示名不能为空"}
	}
	if input.LabelConditions == nil {
		input.LabelConditions = map[string]string{}
	}
	for name, value := range input.LabelConditions {
		if name == "" || value == "" {
			return normalized, &execution.Rejection{Code: "malformed_scope", Detail: "标签条件必须是非空 label=value 对"}
		}
	}
	conditions, err := json.Marshal(input.LabelConditions)
	if err != nil {
		return normalized, err
	}
	normalized.conditions = string(conditions)
	if input.ConnectionName != "" {
		if err := q.QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, input.ConnectionName).Scan(&normalized.connectionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return normalized, &execution.Rejection{Code: "unknown_connection", Detail: "来源接入不存在"}
			}
			return normalized, err
		}
	}
	return normalized, nil
}

// translateRunnerError 把执行器的确定性结果映射回模块对外的错误形状，保持
// HTTP 问题映射与既有调用方的错误输出不变。其余错误（缺执行上下文、会话
// 复核失败、基础设施故障）原样上抛——它们没有持久化的权威结果可引用。
func translateRunnerError(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		return &ConflictError{Code: rejection.Code, Detail: rejection.Detail}
	}
	if errors.Is(err, execution.ErrCommandReused) {
		return &ConflictError{Code: "command_reused", Detail: "命令标识已用于其它请求，请更换后重试"}
	}
	return err
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
