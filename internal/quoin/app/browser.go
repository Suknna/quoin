package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/danielgtaylor/huma/v2"
)

// registerBrowserRoutes owns the human-triggered Browser Identity slice. The
// runtime remains responsible for processes: accepting a command only commits
// its durable operation; reconnect dispatch is deliberately not an HTTP concern.
func (application *apiServer) registerBrowserRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-systems/{systemKey}/browser-identity", OperationID: "getBrowserIdentity"}, application.getBrowserIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/business-systems/{systemKey}/browser-identity", OperationID: "configureBrowserIdentity"}, application.configureBrowserIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-login/{systemKey}/operations", OperationID: "startBrowserManualLogin"}, application.startBrowserManualLogin)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/browser-login/{systemKey}/operations/{browserOperationId}", OperationID: "getBrowserLoginOperation"}, application.getBrowserLoginOperation)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-login/{systemKey}/operations/{browserOperationId}/cancel", OperationID: "cancelBrowserLoginOperation"}, application.cancelBrowserLoginOperation)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-login/{systemKey}/operations/{browserOperationId}/publish", OperationID: "publishBrowserProfileGeneration"}, application.publishBrowserProfile)
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
	if _, err := application.authenticateFull(ctx, input.Session, "读取浏览器身份"); err != nil {
		return nil, err
	}
	identity, err := application.browsers.GetIdentity(ctx, input.SystemKey)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserIdentityOutput{CacheControl: "no-store", Body: identity}, nil
}

type configureBrowserIdentityInput struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Body      struct {
		ClientCommandID     string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion  *int64 `json:"expectedRowVersion,omitempty" minimum:"1"`
		Name                string `json:"name" minLength:"1" maxLength:"200"`
		StartURL            string `json:"startUrl" minLength:"1"`
		AuthenticationProbe struct {
			JourneyID string         `json:"journeyId" minLength:"1"`
			Version   int64          `json:"journeyVersion" minimum:"1"`
			Params    map[string]any `json:"params"`
		} `json:"authenticationProbe"`
	}
}
type configureBrowserIdentityOutput struct {
	Status int `header:"-"`
	Body   struct {
		Identity       browser.Identity   `json:"identity"`
		ProbeOperation *browser.Operation `json:"probeOperation"`
	} `json:"body"`
}

func (application *apiServer) configureBrowserIdentity(ctx context.Context, input *configureBrowserIdentityInput) (*configureBrowserIdentityOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "配置浏览器身份")
	if err != nil {
		return nil, err
	}
	params, err := json.Marshal(input.Body.AuthenticationProbe.Params)
	if err != nil {
		return nil, problemUnprocessable("authenticationProbe.params 必须是对象。")
	}
	identity, operation, err := application.browsers.Configure(ctx, session.User.ID, browser.ConfigureInput{
		SystemKey: input.SystemKey, Name: input.Body.Name, StartURL: input.Body.StartURL,
		Probe:              browser.ProbeConfig{JourneyID: input.Body.AuthenticationProbe.JourneyID, Version: input.Body.AuthenticationProbe.Version, Params: params},
		ExpectedRowVersion: input.Body.ExpectedRowVersion, ClientCommandID: input.Body.ClientCommandID,
	})
	if err != nil {
		return nil, browserHTTPError(err)
	}
	out := &configureBrowserIdentityOutput{Status: http.StatusAccepted}
	out.Body.Identity, out.Body.ProbeOperation = identity, operation
	return out, nil
}

type startBrowserLoginInput struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Body      struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}
type browserOperationOutput struct {
	Status int               `header:"-"`
	Body   browser.Operation `json:"body"`
}

func (application *apiServer) startBrowserManualLogin(ctx context.Context, input *startBrowserLoginInput) (*browserOperationOutput, error) {
	session, err := application.authenticateFull(ctx, input.Session, "启动人工浏览器登录")
	if err != nil {
		return nil, err
	}
	op, err := application.browsers.StartManualLogin(ctx, input.SystemKey, session.User.ID, session.ID, input.Body.ExpectedRowVersion, input.Body.ClientCommandID)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserOperationOutput{Status: http.StatusAccepted, Body: op}, nil
}

type browserOperationInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	SystemKey   string `path:"systemKey"`
	OperationID string `path:"browserOperationId"`
}

func (application *apiServer) getBrowserLoginOperation(ctx context.Context, input *browserOperationInput) (*browserOperationOutput, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取人工浏览器登录"); err != nil {
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

type cancelBrowserOperationInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	SystemKey   string `path:"systemKey"`
	OperationID string `path:"browserOperationId"`
	Body        struct {
		ClientCommandID             string `json:"clientCommandId" minLength:"8"`
		ExpectedOperationRowVersion int64  `json:"expectedOperationRowVersion" minimum:"1"`
	}
}

func (application *apiServer) cancelBrowserLoginOperation(ctx context.Context, input *cancelBrowserOperationInput) (*browserOperationOutput, error) {
	session, err := application.authenticateFull(ctx, input.Session, "取消人工浏览器登录")
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(input.OperationID, 10, 64)
	if err != nil || id < 1 {
		return nil, problemUnprocessable("browserOperationId 无效。")
	}
	op, err := application.browsers.Cancel(ctx, input.SystemKey, id, session.User.ID, input.Body.ExpectedOperationRowVersion, input.Body.ClientCommandID)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	// Closing the in-memory relay is immediate; physical Stop may retry after a
	// transient Runtime outage, but a cancelled operator must not retain RFB I/O.
	application.browserTunnels.closeOperation(id)
	// A pre-dispatch cancellation is already physically fenced; otherwise the
	// terminal record is durable and Stop is retried on later reconciliation.
	if op.StopConfirmedAt == nil && application.browserStopDispatchFunc != nil {
		_ = application.browserStopDispatchFunc(ctx, id)
	}
	return &browserOperationOutput{Status: http.StatusOK, Body: op}, nil
}

type publishBrowserProfileInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	SystemKey   string `path:"systemKey"`
	OperationID string `path:"browserOperationId"`
	Body        struct {
		ClientCommandID             string `json:"clientCommandId" minLength:"8"`
		ExpectedOperationRowVersion int64  `json:"expectedOperationRowVersion" minimum:"1"`
	}
}

// publishBrowserProfile accepts the idempotent publish command and forwards the
// typed request to Lintel. The authoritative profile is returned by the
// operation GET after the asynchronous Runtime result commits it.
func (application *apiServer) publishBrowserProfile(ctx context.Context, input *publishBrowserProfileInput) (*browserOperationOutput, error) {
	session, err := application.authenticateFull(ctx, input.Session, "发布浏览器登录身份")
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(input.OperationID, 10, 64)
	if err != nil || id < 1 {
		return nil, problemUnprocessable("browserOperationId 无效。")
	}
	request, err := application.browsers.PreparePublish(ctx, input.SystemKey, id, session.User.ID, input.Body.ExpectedOperationRowVersion, input.Body.ClientCommandID)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	if request.AlreadyPublished {
		op, lookupErr := application.browsers.GetOperation(ctx, input.SystemKey, id)
		if lookupErr != nil {
			return nil, browserHTTPError(lookupErr)
		}
		return &browserOperationOutput{Status: http.StatusOK, Body: op}, nil
	}
	if application.browserPublishDispatchFunc == nil {
		return nil, problem(http.StatusServiceUnavailable, "runtime_unavailable", "浏览器运行时暂不可用。")
	}
	if err = application.browserPublishDispatchFunc(ctx, request); err != nil {
		return nil, problem(http.StatusServiceUnavailable, "runtime_unavailable", "浏览器运行时暂不可用。")
	}
	op, err := application.browsers.GetOperation(ctx, input.SystemKey, id)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserOperationOutput{Status: http.StatusAccepted, Body: op}, nil
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
