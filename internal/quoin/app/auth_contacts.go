package app

import (
	"context"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

func (application *apiServer) registerAuthContactRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/admin/users/{userId}/contacts", OperationID: "setUserContacts"}, application.setUserContacts)
}

func (application *apiServer) setUserContacts(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	UserID  string `path:"userId"`
	Body    struct {
		ClientCommandID    string              `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion int64               `json:"expectedRowVersion" minimum:"1"`
		Contacts           []auth.ContactInput `json:"contacts" minItems:"1" maxItems:"2"`
	}
},
) (*struct{ Body auth.User }, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "配置用户验证渠道")
	if err != nil {
		return nil, err
	}
	userID, err := parseLocatorParam(input.UserID)
	if err != nil {
		return nil, err
	}
	digest := auth.DigestCommand("user.contacts", map[string]any{"userId": userID, "expectedRowVersion": input.Body.ExpectedRowVersion, "contacts": input.Body.Contacts})
	result, _, err := application.auth.SetUserContacts(ctx, session, auth.SetUserContactsInput{ClientCommandID: input.Body.ClientCommandID, Digest: digest, UserID: userID, ExpectedRow: input.Body.ExpectedRowVersion, Contacts: input.Body.Contacts})
	if err != nil {
		return nil, commandUserError(err)
	}
	return &struct{ Body auth.User }{Body: *result.User}, nil
}
