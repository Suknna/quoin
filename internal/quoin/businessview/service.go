// Package businessview owns the optional Business View aggregate (ADR-0004):
// a versioned-by-row-version organisation over source integration scope and
// explicit label conditions. A view owns no resource identity, no credentials
// and no additional authority — it only narrows candidates that its consumers
// (currently inspection plans) freeze at their own creation time.
package businessview

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// viewKeyPattern 与业务声明稳定 key 共用同一封闭词表；退役不复用。
var viewKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ConflictError 是命令台账记录的确定性拒绝。
type ConflictError struct {
	Code, Detail string
}

func (e *ConflictError) Error() string { return e.Detail }

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

func (s *Service) nowText() string { return s.now().UTC().Format(time.RFC3339Nano) }

// View 是视图的可读投影；scope.connectionName 为空表示跨来源候选集合。
// 计划消费视图时仍必须固定自己的接入（固定相交，绝不全源查询）。
type View struct {
	ViewKey      string         `json:"viewKey"`
	DisplayName  string         `json:"displayName"`
	Description  string         `json:"description"`
	Scope        ViewScope      `json:"scope"`
	RowVersion   int64          `json:"rowVersion"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
	viewID       int64
	conditionsDB string
}

// ViewScope 的 wire 形状：connectionName 可空；labelConditions 是精确
// label=value 条件集合。
type ViewScope struct {
	ConnectionName  string           `json:"connectionName,omitempty"`
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

// CreateView 在一个事务中校验并创建视图。
func (s *Service) CreateView(ctx context.Context, principalID int64, clientCommandID string, input ViewInput) (View, error) {
	const command = "business_view.create"
	normalized, err := s.validateInput(ctx, input)
	if err != nil {
		return View{}, err
	}
	digest := auth.DigestCommand(command, map[string]any{"viewKey": input.ViewKey, "displayName": input.DisplayName, "connectionName": input.ConnectionName})
	if record, replayed, err := replayView(ctx, s.db, principalID, clientCommandID, digest); replayed || err != nil {
		return decodeViewReplay(record, replayed, err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return View{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return View{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return View{}, err
	}
	if found {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return View{}, err
		}
		committed = true
		return decodeViewReplay(record, true, nil)
	}
	var exists int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM business_views WHERE view_key=?`, input.ViewKey).Scan(&exists); err != nil {
		return View{}, err
	}
	if exists != 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &ConflictError{Code: "view_exists", Detail: "同名业务视图已存在，key 退役后不可复用"}, &committed)
	}
	now := s.nowText()
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO business_views(view_key,display_name,description,connection_id,label_conditions_json,row_version,created_by,created_at,updated_at)
		VALUES(?,?,?,?,?,1,?,?,?)`,
		input.ViewKey, input.DisplayName, input.Description, nullableInt64(normalized.connectionID), normalized.conditions, principalID, now, now); err != nil {
		return View{}, err
	}
	view, err := s.viewOn(ctx, conn, input.ViewKey)
	if err != nil {
		return View{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, view.viewID, now); err != nil {
		return View{}, err
	}
	if err = recordViewCommand(ctx, conn, principalID, clientCommandID, command, digest, view.viewID, view); err != nil {
		return View{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return View{}, err
	}
	committed = true
	return view, nil
}

// UpdateView 以整体提交方式更新视图；expectedRowVersion 是并发前提。更新不
// 改写任何已冻结该视图内容的计划或 Run。
func (s *Service) UpdateView(ctx context.Context, principalID int64, clientCommandID string, input ViewInput, expectedRowVersion int64) (View, error) {
	const command = "business_view.update"
	normalized, err := s.validateInput(ctx, input)
	if err != nil {
		return View{}, err
	}
	digest := auth.DigestCommand(command, map[string]any{"viewKey": input.ViewKey, "expectedRowVersion": expectedRowVersion, "displayName": input.DisplayName})
	if record, replayed, err := replayView(ctx, s.db, principalID, clientCommandID, digest); replayed || err != nil {
		return decodeViewReplay(record, replayed, err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return View{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return View{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	record, found, err := auth.LookupCommandOn(ctx, conn, principalID, clientCommandID)
	if err != nil {
		return View{}, err
	}
	if found {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return View{}, err
		}
		committed = true
		return decodeViewReplay(record, true, nil)
	}
	result, err := conn.ExecContext(ctx, `
		UPDATE business_views SET display_name=?,description=?,connection_id=?,label_conditions_json=?,
		  row_version=row_version+1,updated_at=?
		WHERE view_key=? AND row_version=?`,
		input.DisplayName, input.Description, nullableInt64(normalized.connectionID), normalized.conditions, s.nowText(),
		input.ViewKey, expectedRowVersion)
	if err != nil {
		return View{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return s.reject(ctx, conn, principalID, clientCommandID, command, digest, &ConflictError{Code: "row_version_conflict", Detail: "业务视图已变化，请刷新后重试"}, &committed)
	}
	view, err := s.viewOn(ctx, conn, input.ViewKey)
	if err != nil {
		return View{}, err
	}
	if err = s.audit(ctx, conn, principalID, clientCommandID, command, view.viewID, s.nowText()); err != nil {
		return View{}, err
	}
	if err = recordViewCommand(ctx, conn, principalID, clientCommandID, command, digest, view.viewID, view); err != nil {
		return View{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return View{}, err
	}
	committed = true
	return view, nil
}

// ListViews 返回全部视图，按 key 稳定序。
func (s *Service) ListViews(ctx context.Context) ([]View, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, `SELECT view_key FROM business_views ORDER BY view_key`)
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
	views := []View{}
	for _, key := range keys {
		view, err := s.viewOn(ctx, conn, key)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// GetView 返回一个视图。
func (s *Service) GetView(ctx context.Context, viewKey string) (View, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return View{}, err
	}
	defer conn.Close()
	return s.viewOn(ctx, conn, viewKey)
}

func (s *Service) viewOn(ctx context.Context, conn *sql.Conn, viewKey string) (View, error) {
	var view View
	var connectionID sql.NullInt64
	var connectionName sql.NullString
	err := conn.QueryRowContext(ctx, `
		SELECT v.id,v.view_key,v.display_name,v.description,v.connection_id,c.name,
		       v.label_conditions_json,v.row_version,v.created_at,v.updated_at
		FROM business_views v LEFT JOIN connections c ON c.id=v.connection_id
		WHERE v.view_key=?`, viewKey).
		Scan(&view.viewID, &view.ViewKey, &view.DisplayName, &view.Description, &connectionID, &connectionName,
			&view.conditionsDB, &view.RowVersion, &view.CreatedAt, &view.UpdatedAt)
	if err == sql.ErrNoRows {
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

// validateInput 静态校验视图定义并解析接入引用。
func (s *Service) validateInput(ctx context.Context, input ViewInput) (normalizedInput, error) {
	var normalized normalizedInput
	if !viewKeyPattern.MatchString(input.ViewKey) {
		return normalized, &ConflictError{Code: "malformed_key", Detail: "视图 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$"}
	}
	if input.DisplayName == "" {
		return normalized, &ConflictError{Code: "malformed_view", Detail: "视图显示名不能为空"}
	}
	if input.LabelConditions == nil {
		input.LabelConditions = map[string]string{}
	}
	for name, value := range input.LabelConditions {
		if name == "" || value == "" {
			return normalized, &ConflictError{Code: "malformed_scope", Detail: "标签条件必须是非空 label=value 对"}
		}
	}
	conditions, err := json.Marshal(input.LabelConditions)
	if err != nil {
		return normalized, err
	}
	normalized.conditions = string(conditions)
	if input.ConnectionName != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM connections WHERE name=?`, input.ConnectionName).Scan(&normalized.connectionID); err != nil {
			if err == sql.ErrNoRows {
				return normalized, &ConflictError{Code: "unknown_connection", Detail: "来源接入不存在"}
			}
			return normalized, err
		}
	}
	return normalized, nil
}

