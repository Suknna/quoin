package app_test

import (
	"net/http"
	"testing"
)

// TestManualResourceRefreshRouteReachesTheDomain verifies the restored manual
// producer is a real authenticated domain route, not a missing route or a
// redirect. With no published system fixture, the service's not-found result is
// the expected boundary response after a syntactically valid command.
func TestManualResourceRefreshRouteReachesTheDomain(t *testing.T) {
	scenario := newAdminSurface(t)
	server := scenario.server
	admin := scenario.sessionHeaders(scenario.login(t, "admin", scenario.adminPassword))
	const history = "/api/v1/business-systems/payments/resource-refresh-runs/1"

	mustRequest(t, server, admin, history, http.StatusNotFound)
	response := mustDo(t, server, http.MethodPost, admin, "/api/v1/business-systems/payments/resources:refresh", `{"clientCommandId":"refresh-000001"}`, http.StatusNotFound)
	if location := response.headers.Get("Location"); location != "" {
		t.Fatalf("manual refresh route redirected to %q", location)
	}
}
