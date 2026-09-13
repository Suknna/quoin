package app_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestOperatorCannotCallManagementAPIs exercises the public HTTP seam. It
// prevents a navigation-only role boundary from exposing management data via a
// bookmarked URL while keeping the non-secret business selector available to
// alert and AI SRE workflows.
func TestOperatorCannotCallManagementAPIs(t *testing.T) {
	server, admin := newAdminSurface(t)
	defer server.Close()

	mustPost(t, server, admin, "/api/v1/admin/users", `{"clientCommandId":"create-operator-96","username":"operator96","displayName":"Operator 96","role":"operator","password":"Operator 96 passphrase 2026!"}`, http.StatusCreated)
	login := mustPost(t, server, map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json"}, "/api/v1/auth/login", `{"username":"operator96","password":"Operator 96 passphrase 2026!"}`, http.StatusOK)
	operator := map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": splitCookie(login.headers.Get("Set-Cookie"))}

	for _, path := range []string{
		"/api/v1/admin/about",
		"/api/v1/connections",
		"/api/v1/business-systems",
		"/api/v1/inspections/runs?businessSystemKey=anything",
		"/api/v1/business-systems/anything/browser-identity",
		"/api/v1/journey-catalog",
		"/api/v1/runtime",
		"/api/v1/audit-events",
		"/api/v1/templates/business-system",
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

	context := mustRequest(t, server, operator, "/api/v1/business-context", http.StatusOK)
	var body struct {
		Items []struct {
			Key         string `json:"key"`
			DisplayName string `json:"displayName"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(context), &body); err != nil {
		t.Fatal(err)
	}
	if body.Items == nil {
		t.Fatalf("business context must serialize an empty items array, got %s", context)
	}
}
