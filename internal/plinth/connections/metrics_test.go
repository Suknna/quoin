package connections

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunPrometheusProbeUsesConfiguredAuthentication(t *testing.T) {
	tests := []struct {
		name   string
		config PrometheusConfig
		secret PrometheusSecret
		check  func(*testing.T, *http.Request)
	}{
		{
			name:   "none",
			config: PrometheusConfig{Type: "prometheus", AuthType: "none"},
			check: func(t *testing.T, request *http.Request) {
				t.Helper()
				if got := request.Header.Get("Authorization"); got != "" {
					t.Fatalf("no-auth request leaked authorization header %q", got)
				}
			},
		},
		{
			name:   "legacy-basic-auth-omitted",
			config: PrometheusConfig{Type: "prometheus", Username: "metrics"},
			secret: PrometheusSecret{Password: "password"},
			check: func(t *testing.T, request *http.Request) {
				t.Helper()
				username, password, ok := request.BasicAuth()
				if !ok || username != "metrics" || password != "password" {
					t.Fatalf("legacy basic auth = %q/%q present=%v", username, password, ok)
				}
			},
		},
		{
			name:   "basic",
			config: PrometheusConfig{Type: "prometheus", AuthType: "basic", Username: "metrics"},
			secret: PrometheusSecret{Password: "password"},
			check: func(t *testing.T, request *http.Request) {
				t.Helper()
				username, password, ok := request.BasicAuth()
				if !ok || username != "metrics" || password != "password" {
					t.Fatalf("basic auth = %q/%q present=%v", username, password, ok)
				}
			},
		},
		{
			name:   "bearer",
			config: PrometheusConfig{Type: "prometheus", AuthType: "bearer"},
			secret: PrometheusSecret{BearerToken: "token"},
			check: func(t *testing.T, request *http.Request) {
				t.Helper()
				if got := request.Header.Get("Authorization"); got != "Bearer token" {
					t.Fatalf("bearer authorization = %q", got)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/query" || request.URL.Query().Get("query") != "vector(1)" {
					t.Fatalf("probe request = %s", request.URL.String())
				}
				test.check(t, request)
				_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"1"]}]}}`))
			}))
			defer server.Close()

			test.config.BaseURL = server.URL
			detail, err := RunPrometheusProbe(context.Background(), test.config, test.secret)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Kind != "prometheus" || detail.SampleValue != "1" {
				t.Fatalf("unexpected probe detail: %+v", detail)
			}
		})
	}
}

func TestRunPrometheusProbeHonorsConfiguredTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer tls-token" {
			t.Fatalf("bearer authorization = %q", got)
		}
		_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[0,"1"]}]}}`))
	}))
	defer server.Close()

	certificate := server.Certificate()
	pemCertificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if _, err := x509.ParseCertificate(certificate.Raw); err != nil {
		t.Fatalf("test server certificate is invalid: %v", err)
	}
	detail, err := RunPrometheusProbe(context.Background(), PrometheusConfig{
		Type: "prometheus", BaseURL: server.URL, TLSCaPem: string(pemCertificate),
		// httptest TLS is issued for example.com; the explicit server name proves
		// the production adapter preserves the existing TLS override path.
		TLSServerName: "example.com", AuthType: "bearer",
	}, PrometheusSecret{BearerToken: "tls-token"})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Kind != "prometheus" || detail.SampleValue != "1" {
		t.Fatalf("unexpected TLS probe detail: %+v", detail)
	}
}

func TestApplyMetricsAuthRejectsInconsistentCredentials(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://metrics.example/api/v1/query", nil)
	if err := ApplyMetricsAuth(request, MetricsConfig{AuthType: "none"}, MetricsSecret{BearerToken: "leak"}); err == nil {
		t.Fatal("no-auth configuration with a credential must fail closed")
	}
	if err := ApplyMetricsAuth(request, MetricsConfig{AuthType: "basic", Username: "metrics"}, MetricsSecret{BearerToken: "token"}); err == nil {
		t.Fatal("basic configuration with bearer credential must fail closed")
	}
	if err := ApplyMetricsAuth(request, MetricsConfig{AuthType: "bearer"}, MetricsSecret{}); err == nil {
		t.Fatal("bearer configuration without token must fail closed")
	}
}
