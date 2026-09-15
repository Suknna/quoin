package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

const flowCookieName = "__Host-quoin-flow"

type authenticationOutput struct {
	SetCookie    string `header:"Set-Cookie"`
	CacheControl string `header:"Cache-Control"`
	Body         auth.Flow
}

type authenticationInput struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Username string `json:"username" maxLength:"200"`
		Password string `json:"password" minLength:"1" maxLength:"512"`
	}
}

func flowCookie(value string, expires time.Time) string {
	cookie := &http.Cookie{Name: flowCookieName, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: expires}
	if value == "" {
		cookie.MaxAge = -1
	}
	return cookie.String()
}

func authenticationError(err error) error {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return problem(http.StatusUnauthorized, "unauthenticated", "凭据无效或认证流程已过期，请重新开始。")
	case errors.Is(err, auth.ErrOtpInvalid):
		return problem(http.StatusUnprocessableEntity, "invalid_code", "验证码无效或已过期，请检查后重试。")
	case errors.Is(err, auth.ErrFlowExpired), errors.Is(err, auth.ErrFlowInvalid):
		return problem(http.StatusUnauthorized, "flow_expired", "认证流程已过期，请重新开始。")
	case errors.Is(err, auth.ErrInitializationIncomplete), errors.Is(err, auth.ErrInitializationRequired):
		return problem(http.StatusUnprocessableEntity, "initialization_required", "请先完成初始化所需步骤。")
	case errors.Is(err, auth.ErrNoContact):
		return problem(http.StatusUnprocessableEntity, "contact_required", "尚未配置可用的验证渠道，请联系管理员。")
	case errors.Is(err, auth.ErrRateLimited), errors.Is(err, auth.ErrChallengeRateLimited):
		return problem(http.StatusTooManyRequests, "rate_limited", "尝试过于频繁，请稍后再试。")
	case errors.Is(err, auth.ErrPasswordPolicy):
		return problemUnprocessable("新密码不满足密码策略。")
	case errors.Is(err, auth.ErrValidation):
		return problemUnprocessable("认证步骤或输入无效，请检查后重试。")
	default:
		return problem(http.StatusServiceUnavailable, "unavailable", "暂时无法完成认证，请稍后重试。")
	}
}

func authenticationRetryError(err error, retryAfter time.Duration) error {
	mapped := authenticationError(err)
	if errors.Is(err, auth.ErrRateLimited) || errors.Is(err, auth.ErrChallengeRateLimited) {
		seconds := max(1, int((retryAfter+time.Second-1)/time.Second))
		return huma.ErrorWithHeaders(mapped, http.Header{"Retry-After": {strconv.Itoa(seconds)}})
	}
	return mapped
}

func (application *apiServer) registerAuthenticationFlows(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "startAuthentication"}, application.startAuthentication)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/flow", OperationID: "readAuthenticationFlow"}, application.readAuthenticationFlow)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/auth/flow/password", OperationID: "setInitializationPassword", DefaultStatus: http.StatusNoContent}, application.setInitializationPassword)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/flow/contacts", OperationID: "registerInitializationContact"}, application.registerInitializationContact)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/flow/challenge", OperationID: "sendAuthenticationChallenge"}, application.sendAuthenticationChallenge)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/flow/verify", OperationID: "verifyInitializationChallenge"}, application.verifyAuthenticationChallenge)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/flow/complete", OperationID: "completeInitialization", DefaultStatus: http.StatusNoContent}, application.completeInitialization)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/activity", OperationID: "touchSessionActivity", DefaultStatus: http.StatusNoContent}, application.touchSessionActivity)
}

func (application *apiServer) startAuthentication(ctx context.Context, input *authenticationInput) (*authenticationOutput, error) {
	flow, retryAfter, err := application.auth.StartAuthentication(ctx, input.Body.Username, input.Body.Password, input.UserAgent)
	if err != nil {
		return nil, authenticationRetryError(err, retryAfter)
	}
	expires, err := time.Parse(time.RFC3339Nano, flow.ExpiresAt)
	if err != nil {
		return nil, authenticationError(err)
	}
	return &authenticationOutput{SetCookie: flowCookie(flow.Bearer, expires), CacheControl: "no-store", Body: flow}, nil
}

func (application *apiServer) readAuthenticationFlow(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
},
) (*authenticationOutput, error) {
	flow, err := application.auth.ReadFlow(ctx, input.Flow)
	if err != nil {
		return nil, authenticationError(err)
	}
	return &authenticationOutput{CacheControl: "no-store", Body: flow}, nil
}

