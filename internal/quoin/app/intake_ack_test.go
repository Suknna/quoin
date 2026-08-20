package app_test

// The intake-issue acknowledge command and the state=Resolved history view
// over the real HTTP surface. Split from alerts_events_test.go to keep each
// file below the 500-line guidance.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// filter on listAlerts.
func TestIntakeIssueAcknowledgeAndHistoryView(t *testing.T) {
	stack := newSSEStack(t)

	// One fingerprint-mismatch item creates an unacknowledged intake issue.
	body := []byte(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"Bad"},"startsAt":"2026-08-18T15:00:00Z","fingerprint":"0123456789abcdef"}],"truncatedAlerts":0}`)
	if _, err := stack.alerts.Deliver(context.Background(), "ack-r1", stack.source, stack.creds, 1, body, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	listURL := stack.server.URL + "/api/v1/alert-intake-issues"
	request, _ := http.NewRequest(http.MethodGet, listURL, nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+stack.cookie)
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var listObj struct {
		Items []struct {
			ID         string `json:"id"`
			Kind       string `json:"kind"`
			RowVersion int64  `json:"rowVersion"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &listObj); err != nil || len(listObj.Items) != 1 {
		t.Fatalf("intake list=%s err=%v", raw, err)
	}
	if listObj.Items[0].Kind != "fingerprint_mismatch" {
		t.Fatalf("kind=%s", listObj.Items[0].Kind)
	}
	issueID, rowVersion := listObj.Items[0].ID, listObj.Items[0].RowVersion

	ack := func(cookie string, expected int64) *http.Response {
		payload := fmt.Sprintf(`{"clientCommandId":"ack-cmd-%d","expectedRowVersion":%d}`, expected, expected)
		request, _ := http.NewRequest(http.MethodPost, listURL+"/"+issueID+"/acknowledge", strings.NewReader(payload))
		request.Header.Set("Cookie", "__Host-quoin-session="+cookie)
		request.Header.Set("Origin", "https://quoin.example.com")
		request.Header.Set("Content-Type", "application/json")
		response, err := stack.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	// Stale expectedRowVersion → 409.
	stale := ack(stack.cookie, rowVersion+5)
	staleRaw, _ := io.ReadAll(stale.Body)
	stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale acknowledge status=%d body=%s", stale.StatusCode, staleRaw)
	}

	// Correct version → 204; the issue leaves the open list.
	ok := ack(stack.cookie, rowVersion)
	okRaw, _ := io.ReadAll(ok.Body)
	ok.Body.Close()
	if ok.StatusCode != http.StatusNoContent {
		t.Fatalf("acknowledge status=%d body=%s", ok.StatusCode, okRaw)
	}

	// Re-acknowledge with the old version → 409 (sticky, cannot repeat).
	again := ack(stack.cookie, rowVersion)
	again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("repeat acknowledge status=%d", again.StatusCode)
	}

	// state=Resolved history view: nothing resolved yet → empty items.
	historyRequest, _ := http.NewRequest(http.MethodGet, stack.server.URL+"/api/v1/alerts?state=Resolved", nil)
	historyRequest.Header.Set("Cookie", "__Host-quoin-session="+stack.cookie)
	historyResponse, err := stack.server.Client().Do(historyRequest)
	if err != nil {
		t.Fatal(err)
	}
	historyRaw, _ := io.ReadAll(historyResponse.Body)
	historyResponse.Body.Close()
	var historyObj struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(historyRaw, &historyObj); err != nil || len(historyObj.Items) != 0 {
		t.Fatalf("history view=%s err=%v", historyRaw, err)
	}

	// Resolve an alert and see it in the history view.
	stack.deliver(t, "ack-r2", "HistOne", "firing")
	stack.deliver(t, "ack-r3", "HistOne", "resolved")
	historyResponse2, err := stack.server.Client().Do(historyRequest)
	if err != nil {
		t.Fatal(err)
	}
	historyRaw2, _ := io.ReadAll(historyResponse2.Body)
	historyResponse2.Body.Close()
	var historyObj2 struct {
		Items []struct {
			State string `json:"state"`
		} `json:"items"`
	}
	if err := json.Unmarshal(historyRaw2, &historyObj2); err != nil || len(historyObj2.Items) != 1 || historyObj2.Items[0].State != "Resolved" {
		t.Fatalf("history view after resolve=%s err=%v", historyRaw2, err)
	}
}
