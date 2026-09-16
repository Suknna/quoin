package appinspection

import (
	"context"
	"net/http"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/danielgtaylor/huma/v2"
)

type planRequest struct {
	ClientCommandID string         `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	PlanKey         string         `json:"planKey" minLength:"1" maxLength:"63" pattern:"^[a-z][a-z0-9-]{0,62}$"`
	DisplayName     string         `json:"displayName" minLength:"1" maxLength:"200"`
	Enabled         bool           `json:"enabled"`
	ConnectionName  string         `json:"connectionName" minLength:"1" maxLength:"200"`
	PluginID        string         `json:"pluginId" minLength:"1"`
	TemplateID      string         `json:"templateId" minLength:"1"`
	TemplateVersion *string        `json:"templateVersion,omitempty"`
	Params          map[string]any `json:"params"`
	// 可选分析语义字段：Run 创建时冻结进 Run，计划修改不改写已存在 Run。
	CheckDescription   *string `json:"checkDescription,omitempty" maxLength:"2000"`
	MetricUnit         *string `json:"metricUnit,omitempty" maxLength:"100"`
	ReportInstructions *string `json:"reportInstructions,omitempty" maxLength:"4000"`
	Scope              struct {
		Kind            string                  `json:"kind" enum:"integration,businessView,objects"`
		BusinessViewKey string                  `json:"businessViewKey,omitempty"`
		Objects         []inspection.PlanObject `json:"objects,omitempty"`
	} `json:"scope"`
	Cron     *string `json:"cron,omitempty"`
	Timezone string  `json:"timezone" minLength:"1"`
}

func (input planRequest) domain() inspection.PlanInput {
	return inspection.PlanInput{
		PlanKey: input.PlanKey, DisplayName: input.DisplayName, Enabled: input.Enabled, ConnectionName: input.ConnectionName,
		PluginID: input.PluginID, TemplateID: input.TemplateID, TemplateVersion: input.TemplateVersion, Params: input.Params,
		ScopeKind: input.Scope.Kind, BusinessViewKey: input.Scope.BusinessViewKey, Objects: input.Scope.Objects,
		Cron: input.Cron, Timezone: input.Timezone,
		CheckDescription:   derefText(input.CheckDescription),
		MetricUnit:         derefText(input.MetricUnit),
		ReportInstructions: derefText(input.ReportInstructions),
	}
}

// derefText 归一可选文本：nil 或空白归一为空串（域层存储为 NULL），其余去除
// 首尾空白。
func derefText(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

type planOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         inspection.Plan
}

type createPlanOutput struct {
	Status       int    `header:"-"`
	CacheControl string `header:"Cache-Control"`
	Body         inspection.Plan
}

func (handler *Handler) registerPlans(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/plans", OperationID: "listPluginInspectionPlans"}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []inspection.Plan `json:"items"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		items, err := handler.Inspections.ListPlans(ctx)
		if err != nil {
			return nil, mapDomainError(err)
		}
		response := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items []inspection.Plan `json:"items"`
			}
		}{CacheControl: noStore()}
		response.Body.Items = items
		return response, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/plans/{planKey}", OperationID: "getPluginInspectionPlan"}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
		PlanKey string `path:"planKey"`
	}) (*planOutput, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		item, err := handler.Inspections.GetPlan(ctx, input.PlanKey)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &planOutput{CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/plans", OperationID: "createPluginInspectionPlan", DefaultStatus: http.StatusCreated}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
		Body    planRequest
	}) (*createPlanOutput, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		item, err := handler.Inspections.CreatePlan(ctx, principal, input.Body.ClientCommandID, input.Body.domain())
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &createPlanOutput{Status: http.StatusCreated, CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/inspections/plans/{planKey}", OperationID: "updatePluginInspectionPlan"}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
		PlanKey string `path:"planKey"`
		Body    struct {
			planRequest
			ExpectedRowVersion int64 `json:"expectedRowVersion" minimum:"1"`
		}
	}) (*planOutput, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		if input.PlanKey != input.Body.PlanKey {
			return nil, problem(http.StatusUnprocessableEntity, "identity_conflict", "路径与计划标识不一致。")
		}
		item, err := handler.Inspections.UpdatePlan(ctx, principal, input.Body.ClientCommandID, input.Body.domain(), input.Body.ExpectedRowVersion)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &planOutput{CacheControl: noStore(), Body: item}, nil
	})
}
