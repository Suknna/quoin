package suites

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// newMultipartUpload builds one YAML file upload request with the
// frozen command-id form field (and one optional extra key=value
// field) the product requires.
func newMultipartUpload(target, filename, content, commandID, extraField string) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("clientCommandId", commandID); err != nil {
		return nil, err
	}
	if extraField != "" {
		key, value, _ := strings.Cut(extraField, "=")
		if err := writer.WriteField(key, value); err != nil {
			return nil, err
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, strings.NewReader(content)); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, target, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request, nil
}

func requestURL(base string) *url.URL {
	parsed, err := url.Parse(base)
	if err != nil {
		return &url.URL{}
	}
	return parsed
}

func readLimited(reader io.Reader, limit int64) (string, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit))
	return string(body), err
}
