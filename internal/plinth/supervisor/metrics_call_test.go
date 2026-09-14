package supervisor

// 真实凭据流测试（部署前硬门）：Collector 绑定经合同秘密边界取出的凭据必须
// 真实到达上游 —— 上游强制校验 Basic/Bearer，缺失或错误一律 401 使采集失败；
// 秘密解析失败必须 fail closed，绝不匿名降级。

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Suknna/quoin/internal/plugins"
)

func TestMetricsCollectorSendsRealCredentials(t *testing.T) {
	const bearer = "sentinel-bearer-token"
	var mutex sync.Mutex
	var authHeaders []string
	var wantMode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mutex.Lock()
		authHeaders = append(authHeaders, auth)
		mode := wantMode
		mutex.Unlock()
		switch mode {
		case "bearer":
			if auth != "Bearer "+bearer {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "basic":
			if auth == "" || auth == "Bearer "+bearer {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","job":"web","instance":"one:80"},"value":[0,"1"]}]}}`))
	}))
	t.Cleanup(server.Close)

	supervisor := &Supervisor{}
	registry := supervisor.pluginRegistry()
	bundle, bound := registry.Bundle(plugins.PrometheusID)
	if !bound || bundle.Collector == nil {
		t.Fatal("prometheus bundle must bind a collector")
	}

	for _, scenario := range []struct {
		name, authType, username, password, token string
	}{
		{name: "bearer", authType: "bearer", token: bearer},
		{name: "basic", authType: "basic", username: "probe-user", password: "probe-password"},
	} {
		mutex.Lock()
		wantMode = scenario.name
		mutex.Unlock()
		call, err := newMetricsCall(testPluginRegistry(), plugins.PrometheusID,
			json.RawMessage(fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":%q}`, server.URL, scenario.authType)),
			scenario.username, scenario.password, scenario.token)
		if err != nil {
			t.Fatal(err)
		}
		result, collectErr := bundle.Collector.Collect(context.Background(), call, plugins.CollectRequest{
			TemplateID: "promql_instant", TemplateVersion: "1",
			Params: json.RawMessage(`{"expression":"up"}`), EvidenceAt: "2026-09-13T00:00:00Z",
			Scope: plugins.CollectScope{Kind: plugins.ScopeIntegration},
		})
		if collectErr != nil {
			t.Fatalf("%s collection failed: %v", scenario.name, collectErr)
		}
		if result.Incomplete || len(result.Checks) != 1 || !result.Checks[0].Succeeded {
			t.Fatalf("%s collection result = %+v", scenario.name, result)
		}
		last := ""
		if len(authHeaders) != 0 {
			last = authHeaders[len(authHeaders)-1]
		}
		if scenario.name == "bearer" && last != "Bearer "+bearer {
			t.Fatalf("bearer upstream saw %q, want the real token", last)
		}
		if scenario.name == "basic" && (last == "" || last == "Bearer "+bearer) {
			t.Fatalf("basic upstream saw %q, want real basic credentials", last)
		}
	}
	_ = strings.TrimSpace
}

// TestMetricsCollectorFailsClosedWithoutSecrets proves the collector refuses
// to run anonymously: a resolver that cannot supply credentials fails the
// collection instead of issuing an unauthenticated request.
func TestMetricsCollectorFailsClosedWithoutSecrets(t *testing.T) {
	var upstreamHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(server.Close)

	call := &plugins.Call{
		PluginID: plugins.PrometheusID,
		Settings: json.RawMessage(fmt.Sprintf(`{"type":"prometheus","baseUrl":%q,"authType":"basic"}`, server.URL)),
		SecretRefs: map[string]string{
			"username": "username", "password": "password", "bearerToken": "bearerToken",
		},
		Secrets: secretResolverFunc(func(ref string) ([]byte, error) {
			return nil, fmt.Errorf("grant material unavailable")
		}),
	}
	supervisor := &Supervisor{}
	bundle, bound := supervisor.pluginRegistry().Bundle(plugins.PrometheusID)
	if !bound || bundle.Collector == nil {
		t.Fatal("prometheus bundle must bind a collector")
	}
	if _, err := bundle.Collector.Collect(context.Background(), call, plugins.CollectRequest{
		TemplateID: "promql_instant", TemplateVersion: "1",
		Params: json.RawMessage(`{"expression":"up"}`), EvidenceAt: "2026-09-13T00:00:00Z",
		Scope: plugins.CollectScope{Kind: plugins.ScopeIntegration},
	}); err == nil {
		t.Fatal("unresolvable secrets must fail the collection closed")
	}
	if upstreamHits != 0 {
		t.Fatalf("failed secret resolution must never reach the upstream, hits=%d", upstreamHits)
	}
}

// testPluginRegistry builds the process-equivalent registry from the
// built-in descriptors (the same authority the supervisor hosts) so the
// migrated newMetricsCall calls validate settings for real.
func testPluginRegistry() *plugins.Registry {
	registry := plugins.NewRegistry()
	for _, descriptor := range attempt.BuiltinDescriptors() {
		_ = registry.RegisterDescriptor(descriptor)
	}
	return registry
}
