package app_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAdminAboutBoundaryAndSanitization proves the About endpoint is an
// administrator-only fact projection. The assertion deliberately checks the
// wire shape rather than internal structs so credentials, maintenance items,
// and repair links cannot accidentally become part of the public contract.
func TestAdminAboutBoundaryAndSanitization(t *testing.T) {
	server, admin := newAdminSurface(t)
	defer server.Close()

	mustPost(t, server, admin, "/api/v1/admin/users", `{"clientCommandId":"create-about-operator-102","username":"aboutoperator","displayName":"About Operator","role":"operator","password":"About operator passphrase 2026!"}`, http.StatusCreated)
	login := mustPost(t, server, map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json"}, "/api/v1/auth/login", `{"username":"aboutoperator","password":"About operator passphrase 2026!"}`, http.StatusOK)
	operator := map[string]string{"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": splitCookie(login.headers.Get("Set-Cookie"))}
	mustRequest(t, server, operator, "/api/v1/admin/about", http.StatusForbidden)

	body := mustRequest(t, server, admin, "/api/v1/admin/about", http.StatusOK)
	var about struct {
		ReleaseVersion string `json:"releaseVersion"`
		Maintenance    struct {
			Active     bool   `json:"active"`
			Reason     string `json:"reason"`
			RowVersion int64  `json:"rowVersion"`
		} `json:"maintenance"`
		Components []struct {
			Slot           string `json:"slot"`
			State          string `json:"state"`
			Connected      bool   `json:"connected"`
			ReleaseVersion string `json:"releaseVersion"`
			LastSeenAt     string `json:"lastSeenAt"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(body), &about); err != nil {
		t.Fatal(err)
	}
	if about.Components == nil || len(about.Components) != 2 {
		t.Fatalf("components=%+v, want the two fixed slots", about.Components)
	}
	for _, component := range about.Components {
		if component.Connected || component.ReleaseVersion != "" || component.LastSeenAt != "" {
			t.Fatalf("unconnected component must preserve unknown facts: %+v", component)
		}
	}
	for _, forbidden := range []string{"credential", "token", "maintenanceItems", "repair", "link", "secret"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Fatalf("About leaked forbidden management material %q: %s", forbidden, body)
		}
	}
}

// TestPlatformFaultCannotCreateAnalysis verifies the HTTP guard runs before
// analysis service work. This prevents a platform-only identifier from creating
// an analysis, execution attempt, grant, or collection side effect.
func TestPlatformFaultCannotCreateAnalysis(t *testing.T) {
	stack := newSSEStack(t)
	if _, err := stack.db.Exec(`INSERT INTO platform_faults(component,reason,state,first_seen_at,last_seen_at) VALUES('plinth','runtime_control_stream_disconnected','Firing','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	response := mustPost(t, stack.server, map[string]string{
		"Origin": "https://quoin.example.com", "Content-Type": "application/json", "Cookie": "__Host-quoin-session=" + stack.cookie,
	}, "/api/v1/alerts/platform:1/analyses", `{"clientCommandId":"platform-analysis-102"}`, http.StatusUnprocessableEntity)
	if !strings.Contains(response.body, "平台内部故障") {
		t.Fatalf("unexpected platform analysis rejection: %s", response.body)
	}
	for _, table := range []string{"initial_analyses", "execution_attempts", "attempt_connection_grants", "attempt_artifact_grants"} {
		var count int
		if err := stack.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("platform analysis created %d %s rows", count, table)
		}
	}
}
