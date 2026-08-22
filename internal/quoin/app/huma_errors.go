package app

import (
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// Huma constructs errors before an operation handler runs (for example when a
// JSON body has an unknown member). Those errors must still use the frozen
// Quoin ErrorModel rather than Huma's default RFC 9457 payload.
var humaErrorModelOnce sync.Once

func configureHumaErrorModel() {
	humaErrorModelOnce.Do(func() {
		// Huma uses NewErrorWithContext for request decoding/validation and
		// untyped handler failures. Handler-authored huma.Error* values continue
		// to preserve their existing endpoint-specific semantics.
		huma.NewErrorWithContext = func(_ huma.Context, status int, message string, errs ...error) huma.StatusError {
			result := problem(status, humaErrorCode(status), humaErrorMessage(status, message))
			for _, err := range errs {
				detailer, ok := err.(huma.ErrorDetailer)
				if !ok || detailer.ErrorDetail() == nil {
					continue
				}
				detail := detailer.ErrorDetail()
				result.FieldErrors = append(result.FieldErrors, problemField{
					Path:   detail.Location,
					Reason: detail.Message,
				})
			}
			return result
		}
	})
}

func humaErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "malformed_request"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "active_conflict"
	case http.StatusGone:
		return "resource_expired"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media"
	case http.StatusUnprocessableEntity:
		return "validation_failed"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "unavailable"
	}
}

func humaErrorMessage(status int, message string) string {
	// Deliberately authored handler messages are already appropriate for people;
	// framework defaults are technical/English and are replaced below.
	if message != "" && message != "validation failed" && message != http.StatusText(status) {
		return message
	}
	if status == http.StatusUnprocessableEntity {
		return "请求字段不满足要求，请检查后重试。"
	}
	if status >= http.StatusInternalServerError {
		return "服务暂时不可用，请稍后重试。"
	}
	return "请求无法处理，请检查后重试。"
}

func (p *problemError) ContentType(string) string { return "application/problem+json" }
