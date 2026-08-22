package appinvestigation

// Attachment HTTP surface (T14): the multipart upload streams the file part
// straight into the staging writer (HTTP-FILE-001 forbids buffering the
// body whole), so it owns the response head like the artifact download
// route instead of going through Huma's body readers. The GET metadata
// route is a plain Huma handler.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/investigation"
)

// maxMultipartFields bounds the form parts one upload may carry (the two
// frozen fields plus a small margin; extra file parts are rejected).
const maxMultipartFields = 8

// ServeUpload is the raw POST /api/v1/investigation-attachments handler
// (registered on the mux by the app package next to the Huma surface).
func (handler *Handler) ServeUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeAttachmentProblem(writer, http.StatusMethodNotAllowed, "malformed_request", "上传只接受 POST 请求。")
		return
	}
	cookie := ""
	for _, candidate := range request.Cookies() {
		if candidate.Name == "__Host-quoin-session" {
			cookie = candidate.Value
		}
	}
	if cookie == "" {
		writeAttachmentProblem(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传附件。")
		return
	}
	principalID, err := handler.Authenticate(request.Context(), cookie)
	if err != nil {
		writeAttachmentProblem(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传附件。")
		return
	}
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeAttachmentProblem(writer, http.StatusUnsupportedMediaType, "unsupported_media", "附件上传需要 multipart/form-data 请求。")
		return
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	var commandID, filename string
	var staged *investigation.StagedBody
	var stageErr error
	parts := 0
	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			staged.Abort()
			writeAttachmentProblem(writer, http.StatusBadRequest, "malformed_request", "上传内容无法解析，请重试。")
			return
		}
		parts++
		if parts > maxMultipartFields {
			part.Close()
			staged.Abort()
			writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "上传表单字段过多。")
			return
		}
		name := part.FormName()
		if name == "clientCommandId" {
			body, readErr := io.ReadAll(io.LimitReader(part, 128+1))
			part.Close()
			if readErr != nil || len(body) > 128 {
				staged.Abort()
				writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "clientCommandId 无效。")
				return
			}
			commandID = string(body)
			continue
		}
		if name == "file" {
			if staged != nil {
				part.Close()
				staged.Abort()
				writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "一次只能上传一个附件文件。")
				return
			}
			// The part streams straight into staging right here: multipart
			// order is client-controlled, so the body cannot wait for the
			// command field (HTTP-FILE-001: no whole-body buffering).
			filename = part.FileName()
			staged, stageErr = handler.Service.BeginAttachment(request.Context(), principalID, filename, part)
			part.Close()
			if stageErr != nil {
				writeStageError(writer, stageErr)
				return
			}
			continue
		}
		part.Close()
	}
	if staged == nil {
		writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "缺少附件文件。")
		return
	}
	if !validCommandID(commandID) {
		staged.Abort()
		writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "clientCommandId 必须是 8–128 位字母、数字、下划线或连字符。")
		return
	}
	summary, err := handler.Service.CommitAttachment(request.Context(), principalID, commandID, filename, staged)
	if err != nil {
		staged.Abort()
		writeStageError(writer, err)
		return
	}
	body, err := json.Marshal(summary)
	if err != nil {
		writeAttachmentProblem(writer, http.StatusInternalServerError, "unavailable", "暂时无法保存附件，请重试。")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusCreated)
	writer.Write(body)
}

func writeStageError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, investigation.ErrAttachmentTooLarge):
		writeAttachmentProblem(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "附件超过大小上限（默认 10 MiB，可与管理员确认部署边界）。")
	case errors.Is(err, investigation.ErrAttachmentText):
		writeAttachmentProblem(writer, http.StatusUnprocessableEntity, "validation_failed", "附件必须是有效 UTF-8 文本且不含 NUL 字符。")
	case errors.Is(err, investigation.ErrCommandReused):
		conflict := map[string]any{"code": "command_id_reused"}
		writeAttachmentProblemJSON(writer, http.StatusConflict, "command_id_reused", "命令 ID 已被其他请求使用，请重新上传。", conflict)
	default:
		writeAttachmentProblem(writer, http.StatusInternalServerError, "unavailable", "暂时无法保存附件，请重试。")
	}
}

func validCommandID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, symbol := range value {
		switch {
		case symbol >= 'a' && symbol <= 'z':
		case symbol >= 'A' && symbol <= 'Z':
		case symbol >= '0' && symbol <= '9':
		case symbol == '_' || symbol == '-':
		default:
			return false
		}
	}
	return true
}

type attachmentProblem struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Conflict  map[string]any `json:"conflict,omitempty"`
}

func writeAttachmentProblem(writer http.ResponseWriter, status int, code, message string) {
	writeAttachmentProblemJSON(writer, status, code, message, nil)
}

func writeAttachmentProblemJSON(writer http.ResponseWriter, status int, code, message string, conflict map[string]any) {
	body, _ := json.Marshal(attachmentProblem{Code: code, Message: message, Retryable: status >= 500 || status == 429, Conflict: conflict})
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	writer.Write(body)
}

// getInvestigationAttachment returns one staging attachment's metadata for
// the uploading principal (foreign attachments behave as not found).
func (handler *Handler) getInvestigationAttachment(ctx context.Context, input *struct {
	Session      string `cookie:"__Host-quoin-session"`
	AttachmentID string `path:"attachmentId"`
}) (*attachmentBody, error) {
	principalID, err := handler.principal(ctx, input.Session)
	if err != nil {
		return nil, err
	}
	attachmentID, err := strconv.ParseInt(input.AttachmentID, 10, 64)
	if err != nil || attachmentID <= 0 {
		return nil, problem(http.StatusNotFound, "not_found", "附件不存在。")
	}
	summary, err := handler.Service.AttachmentFor(ctx, principalID, attachmentID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			return nil, problem(http.StatusNotFound, "not_found", "附件不存在。")
		}
		return nil, problem(http.StatusInternalServerError, "unavailable", "暂时无法读取附件，请重试。")
	}
	return &attachmentBody{Body: summary}, nil
}

type attachmentBody struct {
	Body investigation.AttachmentView `json:"body"`
}
