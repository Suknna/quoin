package app

// Standalone Browser Identity Admin surface (ADR-0004): the plugin-owned
// identities live outside every business view and are addressed by their
// stable identity_key. The operations sub-protocol is byte-identical to the
// retired historical browser-login commands; only the locator differs, so the
// same durable fences and the same real Lintel dispatch serve this surface.
//
// Enablement is the single capability fact browserPluginEnabled() (the
// deployment-resolved browser plugin state from the shared plugin registry).
// Because enablement resolves at boot after the handler graph is built, the
// fact is evaluated per request: with the plugin disabled every write behaves
// as an absent route (404) and only auditable reads remain — the historical
// identities stay read-only without any Lintel dependency.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/browser"
	"github.com/danielgtaylor/huma/v2"
)

// registerBrowserStandaloneRoutes installs the standalone identity surface.
// Read routes stay available for audit; every state-changing command and the
// noVNC attachment re-check the capability fact before touching anything.
func (application *apiServer) registerBrowserStandaloneRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/browser-identities", OperationID: "listStandaloneBrowserIdentities"}, application.listStandaloneBrowserIdentities)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-identities", OperationID: "createStandaloneBrowserIdentity"}, application.createBrowserStandaloneIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/browser-identities/{identityKey}", OperationID: "getStandaloneBrowserIdentity"}, application.getBrowserStandaloneIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/browser-identities/{identityKey}", OperationID: "updateStandaloneBrowserIdentity"}, application.updateBrowserStandaloneIdentity)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-identities/{identityKey}/operations", OperationID: "startStandaloneBrowserLogin"}, application.startBrowserStandaloneLogin)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/browser-identities/{identityKey}/operations/{operationId}", OperationID: "getStandaloneBrowserOperation"}, application.getBrowserStandaloneOperation)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-identities/{identityKey}/operations/{operationId}/cancel", OperationID: "cancelStandaloneBrowserOperation"}, application.cancelBrowserStandaloneOperation)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/browser-identities/{identityKey}/operations/{operationId}/publish", OperationID: "publishStandaloneBrowserOperation"}, application.publishBrowserStandaloneOperation)
}

// browserStandaloneWritesEnabled is the one capability gate every standalone
// write shares. It must never guess: the registry resolves enablement from
// the deployment YAML exactly once per process.
func (application *apiServer) browserStandaloneWritesEnabled() bool {
	return application.browserPluginEnabled()
}

func browserStandaloneDisabled() error {
	return problem(http.StatusNotFound, "not_found", "当前部署未启用受控浏览器插件。")
}

type browserIdentitiesInput struct {
	Session string `cookie:"__Host-quoin-session"`
}

type browserIdentitiesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []browser.Identity `json:"items"`
	} `json:"body"`
}

func (application *apiServer) listStandaloneBrowserIdentities(ctx context.Context, input *browserIdentitiesInput) (*browserIdentitiesOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取浏览器身份列表"); err != nil {
		return nil, err
	}
	items, err := application.browsers.ListStandaloneIdentities(ctx)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	if items == nil {
		items = []browser.Identity{}
	}
	out := &browserIdentitiesOutput{CacheControl: "no-store"}
	out.Body.Items = items
	return out, nil
}

type browserStandaloneIdentityInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	IdentityKey string `path:"identityKey"`
}

type browserStandaloneIdentityOutput struct {
	Status       int              `header:"-"`
	CacheControl string           `header:"Cache-Control"`
	Body         browser.Identity `json:"body"`
}

func (application *apiServer) getBrowserStandaloneIdentity(ctx context.Context, input *browserStandaloneIdentityInput) (*browserStandaloneIdentityOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取浏览器身份"); err != nil {
		return nil, err
	}
	identity, err := application.browsers.GetStandaloneIdentity(ctx, input.IdentityKey)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserStandaloneIdentityOutput{Status: http.StatusOK, CacheControl: "no-store", Body: identity}, nil
}

type createBrowserStandaloneIdentityInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID     string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		Name                string `json:"name" minLength:"1" maxLength:"200"`
		StartURL            string `json:"startUrl" minLength:"1"`
		AuthenticationProbe struct {
			JourneyID string         `json:"journeyId" minLength:"1"`
			Version   int64          `json:"journeyVersion" minimum:"1"`
			Params    map[string]any `json:"params"`
		} `json:"authenticationProbe"`
	}
}

