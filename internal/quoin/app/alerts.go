package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/secrets"
	"github.com/danielgtaylor/huma/v2"
)

// alertHandlers owns the /api/v1/alerts and /api/v1/alert-sources HTTP surface.

type createSourceInput struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		Key             string `json:"key" maxLength:"200"`
		Protocol        string `json:"protocol"`
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"200"`
	}
}

type createSourceOutput struct {
	Status int `header:"-"`
	Body   struct {
		SourceKey       string `json:"sourceKey"`
		CredentialID    string `json:"credentialId"`
		RevealAvailable bool   `json:"revealAvailable"`
		RevealHandle    string `json:"revealHandle,omitempty"`
	} `json:"body"`
}

func (application *apiServer) createAlertSource(ctx context.Context, input *createSourceInput) (*createSourceOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	if session.User.Role != "admin" {
		return nil, huma.Error403Forbidden("需要管理员权限")
	}
	if input.Body.Protocol != "alertmanager" {
		return nil, huma.Error400BadRequest("不支持的协议", fmt.Errorf("protocol must be alertmanager"))
	}
	key := strings.TrimSpace(input.Body.Key)
	if key == "" {
		return nil, huma.Error400BadRequest("告警源 key 不能为空", nil)
	}
	// Command replay: same clientCommandId + same digest returns the original
	// result; a different digest conflicts (HTTP-COMMAND-003). We keep a
	// small in-process map keyed by (session, command) because the frozen
	// client_commands table is in scope of a later ticket; the reveal path
	// still enforces one-time semantics via the secrets store.
	if previous, ok := application.commands.lookup(session.User.ID, input.Body.ClientCommandID); ok {
		if previous.digest != commandDigest(key, input.Body.Protocol) {
			return nil, huma.Error409Conflict("命令 ID 已被使用", fmt.Errorf("command_id_reused"))
		}
		output := createSourceOutput{Status: http.StatusCreated}
		output.Body.SourceKey = previous.result.SourceKey
		output.Body.CredentialID = strconv.FormatInt(previous.result.CredentialID, 10)
		if handle, credentialID, valid := application.reveals.Lookup(secrets.SessionDigest(input.Session), input.Body.ClientCommandID); valid {
			output.Body.RevealAvailable = true
			output.Body.RevealHandle = handle
			output.Body.CredentialID = strconv.FormatInt(credentialID, 10)
		}
		return &output, nil
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, huma.Error500InternalServerError("无法创建凭据", err)
	}
	digest := sha256.Sum256(raw)
	result, err := application.alerts.CreateSource(ctx, key, input.Body.Protocol, digest[:], session.User.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		if isUniqueViolation(err) {
			// A concurrent same-command request may have won the race: replay
			// the original result instead of reporting a conflict.
			if previous, ok := application.commands.lookup(session.User.ID, input.Body.ClientCommandID); ok && previous.digest == commandDigest(key, input.Body.Protocol) {
				output := createSourceOutput{Status: http.StatusCreated}
				output.Body.SourceKey = previous.result.SourceKey
				output.Body.CredentialID = strconv.FormatInt(previous.result.CredentialID, 10)
				if handle, credentialID, valid := application.reveals.Lookup(secrets.SessionDigest(input.Session), input.Body.ClientCommandID); valid {
					output.Body.RevealAvailable = true
					output.Body.RevealHandle = handle
					output.Body.CredentialID = strconv.FormatInt(credentialID, 10)
				}
				return &output, nil
			}
			return nil, huma.Error409Conflict("告警源已存在", err)
		}
		return nil, huma.Error500InternalServerError("无法创建告警源", err)
	}
	handle := application.reveals.Create(secrets.SessionDigest(input.Session), input.Body.ClientCommandID, result.CredentialID, base64.RawURLEncoding.EncodeToString(raw))
	application.commands.remember(session.User.ID, input.Body.ClientCommandID, commandDigest(key, input.Body.Protocol), result)
	output := createSourceOutput{Status: http.StatusCreated}
	output.Body.SourceKey = result.SourceKey
	output.Body.CredentialID = strconv.FormatInt(result.CredentialID, 10)
	output.Body.RevealAvailable = true
	output.Body.RevealHandle = handle
	return &output, nil
}

// revealAlertSourceCredential redeems a one-time handle (SEC-REVEAL-*).
func (application *apiServer) revealAlertSourceCredential(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		RevealHandle string `json:"revealHandle" minLength:"32" maxLength:"256"`
	}
}) (*struct {
	Body struct {
		CredentialID string `json:"credentialId"`
		BearerToken  string `json:"bearerToken"`
	}
}, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	if session.User.Role != "admin" {
		return nil, huma.Error403Forbidden("需要管理员权限")
	}
	bearer, credentialID, ok, sessionMismatch := application.reveals.Consume(input.Body.RevealHandle, secrets.SessionDigest(input.Session))
	if sessionMismatch {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	if !ok {
		return nil, huma.Error410Gone("一次性句柄已失效或已消费", nil)
	}
	// HTTP-COMMAND-008: the non-secret reveal audit event is written before
	// the raw bearer leaves the handler; no handle or raw value ever enters
	// the audit trail.
	if err := application.alerts.RecordRevealAudit(ctx, session.User.ID, credentialID, "success", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, huma.Error500InternalServerError("无法记录审计", err)
	}
	return &struct {
		Body struct {
			CredentialID string `json:"credentialId"`
			BearerToken  string `json:"bearerToken"`
		}
	}{Body: struct {
		CredentialID string `json:"credentialId"`
		BearerToken  string `json:"bearerToken"`
	}{CredentialID: strconv.FormatInt(credentialID, 10), BearerToken: bearer}}, nil
}

type alertSnapshotOutput struct {
	Body alerts.AlertSnapshot `json:"body"`
}

// asItems guarantees the OpenAPI array contract: an empty result set is an
// empty JSON array, never null (nil slice would serialize as null).
func asItems[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func (application *apiServer) listAlerts(ctx context.Context, input *struct {
	Session           string `cookie:"__Host-quoin-session"`
	State             string `query:"state" enum:"Firing,Resolved"`
	BusinessSystemKey string `query:"businessSystemKey"`
}) (*alertSnapshotOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	_ = session
	state := input.State
	if state == "" {
		state = "Firing"
	}
	snapshot, err := application.alerts.AlertSnapshot(ctx, state, input.BusinessSystemKey)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取告警列表", err)
	}
	return &alertSnapshotOutput{Body: snapshot}, nil
}

func (application *apiServer) getAlertOccurrence(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
}) (*struct {
	Body alerts.OccurrenceSummary `json:"body"`
}, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	id, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || id <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	occurrence, err := application.alerts.GetOccurrence(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("告警不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取告警", err)
	}
	return &struct {
		Body alerts.OccurrenceSummary `json:"body"`
	}{Body: occurrence}, nil
}

func (application *apiServer) listAlertObservations(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	OccurrenceID string `path:"occurrenceId"`
}) (*struct {
	Body struct {
		Items []alerts.Observation `json:"items"`
	} `json:"body"`
}, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	id, err := strconv.ParseInt(input.OccurrenceID, 10, 64)
	if err != nil || id <= 0 {
		return nil, huma.Error404NotFound("告警不存在", nil)
	}
	observations, err := application.alerts.ListObservations(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取观测", err)
	}
	return &struct {
		Body struct {
			Items []alerts.Observation `json:"items"`
		} `json:"body"`
	}{Body: struct {
		Items []alerts.Observation `json:"items"`
	}{Items: asItems(observations)}}, nil
}

func (application *apiServer) listIntakeIssues(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	Acknowledged bool   `query:"acknowledged"`
}) (*struct {
	Body struct {
		Items      []alerts.IntakeIssue `json:"items"`
		NextCursor string               `json:"nextCursor,omitempty"`
	} `json:"body"`
}, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	issues, err := application.alerts.ListIntakeIssues(ctx, input.Acknowledged)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取接入问题", err)
	}
	return &struct {
		Body struct {
			Items      []alerts.IntakeIssue `json:"items"`
			NextCursor string               `json:"nextCursor,omitempty"`
		} `json:"body"`
	}{Body: struct {
		Items      []alerts.IntakeIssue `json:"items"`
		NextCursor string               `json:"nextCursor,omitempty"`
	}{Items: asItems(issues)}}, nil
}

func (application *apiServer) listAlertSources(ctx context.Context, input *authInput) (*struct {
	Body struct {
		Items      []alerts.SourceSummary `json:"items"`
		NextCursor string                 `json:"nextCursor,omitempty"`
	} `json:"body"`
}, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	sources, err := application.alerts.ListSources(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取告警源", err)
	}
	return &struct {
		Body struct {
			Items      []alerts.SourceSummary `json:"items"`
			NextCursor string                 `json:"nextCursor,omitempty"`
		} `json:"body"`
	}{Body: struct {
		Items      []alerts.SourceSummary `json:"items"`
		NextCursor string                 `json:"nextCursor,omitempty"`
	}{Items: asItems(sources)}}, nil
}

func (application *apiServer) getAlertSource(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SourceKey string `path:"sourceKey"`
}) (*struct {
	Body alerts.SourceDetail `json:"body"`
}, error) {
	if _, err := application.auth.Authenticate(ctx, input.Session); err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	detail, err := application.alerts.GetSource(ctx, input.SourceKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("告警源不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取告警源", err)
	}
	return &struct {
		Body alerts.SourceDetail `json:"body"`
	}{Body: detail}, nil
}

func (application *apiServer) registerAlertRoutes(api huma.API) {
	// listAlerts declares 410 because the snapshot cursor contract
	// (HTTP-PAGE-006) expires; cursor pagination itself lands with the SSE
	// ticket, so no cursor is issued yet and 410 is never produced today.
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alerts", OperationID: "listAlerts", Errors: []int{http.StatusGone}}, application.listAlerts)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alerts/{occurrenceId}", OperationID: "getAlertOccurrence"}, application.getAlertOccurrence)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alerts/{occurrenceId}/observations", OperationID: "listAlertObservations"}, application.listAlertObservations)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-intake-issues", OperationID: "listAlertIntakeIssues"}, application.listIntakeIssues)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-intake-issues/{issueId}/acknowledge", OperationID: "acknowledgeIntakeIssue", Errors: []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests}}, application.acknowledgeIntakeIssue)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources", OperationID: "listAlertSources"}, application.listAlertSources)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources/{sourceKey}", OperationID: "getAlertSource"}, application.getAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources", OperationID: "createAlertSource", DefaultStatus: http.StatusCreated}, application.createAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/credentials/reveal", OperationID: "revealAlertSourceCredential"}, application.revealAlertSourceCredential)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources/{sourceKey}/credentials", OperationID: "listAlertSourceCredentials"}, application.listAlertSourceCredentials)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/rotate", OperationID: "rotateAlertSourceCredential"}, application.rotateAlertSourceCredential)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/credentials/{credentialId}/retire", OperationID: "retireAlertSourceCredential"}, application.retireAlertSourceCredential)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/{sourceKey}/disable", OperationID: "disableAlertSource"}, application.disableAlertSource)
}

// acknowledgeIntakeIssue is the Admin-only, one-way sticky confirmation
// (DATA-ALERT-011): stale expectedRowVersion returns 409, an unknown issue
// 404, Operator gets 403.
func (application *apiServer) acknowledgeIntakeIssue(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	IssueID string `path:"issueId"`
	Body    struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}) (*struct {
	Status int `header:"-"`
}, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	if session.User.Role != "admin" {
		return nil, huma.Error403Forbidden("需要管理员权限")
	}
	issueID, err := strconv.ParseInt(input.IssueID, 10, 64)
	if err != nil || issueID <= 0 {
		return nil, huma.Error404NotFound("接入问题不存在", nil)
	}
	applied, err := application.alerts.AcknowledgeIntakeIssue(ctx, issueID, session.User.ID, input.Body.ExpectedRowVersion, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, huma.Error500InternalServerError("无法确认接入问题", err)
	}
	if !applied {
		// Either the expected row version is stale or the issue is already
		// acknowledged — both are deterministic conflicts for the caller.
		return nil, huma.Error409Conflict("接入问题版本已变化，请刷新后重试", fmt.Errorf("row_version_conflict"))
	}
	return &struct {
		Status int `header:"-"`
	}{Status: http.StatusNoContent}, nil
}

func isUniqueViolation(err error) bool {
	message := err.Error()
	// Only the source_key uniqueness maps to 409; CHECK/trigger failures are
	// internal faults and must not be reported as "already exists".
	return strings.Contains(message, "UNIQUE constraint failed") || strings.Contains(message, "UNIQUE")
}

// commandDigest is the non-secret semantic digest for a create-source command.
func commandDigest(key, protocol string) string {
	return alertSourceCommandDigest("create", key, protocol)
}

func alertSourceCommandDigest(operation, sourceKey, subject string) string {
	sum := sha256.Sum256([]byte("alert_source." + operation + ":" + sourceKey + ":" + subject))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type alertSourceCommandInput struct {
	Session   string `cookie:"__Host-quoin-session"`
	SourceKey string `path:"sourceKey"`
	Body      struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}

type rotateAlertSourceCredentialInput struct {
	Session   string `cookie:"__Host-quoin-session"`
	SourceKey string `path:"sourceKey"`
	Body      struct {
		ClientCommandID string `json:"clientCommandId" minLength:"8" maxLength:"128"`
	}
}

type retireAlertCredentialInput struct {
	Session      string `cookie:"__Host-quoin-session"`
	SourceKey    string `path:"sourceKey"`
	CredentialID string `path:"credentialId"`
	Body         struct {
		ClientCommandID    string `json:"clientCommandId" minLength:"8" maxLength:"128"`
		ExpectedRowVersion int64  `json:"expectedRowVersion" minimum:"1"`
	}
}

func (application *apiServer) listAlertSourceCredentials(ctx context.Context, input *struct {
	Session   string `cookie:"__Host-quoin-session"`
	SourceKey string `path:"sourceKey"`
	Cursor    string `query:"cursor"`
	Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}) (*struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []alerts.CredentialSummary `json:"items"`
		NextCursor string                     `json:"nextCursor,omitempty"`
	} `json:"body"`
}, error) {
	if _, err := application.authenticateFull(ctx, input.Session, "读取告警源凭据"); err != nil {
		return nil, err
	}
	if _, err := application.alerts.GetSource(ctx, input.SourceKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("告警源不存在", nil)
		}
		return nil, huma.Error500InternalServerError("无法读取告警源", err)
	}
	items, err := application.alerts.ListCredentials(ctx, input.SourceKey)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取告警源凭据", err)
	}
	limit := input.Limit
	if limit == 0 {
		limit = 50
	}
	start := 0
	if input.Cursor != "" {
		for start < len(items) && items[start].ID != input.Cursor {
			start++
		}
		if start == len(items) {
			return nil, huma.Error400BadRequest("无效的游标", nil)
		}
		start++
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	out := &struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Items      []alerts.CredentialSummary `json:"items"`
			NextCursor string                     `json:"nextCursor,omitempty"`
		} `json:"body"`
	}{CacheControl: "no-store"}
	out.Body.Items = asItems(items[start:end])
	if end < len(items) {
		out.Body.NextCursor = items[end-1].ID
	}
	return out, nil
}

func (application *apiServer) rotateAlertSourceCredential(ctx context.Context, input *rotateAlertSourceCredentialInput) (*createSourceOutput, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "轮换告警源凭据")
	if err != nil {
		return nil, err
	}
	commandDigest := alertSourceCommandDigest("rotate", input.SourceKey, "")
	if previous, ok := application.commands.lookup(session.User.ID, input.Body.ClientCommandID); ok {
		if previous.digest != commandDigest {
			return nil, huma.Error409Conflict("命令 ID 已被使用", nil)
		}
		out := &createSourceOutput{Status: http.StatusOK}
		out.Body.SourceKey, out.Body.CredentialID = previous.result.SourceKey, strconv.FormatInt(previous.result.CredentialID, 10)
		if handle, credentialID, valid := application.reveals.Lookup(secrets.SessionDigest(input.Session), input.Body.ClientCommandID); valid {
			out.Body.RevealAvailable, out.Body.RevealHandle, out.Body.CredentialID = true, handle, strconv.FormatInt(credentialID, 10)
		}
		return out, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, huma.Error500InternalServerError("无法创建凭据", err)
	}
	digest := sha256.Sum256(raw)
	result, err := application.alerts.RotateCredential(ctx, input.SourceKey, digest[:], session.User.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("告警源不存在", err)
	}
	if err != nil {
		return nil, huma.Error409Conflict("无法轮换告警源凭据", err)
	}
	handle := application.reveals.Create(secrets.SessionDigest(input.Session), input.Body.ClientCommandID, result.CredentialID, base64.RawURLEncoding.EncodeToString(raw))
	application.commands.remember(session.User.ID, input.Body.ClientCommandID, commandDigest, result)
	out := &createSourceOutput{Status: http.StatusOK}
	out.Body.SourceKey, out.Body.CredentialID, out.Body.RevealAvailable, out.Body.RevealHandle = result.SourceKey, strconv.FormatInt(result.CredentialID, 10), true, handle
	return out, nil
}

func (application *apiServer) retireAlertSourceCredential(ctx context.Context, input *retireAlertCredentialInput) (*struct {
	Body alerts.CredentialSummary `json:"body"`
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "退休告警源凭据")
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(input.CredentialID, 10, 64)
	if err != nil || id < 1 {
		return nil, problemUnprocessable("credentialId 必须是十进制 locator。")
	}
	commandDigest := alertSourceCommandDigest("retire", input.SourceKey, input.CredentialID)
	if previous, ok := application.commands.lookup(session.User.ID, input.Body.ClientCommandID); ok {
		if previous.digest != commandDigest || previous.credential == nil {
			return nil, huma.Error409Conflict("命令 ID 已被使用", nil)
		}
		return &struct {
			Body alerts.CredentialSummary `json:"body"`
		}{Body: *previous.credential}, nil
	}
	ok, err := application.alerts.RetireCredential(ctx, input.SourceKey, id, input.Body.ExpectedRowVersion, session.User.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, huma.Error500InternalServerError("无法退休告警源凭据", err)
	}
	if !ok {
		return nil, huma.Error409Conflict("告警源凭据已变化", nil)
	}
	credentials, err := application.alerts.ListCredentials(ctx, input.SourceKey)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取告警源凭据", err)
	}
	for _, credential := range credentials {
		if credential.ID == input.CredentialID {
			application.commands.rememberCredential(session.User.ID, input.Body.ClientCommandID, commandDigest, credential)
			return &struct {
				Body alerts.CredentialSummary `json:"body"`
			}{Body: credential}, nil
		}
	}
	return nil, huma.Error404NotFound("告警源凭据不存在", nil)
}

func (application *apiServer) disableAlertSource(ctx context.Context, input *alertSourceCommandInput) (*struct {
	Body alerts.SourceDetail `json:"body"`
}, error) {
	session, err := application.authenticateAdmin(ctx, input.Session, "停用告警源")
	if err != nil {
		return nil, err
	}
	ok, err := application.alerts.SetSourceEnabled(ctx, input.SourceKey, false, input.Body.ExpectedRowVersion, session.User.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("告警源不存在", err)
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("无法停用告警源", err)
	}
	if !ok {
		return nil, huma.Error409Conflict("告警源已变化", nil)
	}
	detail, err := application.alerts.GetSource(ctx, input.SourceKey)
	if err != nil {
		return nil, huma.Error500InternalServerError("无法读取告警源", err)
	}
	return &struct {
		Body alerts.SourceDetail `json:"body"`
	}{Body: detail}, nil
}
