package appconfig

// The two strict-YAML multipart uploads own their response heads like the
// attachment staging route (HTTP-FILE-001): parts are read streaming, the
// document byte limit is enforced while bytes arrive (413), field errors
// render the complete frozen fieldErrors list (422) and the response is
// always the problem+json envelope or the frozen detail JSON.

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/businesssystem"
	"github.com/Suknna/quoin/internal/quoin/config"
)

// configLimitBytes resolves the deployment document boundary (default 10 MiB,
// QUOIN_CONFIG_LIMIT_BYTES; CFG-YAML-002 / HTTP-FILE-002).
func configLimitBytes() int64 {
	if raw := os.Getenv("QUOIN_CONFIG_LIMIT_BYTES"); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
			return value
		}
	}
	return config.DefaultMaxDocumentBytes
}

const maxMultipartFields = 12

type uploadProblem struct {
	Code        string              `json:"code"`
	Message     string              `json:"message"`
	Retryable   bool                `json:"retryable"`
	FieldErrors []config.FieldError `json:"fieldErrors,omitempty"`
}

func writeProblemJSON(writer http.ResponseWriter, status int, code, message string, fieldErrors []config.FieldError) {
	body, _ := json.Marshal(uploadProblem{Code: code, Message: message, Retryable: status >= 500 || status == 429, FieldErrors: fieldErrors})
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	writer.Write(body)
}

func writeDetailJSON(writer http.ResponseWriter, status int, detail any) {
	body, err := json.Marshal(detail)
	if err != nil {
		writeProblemJSON(writer, http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	writer.Write(body)
}

func sessionCookieOf(request *http.Request) string {
	for _, candidate := range request.Cookies() {
		if candidate.Name == "__Host-quoin-session" {
			return candidate.Value
		}
	}
	return ""
}

// ServeBusinessSystemUpload handles POST /api/v1/business-systems
// (uploadBusinessSystemConfig).
func (handler *Handler) ServeBusinessSystemUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProblemJSON(writer, http.StatusMethodNotAllowed, "malformed_request", "上传只接受 POST 请求。", nil)
		return
	}
	cookie := sessionCookieOf(request)
	if cookie == "" {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传配置。", nil)
		return
	}
	principalID, err := handler.admin(request.Context(), cookie)
	if err != nil {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传配置。", nil)
		return
	}
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeProblemJSON(writer, http.StatusUnsupportedMediaType, "unsupported_media", "配置上传需要 multipart/form-data 请求。", nil)
		return
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	var (
		commandID    string
		targetString string
		catalogInput string
		document     []byte
		tooLarge     bool
		parts        int
	)
	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			writeProblemJSON(writer, http.StatusBadRequest, "malformed_request", "上传内容无法解析，请重试。", nil)
			return
		}
		parts++
		if parts > maxMultipartFields {
			part.Close()
			writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "上传表单字段过多。", nil)
			return
		}
		switch part.FormName() {
		case "clientCommandId":
			body, _ := io.ReadAll(io.LimitReader(part, 129))
			part.Close()
			commandID = string(body)
		case "targetLabelContractVersion":
			body, _ := io.ReadAll(io.LimitReader(part, 33))
			part.Close()
			targetString = string(body)
		case "journeyCatalogDigest":
			body, _ := io.ReadAll(io.LimitReader(part, 65))
			part.Close()
			catalogInput = string(body)
		case "file":
			if document != nil {
				part.Close()
				writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "只能上传一份 YAML 文档。", nil)
				return
			}
			limited := io.LimitReader(part, configLimitBytes()+1)
			document, err = io.ReadAll(limited)
			part.Close()
			if err != nil {
				writeProblemJSON(writer, http.StatusBadRequest, "malformed_request", "上传内容无法读取，请重试。", nil)
				return
			}
			if int64(len(document)) > configLimitBytes() {
				tooLarge = true
			}
		default:
			part.Close()
		}
	}
	if tooLarge {
		writeProblemJSON(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "配置文档超过大小上限（10 MiB），请精简后上传。", nil)
		return
	}
	var fieldErrors []config.FieldError
	if len(commandID) < 8 || len(commandID) > 128 {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "clientCommandId", Reason: "clientCommandId 必须是 8-128 个字符。"})
	}
	targetVersion, parseErr := strconv.ParseInt(targetString, 10, 64)
	if parseErr != nil || targetVersion < 1 {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "targetLabelContractVersion", Reason: "targetLabelContractVersion 必须是正整数（目标 Label Contract 版本）。", Remediation: "选择一个已上传的契约版本"})
	}
	if catalogInput != "" && !isHex64(catalogInput) {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "journeyCatalogDigest", Reason: "journeyCatalogDigest 必须是 64 位十六进制 digest。"})
	}
	if len(document) == 0 {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "file", Reason: "缺少 YAML 文档。", Remediation: "选择一份业务系统配置 YAML 文件"})
	}
	if len(fieldErrors) > 0 {
		writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "上传表单字段不完整或不合法。", fieldErrors)
		return
	}
	detail, err := handler.Systems.Upload(request.Context(), principalID, commandID, businesssystem.UploadInput{
		YAMLBody:                   document,
		TargetLabelContractVersion: targetVersion,
		JourneyCatalogDigest:       catalogInput,
	}, config.Limits{MaxDocumentBytes: configLimitBytes()})
	writeDetail(writer, err, func() { writeDetailJSON(writer, http.StatusCreated, detail) })
}

