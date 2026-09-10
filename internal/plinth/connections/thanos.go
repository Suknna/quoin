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

// MetricsConfig is the shared Prometheus-compatible non-secret projection.
// Type remains a required discriminator so a Thanos selection can never be
// silently treated as a Prometheus one (or vice versa).
type MetricsConfig struct {
	Type          string `json:"type"`
	BaseURL       string `json:"baseUrl"`
	TLSCaPem      string `json:"tlsCaPem,omitempty"`
	TLSServerName string `json:"tlsServerName,omitempty"`
	TLSSkipVerify bool   `json:"tlsSkipVerify,omitempty"`
	AuthType      string `json:"authType,omitempty"`
	Username      string `json:"username,omitempty"`
}

// MetricsSecret only exists in supervisor memory after an attempt-scoped
// credential grant. Exactly one of Password/BearerToken is used according to
// MetricsConfig.AuthType.
type MetricsSecret struct {
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	BearerToken string `json:"bearerToken,omitempty"`
}

// The concrete aliases preserve existing Thanos tool call sites while exposing
// a distinct Prometheus adapter surface to new business-scoped callers.
type ThanosConfig = MetricsConfig
type ThanosSecret = MetricsSecret
type PrometheusConfig = MetricsConfig
type PrometheusSecret = MetricsSecret

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
// RunPrometheusProbe executes the same Prometheus HTTP API contract while
// retaining the configured Prometheus identity in its evidence detail.
func RunPrometheusProbe(ctx context.Context, config PrometheusConfig, secret PrometheusSecret) (ThanosProbeDetail, error) {
	detail, err := RunThanosProbe(ctx, config, secret)
	detail.Kind = "prometheus"
	return detail, err
}

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
	if err := ApplyMetricsAuth(request, config, secret); err != nil {
		return ThanosProbeDetail{Kind: config.Type, Query: "vector(1)"}, err
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

// applyMetricsAuth materializes the closed persisted auth choice. It rejects
// inconsistent snapshots rather than guessing from a leftover secret.
func ApplyMetricsAuth(request *http.Request, config MetricsConfig, secret MetricsSecret) error {
	authType := config.AuthType
	if authType == "" {
		// Revisions created before authType existed used an optional Basic
		// password carrier. Preserve that behavior during the transition: an
		// absent mode with password means Basic, otherwise no authentication.
		// New public inputs must always declare one of the closed modes.
		authType = "none"
		if secret.Password != "" {
			authType = "basic"
		}
	}
	switch authType {
	case "none":
		if secret.Username != "" || secret.Password != "" || secret.BearerToken != "" {
			return errors.New("no-auth metrics connection carries credentials")
		}
	case "basic":
		username := secret.Username
		if username == "" {
			username = config.Username
		}
		if username == "" || secret.Password == "" || secret.BearerToken != "" {
			return errors.New("basic metrics credentials are incomplete")
		}
		request.SetBasicAuth(username, secret.Password)
	case "bearer":
		if secret.BearerToken == "" || secret.Username != "" || secret.Password != "" {
			return errors.New("bearer metrics credentials are incomplete")
		}
		request.Header.Set("Authorization", "Bearer "+secret.BearerToken)
	default:
		return fmt.Errorf("unsupported metrics auth type %q", authType)
	}
	return nil
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
