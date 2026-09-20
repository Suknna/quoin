package app

// OIDC redirect endpoints (ADR-0010 / docs/authentication-design.md §4).
// Both routes own their response head (302 + cookies), so they ride the raw
// admission wrapper like the other streaming/download routes. The browser
// never sees a token: the exchange, verification and session issue are all
// server-side; failures return to /login with a stable, non-secret error
// code.

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/operations"
)

// oidcErrorRedirect sends the browser back to the login page with a stable
// error code; the codes carry no secret and the frontend maps them to text.
func oidcErrorRedirect(writer http.ResponseWriter, request *http.Request, code string) {
	http.Redirect(writer, request, "/login?error="+url.QueryEscape(code), http.StatusFound)
}

func (application *apiServer) beginOIDCLogin() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provider, err := application.oidcProvider()
		if err != nil {
			oidcErrorRedirect(writer, request, "idp_unavailable")
			return
		}
		intent, err := provider.BeginLogin(request.Context(), request.URL.Query().Get("return_to"))
		if err != nil {
			if errors.Is(err, auth.ErrOIDCMisconfigured) {
				oidcErrorRedirect(writer, request, "oidc_misconfigured")
				return
			}
			oidcErrorRedirect(writer, request, "idp_unavailable")
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name:     "__Host-quoin-oidc-state",
			Value:    intent.StateCookie,
			Path:     "/",
			MaxAge:   intent.CookieMaxAge,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(writer, request, intent.Redirect, http.StatusFound)
	})
}

func (application *apiServer) completeOIDCLogin() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("error") != "" {
			oidcErrorRedirect(writer, request, "access_denied")
			return
		}
		provider, err := application.oidcProvider()
		if err != nil {
			oidcErrorRedirect(writer, request, "idp_unavailable")
			return
		}
		stateCookie, err := request.Cookie("__Host-quoin-oidc-state")
		if err != nil || stateCookie.Value == "" {
			oidcErrorRedirect(writer, request, "state_invalid")
			return
		}
		result, returnTo, err := provider.CompleteLogin(request.Context(), auth.LoginCompletion{
			StateCookie: stateCookie.Value,
			State:       query.Get("state"),
			Code:        query.Get("code"),
			UserAgent:   request.UserAgent(),
		})
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrOIDCStateInvalid):
				oidcErrorRedirect(writer, request, "state_invalid")
			case errors.Is(err, auth.ErrAccountDisabled):
				oidcErrorRedirect(writer, request, "account_disabled")
			case errors.Is(err, auth.ErrOIDCProviderDown), errors.Is(err, auth.ErrOIDCMisconfigured):
				oidcErrorRedirect(writer, request, "idp_unavailable")
			default:
				oidcErrorRedirect(writer, request, "login_rejected")
			}
			return
		}
		// The one-time state cookie dies with the callback; the session
		// cookie takes over. The provider already sanitized returnTo.
		writer.Header().Add("Set-Cookie", (&http.Cookie{
			Name: "__Host-quoin-oidc-state", Value: "", Path: "/",
			MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		}).String())
		writer.Header().Add("Set-Cookie", sessionCookie(result.Bearer, 0))
		http.Redirect(writer, request, returnTo, http.StatusFound)
	})
}

// registerOIDCRoutes mounts the two redirect endpoints through the raw
// admission wrapper on one surface's mux.
func (application *apiServer) registerOIDCRoutes(guard *operations.Admission, mux *http.ServeMux) error {
	login, err := guard.Wrap("beginOIDCLogin", application.beginOIDCLogin())
	if err != nil {
		return err
	}
	callback, err := guard.Wrap("completeOIDCLogin", application.completeOIDCLogin())
	if err != nil {
		return err
	}
	mux.Handle("GET /api/v1/auth/oidc/login", login)
	mux.Handle("GET /api/v1/auth/oidc/callback", callback)
	return nil
}

// oidcProvider returns the configured OIDC provider, or a sentinel error
// when the channel is off (the handler maps that to idp_unavailable).
func (application *apiServer) oidcProvider() (*auth.OIDCProvider, error) {
	if application.providers == nil {
		return nil, auth.ErrOIDCMisconfigured
	}
	provider, err := application.providers.Get(auth.ProviderKindOIDC)
	if err != nil {
		return nil, auth.ErrOIDCMisconfigured
	}
	oidcProvider, ok := provider.(*auth.OIDCProvider)
	if !ok {
		return nil, auth.ErrOIDCMisconfigured
	}
	return oidcProvider, nil
}
