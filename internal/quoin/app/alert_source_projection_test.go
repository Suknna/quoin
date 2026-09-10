package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/auth"
)

// TestAlertSourceManagementProjection uses the public HTTP seams: only Admin
// may view source/intake management; receiver configuration is deployment
// authority rather than the request host; waiting is represented by no event
// timestamp and valid observations alone advance that timestamp.
func createOperatorSession(t *testing.T, stack *sseStack) string {
	t.Helper()
	password := "Alert operator passphrase 2026!"
	phc, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := stack.db.Exec(`INSERT INTO users(username,display_name,role,enabled,password_phc,password_change_required,created_at,updated_at) VALUES('alert-operator','Alert Operator','operator',1,?,0,?,?)`, phc, now, now); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, stack.server.URL+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"alert-operator","password":%q}`, password)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://quoin.example.com")
	request.Header.Set("Content-Type", "application/json")
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("operator login status=%d", response.StatusCode)
	}
	return strings.Split(strings.Split(response.Header.Get("Set-Cookie"), ";")[0], "=")[1]
}

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
	if receiverBody.PublicReceiverURL != "https://alerts.example.com/stele/alerts" {
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
