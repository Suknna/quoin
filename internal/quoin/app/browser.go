package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/danielgtaylor/huma/v2"
)

// registerBrowserRoutes owns the retained human-readable Browser Identity
// reads. ADR-0004 moved identity authoring to the standalone plugin surface
// (registerBrowserStandaloneRoutes): a historical business-bound identity is
// read-only here, and its former configure/login/cancel/publish commands were
// retired without a compatibility layer.
func (application *apiServer) registerBrowserRoutes(api huma.API) {
	application.registerBrowserStandaloneRoutes(api)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/browser-identity", OperationID: "getBrowserIdentity"}, application.getBrowserIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/browser-login/{systemKey}/operations/{browserOperationId}", OperationID: "getBrowserLoginOperation"}, application.getBrowserLoginOperation)
}

type browserIdentityInput struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
}
type browserIdentityOutput struct {
	CacheControl string           `header:"Cache-Control"`
	Body         browser.Identity `json:"body"`
}

func (application *apiServer) getBrowserIdentity(ctx context.Context, input *browserIdentityInput) (*browserIdentityOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取浏览器身份"); err != nil {
		return nil, err
	}
	identity, err := application.browsers.GetIdentity(ctx, input.SystemKey)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserIdentityOutput{CacheControl: "no-store", Body: identity}, nil
}

type browserOperationOutput struct {
	Status int               `header:"-"`
	Body   browser.Operation `json:"body"`
}

type browserOperationInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	SystemKey   string `path:"systemKey"`
	OperationID string `path:"browserOperationId"`
}

func (application *apiServer) getBrowserLoginOperation(ctx context.Context, input *browserOperationInput) (*browserOperationOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取人工浏览器登录"); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(input.OperationID, 10, 64)
	if err != nil || id < 1 {
		return nil, problemUnprocessable("browserOperationId 无效。")
	}
	op, err := application.browsers.GetOperation(ctx, input.SystemKey, id)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserOperationOutput{Status: http.StatusOK, Body: op}, nil
}

func browserHTTPError(err error) error {
	var rowVersion *browser.RowVersionError
	switch {
	case errors.Is(err, browser.ErrNotFound):
		return problem(http.StatusNotFound, "not_found", "浏览器身份或操作不存在。")
	case errors.As(err, &rowVersion):
		p := problem(http.StatusConflict, "row_version_conflict", "浏览器对象刚被其他操作修改，请刷新后重试。")
		p.Conflict = map[string]any{"code": "row_version_conflict", "rowVersion": rowVersion.Current}
		return p
	case errors.Is(err, browser.ErrSessionRevoked):
		return huma.Error401Unauthorized("登录状态已失效，请重新登录。")
	case errors.Is(err, browser.ErrConflict):
		return problem(http.StatusConflict, "identity_busy", "该浏览器身份仍有未停止的操作。")
	case errors.Is(err, browser.ErrInvalid):
		return problemUnprocessable("浏览器身份配置无效。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法处理浏览器操作。")
	}
}
