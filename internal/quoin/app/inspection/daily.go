package appinspection

// 跨来源日报 HTTP 面（ADR-0014）：配置管理、报告读模型与人工补跑/重分析。
// 本层不拥有 SQL：全部经由 inspection 域命令与只读投影，鉴权沿用本包的
// Admin 会话接缝。

import (
	"context"
	"net/http"

	"github.com/Suknna/quoin/internal/quoin/inspection"
	"github.com/danielgtaylor/huma/v2"
)

// DailyReportConfigRequest is the create/update DTO; the update path adds the
// optimistic expectedRowVersion.
type DailyReportConfigRequest struct {
	ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
	ConfigKey       string `json:"configKey" minLength:"1" maxLength:"63" pattern:"^[a-z][a-z0-9-]{0,62}$"`
	DisplayName     string `json:"displayName" minLength:"1" maxLength:"200"`
	Enabled         bool   `json:"enabled"`
	Timezone        string `json:"timezone" minLength:"1" maxLength:"64"`
	// TriggerTime is the local wall clock 'HH:MM' (24h) of the daily trigger.
	TriggerTime string   `json:"triggerTime" minLength:"5" maxLength:"5" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$"`
	PlanKeys    []string `json:"planKeys" minItems:"1" maxItems:"200"`
	// Optional human expectation, frozen per report version and escaped in the
	// XML prompt; it cannot grant tools or expand evidence access.
	ReportInstructions *string `json:"reportInstructions,omitempty" maxLength:"4000"`
}

func (input DailyReportConfigRequest) domain() inspection.DailyReportConfigInput {
	return inspection.DailyReportConfigInput{
		ConfigKey: input.ConfigKey, DisplayName: input.DisplayName, Enabled: input.Enabled,
		Timezone: input.Timezone, TriggerTime: input.TriggerTime, PlanKeys: input.PlanKeys,
		ReportInstructions: input.ReportInstructions,
	}
}

