// Config Verification Run and Label Contract readiness routes (T17): the
// prepublish run command over one immutable draft, the run history/detail
// reads, the cancel fence and the derived activation readiness view.

package appconfig

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/labelcontract"
)

type verificationRunListing struct {
	Items      []businesssystem.VerificationRunSummary `json:"items"`
	NextCursor string                                  `json:"nextCursor,omitempty"`
}

func (handler *Handler) listConfigVerificationRuns(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	VersionID string `path:"versionId"`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit"`
}) (*struct {
	CacheControl string                 `header:"Cache-Control"`
	Body         verificationRunListing `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	cursor, cursorProblem := decodeVerificationCursor(input.Cursor)
	if cursorProblem != nil {
		return nil, cursorProblem
	}
	items, more, err := handler.Systems.ListVerifications(ctx, input.SystemKey, versionID, cursor, input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	response := &struct {
		CacheControl string                 `header:"Cache-Control"`
		Body         verificationRunListing `json:"body"`
	}{CacheControl: noStore()}
	response.Body.Items = items
	if more && len(items) > 0 {
		last := items[len(items)-1]
		response.Body.NextCursor = encodeCursor(last.CreatedAt + "\x00" + last.ID)
	}
	return response, nil
}

func decodeVerificationCursor(raw string) (string, *problemError) {
	if raw == "" {
		return "", nil
	}
	decoded, err := base64Decode(raw)
	if err != nil {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	cursor := string(decoded)
	createdAt, id, found := strings.Cut(cursor, "\x00")
	if !found || createdAt == "" {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	if value, err := strconv.ParseInt(id, 10, 64); err != nil || value <= 0 {
		return "", problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请从第一页重新读取。")
	}
	return cursor, nil
}

func (handler *Handler) runConfigVerificationRun(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	VersionID string `path:"versionId"`
	Body      struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		Purpose         string `json:"purpose" enum:"prepublish"`
	}
}) (*struct {
	Status       int                                  `header:"-"`
	CacheControl string                               `header:"Cache-Control"`
	Body         businesssystem.VerificationRunDetail `json:"body"`
}, error) {
	principalID, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	if input.Body.Purpose != "prepublish" {
		return nil, problem(http.StatusUnprocessableEntity, "validation_failed", "本端点只接受 purpose=prepublish 的验证 Run。")
	}
	detail, err := handler.Systems.RunVerification(ctx, principalID, input.Body.ClientCommandID, input.SystemKey, versionID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		Status       int                                  `header:"-"`
		CacheControl string                               `header:"Cache-Control"`
		Body         businesssystem.VerificationRunDetail `json:"body"`
	}{Status: http.StatusAccepted, CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) getConfigVerificationRun(ctx context.Context, input *struct {
	Session           string `cookie:"__Host-quoin-session"`
	SystemKey         string `path:"systemKey"`
	VersionID         string `path:"versionId"`
	VerificationRunID string `path:"verificationRunId"`
}) (*struct {
	CacheControl string                               `header:"Cache-Control"`
	Body         businesssystem.VerificationRunDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	runID, runProblem := parseLocator(input.VerificationRunID)
	if runProblem != nil {
		return nil, runProblem
	}
	detail, err := handler.Systems.GetVerification(ctx, input.SystemKey, versionID, runID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                               `header:"Cache-Control"`
		Body         businesssystem.VerificationRunDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) cancelConfigVerificationRun(ctx context.Context, input *struct {
	// Flattened rather than embedded: huma v2.39.1 does not bind cookie
	// parameters from embedded structs when the input also has a Body (same
	// frozen workaround as publishBody).
	Session           string `cookie:"__Host-quoin-session"`
	SystemKey         string `path:"systemKey"`
	VersionID         string `path:"versionId"`
	VerificationRunID string `path:"verificationRunId"`
	Body              struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	CacheControl string                               `header:"Cache-Control"`
	Body         businesssystem.VerificationRunDetail `json:"body"`
}, error) {
	principalID, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	runID, runProblem := parseLocator(input.VerificationRunID)
	if runProblem != nil {
		return nil, runProblem
	}
	detail, err := handler.Systems.CancelVerification(ctx, principalID, input.Body.ClientCommandID, input.SystemKey, versionID, runID, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                               `header:"Cache-Control"`
		Body         businesssystem.VerificationRunDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

// getLabelContractActivationReadiness derives the per-enabled-system
// candidate/blocker view for one target contract (DATA-CONFIG-002/007); any
// logged-in user may read it, only Admin can act on it.
func (handler *Handler) getLabelContractActivationReadiness(ctx context.Context, input *struct {
	Session         string `cookie:"__Host-quoin-session"`
	ContractVersion int64  `path:"contractVersion"`
}) (*struct {
	CacheControl string                  `header:"Cache-Control"`
	Body         labelcontract.Readiness `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	readiness, err := handler.Contracts.Readiness(ctx, input.ContractVersion)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                  `header:"Cache-Control"`
		Body         labelcontract.Readiness `json:"body"`
	}{CacheControl: noStore(), Body: readiness}, nil
}