// ServeLabelContractUpload handles POST /api/v1/label-contracts
// (createLabelContractDraft).
func (handler *Handler) ServeLabelContractUpload(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeProblemJSON(writer, http.StatusMethodNotAllowed, "malformed_request", "上传只接受 POST 请求。", nil)
		return
	}
	cookie := sessionCookieOf(request)
	if cookie == "" {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传契约。", nil)
		return
	}
	principalID, err := handler.admin(request.Context(), cookie)
	if err != nil {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再上传契约。", nil)
		return
	}
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeProblemJSON(writer, http.StatusUnsupportedMediaType, "unsupported_media", "契约上传需要 multipart/form-data 请求。", nil)
		return
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	var commandID string
	var document []byte
	tooLarge := false
	parts := 0
	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			writeProblemJSON(writer, http.StatusBadRequest, "malformed_request", "上传内容无法解析，请重试。", nil)
			return
		}
		parts++
		if parts > maxMultipartFields {
			part.Close()
			writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "上传表单字段过多。", nil)
			return
		}
		switch part.FormName() {
		case "clientCommandId":
			body, _ := io.ReadAll(io.LimitReader(part, 129))
			part.Close()
			commandID = string(body)
		case "file":
			if document != nil {
				part.Close()
				writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "只能上传一份 YAML 文档。", nil)
				return
			}
			limited := io.LimitReader(part, configLimitBytes()+1)
			document, err = io.ReadAll(limited)
			part.Close()
			if err != nil {
				writeProblemJSON(writer, http.StatusBadRequest, "malformed_request", "上传内容无法读取，请重试。", nil)
				return
			}
			if int64(len(document)) > configLimitBytes() {
				tooLarge = true
			}
		default:
			part.Close()
		}
	}
	if tooLarge {
		writeProblemJSON(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "契约文档超过大小上限（10 MiB），请精简后上传。", nil)
		return
	}
	var fieldErrors []config.FieldError
	if len(commandID) < 8 || len(commandID) > 128 {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "clientCommandId", Reason: "clientCommandId 必须是 8-128 个字符。"})
	}
	if len(document) == 0 {
		fieldErrors = append(fieldErrors, config.FieldError{Path: "file", Reason: "缺少 YAML 文档。", Remediation: "选择一份 Label Contract YAML 文件"})
	}
	if len(fieldErrors) > 0 {
		writeProblemJSON(writer, http.StatusUnprocessableEntity, "validation_failed", "上传表单字段不完整或不合法。", fieldErrors)
		return
	}
	detail, err := handler.Contracts.CreateDraft(request.Context(), principalID, commandID, document, config.Limits{MaxDocumentBytes: configLimitBytes()})
	writeDetail(writer, err, func() { writeDetailJSON(writer, http.StatusCreated, detail) })
}

// writeDetail maps the service error lattice once for both uploads.
func writeDetail(writer http.ResponseWriter, err error, success func()) {
	if err == nil {
		success()
		return
	}
	mapped := mapDomainError(err)
	problemErr, ok := mapped.(*problemError)
	if !ok {
		writeProblemJSON(writer, http.StatusInternalServerError, "unavailable", "暂时无法完成操作，请稍后重试。", nil)
		return
	}
	writeProblemJSON(writer, problemErr.status, problemErr.Code, problemErr.Message, problemErr.FieldErrors)
}

func isHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

// ServeBusinessSystemTemplate handles GET /api/v1/templates/business-system
// (downloadBusinessSystemTemplate, CFG-EXPORT-001).
func (handler *Handler) ServeBusinessSystemTemplate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeProblemJSON(writer, http.StatusMethodNotAllowed, "malformed_request", "模板下载只接受 GET 请求。", nil)
		return
	}
	cookie := sessionCookieOf(request)
	if cookie == "" {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再下载模板。", nil)
		return
	}
	if _, err := handler.reader(request.Context(), cookie); err != nil {
		writeProblemJSON(writer, http.StatusUnauthorized, "unauthenticated", "请重新登录后再下载模板。", nil)
		return
	}
	writer.Header().Set("Content-Type", "application/yaml")
	writer.Header().Set("Content-Disposition", `attachment; filename="business-system-template.yaml"`)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	writer.Write([]byte(businessSystemTemplateYAML))
}

// businessSystemTemplateYAML is the credential-free starter document
// exercising every closed variant of the frozen schema.
const businessSystemTemplateYAML = `system_key: example-system
display_name: 示例业务系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: example-discovery
    display_name: 示例资源发现
    selector: 'up{business_system="example-system", job="example"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: example-plan
    display_name: 示例巡检计划
    cron: "30 8 * * *"
    checks:
      - key: example-instant-check
        display_name: 即时查询巡检项
        analysis_question: 当前服务是否可用？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="example-system"}'
      - key: example-range-check
        display_name: 区间查询巡检项
        analysis_question: 请求速率趋势如何？
        kind: promql
        query:
          mode: range
          expression: 'rate(http_requests_total{business_system="example-system"}[5m])'
          range_seconds: 3600
          step_seconds: 60
`
