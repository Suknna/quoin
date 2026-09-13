package appconfig

// The huma-owned configuration routes: business-system list/detail/history,
// the publish command and the read-only embedded Journey Catalog view.

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/config"
)

func base64Encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
func base64Decode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

func noStore() string { return "no-store" }

// --- Business Systems -----------------------------------------------------

func (handler *Handler) listBusinessSystems(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Enabled string `query:"enabled"`
	Query   string `query:"q"`
	Cursor  string `query:"cursor"`
	Limit   int    `query:"limit"`
}) (*struct {
	CacheControl string                             `header:"Cache-Control"`
	Body         businesssystem.SystemDetailListing `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	var enabledFilter *bool
	switch input.Enabled {
	case "true":
		value := true
		enabledFilter = &value
	case "false":
		value := false
		enabledFilter = &value
	case "":
	default:
		return nil, problem(http.StatusUnprocessableEntity, "validation_failed", "enabled 筛选必须是 true 或 false。")
	}
	items, nextCursor, err := handler.Systems.ListSystems(ctx, enabledFilter, input.Query, decodeCursor(input.Cursor), input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	response := &struct {
		CacheControl string                             `header:"Cache-Control"`
		Body         businesssystem.SystemDetailListing `json:"body"`
	}{CacheControl: noStore()}
	response.Body.Items = items
	if nextCursor != "" {
		response.Body.NextCursor = encodeCursor(nextCursor)
	}
	return response, nil
}

func (handler *Handler) getBusinessSystem(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
}) (*struct {
	CacheControl string                              `header:"Cache-Control"`
	Body         businesssystem.BusinessSystemDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	detail, err := handler.Systems.GetSystem(ctx, input.SystemKey)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                              `header:"Cache-Control"`
		Body         businesssystem.BusinessSystemDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) listBusinessSystemConfigs(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit"`
}) (*struct {
	CacheControl string                               `header:"Cache-Control"`
	Body         businesssystem.VersionSummaryListing `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	items, more, err := handler.Systems.ListVersions(ctx, input.SystemKey, decodeCursor(input.Cursor), input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	response := &struct {
		CacheControl string                               `header:"Cache-Control"`
		Body         businesssystem.VersionSummaryListing `json:"body"`
	}{CacheControl: noStore()}
	response.Body.Items = items
	if more && len(items) > 0 {
		response.Body.NextCursor = encodeCursor(items[len(items)-1].ID)
	}
	return response, nil
}

func (handler *Handler) getBusinessSystemConfig(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	VersionID string `path:"versionId"`
}) (*struct {
	CacheControl string                             `header:"Cache-Control"`
	Body         businesssystem.ConfigVersionDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	detail, err := handler.Systems.GetVersion(ctx, input.SystemKey, versionID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                             `header:"Cache-Control"`
		Body         businesssystem.ConfigVersionDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

type publishBody struct {
	ClientCommandID                   string  `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	ExpectedCurrentPublishedVersionID *string `json:"expectedCurrentPublishedVersionId" pattern:"^[1-9][0-9]*$"`
}

func (handler *Handler) publishBusinessSystemConfig(ctx context.Context, input *struct {
	// Flattened rather than embedded: huma v2.39.1 does not bind cookie
	// parameters from embedded structs when the input also has a Body (same
	// frozen workaround as passwordInput).
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	VersionID string `path:"versionId"`
	Body      publishBody
}) (*struct {
	CacheControl string                              `header:"Cache-Control"`
	Body         businesssystem.BusinessSystemDetail `json:"body"`
}, error) {
	principalID, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	versionID, locatorProblem := parseLocator(input.VersionID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	var expected *int64
	if input.Body.ExpectedCurrentPublishedVersionID != nil {
		parsed, parseErr := strconv.ParseInt(*input.Body.ExpectedCurrentPublishedVersionID, 10, 64)
		if parseErr != nil || parsed <= 0 {
			return nil, problem(http.StatusUnprocessableEntity, "validation_failed", "expectedCurrentPublishedVersionId 必须是正整数或 null。")
		}
		expected = &parsed
	}
	detail, err := handler.Systems.Publish(ctx, principalID, input.Body.ClientCommandID, input.SystemKey, versionID, expected)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                              `header:"Cache-Control"`
		Body         businesssystem.BusinessSystemDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

// --- Business System Kubernetes connections -----------------------------

type kubernetesMappingCreateBody struct {
	ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	ConnectionID    int64  `json:"connectionId,string" minimum:"1"`
}

func (handler *Handler) listKubernetesConnections(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
}) (*struct {
	CacheControl string                                       `header:"Cache-Control"`
	Body         []businesssystem.KubernetesConnectionMapping `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	mappings, err := handler.Systems.ListKubernetesConnectionMappings(ctx, input.SystemKey)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                                       `header:"Cache-Control"`
		Body         []businesssystem.KubernetesConnectionMapping `json:"body"`
	}{CacheControl: noStore(), Body: mappings}, nil
}

func (handler *Handler) createKubernetesConnection(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Body      kubernetesMappingCreateBody
}) (*struct {
	CacheControl string                                     `header:"Cache-Control"`
	Body         businesssystem.KubernetesConnectionMapping `json:"body"`
}, error) {
	principal, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	mapping, err := handler.Systems.CreateKubernetesConnectionMapping(ctx, principal, input.Body.ClientCommandID, input.SystemKey, input.Body.ConnectionID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                                     `header:"Cache-Control"`
		Body         businesssystem.KubernetesConnectionMapping `json:"body"`
	}{CacheControl: noStore(), Body: mapping}, nil
}

type kubernetesMappingRetireBody struct {
	ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
}

func (handler *Handler) retireKubernetesConnection(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	MappingID string `path:"mappingId" pattern:"^[1-9][0-9]*$"`
	Body      kubernetesMappingRetireBody
}) (*struct {
	CacheControl string                                     `header:"Cache-Control"`
	Body         businesssystem.KubernetesConnectionMapping `json:"body"`
}, error) {
	principal, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	mappingID, locatorProblem := parseLocator(input.MappingID)
	if locatorProblem != nil {
		return nil, locatorProblem
	}
	mapping, err := handler.Systems.RetireKubernetesConnectionMapping(ctx, principal, input.Body.ClientCommandID, input.SystemKey, mappingID, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                                     `header:"Cache-Control"`
		Body         businesssystem.KubernetesConnectionMapping `json:"body"`
	}{CacheControl: noStore(), Body: mapping}, nil
}

// --- Journey Catalog ------------------------------------------------------

func (handler *Handler) getJourneyCatalog(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*struct {
	CacheControl string         `header:"Cache-Control"`
	Body         map[string]any `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	document, version, digest, err := config.JourneyCatalog()
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取 Journey Catalog。")
	}
	return &struct {
		CacheControl string         `header:"Cache-Control"`
		Body         map[string]any `json:"body"`
	}{CacheControl: noStore(), Body: map[string]any{
		"version": version, "digest": digest, "catalogJson": document,
	}}, nil
}
