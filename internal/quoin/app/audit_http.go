package app

// Consolidated audit HTTP surface (docs/audit-design.md §6-7): the unified
// event list backed by the audit read model (audit.QueryEvents, replacing the
// old legacy-projection listAudit route) plus the admin retention settings
// read, preview and update. Reads go through the read-only database
// capability; the update runs through the shared execution runner so the
// durable command ledger and the success/rejected audit events commit in the
// same transaction as the settings change. JSON bodies mirror
// web/src/features/audit/api.ts exactly (camelCase, numeric ids as strings).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/danielgtaylor/huma/v2"
)

// registerAuditRoutes mounts the consolidated audit operations. Main swaps the
// old /api/v1/audit-events registration for this registrar; the list
// OperationID stays listAuditEvents so the frozen OpenAPI contract is
// unchanged, while the handler function is queryAuditEvents (the previous Go
// name stays reserved by the legacy route until main removes it).
func (application *apiServer) registerAuditRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/audit-events", OperationID: "listAuditEvents"}, application.queryAuditEvents)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/admin/audit-settings", OperationID: "getAuditSettings"}, application.getAuditSettings)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/admin/audit-settings/preview", OperationID: "previewAuditRetention"}, application.previewAuditRetention)
	huma.Register(api, huma.Operation{Method: http.MethodPatch, Path: "/api/v1/admin/audit-settings", OperationID: "updateAuditSettings"}, application.updateAuditSettings)
}

// auditEventJSON mirrors the UI AuditEvent interface field-for-field. Numeric
// ids travel as strings so JavaScript numbers cannot silently lose precision;
// taskId/attemptId have no consolidated read-model source yet and stay absent
// until main supplies the target object fields.
type auditEventJSON struct {
	ID              string `json:"id"`
	CorrelationID   string `json:"correlationId,omitempty"`
	ActorType       string `json:"actorType"`
	ActorID         string `json:"actorId"`
	Action          string `json:"action"`
	Outcome         string `json:"outcome"`
	Phase           string `json:"phase,omitempty"`
	DomainRefType   string `json:"domainRefType,omitempty"`
	DomainRefID     string `json:"domainRefId,omitempty"`
	ClientCommandID string `json:"clientCommandId,omitempty"`
	RequestID       string `json:"requestId,omitempty"`
	TaskID          string `json:"taskId,omitempty"`
	AttemptID       string `json:"attemptId,omitempty"`
	CreatedAt       string `json:"createdAt"`
}

func auditEventJSONFrom(event audit.EventView) auditEventJSON {
	projected := auditEventJSON{
		ID:              strconv.FormatInt(event.ID, 10),
		CorrelationID:   event.CorrelationID,
		ActorType:       event.ActorType,
		ActorID:         strconv.FormatInt(event.ActorID, 10),
		Action:          event.Action,
		Outcome:         event.Outcome,
		Phase:           event.Phase,
		DomainRefType:   event.DomainRefType,
		ClientCommandID: event.ClientCommandID,
		RequestID:       event.RequestID,
		CreatedAt:       event.CreatedAt,
	}
	if event.DomainRefType != "" {
		projected.DomainRefID = strconv.FormatInt(event.DomainRefID, 10)
	}
	return projected
}

// auditCleanupStatusJSON mirrors AuditCleanupStatus; every field is nullable
// because a deployment that never ran cleanup must not look like a zero
// deletion count.
type auditCleanupStatusJSON struct {
	LastRunAt               *string `json:"lastRunAt"`
	LastSuccessCutoffAt     *string `json:"lastSuccessCutoffAt"`
	LastSuccessDeletedCount *int64  `json:"lastSuccessDeletedCount"`
	LastFailureAt           *string `json:"lastFailureAt"`
	LastErrorCode           *string `json:"lastErrorCode"`
}

// auditSettingsJSON mirrors the UI AuditSettings interface. updatedBy is the
// display projection of updated_by_type/updated_by_id, never a raw column dump.
type auditSettingsJSON struct {
	RetentionMonths    int                     `json:"retentionMonths"`
	MinRetentionMonths int                     `json:"minRetentionMonths"`
	RowVersion         int64                   `json:"rowVersion"`
	UpdatedAt          *string                 `json:"updatedAt"`
	UpdatedBy          *string                 `json:"updatedBy"`
	Cleanup            *auditCleanupStatusJSON `json:"cleanup"`
}

