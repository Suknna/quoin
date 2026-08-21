package connections

// Typed connection probe executors for the Plinth supervisor (T07). The
// executors implement exactly the closed action sets frozen in
// contracts/connection-probes.yaml: thanos vector(1) against the configured
// Prometheus-compatible query endpoint, and the kubernetes read-capability
// set (server version, core/grouped discovery, four SelfSubjectAccess
// Reviews). Every outcome is a typed observation; nothing here infers
// capabilities beyond the frozen sets.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// probeTimeout bounds each individual probe action.
const probeTimeout = 15 * time.Second

// ThanosConfig is the non-secret thanos revision projection.
type ThanosConfig struct {
	Type          string `json:"type"`
	BaseURL       string `json:"baseUrl"`
	TLSCaPem      string `json:"tlsCaPem,omitempty"`
	TLSServerName string `json:"tlsServerName,omitempty"`
	TLSSkipVerify bool   `json:"tlsSkipVerify,omitempty"`
	Username      string `json:"username,omitempty"`
}

// ThanosSecret is the decrypted thanos credential.
type ThanosSecret struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password"`
}

// KubernetesConfig is the non-secret kubernetes revision projection.
type KubernetesConfig struct {
	Type             string `json:"type"`
	ContextName      string `json:"contextName,omitempty"`
	DefaultNamespace string `json:"defaultNamespace,omitempty"`
}

// KubernetesSecret is the decrypted kubeconfig credential.
type KubernetesSecret struct {
	Kubeconfig string `json:"kubeconfig"`
}

// ThanosProbeDetail is the canonical thanos result detail.
type ThanosProbeDetail struct {
	Kind         string `json:"kind"`
	Query        string `json:"query"`
	ResponseType string `json:"responseType"`
	SampleCount  int    `json:"sampleCount"`
	SampleValue  string `json:"sampleValue"`
}

// KubernetesProbeDetail is the canonical kubernetes result detail.
type KubernetesProbeDetail struct {
	Kind               string `json:"kind"`
	EffectiveNamespace string `json:"effectiveNamespace"`
	VersionOK          bool   `json:"versionOk"`
	CoreDiscoveryOK    bool   `json:"coreDiscoveryOk"`
	GroupedDiscoveryOK bool   `json:"groupedDiscoveryOk"`
	PodsGetAllowed     bool   `json:"podsGetAllowed"`
	PodsListAllowed    bool   `json:"podsListAllowed"`
	EventsListAllowed  bool   `json:"eventsListAllowed"`
	PodsLogGetAllowed  bool   `json:"podsLogGetAllowed"`
}

// RunThanosProbe executes the frozen thanos-query-v1 action set.
func RunThanosProbe(ctx context.Context, config ThanosConfig, secret ThanosSecret) (ThanosProbeDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	client, err := thanosHTTPClient(config)
	if err != nil {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, err
	}
	target := strings.TrimSuffix(config.BaseURL, "/") + "/api/v1/query?query=" + url.QueryEscape("vector(1)")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, err
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
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, fmt.Errorf("查询请求失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, fmt.Errorf("读取响应失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, fmt.Errorf("查询端点返回 HTTP %d", response.StatusCode)
	}
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	if parsed.Status != "success" {
		return ThanosProbeDetail{Kind: "thanos", Query: "vector(1)"}, fmt.Errorf("查询状态为 %s", parsed.Status)
	}
	detail := ThanosProbeDetail{Kind: "thanos", Query: "vector(1)", ResponseType: parsed.Data.ResultType, SampleCount: len(parsed.Data.Result)}
	if len(parsed.Data.Result) == 1 {
		if sample, ok := parsed.Data.Result[0].Value[1].(string); ok {
			detail.SampleValue = sample
		}
	}
	if parsed.Data.ResultType != "vector" {
		return detail, fmt.Errorf("resultType 是 %s，期望 vector", parsed.Data.ResultType)
	}
	if len(parsed.Data.Result) != 1 {
		return detail, fmt.Errorf("样本数是 %d，期望 1", len(parsed.Data.Result))
	}
	if detail.SampleValue != "1" {
		return detail, fmt.Errorf("样本值是 %s，期望 1", detail.SampleValue)
	}
	return detail, nil
}

func thanosHTTPClient(config ThanosConfig) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.TLSSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if config.TLSCaPem != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.TLSCaPem)) {
			return nil, errors.New("tlsCaPem 无法解析")
		}
		tlsConfig.RootCAs = pool
	}
	if config.TLSServerName != "" {
		tlsConfig.ServerName = config.TLSServerName
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: probeTimeout}, nil
}

// NewHTTPClient builds the HTTP client for the frozen Thanos connection
// projection (shared by the probe executor and the supervisor-typed
// thanos_query tool; the query tool bounds each call with its own context
// deadline on top of the client timeout).
func NewHTTPClient(config ThanosConfig) (*http.Client, error) {
	return thanosHTTPClient(config)
}
