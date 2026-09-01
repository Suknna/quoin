package app

// Backup HTTP surface (T32). The service keeps the state machine, manifest and
// filesystem authority; this file only authenticates Admin callers and maps
// its projections to the frozen HTTP shapes.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/danielgtaylor/huma/v2"
)

type backupSummary struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	Stage          string  `json:"stage"`
	TriggerKind    string  `json:"triggerKind"`
	ExecutionMode  string  `json:"executionMode"`
	ScheduledFor   *string `json:"scheduledFor"`
	RowVersion     int64   `json:"rowVersion"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	StartedAt      *string `json:"startedAt"`
	CompletedAt    *string `json:"completedAt"`
	DBSHA256       *string `json:"dbSha256"`
	ManifestSHA256 *string `json:"manifestSha256"`
	ArtifactCount  *int    `json:"artifactCount"`
	SizeBytes      int64   `json:"sizeBytes"`
	ErrorCode      *string `json:"errorCode"`
	Retryable      *bool   `json:"retryable"`
	ErrorDetail    *string `json:"errorDetail"`
}
type backupSettings struct {
	Enabled        bool    `json:"enabled"`
	ScheduleCron   *string `json:"scheduleCron,omitempty"`
	Timezone       string  `json:"timezone"`
	BackupTarget   string  `json:"backupTarget"`
	RetentionCount int64   `json:"retentionCount"`
	RowVersion     int64   `json:"rowVersion"`
}
type artifactRetentionSettings struct {
	GeneratedRetentionDays int64 `json:"generatedRetentionDays"`
	RowVersion             int64 `json:"rowVersion"`
}

// optionalString preserves JSON field presence: an omitted scheduleCron leaves
// the schedule unchanged, while an explicit null clears it.
type optionalString struct {
	Present bool
	Value   *string
}

func (value *optionalString) UnmarshalJSON(body []byte) error {
	value.Present = true
	if string(body) == "null" {
		value.Value = nil
		return nil
	}
	var decoded string
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	value.Value = &decoded
	return nil
}

func (optionalString) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Nullable: true}
}

func (value optionalString) updateValue() *string {
	if !value.Present {
		return nil
	}
	if value.Value != nil {
		return value.Value
	}
	cleared := ""
	return &cleared
}

const (
	backupCursorSort         = "id_desc"
	backupCursorFilterDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // SHA-256("")
)

type backupCursor struct {
	OperationID  string `json:"operationId"`
	Sort         string `json:"sort"`
	FilterDigest string `json:"filterDigest"`
	BeforeID     int64  `json:"beforeId"`
}

type backupPageOutput struct {
	Body struct {
		Items      []backupSummary `json:"items"`
		NextCursor string          `json:"nextCursor,omitempty"`
	} `json:"body"`
}
type backupOutput struct {
	Body backupSummary `json:"body"`
}
type backupSettingsOutput struct {
	Body backupSettings `json:"body"`
}
type artifactRetentionOutput struct {
	Body artifactRetentionSettings `json:"body"`
}

func (application *apiServer) SetBackupService(service *backup.Service) {
	application.backups = service
}
func (application *apiServer) registerBackupRoutes(api huma.API) {
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/backups", OperationID: "listBackups"}, application.listBackups)
	huma.Register(api, huma.Operation{Method: http.MethodPost, Path: "/api/v1/backups", OperationID: "triggerBackup", DefaultStatus: http.StatusAccepted}, application.triggerBackup)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/backups/{backupId}", OperationID: "getBackup"}, application.getBackup)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/backups/settings", OperationID: "getBackupSettings"}, application.getBackupSettings)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/backups/settings", OperationID: "updateBackupSettings"}, application.updateBackupSettings)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/api/v1/artifacts/retention-settings", OperationID: "getArtifactRetentionSettings"}, application.getArtifactRetentionSettings)
	huma.Register(api, huma.Operation{Method: http.MethodPut, Path: "/api/v1/artifacts/retention-settings", OperationID: "updateArtifactRetentionSettings"}, application.updateArtifactRetentionSettings)
}
func (application *apiServer) authorizeBackup(ctx context.Context, cookie, action string) (auth.Session, error) {
	var session auth.Session
	var err error
	if application.backupAuthorize != nil {
		session, err = application.backupAuthorize(ctx, cookie, action)
	} else {
		session, err = application.authenticateAdmin(ctx, cookie, action)
	}
	if err != nil {
		return session, err
	}
	if application.db == nil {
		return session, nil
	}
	var active int
	if err = application.db.QueryRowContext(ctx, `SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil {
		return session, problem(http.StatusServiceUnavailable, "unavailable", "无法确认维护状态，请稍后重试。")
	}
	if active != 0 {
		return session, problem(http.StatusServiceUnavailable, "maintenance_active", "系统维护中，暂时无法下载备份归档。")
	}
	return session, nil
}

