package appconfig

// The huma-owned configuration routes: business-system list/detail/history,
// the publish command. The read-only embedded Journey Catalog view is retired
// with the browser business (受控浏览器退役).

import (
	"context"
	"encoding/base64"
	"net/http"

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

func (handler *Handler) getJourneyCatalog(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*struct {
	CacheControl string         `header:"Cache-Control"`
	Body         map[string]any `json:"body"`
}, error) {
	// The catalog authenticates authentication-probe journeys: pure browser
	// plugin capability, so a deployment without the plugin reads nothing.
	if handler.BrowserEnabled != nil && !handler.BrowserEnabled() {
		return nil, problem(http.StatusNotFound, "not_found", "当前部署未启用受控浏览器插件。")
	}
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
