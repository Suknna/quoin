package app_test

// HTTP-level coverage for the T05 admin surface over the real Huma handler:
// the authorization matrix (admin/operator/restricted session), the frozen
// problem+json envelope with the conflict block, ledger replay through the
// real transport, and revoked-session rejection on subsequent requests.
// Sessions come from the shared real-auth fixture: initialization through the
// real flows, logins through the two-step HTTP login.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// newAdminSurface boots the shared real-auth fixture for the admin-surface
// tests. The pending bootstrap administrator is already initialized through
// the real flow; log in over HTTP for an unrestricted admin session.
func newAdminSurface(t *testing.T) *authScenario {
	t.Helper()
	return newAuthScenario(t)
}

func TestAdminUserAuthorizationMatrix(t *testing.T) {
	scenario := newAdminSurface(t)
	server := scenario.server
	adminSession := scenario.login(t, "admin", scenario.adminPassword)
	admin := scenario.sessionHeaders(adminSession)

	// Operator: cannot access audit or user-management APIs. Contacts are
	// display-only since ADR-0010: creation works with or without them, so
	// the authorization matrix is exercised through the contactless shape.
	scenario.createOperator(t, adminSession, "authz-create-01", "op1", "Operator One", "Operator one passphrase 2026!", "op1@example.test")
	operator := scenario.sessionHeaders(scenario.initializeOperatorSession(t, "op1", "Operator one passphrase 2026!", "Operator working passphrase 2028!"))

	listForbidden := mustRequest(t, server, operator, `/api/v1/admin/users`, http.StatusForbidden)
	if !strings.Contains(listForbidden, "forbidden") {
		t.Fatalf("operator listUsers must be forbidden with code, got %s", listForbidden)
	}
	denied := mustPost(t, server, operator, `/api/v1/admin/users`, `{"clientCommandId":"authz-create-02","username":"op2","displayName":"Two","role":"operator","password":"Another operator passphrase 2027!","contacts":[{"channel":"email","target":"op2@example.test"}]}`, http.StatusForbidden)
	if !strings.Contains(denied.body, "需要管理员权限") {
		t.Fatalf("operator write must be forbidden with a human message, got %s", denied)
	}
	// Audit records are platform management data, not ordinary business context.
	audit := mustRequest(t, server, operator, `/api/v1/audit-events?action=user.create`, http.StatusForbidden)
	if !strings.Contains(audit, `"forbidden"`) {
		t.Fatalf("operator audit access must be forbidden, got %s", audit)
	}
	// Own sessions visible to the operator with the current marker.
	sessions := mustRequest(t, server, operator, `/api/v1/auth/sessions`, http.StatusOK)
	if !strings.Contains(sessions, `"current":true`) {
		t.Fatalf("own sessions must mark the current session, got %s", sessions)
	}

	// Reset returns the operator to the uninitialized temporary state, so the
	// temporary credential re-enters the operator initialization flow (new
	// formal password plus the assigned factor) instead of ever issuing a
	// workbench session — full or restricted alike.
	users := mustRequest(t, server, admin, `/api/v1/admin/users`, http.StatusOK)
	var list struct {
		Items []struct {
			ID         string `json:"id"`
			RowVersion int64  `json:"rowVersion"`
			Username   string `json:"username"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(users), &list); err != nil {
		t.Fatal(err)
	}
	var op1 string
	var op1Row int64
	for _, item := range list.Items {
		if item.Username == "op1" {
			op1, op1Row = item.ID, item.RowVersion
		}
	}
	if op1 == "" {
		t.Fatalf("op1 missing from user list: %s", users)
	}
	reset := mustPost(t, server, admin, `/api/v1/admin/users/`+op1+`/reset-password`, `{"clientCommandId":"authz-reset-01","expectedRowVersion":`+itoa(op1Row)+`,"newPassword":"Replacement passphrase 2027!"}`, http.StatusOK)
	if !strings.Contains(reset.body, `"revokedSessionCount":2`) {
		t.Fatalf("reset must report the revoked session count, got %s", reset)
	}
	if !strings.Contains(reset.body, `"initialized":false`) {
		t.Fatalf("reset must return the operator to the uninitialized state, got %s", reset)
	}
	// The temporary credential drives the real operator initialization flow
	// over HTTP (the helper asserts the flow type is operator_initialize) and
	// ends in the two-step login with a full, unrestricted session.
	operator = scenario.sessionHeaders(scenario.initializeOperatorSession(t, "op1", "Replacement passphrase 2027!", "Replacement working passphrase 2029!"))
	sessionsAfter := mustRequest(t, server, operator, `/api/v1/auth/sessions`, http.StatusOK)
	if strings.Contains(sessionsAfter, "password_change_required") {
		t.Fatalf("the re-initialized session must not be restricted, got %s", sessionsAfter)
	}
	// Revoked session: admin revokes the operator's sessions; the very next
	// authenticated request fails with 401.
	usersAfter := mustRequest(t, server, admin, `/api/v1/admin/users`, http.StatusOK)
	if err := json.Unmarshal([]byte(usersAfter), &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.Username == "op1" {
			op1, op1Row = item.ID, item.RowVersion
		}
	}
	mustPost(t, server, admin, `/api/v1/admin/users/`+op1+`/revoke-sessions`, `{"clientCommandId":"authz-revoke-01"}`, http.StatusOK)
	rejected := manualGet(t, server, operator, `/api/v1/auth/sessions`)
	if rejected != http.StatusUnauthorized {
		t.Fatalf("revoked session must be rejected with 401, got %d", rejected)
	}
}

func manualGet(t *testing.T, server *httptest.Server, headers map[string]string, path string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func TestAdminUserCommandReplayOverHTTP(t *testing.T) {
	scenario := newAdminSurface(t)
	server := scenario.server
	admin := scenario.sessionHeaders(scenario.login(t, "admin", scenario.adminPassword))
	// Contacts ride along as display-only data (ADR-0010); the replay
	// semantics below are independent of their presence.
	command := `{"clientCommandId":"replay-create-01","username":"op9","displayName":"Nine","role":"operator","password":"Operator nine passphrase 2026!","contacts":[{"channel":"email","target":"op9@example.test"}]}`
	first := mustPost(t, server, admin, `/api/v1/admin/users`, command, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(first.body), &created); err != nil {
		t.Fatal(err)
	}
	second := mustPost(t, server, admin, `/api/v1/admin/users`, command, http.StatusCreated)
	if first.body != second.body {
		t.Fatalf("replay must return the identical body:\nfirst=%s\nsecond=%s", first.body, second.body)
	}
	if duplicate := mustPost(t, server, admin, `/api/v1/admin/users`, `{"clientCommandId":"replay-create-02","username":"op9","displayName":"Nine","role":"operator","password":"Operator nine passphrase 2026!","contacts":[{"channel":"email","target":"op9@example.test"}]}`, http.StatusConflict); !strings.Contains(duplicate.body, "用户名已存在") {
		t.Fatalf("duplicate username must conflict with a human message, got %s", duplicate.body)
	}
	reused := mustPost(t, server, admin, `/api/v1/admin/users`, `{"clientCommandId":"replay-create-01","username":"op9","displayName":"Renamed","role":"operator","password":"Operator nine passphrase 2026!","contacts":[{"channel":"email","target":"op9@example.test"}]}`, http.StatusConflict)
	if !strings.Contains(reused.body, `"conflict":{"code":"command_id_reused"}`) && !strings.Contains(reused.body, `"code":"command_id_reused"`) {
		t.Fatalf("same key with a different request must reuse-conflict, got %s", reused.body)
	}
	// Row-version conflict carries the authoritative version in the envelope.
	mustDo(t, server, http.MethodPatch, admin, `/api/v1/admin/users/`+created.ID, `{"clientCommandId":"replay-update-01","expectedRowVersion":1,"displayName":"Nine Renamed"}`, http.StatusOK)
	stale := mustDo(t, server, http.MethodPatch, admin, `/api/v1/admin/users/`+created.ID, `{"clientCommandId":"replay-update-02","expectedRowVersion":1,"displayName":"Stale Name"}`, http.StatusConflict)
	if !strings.Contains(stale.body, `"rowVersion":`) {
		t.Fatalf("row-version conflict must carry the current version, got %s", stale.body)
	}
	// The frozen problem envelope fields are present.
	var problem struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
		Conflict  struct {
			Code       string `json:"code"`
			RowVersion int64  `json:"rowVersion"`
		} `json:"conflict"`
	}
	if err := json.Unmarshal([]byte(stale.body), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "row_version_conflict" || problem.Conflict.Code != "row_version_conflict" || problem.Conflict.RowVersion == 0 {
		t.Fatalf("frozen envelope mismatch: %+v", problem)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// TestCreateUserContactsOptionalOverHTTP pins the ADR-0010 acceptance drift:
// contacts are display-only and optional (0..2), creation succeeds with and
// without them, and the structured email/sms targets stay validated.
func TestCreateUserContactsOptionalOverHTTP(t *testing.T) {
	scenario := newAdminSurface(t)
	server := scenario.server
	admin := scenario.sessionHeaders(scenario.login(t, "admin", scenario.adminPassword))

	// Omitted entirely (the shape the real #96/#102 e2e drivers send): 201.
	created := mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-01","username":"opnoc","displayName":"No Contacts","role":"operator","password":"Operator noc passphrase 2026!"}`,
		http.StatusCreated)
	if !strings.Contains(created.body, `"username":"opnoc"`) {
		t.Fatalf("contactless creation must return the created user, got %s", created.body)
	}

	// Explicit empty array carries the same meaning: 201.
	mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-02","username":"opempty","displayName":"Empty Set","role":"operator","password":"Operator empty passphrase 2026!","contacts":[]}`,
		http.StatusCreated)

	// With contacts (the OIDC-era informational address): still 201.
	mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-03","username":"opwith","displayName":"With Contacts","role":"operator","password":"Operator with passphrase 2026!","contacts":[{"channel":"email","target":"opwith@example.test"},{"channel":"sms","target":"+8613800000000"}]}`,
		http.StatusCreated)

	// Already-configured display contacts are preserved, never dropped by
	// the optional-contacts change: the owner sees them masked.
	opwithFormal := scenario.initializeOperatorSession(t, "opwith", "Operator with passphrase 2026!", "Operator with formal passphrase 2027!")
	own := mustRequest(t, server, scenario.sessionHeaders(opwithFormal), `/api/v1/auth/contacts`, http.StatusOK)
	if !strings.Contains(own, "o***@example.test") || !strings.Contains(own, `"channel":"sms"`) {
		t.Fatalf("configured contacts must survive creation and display masked, got %s", own)
	}
	if strings.Contains(own, "opwith@example.test") || strings.Contains(own, "+8613800000000") {
		t.Fatalf("raw targets must never leave the server, got %s", own)
	}

	// Duplicate channels stay a deterministic 422 (one contact per channel).
	duplicate := mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-04","username":"opdupchan","displayName":"Dup Channel","role":"operator","password":"Operator dupchan passphrase 2026!","contacts":[{"channel":"email","target":"a@example.test"},{"channel":"email","target":"b@example.test"}]}`,
		http.StatusUnprocessableEntity)
	if !strings.Contains(duplicate.body, "validation_failed") {
		t.Fatalf("duplicate channel must fail validation with the frozen code, got %s", duplicate.body)
	}

	// Structured targets keep their server-side validation: a malformed
	// email or phone number is a 422, never a silent store.
	badEmail := mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-05","username":"opbadmail","displayName":"Bad Mail","role":"operator","password":"Operator badmail passphrase 2026!","contacts":[{"channel":"email","target":"not-an-email"}]}`,
		http.StatusUnprocessableEntity)
	if !strings.Contains(badEmail.body, "validation_failed") {
		t.Fatalf("malformed email must fail validation, got %s", badEmail.body)
	}
	badPhone := mustPost(t, server, admin, `/api/v1/admin/users`,
		`{"clientCommandId":"contacts-optional-06","username":"opbadphone","displayName":"Bad Phone","role":"operator","password":"Operator badphone passphrase 2026!","contacts":[{"channel":"sms","target":"123"}]}`,
		http.StatusUnprocessableEntity)
	if !strings.Contains(badPhone.body, "validation_failed") {
		t.Fatalf("malformed phone must fail validation, got %s", badPhone.body)
	}

	// The contactless operator follows the ADR-0010 lifecycle: the temporary
	// password logs into a restricted session and the forced change unlocks
	// the workbench — no code reception anywhere.
	formal := scenario.initializeOperatorSession(t, "opnoc", "Operator noc passphrase 2026!", "Operator noc formal passphrase 2027!")
	users := mustRequest(t, server, scenario.sessionHeaders(formal), `/api/v1/auth/sessions`, http.StatusOK)
	if strings.Contains(users, "password_change_required") {
		t.Fatalf("the contactless operator must reach a full session after the forced change, got %s", users)
	}
}
