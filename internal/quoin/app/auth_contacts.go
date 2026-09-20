package app

import (
	"context"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/danielgtaylor/huma/v2"
)

func (application *apiServer) registerAuthContactRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/auth/contacts", OperationID: "listOwnContacts"}, application.listOwnContacts)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/admin/users/{userId}/contacts", OperationID: "setUserContacts"}, application.setUserContacts)
}

// listOwnContacts returns the session user's masked display contacts.
func (application *apiServer) listOwnContacts(ctx context.Context, input *authInput) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []auth.MaskedContact `json:"items"`
	}
}, error,
) {
	session, err := application.authenticateFull(ctx, input.Session, "读取联系方式")
	if err != nil {
		return nil, err
	}
	contacts, err := application.auth.ListOwnContacts(ctx, session)
	if err != nil {
		return nil, authenticationError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []auth.MaskedContact `json:"items"`
		}
	}{CacheControl: "no-store"}
	output.Body.Items = contacts
	return output, nil
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
