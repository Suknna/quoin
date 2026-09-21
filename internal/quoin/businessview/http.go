// 业务视图 HTTP 面（ADR-0004，HTTP-BVIEW）：列表/详情读与创建/更新写命令。
// 视图是可选组织能力，只做候选收窄；服务端按已认证会话逐请求裁决 Admin 边界。
package businessview

import (
	"context"
	"errors"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

// Handler 由 app 包接线；Authenticate 解析完整会话为 principal。
type Handler struct {
	Views *Service
	// Authenticate 仅 Admin 可管理业务视图（Operator 无管理入口）。
	Authenticate func(ctx context.Context, cookie string) (int64, error)
	ReadSession  func(ctx context.Context, cookie string) (auth.Session, error)
}

type problemError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *problemError) Error() string  { return p.Message }
func (p *problemError) GetStatus() int { return p.status }

func problem(status int, code, message string) *problemError {
	return &problemError{status: status, Code: code, Message: message}
}

func mapError(err error) *problemError {
	var rejection *ConflictError
	if errors.As(err, &rejection) {
		status := http.StatusUnprocessableEntity
		switch rejection.Code {
		case "view_exists", "row_version_conflict", "command_reused":
			status = http.StatusConflict
		case "not_found", "unknown_connection", "unknown_alert_source":
			status = http.StatusNotFound
		}
		return problem(status, rejection.Code, rejection.Detail)
	}
	return problem(http.StatusInternalServerError, "internal", "请求无法完成，请稍后重试")
}

type BusinessView View

type viewListing struct {
	Items []BusinessView `json:"items"`
}

func (h *Handler) admin(ctx context.Context, cookie string) (int64, error) {
	if h.Authenticate == nil {
		return 0, problem(http.StatusServiceUnavailable, "unavailable", "服务尚未就绪")
	}
	principal, err := h.Authenticate(ctx, cookie)
	if err != nil {
		var status interface{ GetStatus() int }
		if errors.Is(err, auth.ErrUnauthenticated) || (errors.As(err, &status) && status.GetStatus() == http.StatusUnauthorized) {
			return 0, problem(http.StatusUnauthorized, "unauthorized", "登录会话无效。")
		}
		if errors.As(err, &status) && status.GetStatus() == http.StatusForbidden {
			return 0, problem(http.StatusForbidden, "forbidden", "该操作需要管理员权限。")
		}
		return 0, problem(http.StatusInternalServerError, "unavailable", "认证服务暂时不可用。")
	}
	return principal, nil
}

func (h *Handler) listViews(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*struct {
	CacheControl string      `header:"Cache-Control"`
	Body         viewListing `json:"body"`
}, error,
) {
	admin := true
	if h.ReadSession != nil {
		session, err := h.ReadSession(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		admin = session.User.Role == "admin"
	} else if _, err := h.admin(ctx, input.Session); err != nil {
		return nil, err
	}
	items, err := h.Views.ListViews(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if !admin {
		for i, item := range items {
			items[i] = View{ViewKey: item.ViewKey, DisplayName: item.DisplayName}
		}
	}
	return &struct {
		CacheControl string      `header:"Cache-Control"`
		Body         viewListing `json:"body"`
	}{CacheControl: "no-store", Body: viewListing{Items: viewProjection(items)}}, nil
}

func (h *Handler) getView(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	ViewKey string `path:"viewKey"`
}) (*struct {
	CacheControl string       `header:"Cache-Control"`
	Body         BusinessView `json:"body"`
}, error,
) {
	if _, err := h.admin(ctx, input.Session); err != nil {
		return nil, err
	}
	view, err := h.Views.GetView(ctx, input.ViewKey)
	if err != nil {
		return nil, mapError(err)
	}
	return &struct {
		CacheControl string       `header:"Cache-Control"`
		Body         BusinessView `json:"body"`
	}{CacheControl: "no-store", Body: BusinessView(view)}, nil
}

// ViewBody 是创建/更新共享的可变字段 wire 形状。视图 key 只由路径携带，
// 请求体不重复（UpdateBusinessViewRequest 不含 viewKey）；类型必须导出：
// Huma 的 schema 生成会跳过未导出的匿名嵌入类型，导致字段从请求 schema
// 中消失并被封闭校验整体拒绝（真实 PUT 422 根因）。
type ViewBody struct {
	ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	DisplayName     string `json:"displayName" minLength:"1" maxLength:"200"`
	Description     string `json:"description" maxLength:"2000"`
	Scope           struct {
		ConnectionName  string            `json:"connectionName,omitempty"`
		LabelConditions map[string]string `json:"labelConditions"`
		AlertSourceKeys []string          `json:"alertSourceKeys,omitempty"`
	} `json:"scope"`
}

func (body ViewBody) input() ViewInput {
	return ViewInput{DisplayName: body.DisplayName, Description: body.Description, ConnectionName: body.Scope.ConnectionName, LabelConditions: body.Scope.LabelConditions, AlertSourceKeys: body.Scope.AlertSourceKeys}
}

func (h *Handler) createView(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ViewBody
		ViewKey string `json:"viewKey" minLength:"1" maxLength:"63" pattern:"^[a-z][a-z0-9-]{0,62}$"`
	}
}) (*struct {
	Status       int          `header:"-"`
	CacheControl string       `header:"Cache-Control"`
	Body         BusinessView `json:"body"`
}, error,
) {
	principal, err := h.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	viewInput := input.Body.input()
	viewInput.ViewKey = input.Body.ViewKey
	view, err := h.Views.CreateView(ctx, principal, input.Body.ClientCommandID, viewInput)
	if err != nil {
		return nil, mapError(err)
	}
	return &struct {
		Status       int          `header:"-"`
		CacheControl string       `header:"Cache-Control"`
		Body         BusinessView `json:"body"`
	}{Status: http.StatusCreated, CacheControl: "no-store", Body: BusinessView(view)}, nil
}

func (h *Handler) updateView(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	ViewKey string `path:"viewKey"`
	Body    struct {
		ViewBody
		ExpectedRowVersion int64 `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	CacheControl string       `header:"Cache-Control"`
	Body         BusinessView `json:"body"`
}, error,
) {
	principal, err := h.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	// 路径是视图 key 的唯一来源；请求体只描述可变内容与并发前提。
	viewInput := input.Body.input()
	viewInput.ViewKey = input.ViewKey
	view, err := h.Views.UpdateView(ctx, principal, input.Body.ClientCommandID, viewInput, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, mapError(err)
	}
	return &struct {
		CacheControl string       `header:"Cache-Control"`
		Body         BusinessView `json:"body"`
	}{CacheControl: "no-store", Body: BusinessView(view)}, nil
}

// Register 挂载业务视图路由（仅 Admin）。
func (h *Handler) Register(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-views", OperationID: "listBusinessViews"}, h.listViews)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-views", OperationID: "createBusinessView"}, h.createView)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-views/{viewKey}", OperationID: "getBusinessView"}, h.getView)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/business-views/{viewKey}", OperationID: "updateBusinessView"}, h.updateView)
}

func viewProjection(items []View) []BusinessView {
	result := make([]BusinessView, 0, len(items))
	for _, item := range items {
		result = append(result, BusinessView(item))
	}
	return result
}
