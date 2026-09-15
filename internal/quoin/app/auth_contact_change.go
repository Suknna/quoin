package app

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

func (application *apiServer) registerContactChangeRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/contact-change", OperationID: "startContactChange"}, application.startContactChange)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/auth/contact-change/complete", OperationID: "completeContactChange", DefaultStatus: http.StatusNoContent}, application.completeContactChange)
}

type contactChangeInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Flow    string `cookie:"__Host-quoin-flow"`
	Body    struct {
		CurrentPassword string `json:"currentPassword" minLength:"1" maxLength:"512"`
	}
}

func (application *apiServer) startContactChange(ctx context.Context, input *contactChangeInput) (*authenticationOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "更换验证渠道")
	if err != nil {
		return nil, err
	}
	flow, _, err := application.auth.StartContactChange(ctx, session, input.Body.CurrentPassword)
	if err != nil {
		return nil, authenticationError(err)
	}
	expires, err := time.Parse(time.RFC3339Nano, flow.ExpiresAt)
	if err != nil {
		return nil, authenticationError(err)
	}
	return &authenticationOutput{SetCookie: flowCookie(flow.Bearer, expires), CacheControl: "no-store", Body: flow}, nil
}

func (application *apiServer) completeContactChange(ctx context.Context, input *contactChangeInput) (*struct {
	SetCookie    []string `header:"Set-Cookie"`
	CacheControl string   `header:"Cache-Control"`
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "确认验证渠道更换")
	if err != nil {
		return nil, err
	}
	if err := application.auth.CompleteContactChange(ctx, session, input.Flow, input.Body.CurrentPassword); err != nil {
		return nil, authenticationError(err)
	}
	return &struct {
		SetCookie    []string `header:"Set-Cookie"`
		CacheControl string   `header:"Cache-Control"`
	}{[]string{sessionCookie("", -time.Second), flowCookie("", time.Unix(1, 0))}, "no-store"}, nil
}
