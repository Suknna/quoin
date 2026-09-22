package app

// enrichment_rules.go 拥有 /api/v1/enrichment-rules 的 Admin HTTP 面
// (ADR-0012)：富化规则列表/详情读与创建/更新写命令。规则是首观测富化求值
// 的输入（alerts 包流水线 Enrich 段），写路径经 businessview 共享执行器，
// 本文件只做会话裁决与 wire 映射。

import (
	"context"
	"errors"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/businessview"
	"github.com/danielgtaylor/huma/v2"
)

type enrichmentRuleListing struct {
	Items []businessview.EnrichmentRule `json:"items"`
}

// enrichmentRuleProblem 把富化规则域的确定性拒绝映射为 problem 响应（与
// business-views 段同一错误风格：冲突 409、未知引用 404、形状 422）。
func enrichmentRuleProblem(err error) error {
	var rejection *businessview.ConflictError
	if errors.As(err, &rejection) {
		status := http.StatusUnprocessableEntity
		switch rejection.Code {
		case "rule_exists", "row_version_conflict", "command_reused":
			status = http.StatusConflict
		case "not_found", "unknown_alert_source":
			status = http.StatusNotFound
		}
		return problem(status, rejection.Code, rejection.Detail)
	}
	return huma.Error500InternalServerError("无法完成富化规则请求", err)
}

func (application *apiServer) listEnrichmentRules(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*struct {
	CacheControl string                `header:"Cache-Control"`
	Body         enrichmentRuleListing `json:"body"`
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取富化规则"); err != nil {
		return nil, err
	}
	items, err := application.enrichmentRules.ListEnrichmentRules(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取富化规则", err)
	}
	if items == nil {
		items = []businessview.EnrichmentRule{}
	}
	return &struct {
		CacheControl string                `header:"Cache-Control"`
		Body         enrichmentRuleListing `json:"body"`
	}{CacheControl: "no-store", Body: enrichmentRuleListing{Items: items}}, nil
}

func (application *apiServer) getEnrichmentRule(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RuleKey string `path:"ruleKey"`
}) (*struct {
	CacheControl string                      `header:"Cache-Control"`
	Body         businessview.EnrichmentRule `json:"body"`
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取富化规则"); err != nil {
		return nil, err
	}
	rule, err := application.enrichmentRules.GetEnrichmentRule(ctx, input.RuleKey)
	if err != nil {
		return nil, enrichmentRuleProblem(err)
	}
	return &struct {
		CacheControl string                      `header:"Cache-Control"`
		Body         businessview.EnrichmentRule `json:"body"`
	}{CacheControl: "no-store", Body: rule}, nil
}

// EnrichmentRuleBody 是创建/更新共享的可变字段 wire 形状。规则 key 只由路径
// 携带（POST 时在请求体内），请求体不重复。类型名必须导出：Huma schema 生
// 成会跳过未导出的匿名嵌入类型。
type EnrichmentRuleBody struct {
	ClientCommandID string            `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	DisplayName     string            `json:"displayName" minLength:"1" maxLength:"200"`
	Description     string            `json:"description" maxLength:"2000"`
	Enabled         bool              `json:"enabled"`
	LabelConditions map[string]string `json:"labelConditions"`
	AlertSourceKeys []string          `json:"alertSourceKeys,omitempty"`
	Outputs         map[string]string `json:"outputs"`
	Priority        int               `json:"priority" minimum:"0" maximum:"1000000"`
}

func (body EnrichmentRuleBody) input() businessview.EnrichmentRuleInput {
	return businessview.EnrichmentRuleInput{
		DisplayName:     body.DisplayName,
		Description:     body.Description,
		Enabled:         body.Enabled,
		LabelConditions: body.LabelConditions,
		AlertSourceKeys: body.AlertSourceKeys,
		Outputs:         body.Outputs,
		Priority:        body.Priority,
	}
}

func (application *apiServer) createEnrichmentRule(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		EnrichmentRuleBody
		RuleKey string `json:"ruleKey" minLength:"1" maxLength:"63" pattern:"^[a-z][a-z0-9-]{0,62}$"`
	}
}) (*struct {
	Status       int                         `header:"-"`
	CacheControl string                      `header:"Cache-Control"`
	Body         businessview.EnrichmentRule `json:"body"`
}, error,
) {
	principal, err := application.authenticateAdmin(ctx, input.Session, "创建富化规则")
	if err != nil {
		return nil, err
	}
	ruleInput := input.Body.input()
	ruleInput.RuleKey = input.Body.RuleKey
	rule, err := application.enrichmentRules.CreateEnrichmentRule(ctx, principal.User.ID, input.Body.ClientCommandID, ruleInput)
	if err != nil {
		return nil, enrichmentRuleProblem(err)
	}
	return &struct {
		Status       int                         `header:"-"`
		CacheControl string                      `header:"Cache-Control"`
		Body         businessview.EnrichmentRule `json:"body"`
	}{Status: http.StatusCreated, CacheControl: "no-store", Body: rule}, nil
}

func (application *apiServer) updateEnrichmentRule(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	RuleKey string `path:"ruleKey"`
	Body    struct {
		EnrichmentRuleBody
		ExpectedRowVersion int64 `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	CacheControl string                      `header:"Cache-Control"`
	Body         businessview.EnrichmentRule `json:"body"`
}, error,
) {
	principal, err := application.authenticateAdmin(ctx, input.Session, "更新富化规则")
	if err != nil {
		return nil, err
	}
	// 路径是规则 key 的唯一来源；请求体只描述可变内容与并发前提。
	ruleInput := input.Body.input()
	ruleInput.RuleKey = input.RuleKey
	rule, err := application.enrichmentRules.UpdateEnrichmentRule(ctx, principal.User.ID, input.Body.ClientCommandID, ruleInput, input.Body.ExpectedRowVersion)
	if err != nil {
		return nil, enrichmentRuleProblem(err)
	}
	return &struct {
		CacheControl string                      `header:"Cache-Control"`
		Body         businessview.EnrichmentRule `json:"body"`
	}{CacheControl: "no-store", Body: rule}, nil
}

func (application *apiServer) registerEnrichmentRuleRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/enrichment-rules", OperationID: "listEnrichmentRules"}, application.listEnrichmentRules)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/enrichment-rules", OperationID: "createEnrichmentRule", DefaultStatus: http.StatusCreated}, application.createEnrichmentRule)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/enrichment-rules/{ruleKey}", OperationID: "getEnrichmentRule"}, application.getEnrichmentRule)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/enrichment-rules/{ruleKey}", OperationID: "updateEnrichmentRule"}, application.updateEnrichmentRule)
}