func (application *apiServer) backupService() (*backup.Service, error) {
	if application.backups == nil {
		return nil, problem(http.StatusServiceUnavailable, "unavailable", "备份服务暂不可用，请稍后重试。")
	}
	return application.backups, nil
}
func projectBackup(value backup.Summary) backupSummary {
	return backupSummary{ID: value.ID, Status: value.Status, Stage: value.Stage, TriggerKind: value.TriggerKind, ExecutionMode: value.ExecutionMode, ScheduledFor: value.ScheduledFor, RowVersion: value.RowVersion, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, StartedAt: value.StartedAt, CompletedAt: value.CompletedAt, DBSHA256: value.DBSHA256, ManifestSHA256: value.ManifestSHA256, ArtifactCount: value.ArtifactCount, SizeBytes: value.SizeBytes, ErrorCode: value.ErrorCode, Retryable: value.Retryable, ErrorDetail: value.ErrorDetail}
}
func projectBackupSettings(value backup.Settings) backupSettings {
	return backupSettings{Enabled: value.Enabled, ScheduleCron: value.ScheduleCron, Timezone: value.Timezone, BackupTarget: value.BackupTarget, RetentionCount: value.RetentionCount, RowVersion: value.RowVersion}
}
func (application *apiServer) listBackups(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Cursor  string `query:"cursor"`
	// String preserves the distinction between an omitted limit (default) and
	// an explicit zero (invalid); Huma does not support pointer query params.
	Limit string `query:"limit"`
}) (*backupPageOutput, error) {
	if _, err := application.authorizeBackup(ctx, input.Session, "读取备份记录"); err != nil {
		return nil, err
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	limit := 50
	if input.Limit != "" {
		parsed, parseErr := strconv.Atoi(input.Limit)
		if parseErr != nil || parsed < 1 || parsed > 200 {
			return nil, problem(http.StatusBadRequest, "malformed_request", "分页大小必须在 1 到 200 之间。")
		}
		limit = parsed
	}
	var beforeID int64
	if input.Cursor != "" {
		cursor, cursorErr := application.decodeBackupCursor(input.Cursor)
		if cursorErr != nil || cursor.OperationID != "listBackups" || cursor.Sort != backupCursorSort || cursor.FilterDigest != backupCursorFilterDigest || cursor.BeforeID < 1 {
			return nil, problem(http.StatusBadRequest, "malformed_request", "分页游标无效。")
		}
		beforeID = cursor.BeforeID
	}
	values, next, err := service.ListPage(ctx, beforeID, limit)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法读取备份记录。")
	}
	out := &backupPageOutput{}
	out.Body.Items = make([]backupSummary, 0, len(values))
	for _, value := range values {
		out.Body.Items = append(out.Body.Items, projectBackup(value))
	}
	if next != nil {
		cursor, encodeErr := application.encodeBackupCursor(backupCursor{OperationID: "listBackups", Sort: backupCursorSort, FilterDigest: backupCursorFilterDigest, BeforeID: *next})
		if encodeErr != nil {
			return nil, problem(http.StatusInternalServerError, "unavailable", "无法创建分页游标。")
		}
		out.Body.NextCursor = cursor
	}
	return out, nil
}

