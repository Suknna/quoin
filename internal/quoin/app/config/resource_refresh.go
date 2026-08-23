package appconfig

import (
	"context"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"net/http"
)

func (handler *Handler) startResourceRefresh(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Body      struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	}
}) (*struct {
	Status       int                                     `header:"-"`
	CacheControl string                                  `header:"Cache-Control"`
	Body         businesssystem.ResourceRefreshRunDetail `json:"body"`
}, error) {
	principalID, err := handler.admin(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	detail, err := handler.Systems.StartResourceRefresh(ctx, principalID, input.Body.ClientCommandID, input.SystemKey)
	if err != nil {
		return nil, mapDomainError(err)
	}
	if handler.DispatchResourceRefresh != nil {
		go handler.DispatchResourceRefresh(context.Background())
	}
	return &struct {
		Status       int                                     `header:"-"`
		CacheControl string                                  `header:"Cache-Control"`
		Body         businesssystem.ResourceRefreshRunDetail `json:"body"`
	}{Status: http.StatusAccepted, CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) getResourceRefreshRun(ctx context.Context, input *struct {
	Session              string `cookie:"__Host-quoin-session"`
	SystemKey            string `path:"systemKey"`
	ResourceRefreshRunID string `path:"resourceRefreshRunId"`
}) (*struct {
	CacheControl string                                  `header:"Cache-Control"`
	Body         businesssystem.ResourceRefreshRunDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	runID, problem := parseLocator(input.ResourceRefreshRunID)
	if problem != nil {
		return nil, problem
	}
	detail, err := handler.Systems.GetResourceRefresh(ctx, input.SystemKey, runID)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                                  `header:"Cache-Control"`
		Body         businesssystem.ResourceRefreshRunDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}

func (handler *Handler) listObservedResources(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SystemKey string `path:"systemKey"`
	Current   string `query:"current"`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []businesssystem.ObservedResourceSummary `json:"items"`
		NextCursor string                                   `json:"nextCursor,omitempty"`
	} `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	var current *bool
	switch input.Current {
	case "":
	case "true":
		value := true
		current = &value
	case "false":
		value := false
		current = &value
	default:
		return nil, problem(422, "validation_failed", "current 筛选必须是 true 或 false。")
	}
	cursor := int64(0)
	if raw := decodeCursor(input.Cursor); raw != "" {
		value, err := parseLocator(raw)
		if err != nil {
			return nil, problem(422, "validation_failed", "cursor 必须是资源 ID。")
		}
		cursor = value
	}
	items, next, err := handler.Systems.ListObservedResources(ctx, input.SystemKey, current, cursor, input.Limit)
	if err != nil {
		return nil, mapDomainError(err)
	}
	out := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []businesssystem.ObservedResourceSummary `json:"items"`
			NextCursor string                                   `json:"nextCursor,omitempty"`
		} `json:"body"`
	}{CacheControl: noStore()}
	out.Body.Items = items
	if next > 0 {
		out.Body.NextCursor = encodeCursor(fmt.Sprint(next))
	}
	return out, nil
}

func (handler *Handler) getObservedResource(ctx context.Context, input *struct {
	Session    string `cookie:"__Host-quoin-session"`
	SystemKey  string `path:"systemKey"`
	ResourceID string `path:"resourceId"`
}) (*struct {
	CacheControl string                                `header:"Cache-Control"`
	Body         businesssystem.ObservedResourceDetail `json:"body"`
}, error) {
	if _, err := handler.reader(ctx, input.Session); err != nil {
		return nil, err
	}
	id, problem := parseLocator(input.ResourceID)
	if problem != nil {
		return nil, problem
	}
	detail, err := handler.Systems.GetObservedResource(ctx, input.SystemKey, id)
	if err != nil {
		return nil, mapDomainError(err)
	}
	return &struct {
		CacheControl string                                `header:"Cache-Control"`
		Body         businesssystem.ObservedResourceDetail `json:"body"`
	}{CacheControl: noStore(), Body: detail}, nil
}