// createBrowserStandaloneIdentity commits the first immutable revision. The
// server derives the stable identityKey from the name; the response carries
// it as the standalone Identity projection's own identityKey field.
func (application *apiServer) createBrowserStandaloneIdentity(ctx context.Context, input *createBrowserStandaloneIdentityInput) (*browserStandaloneIdentityOutput, error) {
	if !application.browserStandaloneWritesEnabled() {
		return nil, browserStandaloneDisabled()
	}
	session, err := application.authenticateAdmin(ctx, input.Session, "创建浏览器身份")
	if err != nil {
		return nil, err
	}
	probe := input.Body.AuthenticationProbe
	identity, err := application.configureBrowserStandalone(ctx, session.User.ID, input.Body.ClientCommandID, nil, input.Body.Name, input.Body.StartURL, probe.JourneyID, probe.Version, probe.Params)
	if err != nil {
		return nil, err
	}
	return &browserStandaloneIdentityOutput{Status: http.StatusCreated, CacheControl: "no-store", Body: identity}, nil
}

type updateBrowserStandaloneIdentityInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	IdentityKey string `path:"identityKey"`
	Body        struct {
		ClientCommandID     string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion  int64  `json:"expectedRowVersion" minimum:"1"`
		Name                string `json:"name" minLength:"1" maxLength:"200"`
		StartURL            string `json:"startUrl" minLength:"1"`
		AuthenticationProbe struct {
			JourneyID string         `json:"journeyId" minLength:"1"`
			Version   int64          `json:"journeyVersion" minimum:"1"`
			Params    map[string]any `json:"params"`
		} `json:"authenticationProbe"`
	}
}

// updateBrowserStandaloneIdentity is the edit-only revision command: it never
// creates, so an unknown identityKey is a clean 404 and the frozen
// expectedRowVersion fences the concurrent-revision race.
func (application *apiServer) updateBrowserStandaloneIdentity(ctx context.Context, input *updateBrowserStandaloneIdentityInput) (*browserStandaloneIdentityOutput, error) {
	if !application.browserStandaloneWritesEnabled() {
		return nil, browserStandaloneDisabled()
	}
	session, err := application.authenticateAdmin(ctx, input.Session, "修订浏览器身份")
	if err != nil {
		return nil, err
	}
	if _, err = application.browsers.GetStandaloneIdentity(ctx, input.IdentityKey); err != nil {
		return nil, browserHTTPError(err)
	}
	probe := input.Body.AuthenticationProbe
	expected := input.Body.ExpectedRowVersion
	identity, err := application.configureBrowserStandalone(ctx, session.User.ID, input.Body.ClientCommandID, &expected, input.Body.Name, input.Body.StartURL, probe.JourneyID, probe.Version, probe.Params)
	if err != nil {
		return nil, err
	}
	return &browserStandaloneIdentityOutput{Status: http.StatusOK, CacheControl: "no-store", Body: identity}, nil
}

// configureBrowserStandalone is the shared create-or-edit bridge to the
// browser authority: it freezes the typed probe params and maps the domain
// error lattice onto the frozen problem envelope.
func (application *apiServer) configureBrowserStandalone(ctx context.Context, actorID int64, clientCommandID string, expectedRowVersion *int64, name, startURL, journeyID string, journeyVersion int64, probeParams map[string]any) (browser.Identity, error) {
	params, err := json.Marshal(probeParams)
	if err != nil {
		return browser.Identity{}, problemUnprocessable("authenticationProbe.params 必须是对象。")
	}
	identity, _, err := application.browsers.ConfigureStandalone(ctx, actorID, browser.StandaloneConfigureInput{
		IdentityKey:        "",
		Name:               name,
		StartURL:           startURL,
		Probe:              browser.ProbeConfig{JourneyID: journeyID, Version: journeyVersion, Params: params},
		ExpectedRowVersion: expectedRowVersion,
		ClientCommandID:    clientCommandID,
	})
	if err != nil {
		return browser.Identity{}, browserHTTPError(err)
	}
	return identity, nil
}

type browserStandaloneOperationInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	IdentityKey string `path:"identityKey"`
	OperationID string `path:"operationId"`
}

type browserStandaloneOperationOutput struct {
	Status int               `header:"-"`
	Body   browser.Operation `json:"body"`
}

// browserStandaloneOperationID parses the operation locator once for every
// operation-scoped command.
func browserStandaloneOperationID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, problemUnprocessable("operationId 无效。")
	}
	return id, nil
}

type startBrowserStandaloneLoginInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	IdentityKey string `path:"identityKey"`
	Body        struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}

func (application *apiServer) startBrowserStandaloneLogin(ctx context.Context, input *startBrowserStandaloneLoginInput) (*browserStandaloneOperationOutput, error) {
	if !application.browserStandaloneWritesEnabled() {
		return nil, browserStandaloneDisabled()
	}
	session, err := application.authenticateAdmin(ctx, input.Session, "启动人工浏览器登录")
	if err != nil {
		return nil, err
	}
	op, err := application.browsers.StartStandaloneManualLogin(ctx, input.IdentityKey, session.User.ID, session.ID, input.Body.ExpectedRowVersion, input.Body.ClientCommandID)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserStandaloneOperationOutput{Status: http.StatusAccepted, Body: op}, nil
}

func (application *apiServer) getBrowserStandaloneOperation(ctx context.Context, input *browserStandaloneOperationInput) (*browserStandaloneOperationOutput, error) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取浏览器操作"); err != nil {
		return nil, err
	}
	id, err := browserStandaloneOperationID(input.OperationID)
	if err != nil {
		return nil, err
	}
	op, err := application.browsers.GetStandaloneOperation(ctx, input.IdentityKey, id)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserStandaloneOperationOutput{Status: http.StatusOK, Body: op}, nil
}

type commandBrowserStandaloneOperationInput struct {
	Session     string `cookie:"__Host-quoin-session"`
	IdentityKey string `path:"identityKey"`
	OperationID string `path:"operationId"`
	Body        struct {
		ClientCommandID             string `json:"clientCommandId" minLength:"8"`
		ExpectedOperationRowVersion int64  `json:"expectedOperationRowVersion" minimum:"1"`
	}
}

func (application *apiServer) cancelBrowserStandaloneOperation(ctx context.Context, input *commandBrowserStandaloneOperationInput) (*browserStandaloneOperationOutput, error) {
	if !application.browserStandaloneWritesEnabled() {
		return nil, browserStandaloneDisabled()
	}
	session, err := application.authenticateAdmin(ctx, input.Session, "取消浏览器登录")
	if err != nil {
		return nil, err
	}
	id, err := browserStandaloneOperationID(input.OperationID)
	if err != nil {
		return nil, err
	}
	op, err := application.browsers.CancelStandalone(ctx, input.IdentityKey, id, session.User.ID, input.Body.ExpectedOperationRowVersion, input.Body.ClientCommandID)
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
	return &browserStandaloneOperationOutput{Status: http.StatusOK, Body: op}, nil
}

// publishBrowserStandaloneOperation accepts the idempotent publish command and
// forwards the typed request to the real Lintel control channel. The
// authoritative profile is returned by the operation GET after the
// asynchronous Runtime result commits it.
func (application *apiServer) publishBrowserStandaloneOperation(ctx context.Context, input *commandBrowserStandaloneOperationInput) (*browserStandaloneOperationOutput, error) {
	if !application.browserStandaloneWritesEnabled() {
		return nil, browserStandaloneDisabled()
	}
	session, err := application.authenticateAdmin(ctx, input.Session, "发布浏览器登录身份")
	if err != nil {
		return nil, err
	}
	id, err := browserStandaloneOperationID(input.OperationID)
	if err != nil {
		return nil, err
	}
	request, err := application.browsers.PrepareStandalonePublish(ctx, input.IdentityKey, id, session.User.ID, input.Body.ExpectedOperationRowVersion, input.Body.ClientCommandID)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	if request.AlreadyPublished {
		op, lookupErr := application.browsers.GetStandaloneOperation(ctx, input.IdentityKey, id)
		if lookupErr != nil {
			return nil, browserHTTPError(lookupErr)
		}
		return &browserStandaloneOperationOutput{Status: http.StatusOK, Body: op}, nil
	}
	if application.browserPublishDispatchFunc == nil {
		return nil, problem(http.StatusServiceUnavailable, "runtime_unavailable", "浏览器运行时暂不可用。")
	}
	if err = application.browserPublishDispatchFunc(ctx, request); err != nil {
		return nil, problem(http.StatusServiceUnavailable, "runtime_unavailable", "浏览器运行时暂不可用。")
	}
	op, err := application.browsers.GetStandaloneOperation(ctx, input.IdentityKey, id)
	if err != nil {
		return nil, browserHTTPError(err)
	}
	return &browserStandaloneOperationOutput{Status: http.StatusOK, Body: op}, nil
}