// auditSettingsPreviewJSON mirrors AuditSettingsPreview; the estimate is an
// impact outlook, never a deletion promise.
type auditSettingsPreviewJSON struct {
	RetentionMonths                int     `json:"retentionMonths"`
	CurrentRetentionMonths         int     `json:"currentRetentionMonths"`
	Shortening                     bool    `json:"shortening"`
	CutoffAt                       *string `json:"cutoffAt"`
	EstimatedExpirableEvents       int64   `json:"estimatedExpirableEvents"`
	EstimatedExpirableCorrelations int64   `json:"estimatedExpirableCorrelations"`
}

func auditNullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func auditCleanupStatusJSONFrom(status audit.CleanupStatus) *auditCleanupStatusJSON {
	var deletedCount *int64
	if status.LastSuccessCutoffAt != "" {
		deletedCount = &status.LastSuccessDeletedCount
	}
	return &auditCleanupStatusJSON{
		LastRunAt:               auditNullableString(status.LastRunAt),
		LastSuccessCutoffAt:     auditNullableString(status.LastSuccessCutoffAt),
		LastSuccessDeletedCount: deletedCount,
		LastFailureAt:           auditNullableString(status.LastFailureAt),
		LastErrorCode:           auditNullableString(status.LastErrorCode),
	}
}

// auditUpdatedByDisplay projects the retention row's updated_by columns into
// the UI's single display string: the username for users (falling back to the
// typed id when the user row is gone), "system" for the system principal and
// type:id for services. An empty updated_by (bootstrap-seeded row) stays null.
func (application *apiServer) auditUpdatedByDisplay(ctx context.Context, settings audit.Settings) *string {
	switch settings.UpdatedByType {
	case "":
		return nil
	case audit.ActorSystem:
		display := audit.ActorSystem
		return &display
	case audit.ActorUser:
		var username string
		if err := application.readAuthority().QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, settings.UpdatedByID).Scan(&username); err == nil && username != "" {
			return &username
		}
	}
	display := fmt.Sprintf("%s:%d", settings.UpdatedByType, settings.UpdatedByID)
	return &display
}