func (application *apiServer) setInitializationPassword(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
	Body struct {
		NewPassword string `json:"newPassword" minLength:"15" maxLength:"512"`
	}
},
) (*noContentOutput, error) {
	if err := application.auth.SetFlowPassword(ctx, input.Flow, input.Body.NewPassword); err != nil {
		return nil, authenticationError(err)
	}
	return &noContentOutput{CacheControl: "no-store"}, nil
}

func (application *apiServer) registerInitializationContact(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
	Body struct {
		Channel string `json:"channel" enum:"email,sms"`
		Target  string `json:"target" maxLength:"320"`
	}
},
) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         auth.MaskedContact
}, error,
) {
	contact, err := application.auth.RegisterFlowContact(ctx, input.Flow, input.Body.Channel, input.Body.Target)
	if err != nil {
		return nil, authenticationError(err)
	}
	return &struct {
		CacheControl string `header:"Cache-Control"`
		Body         auth.MaskedContact
	}{"no-store", contact}, nil
}

func (application *apiServer) sendAuthenticationChallenge(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
	Body struct {
		ContactID string `json:"contactId" maxLength:"32"`
	}
},
) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         auth.MaskedContact
}, error,
) {
	contact, retryAfter, err := application.auth.SendFlowChallenge(ctx, input.Flow, input.Body.ContactID)
	if err != nil {
		return nil, authenticationRetryError(err, retryAfter)
	}
	return &struct {
		CacheControl string `header:"Cache-Control"`
		Body         auth.MaskedContact
	}{"no-store", contact}, nil
}

func (application *apiServer) verifyAuthenticationChallenge(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
	Body struct {
		Code string `json:"code" minLength:"6" maxLength:"6"`
	}
},
) (*struct {
	SetCookie    []string `header:"Set-Cookie"`
	CacheControl string   `header:"Cache-Control"`
	Body         struct {
		Completed bool       `json:"completed"`
		User      *auth.User `json:"user,omitempty"`
	}
}, error,
) {
	flow, err := application.auth.ReadFlow(ctx, input.Flow)
	if err != nil {
		return nil, authenticationError(err)
	}
	output := &struct {
		SetCookie    []string `header:"Set-Cookie"`
		CacheControl string   `header:"Cache-Control"`
		Body         struct {
			Completed bool       `json:"completed"`
			User      *auth.User `json:"user,omitempty"`
		}
	}{CacheControl: "no-store"}
	if string(flow.Type) == "login" {
		result, err := application.auth.CompleteLogin(ctx, input.Flow, input.Body.Code)
		if err != nil {
			return nil, authenticationError(err)
		}
		output.SetCookie = []string{sessionCookie(result.Bearer, 0), flowCookie("", time.Unix(1, 0))}
		output.Body.Completed = true
		output.Body.User = &result.User
	} else if err := application.auth.VerifyFlowChallenge(ctx, input.Flow, input.Body.Code); err != nil {
		return nil, authenticationError(err)
	}
	return output, nil
}

func (application *apiServer) completeInitialization(ctx context.Context, input *struct {
	Flow string `cookie:"__Host-quoin-flow"`
},
) (*noContentOutput, error) {
	flow, err := application.auth.ReadFlow(ctx, input.Flow)
	if err != nil {
		return nil, authenticationError(err)
	}
	switch string(flow.Type) {
	case "admin_initialize":
		err = application.auth.CompleteAdminInitialization(ctx, input.Flow)
	case "operator_initialize":
		err = application.auth.CompleteOperatorInitialization(ctx, input.Flow)
	default:
		return nil, problemUnprocessable("当前流程不是初始化流程。")
	}
	if err != nil {
		return nil, authenticationError(err)
	}
	// Restore checklist: a restored administrator completes through the same
	// unified initialization flow, so the AdminPassword completion edge is
	// satisfied here exactly like the legacy password change. Errors
	// propagate: a silently blocked Restore checklist is worse than a failed
	// completion response.
	if string(flow.Type) == "admin_initialize" {
		if err := application.maintenance.MarkAdminPasswordSafe(ctx, flow.User.ID); err != nil {
			return nil, huma.Error500InternalServerError("暂时无法更新恢复清单，请重试。", err)
		}
	}
	return &noContentOutput{SetCookie: flowCookie("", time.Unix(1, 0)), CacheControl: "no-store"}, nil
}

func (application *apiServer) touchSessionActivity(ctx context.Context, input *authInput) (*noContentOutput, error) {
	if err := application.auth.TouchSessionActivity(ctx, input.Session); err != nil {
		return nil, authenticationError(err)
	}
	return &noContentOutput{CacheControl: "no-store"}, nil
}