func (s *Service) audit(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command string, viewID int64, now string) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,?,?,'success','business_view',?,?)`, principalID, command, clientCommandID, viewID, now)
	return err
}

func recordViewCommand(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, viewID int64, view View) error {
	payload, err := json.Marshal(view)
	if err != nil {
		return err
	}
	return auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeCommitted, "business_view", viewID, string(payload))
}

func replayView(ctx context.Context, db *sql.DB, principalID int64, clientCommandID, digest string) (auth.CommandRecord, bool, error) {
	record, found, err := auth.LookupCommand(ctx, db, principalID, clientCommandID)
	if err != nil || !found {
		return record, false, err
	}
	if record.RequestDigest != digest {
		return record, false, &ConflictError{Code: "command_reused", Detail: "命令标识已用于其它请求，请更换后重试"}
	}
	return record, true, nil
}

func decodeViewReplay(record auth.CommandRecord, replayed bool, err error) (View, error) {
	if err != nil {
		return View{}, err
	}
	if !replayed {
		return View{}, nil
	}
	if record.Outcome == auth.OutcomeRejectedKnown {
		var rejection ConflictError
		if err := json.Unmarshal([]byte(record.ResultPayload), &rejection); err != nil {
			return View{}, err
		}
		return View{}, &rejection
	}
	var view View
	if err := json.Unmarshal([]byte(record.ResultPayload), &view); err != nil {
		return View{}, err
	}
	return view, nil
}

func (s *Service) reject(ctx context.Context, conn *sql.Conn, principalID int64, clientCommandID, command, digest string, rejection *ConflictError, committed *bool) (View, error) {
	payload, _ := json.Marshal(rejection)
	if err := auth.RecordCommand(ctx, conn, principalID, clientCommandID, command, digest, auth.OutcomeRejectedKnown, "business_view", 0, string(payload)); err != nil {
		return View{}, err
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,client_command_id,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES('user',?,?,?,'rejected','business_view',NULL,?)`, principalID, command, clientCommandID, s.nowText()); err != nil {
		return View{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return View{}, err
	}
	*committed = true
	return View{}, rejection
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