func (application *apiServer) queryAuditEvents(ctx context.Context, input *struct {
	Session       string `cookie:"__Host-quoin-session"`
	CorrelationID string `query:"correlationId"`
	ActorType     string `query:"actorType"`
	Action        string `query:"action"`
	Outcome       string `query:"outcome"`
	Since         string `query:"since"`
	Until         string `query:"until"`
	Cursor        string `query:"cursor"`
	Limit         int    `query:"limit"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []auditEventJSON `json:"items"`
		NextCursor string           `json:"nextCursor,omitempty"`
	} `json:"body"`
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取审计事件"); err != nil {
		return nil, err
	}
	// Zero-value filter fields are ignored by the read model, so blank query
	// strings never become criteria (audit.Filter contract). Reads go through
	// the read-only authority; only the runner's transaction ever writes.
	page, err := audit.QueryEvents(ctx, application.readAuthority(), audit.Filter{
		CorrelationID: input.CorrelationID,
		ActorType:     input.ActorType,
		Action:        input.Action,
		Outcome:       input.Outcome,
		Since:         input.Since,
		Until:         input.Until,
	}, input.Cursor, input.Limit)
	if err != nil {
		return nil, auditQueryError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []auditEventJSON `json:"items"`
			NextCursor string           `json:"nextCursor,omitempty"`
		} `json:"body"`
	}{CacheControl: "no-store"}
	output.Body.Items = make([]auditEventJSON, 0, len(page.Events))
	for _, event := range page.Events {
		output.Body.Items = append(output.Body.Items, auditEventJSONFrom(event))
	}
	output.Body.NextCursor = page.NextCursor
	return output, nil
}

// auditQueryError maps read-model rejections onto the frozen problem envelope.
func auditQueryError(err error) error {
	switch {
	case errors.Is(err, audit.ErrInvalidFilter):
		return problemUnprocessable("审计事件查询条件无效，请检查筛选值后重试。")
	case errors.Is(err, audit.ErrInvalidCursor):
		return problem(http.StatusBadRequest, "malformed_request", "分页游标无效，请刷新列表后重试。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法读取审计事件，请稍后重试。")
	}
}

func (application *apiServer) getAuditSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         auditSettingsJSON
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "读取审计保留设置"); err != nil {
		return nil, err
	}
	settings, err := audit.ReadSettings(ctx, application.readAuthority())
	if err != nil {
		// The bootstrap seeds the singleton; a missing row is an
		// infrastructure fault, not a 404 the admin could act on.
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取审计保留设置，请稍后重试。")
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         auditSettingsJSON
	}{CacheControl: "no-store"}
	output.Body = auditSettingsJSON{
		RetentionMonths:    settings.RetentionMonths,
		MinRetentionMonths: settings.MinRetentionMonths,
		RowVersion:         settings.RowVersion,
		UpdatedAt:          auditNullableString(settings.UpdatedAt),
		UpdatedBy:          application.auditUpdatedByDisplay(ctx, settings),
		Cleanup:            auditCleanupStatusJSONFrom(settings.Cleanup),
	}
	return output, nil
}

func (application *apiServer) previewAuditRetention(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		RetentionMonths int `json:"retentionMonths" minimum:"6"`
	}
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         auditSettingsPreviewJSON
}, error,
) {
	if _, err := application.authenticateAdmin(ctx, input.Session, "预览审计保留影响"); err != nil {
		return nil, err
	}
	// Preview is always supported — including while cleanup is disabled — so a
	// first-time enable can show its impact before anything is confirmed. The
	// estimate reads through the read-only authority.
	preview, err := audit.PreviewSettings(ctx, application.readAuthority(), input.Body.RetentionMonths, time.Now())
	if err != nil {
		if errors.Is(err, audit.ErrRetentionTooShort) {
			return nil, problemUnprocessable("保留期不能低于六个月，请调整后重试。")
		}
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法预览保留影响，请稍后重试。")
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         auditSettingsPreviewJSON
	}{CacheControl: "no-store"}
	output.Body = auditSettingsPreviewJSON{
		RetentionMonths:                preview.RetentionMonths,
		CurrentRetentionMonths:         preview.CurrentRetentionMonths,
		Shortening:                     preview.Shortening,
		CutoffAt:                       auditNullableString(preview.CutoffAt),
		EstimatedExpirableEvents:       preview.EstimatedExpirableEvents,
		EstimatedExpirableCorrelations: preview.EstimatedExpirableCorrelations,
	}
	return output, nil
}

// auditRetentionUpdate is the registered write operation behind PATCH
// /api/v1/admin/audit-settings; Authorize revalidates the acting session
// inside the runner transaction below.
const auditRetentionUpdateOperation = "audit.retention.update"

func (application *apiServer) updateAuditSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		RetentionMonths    int    `json:"retentionMonths" minimum:"6"`
		CleanupEnabled     *bool  `json:"cleanupEnabled,omitempty"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128"`
	}
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         auditSettingsJSON
}, error,
) {
	session, err := application.authenticateAdmin(ctx, input.Session, "更新审计保留设置")
	if err != nil {
		return nil, err
	}
	// The execution metadata comes from the admission middleware — required,
	// never synthesized: a request without it is an infrastructure contract
	// breach and fails closed instead of guessing an identity.
	meta, err := execution.Require(ctx)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
	}
	// The digest covers the non-secret semantic fields only; cleanupEnabled
	// participates only when the client actually sent it, so a replay of the
	// same request always reproduces the same digest (DATA-COMMAND-002).
	fields := map[string]any{
		"retentionMonths":    input.Body.RetentionMonths,
		"expectedRowVersion": input.Body.ExpectedRowVersion,
	}
	if input.Body.CleanupEnabled != nil {
		fields["cleanupEnabled"] = *input.Body.CleanupEnabled
	}
	digest := auth.DigestCommand(auditRetentionUpdateOperation, fields)

	registry := execution.NewRegistry()
	op, err := registry.Register(execution.Operation{
		Name:       auditRetentionUpdateOperation,
		Class:      execution.ClassWrite,
		ObjectType: "audit_retention",
		// Re-validate on the open transaction that the admission-resolved
		// session proof still names a current, enabled, initialized admin
		// session at an unchanged auth revision — a plain error (clean
		// rollback, no durable record) is the correct fate for a session
		// revoked or demoted between entry authentication and the commit.
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			return auth.VerifyExecutionSession(ctx, tx, "admin")
		},
	})
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
	}
	outcome, err := execution.Run(ctx, execution.NewRunner(application.db, registry, audit.NewWriter()), op,
		execution.Command{
			PrincipalType:   string(meta.Actor.Kind),
			PrincipalID:     meta.Actor.ID,
			ClientCommandID: input.Body.ClientCommandID,
			Digest:          digest,
		},
		func(tx *execution.Tx) (audit.Settings, execution.Change, error) {
			settings, err := audit.UpdateSettingsOn(ctx, tx, audit.SettingsUpdate{
				RetentionMonths:    input.Body.RetentionMonths,
				CleanupEnabled:     input.Body.CleanupEnabled,
				ExpectedRowVersion: input.Body.ExpectedRowVersion,
				ActorType:          audit.ActorUser,
				ActorID:            session.User.ID,
			})
			if err != nil {
				return audit.Settings{}, execution.Unchanged, auditSettingsRejection(err)
			}
			return settings, execution.Changed, nil
		},
		func(audit.Settings) int64 { return 1 },
	)
	if err != nil {
		return nil, auditSettingsCommandError(err)
	}
	output := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         auditSettingsJSON
	}{CacheControl: "no-store"}
	output.Body = auditSettingsJSON{
		RetentionMonths:    outcome.Result.RetentionMonths,
		MinRetentionMonths: outcome.Result.MinRetentionMonths,
		RowVersion:         outcome.Result.RowVersion,
		UpdatedAt:          auditNullableString(outcome.Result.UpdatedAt),
		UpdatedBy:          application.auditUpdatedByDisplay(ctx, outcome.Result),
		Cleanup:            auditCleanupStatusJSONFrom(outcome.Result.Cleanup),
	}
	return output, nil
}

