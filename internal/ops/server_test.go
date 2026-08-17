package ops_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/ops"
)

func TestHealthAndMetricsSurfacesUseFrozenShapes(t *testing.T) {
	server, err := ops.New("plinth", "127.0.0.1:0", ops.RuntimeUnregistered)
	if err != nil {
		t.Fatal(err)
	}
	live := httptest.NewRecorder()
	server.Handler().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if live.Code != http.StatusOK || live.Body.String() != "ok\n" || live.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected liveness response: code=%d type=%q body=%q", live.Code, live.Header().Get("Content-Type"), live.Body.String())
	}
	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness code=%d", ready.Code)
	}
	var state ops.Readiness
	if err := json.Unmarshal(ready.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Component != "plinth" || state.Reason != ops.RuntimeUnregistered || state.AcceptingWork {
		t.Fatalf("unexpected readiness: %+v", state)
	}
	metrics := httptest.NewRecorder()
	server.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(metrics.Result().Body)
	text := string(body)
	if !strings.Contains(text, "plinth_ready 0") || strings.Contains(text, "go_gc_duration") {
		t.Fatalf("metrics do not use isolated declared registry:\n%s", text)
	}
}
