package app_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestOperatorCannotCallManagementAPIs exercises the public HTTP seam. It
// prevents a navigation-only role boundary from exposing management data via a
// bookmarked URL while keeping the non-secret business selector available to
// alert and AI SRE workflows. The operator reaches the surface through the
// shared real-auth fixture: real creation with an assigned contact, real
// operator initialization, real two-step login.
func TestOperatorCannotCallManagementAPIs(t *testing.T) {
	scenario := newAdminSurface(t)
	server := scenario.server

	adminSession := scenario.login(t, "admin", scenario.adminPassword)
	scenario.createOperator(t, adminSession, "create-operator-96", "operator96", "Operator 96", "Operator 96 passphrase 2026!", "operator96@example.test")
	operator := scenario.sessionHeaders(scenario.initializeOperatorSession(t, "operator96", "Operator 96 passphrase 2026!", "Operator 96 formal passphrase 2027!"))

	for _, path := range []string{
		"/api/v1/admin/about",
		"/api/v1/connections",
		"/api/v1/inspections/runs?businessSystemKey=anything",
		"/api/v1/audit-events",
		"/api/v1/integrations/plugins",
		"/api/v1/inspections/plans",
	} {
		response := mustRequest(t, server, operator, path, http.StatusForbidden)
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(response), &problem); err != nil {
			t.Fatalf("GET %s did not return JSON problem: %v", path, err)
		}
		if problem.Code != "forbidden" {
			t.Errorf("GET %s code=%q, want forbidden", path, problem.Code)
		}
	}

	alerts := mustRequest(t, server, operator, "/api/v1/alerts?state=Firing", http.StatusOK)
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(alerts), &body); err != nil {
		t.Fatal(err)
	}
	if body.Items == nil {
		t.Fatalf("alerts must serialize an empty items array, got %s", alerts)
	}
}