// auditSettingsRejection classifies deterministic settings failures for the
// runner so they persist as rejected_known ledger rows plus rejected audit
// events; infrastructure errors pass through untouched and roll back cleanly.
func auditSettingsRejection(err error) error {
	switch {
	case errors.Is(err, audit.ErrSettingsConflict):
		return &execution.Rejection{Code: "row_version_conflict", ObjectID: 1}
	case errors.Is(err, audit.ErrCleanupRunning):
		// A fresh cleanup permit holds an armed cutoff snapshot; the policy
		// change would race that snapshot (retention.go ErrCleanupRunning).
		return &execution.Rejection{Code: "cleanup_running", ObjectID: 1}
	case errors.Is(err, audit.ErrRetentionTooShort):
		return &execution.Rejection{Code: "retention_too_short", ObjectID: 1}
	case errors.Is(err, audit.ErrInvalidSettings):
		return &execution.Rejection{Code: "validation_failed", ObjectID: 1}
	default:
		return err
	}
}

// auditSettingsCommandError maps runner outcomes onto the frozen problem
// envelope. The runner returns rejection values verbatim, so replay, reuse and
// conflict semantics match the rest of the admin command surface.
func auditSettingsCommandError(err error) error {
	var rejection *execution.Rejection
	switch {
	case errors.Is(err, execution.ErrCommandReused):
		problemErr := problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
		problemErr.Conflict = map[string]any{"code": "command_id_reused"}
		return problemErr
	case errors.As(err, &rejection):
		switch rejection.Code {
		case "row_version_conflict":
			problemErr := problem(http.StatusConflict, "row_version_conflict", "审计保留设置刚被其他操作修改，请刷新后重试。")
			problemErr.Conflict = map[string]any{"code": "row_version_conflict", "objectType": "audit_retention", "objectId": "1"}
			return problemErr
		case "cleanup_running":
			return problem(http.StatusConflict, "cleanup_running", "审计清理正在进行，待本轮结束后再调整保留设置。")
		case "retention_too_short":
			return problemUnprocessable("保留期不能低于六个月，请调整后重试。")
		default:
			return problemUnprocessable("请求字段不满足要求，请检查后重试。")
		}
	case errors.Is(err, auth.ErrActorChanged):
		// The in-transaction session revalidation failed: the acting session
		// was revoked, expired, disabled or demoted mid-command.
		return problem(http.StatusUnauthorized, "unauthenticated", "请重新登录。")
	default:
		return problem(http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。")
	}
}
