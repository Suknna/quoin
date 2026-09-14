package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/observation"
	"github.com/danielgtaylor/huma/v2"
)

type observationListInput struct {
	Session        string `cookie:"__Host-quoin-session"`
	ConnectionName string `path:"connectionName"`
	Cursor         string `query:"cursor"`
	Limit          int    `query:"limit" default:"50" minimum:"1" maximum:"200"`
	ObjectType     string `query:"objectType"`
	State          string `query:"state" enum:",observed,not_observed,stale"`
}

func observationCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, huma.Error400BadRequest("分页游标无效。")
	}
	id, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || id < 1 {
		return 0, huma.Error400BadRequest("分页游标无效。")
	}
	return id, nil
}

func observationNext(id int64) string {
	if id == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func observationError(err error) error {
	switch {
	case errors.Is(err, observation.ErrNotFound):
		return huma.Error404NotFound("接入或观测记录不存在。")
	case errors.Is(err, observation.ErrCommandReused):
		return huma.Error409Conflict("命令标识已用于其他请求。")
	case errors.Is(err, observation.ErrMaintenanceActive):
		return huma.Error503ServiceUnavailable("维护期间不启动新的观测。")
	case errors.Is(err, observation.ErrNotObservable):
		return huma.Error422UnprocessableEntity("接入未启用或插件不支持观测。")
	default:
		return huma.Error500InternalServerError("观测请求未完成，请稍后重试。")
	}
}

func (application *apiServer) registerObservationRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/{connectionName}/resources", OperationID: "listSourceObservedResources"}, func(ctx context.Context, input *observationListInput) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []observation.ResourceSummary `json:"items"`
			NextCursor string                        `json:"nextCursor,omitempty"`
		}
	}, error,
	) {
		if _, err := application.authenticateAdmin(ctx, input.Session, "查看来源资源"); err != nil {
			return nil, err
		}
		after, err := observationCursor(input.Cursor)
		if err != nil {
			return nil, err
		}
		items, next, err := application.observations.ListResources(ctx, input.ConnectionName, input.ObjectType, input.State, after, input.Limit)
		if err != nil {
			return nil, observationError(err)
		}
		output := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items      []observation.ResourceSummary `json:"items"`
				NextCursor string                        `json:"nextCursor,omitempty"`
			}
		}{CacheControl: "no-store"}
		output.Body.Items = items
		output.Body.NextCursor = observationNext(next)
		return output, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/{connectionName}/resources/{resourceId}", OperationID: "getSourceObservedResource"}, func(ctx context.Context, input *struct {
		Session        string `cookie:"__Host-quoin-session"`
		ConnectionName string `path:"connectionName"`
		ResourceID     int64  `path:"resourceId" minimum:"1"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         observation.ResourceSummary
	}, error,
	) {
		if _, err := application.authenticateAdmin(ctx, input.Session, "查看来源资源详情"); err != nil {
			return nil, err
		}
		item, err := application.observations.GetResource(ctx, input.ConnectionName, input.ResourceID)
		if err != nil {
			return nil, observationError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         observation.ResourceSummary
		}{CacheControl: "no-store", Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/{connectionName}/observation-runs", OperationID: "listSourceObservationRuns"}, func(ctx context.Context, input *observationListInput) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []observation.SourceObservationRun `json:"items"`
			NextCursor string                             `json:"nextCursor,omitempty"`
		}
	}, error,
	) {
		if _, err := application.authenticateAdmin(ctx, input.Session, "查看观测执行"); err != nil {
			return nil, err
		}
		after, err := observationCursor(input.Cursor)
		if err != nil {
			return nil, err
		}
		items, next, err := application.observations.ListRuns(ctx, input.ConnectionName, after, input.Limit)
		if err != nil {
			return nil, observationError(err)
		}
		output := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items      []observation.SourceObservationRun `json:"items"`
				NextCursor string                             `json:"nextCursor,omitempty"`
			}
		}{CacheControl: "no-store"}
		output.Body.Items = items
		output.Body.NextCursor = observationNext(next)
		return output, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/integrations/{connectionName}/observation-runs/{observationRunId}", OperationID: "getSourceObservationRun"}, func(ctx context.Context, input *struct {
		Session        string `cookie:"__Host-quoin-session"`
		ConnectionName string `path:"connectionName"`
		RunID          int64  `path:"observationRunId" minimum:"1"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         observation.SourceObservationRun
	}, error,
	) {
		if _, err := application.authenticateAdmin(ctx, input.Session, "查看观测执行详情"); err != nil {
			return nil, err
		}
		item, err := application.observations.GetRun(ctx, input.ConnectionName, input.RunID)
		if err != nil {
			return nil, observationError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         observation.SourceObservationRun
		}{CacheControl: "no-store", Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/integrations/{connectionName}/resources:refresh", OperationID: "refreshIntegrationResources", DefaultStatus: http.StatusAccepted}, func(ctx context.Context, input *struct {
		Session        string `cookie:"__Host-quoin-session"`
		ConnectionName string `path:"connectionName"`
		Body           struct {
			ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		}
	}) (*struct {
		Status       int    `header:"-"`
		CacheControl string `header:"Cache-Control"`
		Body         observation.SourceObservationRun
	}, error,
	) {
		session, err := application.authenticateAdmin(ctx, input.Session, "刷新来源观测")
		if err != nil {
			return nil, err
		}
		run, err := application.observations.StartRun(ctx, session.User.ID, input.Body.ClientCommandID, input.ConnectionName, "manual", nil)
		if err != nil {
			return nil, observationError(err)
		}
		if application.observationDispatchFunc != nil {
			go application.observationDispatchFunc(context.Background())
		}
		return &struct {
			Status       int    `header:"-"`
			CacheControl string `header:"Cache-Control"`
			Body         observation.SourceObservationRun
		}{Status: http.StatusAccepted, CacheControl: "no-store", Body: run}, nil
	})
}
