package app

// Runtime slot projection and registration gate coverage: the browser
// business is retired (受控浏览器退役), so no deployment projects a Lintel
// slot, its registration commands behave as absent routes, and long-lived
// Lintel credentials cannot come back through the HTTP surface.

import (
	"net/http"
	"strings"
	"testing"
)

func TestRuntimeSlotGateHidesLintelWhenBrowserDisabled(t *testing.T) {
	surface := newStandaloneSurface(t, nil)

	status, body := surface.request(t, http.MethodGet, "/api/v1/runtime", "")
	if status != http.StatusOK {
		t.Fatalf("runtime read must stay available: %d %s", status, body)
	}
	if !strings.Contains(body, `"plinth"`) {
		t.Fatalf("plinth slot must stay projected: %s", body)
	}
	if strings.Contains(body, `"lintel"`) {
		t.Fatalf("retired browser business must not project a lintel slot: %s", body)
	}
	status, body = surface.request(t, http.MethodGet, "/api/v1/admin/about", "")
	if status != http.StatusOK || strings.Contains(body, `"slot":"lintel"`) {
		t.Fatalf("about components must omit lintel: %d %s", status, body)
	}
	// The registration commands behave as absent routes.
	status, body = surface.request(t, http.MethodPost, "/api/v1/runtime-slots/lintel/registration/prepare", `{"clientCommandId":"lintel-prepare-01","expectedRowVersion":1}`)
	if status != http.StatusNotFound || !strings.Contains(body, "not_found") {
		t.Fatalf("retired lintel prepare must be a 404 problem: %d %s", status, body)
	}
	status, body = surface.request(t, http.MethodPost, "/api/v1/runtime-slots/lintel/retiring-credential/retire", `{"clientCommandId":"lintel-retire-001","expectedRowVersion":1}`)
	if status != http.StatusNotFound {
		t.Fatalf("retired lintel retire must be 404: %d %s", status, body)
	}
	// Plinth registration stays untouched on the same deployment.
	status, _ = surface.request(t, http.MethodPost, "/api/v1/runtime-slots/plinth/registration/prepare", `{"clientCommandId":"plinth-prepare-01","expectedRowVersion":1}`)
	if status != http.StatusOK {
		t.Fatalf("plinth prepare must keep working: %d", status)
	}
}