func (handler *Handler) registerDailyReports(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-report-configs", OperationID: "listInspectionDailyReportConfigs"}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []inspection.DailyReportConfig `json:"items"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		items, err := handler.Inspections.ListDailyReportConfigs(ctx)
		if err != nil {
			return nil, mapDomainError(err)
		}
		response := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items []inspection.DailyReportConfig `json:"items"`
			}
		}{CacheControl: noStore()}
		response.Body.Items = items
		return response, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/daily-report-configs", OperationID: "createInspectionDailyReportConfig", DefaultStatus: http.StatusCreated}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
		Body    DailyReportConfigRequest
	}) (*struct {
		Status       int    `header:"-"`
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportConfig
	}, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		item, err := handler.Inspections.CreateDailyReportConfig(ctx, principal, input.Body.ClientCommandID, input.Body.domain())
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			Status       int    `header:"-"`
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportConfig
		}{Status: http.StatusCreated, CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-report-configs/{configKey}", OperationID: "getInspectionDailyReportConfig"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportConfig
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		item, err := handler.Inspections.GetDailyReportConfig(ctx, input.ConfigKey)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportConfig
		}{CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/inspections/daily-report-configs/{configKey}", OperationID: "updateInspectionDailyReportConfig"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
		Body      struct {
			DailyReportConfigRequest
			ExpectedRowVersion int64 `json:"expectedRowVersion" minimum:"1"`
		}
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportConfig
	}, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		if input.ConfigKey != input.Body.ConfigKey {
			return nil, problem(http.StatusUnprocessableEntity, "identity_conflict", "路径与配置标识不一致。")
		}
		item, err := handler.Inspections.UpdateDailyReportConfig(ctx, principal, input.Body.ClientCommandID, input.Body.domain(), input.Body.ExpectedRowVersion)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportConfig
		}{CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports", OperationID: "listInspectionDailyReports"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `query:"configKey"`
		Limit     int    `query:"limit"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []inspection.DailyReportSummary `json:"items"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		items, err := handler.Inspections.ListDailyReports(ctx, input.ConfigKey, input.Limit)
		if err != nil {
			return nil, mapDomainError(err)
		}
		response := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items []inspection.DailyReportSummary `json:"items"`
			}
		}{CacheControl: noStore()}
		response.Body.Items = items
		return response, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports/missing", OperationID: "listMissingInspectionDailyReports"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `query:"configKey" minLength:"1"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []inspection.MissingDailyDate `json:"items"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		if input.ConfigKey == "" {
			return nil, huma.Error400BadRequest("需要日报配置标识", nil)
		}
		items, err := handler.Inspections.MissingDailyReports(ctx, input.ConfigKey)
		if err != nil {
			return nil, mapDomainError(err)
		}
		response := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items []inspection.MissingDailyDate `json:"items"`
			}
		}{CacheControl: noStore()}
		response.Body.Items = items
		return response, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports/{configKey}/{localDate}", OperationID: "getInspectionDailyReport"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
		LocalDate string `path:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportDetail
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		item, err := handler.Inspections.GetDailyReport(ctx, input.ConfigKey, input.LocalDate)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportDetail
		}{CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports/{configKey}/{localDate}/versions/{version}", OperationID: "getInspectionDailyReportVersion"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
		LocalDate string `path:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
		Version   int64  `path:"version" minimum:"1"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Content string `json:"content"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		content, err := handler.Inspections.GetDailyReportVersion(ctx, input.ConfigKey, input.LocalDate, input.Version)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Content string `json:"content"`
			}
		}{CacheControl: noStore(), Body: struct {
			Content string `json:"content"`
		}{Content: content}}, nil
	})
	// 人工补跑（漏过的整日）：窗口仍是请求的原日期，绝不偷换为当前日期。
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/daily-reports/backfill", OperationID: "backfillInspectionDailyReport", DefaultStatus: http.StatusAccepted}, func(ctx context.Context, input *struct {
		Session string `cookie:"__Host-quoin-session"`
		Body    struct {
			ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
			ConfigKey       string `json:"configKey" minLength:"1" maxLength:"63"`
			LocalDate       string `json:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
		}
	}) (*struct {
		Status       int    `header:"-"`
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportSummary
	}, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		item, err := handler.Inspections.CreateManualDailyReport(ctx, principal, input.Body.ClientCommandID, input.Body.ConfigKey, input.Body.LocalDate)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			Status       int    `header:"-"`
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportSummary
		}{Status: http.StatusAccepted, CacheControl: noStore(), Body: item}, nil
	})
	// 人工重分析：从同一冻结窗口追加新版本，旧版本保持可读可比较。
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/inspections/daily-reports/{configKey}/{localDate}/rerun", OperationID: "rerunInspectionDailyReport", DefaultStatus: http.StatusAccepted}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
		LocalDate string `path:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
		Body      struct {
			ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		}
	}) (*struct {
		Status       int    `header:"-"`
		CacheControl string `header:"Cache-Control"`
		Body         inspection.DailyReportSummary
	}, error) {
		principal, err := handler.reader(ctx, input.Session)
		if err != nil {
			return nil, err
		}
		item, err := handler.Inspections.RerunDailyReport(ctx, principal, input.Body.ClientCommandID, input.ConfigKey, input.LocalDate)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			Status       int    `header:"-"`
			CacheControl string `header:"Cache-Control"`
			Body         inspection.DailyReportSummary
		}{Status: http.StatusAccepted, CacheControl: noStore(), Body: item}, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports/{configKey}/{localDate}/analyses", OperationID: "listInspectionDailyReportAnalyses"}, func(ctx context.Context, input *struct {
		Session   string `cookie:"__Host-quoin-session"`
		ConfigKey string `path:"configKey"`
		LocalDate string `path:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items []inspection.DailyReportAnalysisSummary `json:"items"`
		}
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		items, err := handler.Inspections.ListDailyReportAnalyses(ctx, input.ConfigKey, input.LocalDate)
		if err != nil {
			return nil, mapDomainError(err)
		}
		response := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Items []inspection.DailyReportAnalysisSummary `json:"items"`
			}
		}{CacheControl: noStore()}
		response.Body.Items = items
		return response, nil
	})
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/inspections/daily-reports/{configKey}/{localDate}/analyses/{analysisVersion}", OperationID: "getInspectionDailyReportAnalysis"}, func(ctx context.Context, input *struct {
		Session         string `cookie:"__Host-quoin-session"`
		ConfigKey       string `path:"configKey"`
		LocalDate       string `path:"localDate" pattern:"^\\d{4}-\\d{2}-\\d{2}$"`
		AnalysisVersion int64  `path:"analysisVersion" minimum:"1"`
	}) (*struct {
		CacheControl string                               `header:"Cache-Control"`
		Body         inspection.DailyReportAnalysisDetail `json:"body"`
	}, error) {
		if _, err := handler.reader(ctx, input.Session); err != nil {
			return nil, err
		}
		item, err := handler.Inspections.GetDailyReportAnalysis(ctx, input.ConfigKey, input.LocalDate, input.AnalysisVersion)
		if err != nil {
			return nil, mapDomainError(err)
		}
		return &struct {
			CacheControl string                               `header:"Cache-Control"`
			Body         inspection.DailyReportAnalysisDetail `json:"body"`
		}{CacheControl: noStore(), Body: item}, nil
	})
}
