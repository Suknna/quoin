package connections

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RunPromQL executes the frozen config query over the existing Thanos HTTP
// transport. It returns the raw successful Prometheus envelope as Evidence
// facts, while warnings remain separately machine-readable.
func RunPromQL(ctx context.Context, config ThanosConfig, secret ThanosSecret, mode, expression string, rangeSeconds, stepSeconds *int64) (json.RawMessage, []string, error) {
	client, err := NewHTTPClient(config)
	if err != nil {
		return nil, nil, err
	}
	endpoint := "/api/v1/query"
	values := url.Values{"query": {expression}}
	if mode == "range" {
		if rangeSeconds == nil || stepSeconds == nil {
			return nil, nil, fmt.Errorf("range query lacks range or step")
		}
		now := time.Now().UTC()
		endpoint = "/api/v1/query_range"
		values.Set("start", strconv.FormatFloat(float64(now.Add(-time.Duration(*rangeSeconds)*time.Second).UnixNano())/1e9, 'f', -1, 64))
		values.Set("end", strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', -1, 64))
		values.Set("step", strconv.FormatInt(*stepSeconds, 10))
	} else if mode != "instant" {
		return nil, nil, fmt.Errorf("unsupported query mode %q", mode)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(config.BaseURL, "/")+endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	if secret.Password != "" {
		username := secret.Username
		if username == "" {
			username = config.Username
		}
		request.SetBasicAuth(username, secret.Password)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("PromQL request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("PromQL endpoint returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings"`
		Error    string   `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil, fmt.Errorf("PromQL response is not valid JSON: %w", err)
	}
	if envelope.Status != "success" {
		return nil, envelope.Warnings, fmt.Errorf("PromQL response status %q: %s", envelope.Status, envelope.Error)
	}
	return body, envelope.Warnings, nil
}
