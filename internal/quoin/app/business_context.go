package app

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// businessContextItem is the deliberately narrow business selector shared by
// alerts and AI SRE. It excludes configurations, connection references, and
// all secret-adjacent metadata while retaining disabled published systems so
// historical alerts continue to have a usable business filter.
type businessContextItem struct {
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
}

type businessContextOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []businessContextItem `json:"items"`
	} `json:"body"`
}

func (application *apiServer) registerBusinessContextRoute(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/business-context", OperationID: "listBusinessContext"}, application.listBusinessContext)
}

func (application *apiServer) listBusinessContext(ctx context.Context, input *authInput) (*businessContextOutput, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取业务上下文"); err != nil {
		return nil, err
	}
	// A current configuration pointer exists only after explicit publication.
	// Do not filter enabled: disabling stops new work but must not erase the
	// selector needed to find historical alerts and investigations.
	rows, err := application.db.QueryContext(ctx, `
		SELECT key, display_name
		FROM business_systems
		WHERE current_config_version_id IS NOT NULL
		ORDER BY display_name COLLATE NOCASE, key`)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取业务上下文，请稍后重试。")
	}
	defer rows.Close()

	output := &businessContextOutput{CacheControl: "no-store"}
	output.Body.Items = []businessContextItem{}
	for rows.Next() {
		var item businessContextItem
		if err := rows.Scan(&item.Key, &item.DisplayName); err != nil {
			return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取业务上下文，请稍后重试。")
		}
		output.Body.Items = append(output.Body.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取业务上下文，请稍后重试。")
	}
	return output, nil
}
