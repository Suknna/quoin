package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/pluginevents"
)

func TestPluginEventDeadlettersAdminListAndReplay(t *testing.T) {
	application, authService := newAdmissionTestServer(t)
	dispatcher, err := pluginevents.NewDispatcher(application.db, application.reader, application.pluginRegistry, nil)
	if err != nil {
		t.Fatal(err)
	}
	application.pluginEvents = dispatcher
	now := time.Now().UTC().Format(time.RFC3339Nano)
	event, err := application.db.Exec(`INSERT INTO plugin_events(event_type,payload_version,committed_at,refs_json) VALUES('quoin.alert.observation.committed',1,?,'{}')`, now)
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := event.LastInsertId()
	delivery, err := application.db.Exec(`INSERT INTO plugin_event_deliveries(event_id,subscriber_id,state,attempts,last_error,created_at) VALUES(?,'test-plugin','deadletter',1,'handler_failed',?)`, eventID, now)
	if err != nil {
		t.Fatal(err)
	}
	deliveryID, _ := delivery.LastInsertId()
	handler, err := NewHandler(application, "https://quoin.example.com")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	path := "/api/v1/integrations/plugin-events/deadletters"
	response, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous deadletter list status=%d", response.StatusCode)
	}
	bearer := admissionLogin(t, authService)
	request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Count int64                       `json:"count"`
		Items []pluginevents.DeadDelivery `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || listed.Count != 1 || len(listed.Items) != 1 || listed.Items[0].DeliveryID != deliveryID {
		t.Fatalf("admin deadletters status=%d body=%+v", response.StatusCode, listed)
	}
	replayURL := server.URL + path + "/" + strconv.FormatInt(deliveryID, 10) + "/replay"
	request, _ = http.NewRequest(http.MethodPost, replayURL, strings.NewReader(`{"clientCommandId":"replay-deadletter-1"}`))
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	request.Header.Set("Origin", "https://quoin.example.com")
	request.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("admin replay status=%d", response.StatusCode)
	}
	var state string
	if err := application.db.QueryRow(`SELECT state FROM plugin_event_deliveries WHERE id=?`, deliveryID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("replayed delivery state=%q err=%v", state, err)
	}
	request, _ = http.NewRequest(http.MethodPost, replayURL, strings.NewReader(`{"clientCommandId":"replay-deadletter-1"}`))
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	request.Header.Set("Origin", "https://quoin.example.com")
	request.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent command replay status=%d, want original success", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, replayURL, strings.NewReader(`{"clientCommandId":"replay-deadletter-2"}`))
	request.Header.Set("Cookie", "__Host-quoin-session="+bearer)
	request.Header.Set("Origin", "https://quoin.example.com")
	request.Header.Set("Content-Type", "application/json")
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("new command replaying a pending delivery status=%d, want conflict", response.StatusCode)
	}
}
