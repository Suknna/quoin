package ops_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/ops"
)

func TestExpositionProbeQuoin(t *testing.T) {
	server, err := ops.New("quoin", "127.0.0.1:0", ops.Ready)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	samples := 0
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			samples++
		}
	}
	if !strings.Contains(body, "quoin_http_requests_total{method=\"get\",route_group=\"auth\",status_class=\"2xx\"} 0") {
		t.Fatal("missing preinitialized http counter series")
	}
	if !strings.Contains(body, "quoin_maintenance{maintenance_reason=\"root_key_rebind\"} 0") {
		t.Fatal("missing preinitialized maintenance series")
	}
	t.Logf("quoin exposition: %d samples", samples)
}
