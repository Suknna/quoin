package app

// Single-step local login and the public auth configuration endpoint
// (ADR-0010 / docs/authentication-design.md §4). The login page renders from
// /api/v1/auth/config, which the provider registry projects; the local
// channel verifies a password once and issues the full session directly.

import (
	"context"
	"errors"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

type loginInput struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Username string `json:"username" maxLength:"200"`
		Password string `json:"password" minLength:"1" maxLength:"512"`
	}
}

type loginOutput struct {
	SetCookie    string `header:"Set-Cookie"`
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Completed bool       `json:"completed"`
		User      *auth.User `json:"user"`
	}
}

func authenticationError(err error) error {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return problem(http.StatusUnauthorized, "unauthenticated", "用户名或密码不正确。")
	case errors.Is(err, auth.ErrLocalLoginUnavailable):
		return problem(http.StatusUnprocessableEntity, "local_login_unavailable", "该账号经统一身份平台登录，请返回使用统一身份入口。")
	case errors.Is(err, auth.ErrInitialPasswordExpired):
		return problem(http.StatusForbidden, "initial_password_expired", "初始管理员密码已过期，请运维执行 quoin admin recover 重新生成。")
	case errors.Is(err, auth.ErrRateLimited):
		return problem(http.StatusTooManyRequests, "rate_limited", "尝试过于频繁，请稍后再试。")
	case errors.Is(err, auth.ErrPasswordPolicy):
		return problemUnprocessable("新密码不满足密码策略。")
	case errors.Is(err, auth.ErrValidation):
		return problemUnprocessable("认证输入无效，请检查后重试。")
	default:
		return problem(http.StatusServiceUnavailable, "unavailable", "暂时无法完成认证，请稍后重试。")
	}
}

func (application *apiServer) registerAuthenticationFlows(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "loginWithPassword"}, application.loginWithPassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/activity", OperationID: "touchSessionActivity", DefaultStatus: http.StatusNoContent}, application.touchSessionActivity)
}

func (application *apiServer) loginWithPassword(ctx context.Context, input *loginInput) (*loginOutput, error) {
	result, err := application.auth.LoginWithPassword(ctx, input.Body.Username, input.Body.Password, input.UserAgent)
	if err != nil {
		return nil, authenticationError(err)
	}
	output := &loginOutput{SetCookie: sessionCookie(result.Bearer, 0), CacheControl: "no-store"}
	output.Body.Completed = true
	output.Body.User = &result.User
	return output, nil
}

func (application *apiServer) touchSessionActivity(ctx context.Context, input *authInput) (*noContentOutput, error) {
	if err := application.auth.TouchSessionActivity(ctx, input.Session); err != nil {
		return nil, authenticationError(err)
	}
	return &noContentOutput{CacheControl: "no-store"}, nil
}

// authConfigOutput is the public login-channel projection. It deliberately
// carries no secrets: the client_secret and the secrets file path never
// appear here (ADR-0010 red line).
type authConfigOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Local struct {
			Enabled bool `json:"enabled"`
			Visible bool `json:"visible"`
		} `json:"local"`
		OIDC struct {
			Enabled bool   `json:"enabled"`
			Label   string `json:"label,omitempty"`
			IconURL string `json:"iconUrl,omitempty"`
		} `json:"oidc"`
	}
}

func (application *apiServer) registerAuthConfigRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/config", OperationID: "readAuthConfig"}, application.readAuthConfig)
}

func (application *apiServer) readAuthConfig(ctx context.Context, _ *struct{}) (*authConfigOutput, error) {
	if application.providers == nil {
		return nil, problem(http.StatusServiceUnavailable, "unavailable", "登录通道配置尚未就绪，请稍后重试。")
	}
	output := &authConfigOutput{CacheControl: "no-store"}
	for _, descriptor := range application.providers.Descriptors() {
		switch descriptor.Kind {
		case auth.ProviderKindLocal:
			output.Body.Local.Enabled = descriptor.Enabled
			output.Body.Local.Visible = descriptor.Visible
		case auth.ProviderKindOIDC:
			output.Body.OIDC.Enabled = descriptor.Enabled
			output.Body.OIDC.Label = descriptor.Label
			output.Body.OIDC.IconURL = descriptor.IconURL
		}
	}
	return output, nil
}
