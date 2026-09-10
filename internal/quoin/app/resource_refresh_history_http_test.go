package app_test

import (
	"net/http"
	"testing"
)

// Historical refresh facts remain readable through the public API, but the
// retired producer endpoint must not be routable again.
func TestHistoricalResourceRefreshRouteIsReadOnly(t *testing.T) {
	server, admin := newAdminSurface(t)
	const history = "/api/v1/business-systems/payments/resource-refresh-runs/1"

	mustRequest(t, server, admin, history, http.StatusNotFound)
	mustDo(t, server, http.MethodPost, admin, "/api/v1/business-systems/payments/resources:refresh", `{}`, http.StatusNotFound)
}
