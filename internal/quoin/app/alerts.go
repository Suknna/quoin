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

func (application *apiServer) listAlerts(ctx context.Context, input *authInput) (*alertSnapshotOutput, error) {
	session, err := application.auth.Authenticate(ctx, input.Session)
	if err != nil {
		return nil, huma.Error401Unauthorized("请重新登录")
	}
	_ = session
	snapshot, err := application.alerts.AlertSnapshot(ctx)
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
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources", OperationID: "listAlertSources"}, application.listAlertSources)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/alert-sources/{sourceKey}", OperationID: "getAlertSource"}, application.getAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources", OperationID: "createAlertSource", DefaultStatus: http.StatusCreated}, application.createAlertSource)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/alert-sources/credentials/reveal", OperationID: "revealAlertSourceCredential"}, application.revealAlertSourceCredential)
}

func isUniqueViolation(err error) bool {
	message := err.Error()
	// Only the source_key uniqueness maps to 409; CHECK/trigger failures are
	// internal faults and must not be reported as "already exists".
	return strings.Contains(message, "UNIQUE constraint failed") || strings.Contains(message, "UNIQUE")
}

// commandDigest is the non-secret semantic digest for a create-source command.
func commandDigest(key, protocol string) string {
	payload := fmt.Sprintf("alert_source.create:%s:%s", key, protocol)
	sum := sha256.Sum256([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