// Backup cursors are opaque HMAC-bound continuation capabilities. Their
// operation ID is part of the signed body so a token cannot cross a route.
func (application *apiServer) encodeBackupCursor(cursor backupCursor) (string, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	key, err := application.rootKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (application *apiServer) decodeBackupCursor(raw string) (backupCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return backupCursor{}, errors.New("invalid cursor framing")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return backupCursor{}, err
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return backupCursor{}, err
	}
	key, err := application.rootKey()
	if err != nil {
		return backupCursor{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return backupCursor{}, errors.New("cursor signature mismatch")
	}
	var cursor backupCursor
	if err = json.Unmarshal(body, &cursor); err != nil {
		return backupCursor{}, err
	}
	return cursor, nil
}
func (application *apiServer) triggerBackup(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID string `json:"clientCommandId"`
	}
}) (*backupOutput, error) {
	session, err := application.authorizeBackup(ctx, input.Session, "立即备份")
	if err != nil {
		return nil, err
	}
	if !backup.ValidCommandID(input.Body.ClientCommandID) {
		return nil, problem(http.StatusBadRequest, "malformed_request", "客户端命令 ID 必须为 8 到 128 个 ASCII 字母、数字、下划线或连字符。")
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	value, err := service.QueueManual(ctx, session.User.ID, input.Body.ClientCommandID)
	if errors.Is(err, backup.ErrActive) {
		conflict := problem(http.StatusConflict, "active_conflict", "已有正在等待或执行的备份。")
		var active *backup.ActiveError
		if errors.As(err, &active) {
			conflict.Conflict = map[string]any{"code": "active_conflict", "objectType": "backup", "objectId": active.ID}
		}
		return nil, conflict
	}
	if errors.Is(err, backup.ErrCommandReused) {
		return nil, problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
	}
	if errors.Is(err, backup.ErrActorUnauthorized) {
		return nil, problem(http.StatusForbidden, "forbidden", "管理员权限已变化，请重新登录。")
	}
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法创建备份任务。")
	}
	go func() { _, _ = service.Run(context.Background(), value.ID) }()
	return &backupOutput{Body: projectBackup(value)}, nil
}
func (application *apiServer) getBackup(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	ID      string `path:"backupId"`
}) (*backupOutput, error) {
	if _, err := application.authorizeBackup(ctx, input.Session, "读取备份记录"); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(input.ID, 10, 64)
	if err != nil || id < 1 {
		return nil, huma.Error404NotFound("备份记录不存在", nil)
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	value, err := service.Get(ctx, id)
	if errors.Is(err, backup.ErrNotFound) {
		return nil, huma.Error404NotFound("备份记录不存在", nil)
	}
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法读取备份记录。")
	}
	return &backupOutput{Body: projectBackup(value)}, nil
}
func (application *apiServer) getBackupSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*backupSettingsOutput, error) {
	if _, err := application.authorizeBackup(ctx, input.Session, "读取备份设置"); err != nil {
		return nil, err
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	value, err := service.Settings(ctx)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法读取备份设置。")
	}
	return &backupSettingsOutput{Body: projectBackupSettings(value)}, nil
}
func (application *apiServer) updateBackupSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID    string         `json:"clientCommandId"`
		ExpectedRowVersion int64          `json:"expectedRowVersion"`
		Enabled            *bool          `json:"enabled,omitempty"`
		ScheduleCron       optionalString `json:"scheduleCron,omitempty"`
		Timezone           *string        `json:"timezone,omitempty"`
		RetentionCount     *int64         `json:"retentionCount,omitempty"`
	}
}) (*backupSettingsOutput, error) {
	session, err := application.authorizeBackup(ctx, input.Session, "更新备份设置")
	if err != nil {
		return nil, err
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	if !backup.ValidCommandID(input.Body.ClientCommandID) {
		return nil, problem(http.StatusBadRequest, "malformed_request", "客户端命令 ID 必须为 8 到 128 个 ASCII 字母、数字、下划线或连字符。")
	}
	value, err := service.UpdateSettingsCommand(ctx, session.User.ID, input.Body.ExpectedRowVersion, input.Body.ClientCommandID, input.Body.Enabled, input.Body.ScheduleCron.updateValue(), input.Body.Timezone, input.Body.RetentionCount)
	if errors.Is(err, backup.ErrCommandReused) {
		return nil, problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
	}
	if errors.Is(err, backup.ErrActorUnauthorized) {
		return nil, problem(http.StatusForbidden, "forbidden", "管理员权限已变化，请重新登录。")
	}
	if errors.Is(err, backup.ErrRowVersionConflict) {
		return nil, problem(http.StatusConflict, "row_version_conflict", "设置已被其他管理员更新，请刷新后重试。")
	}
	if errors.Is(err, backup.ErrNoSettingsChange) || errors.Is(err, backup.ErrInvalidSettings) {
		return nil, problemUnprocessable("备份设置不合法或没有变化。")
	}
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法保存备份设置。")
	}
	return &backupSettingsOutput{Body: projectBackupSettings(value)}, nil
}
func (application *apiServer) getArtifactRetentionSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
}) (*artifactRetentionOutput, error) {
	if _, err := application.authorizeBackup(ctx, input.Session, "读取产物保留设置"); err != nil {
		return nil, err
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	value, err := service.ArtifactRetention(ctx)
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法读取产物保留设置。")
	}
	return &artifactRetentionOutput{Body: artifactRetentionSettings{value.GeneratedRetentionDays, value.RowVersion}}, nil
}
func (application *apiServer) updateArtifactRetentionSettings(ctx context.Context, input *struct {
	Session string `cookie:"__Host-quoin-session"`
	Body    struct {
		ClientCommandID        string `json:"clientCommandId"`
		ExpectedRowVersion     int64  `json:"expectedRowVersion"`
		GeneratedRetentionDays int64  `json:"generatedRetentionDays"`
	}
}) (*artifactRetentionOutput, error) {
	session, err := application.authorizeBackup(ctx, input.Session, "更新产物保留设置")
	if err != nil {
		return nil, err
	}
	service, err := application.backupService()
	if err != nil {
		return nil, err
	}
	if !backup.ValidCommandID(input.Body.ClientCommandID) {
		return nil, problem(http.StatusBadRequest, "malformed_request", "客户端命令 ID 必须为 8 到 128 个 ASCII 字母、数字、下划线或连字符。")
	}
	value, err := service.UpdateArtifactRetentionCommand(ctx, session.User.ID, input.Body.ExpectedRowVersion, input.Body.GeneratedRetentionDays, input.Body.ClientCommandID)
	if errors.Is(err, backup.ErrCommandReused) {
		return nil, problem(http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新发起操作。")
	}
	if errors.Is(err, backup.ErrActorUnauthorized) {
		return nil, problem(http.StatusForbidden, "forbidden", "管理员权限已变化，请重新登录。")
	}
	if errors.Is(err, backup.ErrRowVersionConflict) {
		return nil, problem(http.StatusConflict, "row_version_conflict", "设置已被其他管理员更新，请刷新后重试。")
	}
	if errors.Is(err, backup.ErrNoSettingsChange) || errors.Is(err, backup.ErrInvalidSettings) {
		return nil, problemUnprocessable("产物保留设置不合法或没有变化。")
	}
	if err != nil {
		return nil, problem(http.StatusInternalServerError, "unavailable", "无法保存产物保留设置。")
	}
	return &artifactRetentionOutput{Body: artifactRetentionSettings{value.GeneratedRetentionDays, value.RowVersion}}, nil
}
