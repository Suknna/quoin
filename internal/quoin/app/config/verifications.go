// Config Verification Run routes: the prepublish run command over one
// immutable draft, its run history/detail reads, and the cancellation fence.

package appconfig

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
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
