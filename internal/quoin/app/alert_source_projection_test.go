package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
)

// createOperatorSession provisions a real operator through the shared
// fixture: created by the admin with an assigned OTP contact (mandatory for
// second-factor delivery), initialized via the real operator flow, session
// from the two-step login.
func createOperatorSession(t *testing.T, stack *sseStack) string {
	t.Helper()
	stack.scenario.createOperator(t, stack.loginAdmin(t), "alert-operator-create-1", "alert-operator", "Alert Operator", "Alert operator passphrase 2026!", "alert-operator@example.test")
	return stack.scenario.initializeOperatorSession(t, "alert-operator", "Alert operator passphrase 2026!", "Alert operator formal passphrase 2027!")
}

// TestAlertSourceManagementProjection uses the public HTTP seams: only Admin
// may view source/intake management; receiver configuration is deployment
// authority rather than the request host; waiting is represented by no event
// timestamp and valid observations alone advance that timestamp.
func TestAlertSourceManagementProjection(t *testing.T) {
	stack := newSSEStack(t)
	operatorCookie := createOperatorSession(t, stack)

	request := func(cookie, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, stack.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", "__Host-quoin-session="+cookie)
		response, err := stack.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	for _, path := range []string{
		"/api/v1/alert-sources",
		"/api/v1/alert-sources/sse",
		"/api/v1/alert-sources/sse/credentials",
		"/api/v1/alert-intake-issues",
		"/api/v1/alert-sources/receiver-config",
	} {
		response := request(operatorCookie, path)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("operator %s: status=%d want=403", path, response.StatusCode)
		}
	}

	receiver := request(stack.cookie, "/api/v1/alert-sources/receiver-config")
	defer receiver.Body.Close()
	if receiver.StatusCode != http.StatusOK {
		t.Fatalf("receiver config status=%d", receiver.StatusCode)
	}
	var receiverBody struct {
		PublicReceiverURL string `json:"publicReceiverUrl"`
	}
	if err := json.NewDecoder(receiver.Body).Decode(&receiverBody); err != nil {
		t.Fatal(err)
	}
	if receiverBody.PublicReceiverURL != "https://alerts.example.com/stele/webhook/alertmanager" {
		t.Fatalf("receiver URL=%q", receiverBody.PublicReceiverURL)
	}

	list := func() alerts.SourceSummary {
		t.Helper()
		response := request(stack.cookie, "/api/v1/alert-sources")
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("source list status=%d", response.StatusCode)
		}
		var body struct {
			Items []alerts.SourceSummary `json:"items"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != 1 {
			t.Fatalf("sources=%+v", body.Items)
		}
		return body.Items[0]
	}

	if source := list(); source.LatestValidEventAt != nil {
		t.Fatalf("new source must be waiting, got latest event=%q", *source.LatestValidEventAt)
	}
	// A malformed body is a persisted rejected delivery but has no normal
	// observation, so it must not make a waiting source look healthy.
	if _, err := stack.alerts.Deliver(context.Background(), "projection-malformed", stack.source, stack.creds, 1, []byte("{"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if source := list(); source.LatestValidEventAt != nil {
		t.Fatalf("malformed delivery advanced latest valid event=%q", *source.LatestValidEventAt)
	}

	stack.deliver(t, "projection-firing", "ReceiverProjection", "firing")
	if source := list(); source.LatestValidEventAt == nil || *source.LatestValidEventAt == "" {
		t.Fatal("valid observation did not advance latest valid event")
	}
}
