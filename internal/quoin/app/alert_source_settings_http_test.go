package app_test

// Alert-source instance settings over the real HTTP surface (ADR-0014 story
// 2): create accepts the settings document, update is a fenced replayable
// admin command, invalid documents are deterministic 400s, and stale row
// versions are deterministic 409s.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAlertSourceSettingsLifecycleOverRealServer(t *testing.T) {
	scenario := newAuthScenario(t)
	server := scenario.server
	adminSession := scenario.login(t, "admin", scenario.adminPassword)
	admin := scenario.sessionHeaders(adminSession)

	// alertmanager declares no settings schema: a non-empty document is a
	// deterministic 400 and creates nothing (invalid config surface).
	mustPost(t, server, admin,
		`/api/v1/alert-sources`, `{"key":"plain-am","protocol":"alertmanager","settings":{"x":1},"clientCommandId":"am-settings-0001"}`, http.StatusBadRequest)
	var count int
	if err := scenario.db.QueryRow(`SELECT COUNT(*) FROM alert_sources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected create persisted rows: %d err=%v", count, err)
	}

	// Absent settings create the default empty document.
	mustPost(t, server, admin,
		`/api/v1/alert-sources`, `{"key":"plain-am","protocol":"alertmanager","clientCommandId":"am-settings-0002"}`, http.StatusCreated)

	// The settings command stores the document, returns the fresh detail and
	// is replayable: the same clientCommandId returns the identical body.
	update := mustPost(t, server, admin,
		`/api/v1/alert-sources/plain-am/settings`, `{"settings":{},"expectedRowVersion":1,"clientCommandId":"am-set-0001"}`, http.StatusOK)
	replay := mustPost(t, server, admin,
		`/api/v1/alert-sources/plain-am/settings`, `{"settings":{},"expectedRowVersion":1,"clientCommandId":"am-set-0001"}`, http.StatusOK)
	if replay.body != update.body {
		t.Fatalf("replay body diverged:\n%s\n%s", update.body, replay.body)
	}
	var detail struct {
		Body struct {
			Key        string          `json:"key"`
			Settings   json.RawMessage `json:"settings"`
			RowVersion int64           `json:"rowVersion"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(update.body), &detail.Body); err != nil {
		t.Fatalf("update body malformed: %s", update.body)
	}
	if detail.Body.Key != "plain-am" || string(detail.Body.Settings) != "{}" {
		t.Fatalf("update detail = %s", update.body)
	}

	// A stale expectedRowVersion is a recorded 409; its command id is now
	// consumed by the rejected ledger row, so the corrected retry carries a
	// fresh id under the current row version.
	mustPost(t, server, admin,
		`/api/v1/alert-sources/plain-am/settings`, `{"settings":{},"expectedRowVersion":1,"clientCommandId":"am-set-0002"}`, http.StatusConflict)
	mustPost(t, server, admin,
		`/api/v1/alert-sources/plain-am/settings`, `{"settings":{},"expectedRowVersion":2,"clientCommandId":"am-set-0003"}`, http.StatusOK)

	// Unknown source is 404.
	mustPost(t, server, admin,
		`/api/v1/alert-sources/ghost/settings`, `{"settings":{},"expectedRowVersion":1,"clientCommandId":"am-set-0004"}`, http.StatusNotFound)

	// The settings read model is authoritative in list + detail.
	list := mustRequest(t, server, admin, `/api/v1/alert-sources`, http.StatusOK)
	var listed struct {
		Items []struct {
			Key      string          `json:"key"`
			Settings json.RawMessage `json:"settings"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(list), &listed); err != nil {
		t.Fatalf("list malformed: %s", list)
	}
	if len(listed.Items) != 1 || listed.Items[0].Key != "plain-am" || string(listed.Items[0].Settings) != "{}" {
		t.Fatalf("list settings = %s", list)
	}

	// Operator role is forbidden from the settings command (same boundary as
	// the other source management commands).
	operatorPassword := "Operator passphrase 2031!"
	scenario.createOperator(t, adminSession, "op-settings-0001", "settings-operator", "Settings Operator", operatorPassword, "operator@example.test")
	operator := scenario.sessionHeaders(scenario.initializeOperatorSession(t, "settings-operator", operatorPassword, "Operator passphrase 2032!"))
	mustPost(t, server, operator,
		`/api/v1/alert-sources/plain-am/settings`, `{"settings":{},"expectedRowVersion":3,"clientCommandId":"am-set-0005"}`, http.StatusForbidden)
}
