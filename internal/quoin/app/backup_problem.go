package app

import (
	"encoding/json"
	"errors"
	"net/http"
)

func writeBackupProblemError(writer http.ResponseWriter, err error) {
	var value *problemError
	if errors.As(err, &value) {
		writeBackupProblem(writer, value.status, value.Code, value.Message, value.Retryable)
		return
	}
	writeBackupProblem(writer, http.StatusUnauthorized, "unauthorized", "请重新登录", false)
}

// writeBackupProblem keeps the frozen ErrorModel for pre-stream failures.
func writeBackupProblem(writer http.ResponseWriter, status int, code, message string, retryable bool) {
	body, err := json.Marshal(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}{Code: code, Message: message, Retryable: retryable})
	if err != nil {
		http.Error(writer, message, status)
		return
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_, _ = writer.Write(append(body, '\n'))
}
