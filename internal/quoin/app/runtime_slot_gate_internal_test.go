package app

// Runtime slot projection coverage: the browser business is retired
// (受控浏览器退役), no deployment projects a Lintel slot, and the
// registration-era commands are gone entirely — component identity is the
// deployment CA-signed mTLS client certificate (ADR-0009), so there is no
// HTTP registration surface left to probe.

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
	// The registration-era commands behave as absent routes on every slot:
	// there is nothing left to register through the HTTP surface.
	for _, path := range []string{
		"/api/v1/runtime-slots/lintel/registration/prepare",
		"/api/v1/runtime-slots/plinth/registration/prepare",
		"/api/v1/runtime-slots/registration-token/reveal",
		"/api/v1/runtime-slots/plinth/retiring-credential/retire",
	} {
		status, body = surface.request(t, http.MethodPost, path, `{"clientCommandId":"retired-command-01","expectedRowVersion":1}`)
		if status != http.StatusNotFound {
			t.Fatalf("retired registration route %s must be 404: %d %s", path, status, body)
		}
	}
}
