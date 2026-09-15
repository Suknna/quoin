package app

// HTTP-level coverage for the consolidated audit surface over the canonical
// contract schema (internal/gen/contracts/schema.sql — the applied DDL, not a
// parallel copy): the event list projection with filters and keyset paging,
// the retention settings read/preview, and the PATCH command path through the
// shared execution runner (ledger, replay, row-version conflict, in-transaction
// session revalidation). The test admission middleware mirrors
// internal/quoin/operations/admission.go: request-scoped execution metadata is
// required, never synthesized inside handlers.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	_ "modernc.org/sqlite"
)

type auditSurface struct {
	server *httptest.Server
	app    *apiServer
	db     *sql.DB
	admin  struct {
		id      int64
		cookie  string
		session int64
	}
	operator struct {
		id      int64
		cookie  string
		session int64
	}
}

func newAuditSurface(t *testing.T) *auditSurface {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "quoin.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=recursive_triggers(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(gencontracts.SchemaSQL); err != nil {
		t.Fatal(err)
	}
	// The bootstrap seeds both audit singletons (bootstrap.initializeDatabase);
	// tests reproduce exactly those rows on the canonical schema.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO audit_retention(id,cleanup_enabled,updated_at) VALUES(1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO audit_cleanup_permits(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}

	adminID, err := newAuditUser(t, db, "admin", "admin", "Audit Admin Passphrase 2026!")
	if err != nil {
		t.Fatal(err)
	}
	operatorID, err := newAuditUser(t, db, "operator", "operator", "Audit Operator Pass 2026!")
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if err := authService.SetReader(reader); err != nil {
		t.Fatal(err)
	}
	application := newAPIServer(authService, db, "")
	if err := application.configureReadOnly(reader); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiConfig := huma.DefaultConfig("Quoin audit test API", "test")
	apiConfig.OpenAPIPath, apiConfig.DocsPath, apiConfig.SchemasPath = "", "", ""
	apiConfig.Transformers, apiConfig.CreateHooks = []huma.Transformer{}, nil
	api := humago.New(mux, apiConfig)
	// Admission simulation: the middleware resolves the session subject and
	// attaches the root execution metadata — including the session proof
	// reference — before any handler runs, exactly like the production guard.
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		correlation, err := execution.NewCorrelationID()
		if err != nil {
			ctx.SetStatus(http.StatusServiceUnavailable)
			return
		}
		meta := execution.Metadata{
			CorrelationID: correlation,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
			Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: correlation},
		}
		if cookie := auditCookieValue(ctx.Header("Cookie"), "__Host-quoin-session"); cookie != "" {
			if session, authErr := application.auth.Authenticate(ctx.Context(), cookie); authErr == nil {
				meta.Actor = execution.Principal{Kind: execution.PrincipalUser, ID: session.User.ID}
				meta.Session = execution.SessionRef{ID: session.ID, AuthRevision: session.User.AuthRevision}
			}
		}
		meta.Initiator = meta.Actor
		enriched, err := execution.WithMetadata(ctx.Context(), meta)
		if err != nil {
			ctx.SetStatus(http.StatusServiceUnavailable)
			return
		}
		next(huma.WithContext(ctx, enriched))
	})
	application.registerAuditRoutes(api)

	surface := &auditSurface{server: httptest.NewServer(mux), app: application, db: db}
	t.Cleanup(surface.server.Close)
	surface.admin.id = adminID
	surface.admin.cookie, surface.admin.session, err = surface.newSession(adminID)
	if err != nil {
		t.Fatal(err)
	}
	surface.operator.id = operatorID
	surface.operator.cookie, surface.operator.session, err = surface.newSession(operatorID)
	if err != nil {
		t.Fatal(err)
	}
	return surface
}

// newAuditUser inserts a ready-to-use user row the way app_test.go seeds its
// operator: real Argon2id hash, unrestricted session flag, current revision.
func newAuditUser(t *testing.T, db *sql.DB, username, displayName, password string) (int64, error) {
	t.Helper()
	phc, err := auth.HashPassword(password)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(context.Background(),
		`INSERT INTO users(username,display_name,role,enabled,auth_revision,initialized,password_phc,password_change_required,created_at,updated_at) VALUES(?,?,?,1,1,1,?,0,?,?)`,
		username, displayName, username, phc, now, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// newSession issues a session row directly and returns its bearer cookie
// value plus the session id; the insert trigger enforces the current auth
// revision exactly like the real login path. The optional window offsets
// (idle, absolute) from now override the production defaults so tests can
// create already-expired sessions — the schema only lets these fields move
// forward after insertion.
func (surface *auditSurface) newSession(userID int64, windows ...time.Duration) (string, int64, error) {
	idleIn, absoluteIn := 12*time.Hour, 7*24*time.Hour
	if len(windows) > 0 {
		idleIn = windows[0]
	}
	if len(windows) > 1 {
		absoluteIn = windows[1]
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256(raw)
	var revision int64
	if err := surface.db.QueryRow(`SELECT auth_revision FROM users WHERE id=?`, userID).Scan(&revision); err != nil {
		return "", 0, err
	}
	now := time.Now().UTC()
	result, err := surface.db.Exec(
		`INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		userID, digest[:], revision, "audit test", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		now.Add(idleIn).Format(time.RFC3339Nano), now.Add(absoluteIn).Format(time.RFC3339Nano))
	if err != nil {
		return "", 0, err
	}
	sessionID, err := result.LastInsertId()
	if err != nil {
		return "", 0, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), sessionID, nil
}

func auditCookieValue(header, name string) string {
	if header == "" {
		return ""
	}
	request := http.Request{Header: http.Header{"Cookie": []string{header}}}
	if cookie, err := request.Cookie(name); err == nil {
		return cookie.Value
	}
	return ""
}

func (surface *auditSurface) do(t *testing.T, method, path, cookie, body string) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, surface.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: "__Host-quoin-session", Value: cookie})
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := surface.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw := &strings.Builder{}
	if _, err := io.Copy(raw, response.Body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw.String()
}

// seedAuditEvent writes one well-formed event through the shared writer so the
// list projection is exercised against rows produced by the real insert path.
func seedAuditEvent(t *testing.T, db *sql.DB, record audit.Record) int64 {
	t.Helper()
	id, err := audit.NewWriter().Write(context.Background(), db, record)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAuditEventsListProjectsFiltersAndPages(t *testing.T) {
	surface := newAuditSurface(t)
	// Pre-consolidation history: correlation-less rows exist only as legacy
	// data, inserted first so record order (id) matches creation order; the
	// read model must surface them verbatim, never guess a correlation
	// (docs/audit-design.md §8).
	if _, err := surface.db.Exec(`INSERT INTO audit_events(actor_type,actor_id,action,outcome,created_at) VALUES('user',?,'legacy.action','success','2020-01-01T00:00:00.000000000Z')`, surface.admin.id); err != nil {
		t.Fatal(err)
	}
	seedAuditEvent(t, surface.db, audit.Record{
		ActorType: audit.ActorUser, ActorID: surface.admin.id, Action: "user.create", Outcome: audit.OutcomeSuccess,
		CorrelationID: "corr-create", RequestID: "req-1", InitiatorType: audit.ActorUser, InitiatorID: surface.admin.id,
		DomainRefType: "user", DomainRefID: surface.operator.id, ClientCommandID: "cmd-create-01",
		Targets: []audit.RecordTarget{{Type: "user", ID: surface.operator.id}},
	})
	seedAuditEvent(t, surface.db, audit.Record{
		ActorType: audit.ActorUser, ActorID: surface.operator.id, Action: "alert.update", Outcome: audit.OutcomeRejected,
		CorrelationID: "corr-reject", InitiatorType: audit.ActorUser, InitiatorID: surface.operator.id,
	})
	seedAuditEvent(t, surface.db, audit.Record{
		ActorType: audit.ActorSystem, ActorID: 0, Action: "audit.retention.cleanup", Outcome: audit.OutcomeSuccess,
		CorrelationID: "corr-cleanup", InitiatorType: audit.ActorSystem,
	})

	status, body := surface.do(t, http.MethodGet, "/api/v1/audit-events", surface.admin.cookie, "")
	if status != http.StatusOK {
		t.Fatalf("GET audit-events: status=%d body=%s", status, body)
	}
	var page struct {
		Items []struct {
			ID            string `json:"id"`
			CorrelationID string `json:"correlationId"`
			ActorType     string `json:"actorType"`
			ActorID       string `json:"actorId"`
			Action        string `json:"action"`
			Outcome       string `json:"outcome"`
			Phase         string `json:"phase"`
			DomainRefType string `json:"domainRefType"`
			DomainRefID   string `json:"domainRefId"`
			ReqID         string `json:"requestId"`
			TaskID        string `json:"taskId"`
			CreatedAt     string `json:"createdAt"`
		} `json:"items"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("body is not the projected page: %s", body)
	}
	if len(page.Items) != 4 {
		t.Fatalf("expected 4 events newest-first, got %d: %s", len(page.Items), body)
	}
	if page.Items[0].Action != "audit.retention.cleanup" || page.Items[3].Action != "legacy.action" {
		t.Fatalf("events must be newest-first, got %s then %s", page.Items[0].Action, page.Items[3].Action)
	}
	newest := page.Items[0]
	if newest.ID == "" || strings.ContainsAny(newest.ID, ".") || newest.ActorType != "system" || newest.ActorID != "0" {
		t.Fatalf("ids and actor must be projected strings, got %+v", newest)
	}
	if newest.CorrelationID != "corr-cleanup" || newest.Outcome != "success" || newest.CreatedAt == "" {
		t.Fatalf("consolidated fields missing on system event: %+v", newest)
	}
	legacy := page.Items[3]
	if legacy.CorrelationID != "" {
		t.Fatalf("legacy history must keep correlation empty, got %+v", legacy)
	}
	// The writer row projection: ids as strings, the target reference and the
	// request id carried through, optional fields the model has no source for
	// left absent.
	created := page.Items[2]
	if created.ID == "" || created.CorrelationID != "corr-create" || created.DomainRefType != "user" || created.DomainRefID == "" {
		t.Fatalf("writer row projection mismatch: %+v", created)
	}
	if created.ReqID == "" || created.TaskID != "" || created.Outcome != "success" {
		t.Fatalf("unexpected projection fields: %+v", created)
	}
	for _, raw := range []string{"\"correlationId\"", "\"actorType\"", "\"actorId\"", "\"createdAt\""} {
		if !strings.Contains(body, raw) {
			t.Fatalf("UI camelCase key %s missing from body: %s", raw, body)
		}
	}

	// Filters hit exactly the requested slice; empty results stay valid pages.
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?outcome=rejected", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, "alert.update") || strings.Contains(body, "user.create") {
		t.Fatalf("outcome filter failed: status=%d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?correlationId=corr-create", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, "user.create") || strings.Contains(body, "alert.update") {
		t.Fatalf("correlation filter failed: status=%d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?actorType=system", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, "audit.retention.cleanup") || strings.Contains(body, "\"actorType\":\"user\"") {
		t.Fatalf("actor filter failed: status=%d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?until=2021-01-01T00:00:00Z", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, "legacy.action") || strings.Contains(body, "user.create") {
		t.Fatalf("until bound must include only the legacy row: status=%d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?since=2099-01-01T00:00:00Z", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, `"items":[]`) {
		t.Fatalf("future since must return an empty page: status=%d body=%s", status, body)
	}
	status, _ = surface.do(t, http.MethodGet, "/api/v1/audit-events?outcome=exploded", surface.admin.cookie, "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown outcome must be rejected with 422, got %d", status)
	}
	status, _ = surface.do(t, http.MethodGet, "/api/v1/audit-events?cursor=zzz", surface.admin.cookie, "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed cursor must be rejected with 400, got %d", status)
	}

	// Keyset paging: a full page carries the opaque cursor; the next page
	// resumes without overlap until exhaustion.
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?limit=2", surface.admin.cookie, "")
	var first struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page must be full with a cursor: status=%d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?limit=2&cursor="+first.NextCursor, surface.admin.cookie, "")
	var second struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || len(second.Items) != 2 {
		t.Fatalf("second page must return the remaining events: status=%d body=%s", status, body)
	}
	// A full page may speculatively carry a successor cursor (keyset contract);
	// following it must yield the honest empty page.
	if second.NextCursor != "" {
		status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events?limit=2&cursor="+second.NextCursor, surface.admin.cookie, "")
		if status != http.StatusOK || !strings.Contains(body, `"items":[]`) {
			t.Fatalf("following the last cursor must yield an empty page: status=%d body=%s", status, body)
		}
	}
}

func TestAuditEventsRequiresCurrentAdmin(t *testing.T) {
	surface := newAuditSurface(t)
	status, body := surface.do(t, http.MethodGet, "/api/v1/audit-events", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous list must be 401, got %d body=%s", status, body)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/audit-events", surface.operator.cookie, "")
	if status != http.StatusForbidden || !strings.Contains(body, `"forbidden"`) {
		t.Fatalf("operator list must be 403 forbidden, got %d body=%s", status, body)
	}
	// A session revoked after issuance is no longer admitted anywhere.
	if _, err := surface.db.Exec(`UPDATE sessions SET revoked_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), surface.admin.session); err != nil {
		t.Fatal(err)
	}
	status, _ = surface.do(t, http.MethodGet, "/api/v1/audit-events", surface.admin.cookie, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked admin session must be 401, got %d", status)
	}
}

func TestAuditSettingsReadAndPreviewShape(t *testing.T) {
	surface := newAuditSurface(t)
	status, body := surface.do(t, http.MethodGet, "/api/v1/admin/audit-settings", surface.admin.cookie, "")
	if status != http.StatusOK {
		t.Fatalf("GET settings: status=%d body=%s", status, body)
	}
	var settings struct {
		RetentionMonths    int     `json:"retentionMonths"`
		MinRetentionMonths int     `json:"minRetentionMonths"`
		RowVersion         int64   `json:"rowVersion"`
		UpdatedAt          *string `json:"updatedAt"`
		UpdatedBy          *string `json:"updatedBy"`
		Cleanup            *struct {
			LastRunAt               *string `json:"lastRunAt"`
			LastSuccessCutoffAt     *string `json:"lastSuccessCutoffAt"`
			LastSuccessDeletedCount *int64  `json:"lastSuccessDeletedCount"`
			LastFailureAt           *string `json:"lastFailureAt"`
			LastErrorCode           *string `json:"lastErrorCode"`
		} `json:"cleanup"`
	}
	if err := json.Unmarshal([]byte(body), &settings); err != nil {
		t.Fatalf("settings body malformed: %s", body)
	}
	if settings.RetentionMonths != 6 || settings.MinRetentionMonths != 6 || settings.RowVersion != 1 {
		t.Fatalf("bootstrap defaults must surface as 6/6/1, got %+v", settings)
	}
	if settings.UpdatedAt == nil || settings.UpdatedBy != nil {
		t.Fatalf("seeded row has an update time but no updating user, got %+v", settings)
	}
	if settings.Cleanup == nil || settings.Cleanup.LastRunAt != nil || settings.Cleanup.LastSuccessCutoffAt != nil || settings.Cleanup.LastErrorCode != nil {
		t.Fatalf("fresh cleanup status must be all-null, got %+v", settings.Cleanup)
	}

	// The display name of the last maintainer is projected from the user row.
	if _, err := surface.db.Exec(`UPDATE audit_retention SET updated_by_type='user', updated_by_id=?, updated_at=? WHERE id=1`,
		surface.admin.id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/admin/audit-settings", surface.admin.cookie, "")
	if status != http.StatusOK || !strings.Contains(body, `"updatedBy":"admin"`) {
		t.Fatalf("updatedBy must project the username, got %s", body)
	}

	// Preview works always — including for an extension and while cleanup is
	// enabled — and reports the cutoff boundary plus impact estimates.
	status, body = surface.do(t, http.MethodPost, "/api/v1/admin/audit-settings/preview", surface.admin.cookie, `{"retentionMonths":12}`)
	if status != http.StatusOK {
		t.Fatalf("preview 12: status=%d body=%s", status, body)
	}
	var preview struct {
		RetentionMonths                int     `json:"retentionMonths"`
		CurrentRetentionMonths         int     `json:"currentRetentionMonths"`
		Shortening                     bool    `json:"shortening"`
		CutoffAt                       *string `json:"cutoffAt"`
		EstimatedExpirableEvents       int64   `json:"estimatedExpirableEvents"`
		EstimatedExpirableCorrelations int64   `json:"estimatedExpirableCorrelations"`
	}
	if err := json.Unmarshal([]byte(body), &preview); err != nil {
		t.Fatalf("preview body malformed: %s", body)
	}
	if preview.RetentionMonths != 12 || preview.CurrentRetentionMonths != 6 || preview.Shortening || preview.CutoffAt == nil {
		t.Fatalf("extension preview mismatch: %+v", preview)
	}
	if preview.EstimatedExpirableEvents != 0 || preview.EstimatedExpirableCorrelations != 0 {
		t.Fatalf("empty history must estimate zero: %+v", preview)
	}

	// Shortening is flagged so the UI can demand the confirmation cycle.
	if _, err := surface.db.Exec(`UPDATE audit_retention SET retention_months=12 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	status, body = surface.do(t, http.MethodPost, "/api/v1/admin/audit-settings/preview", surface.admin.cookie, `{"retentionMonths":6}`)
	if status != http.StatusOK || !strings.Contains(body, `"shortening":true`) {
		t.Fatalf("shortening preview must be flagged: status=%d body=%s", status, body)
	}
	status, _ = surface.do(t, http.MethodPost, "/api/v1/admin/audit-settings/preview", surface.admin.cookie, `{"retentionMonths":5}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("months below the minimum must be rejected with 422, got %d", status)
	}
	status, body = surface.do(t, http.MethodGet, "/api/v1/admin/audit-settings", surface.operator.cookie, "")
	if status != http.StatusForbidden {
		t.Fatalf("operator settings read must be forbidden, got %d body=%s", status, body)
	}
}

func TestAuditSettingsUpdateCommandLedgerReplayAndConflict(t *testing.T) {
	surface := newAuditSurface(t)
	patch := `{"retentionMonths":12,"expectedRowVersion":1,"clientCommandId":"audit-patch-01"}`
	status, body := surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie, patch)
	if status != http.StatusOK {
		t.Fatalf("PATCH settings: status=%d body=%s", status, body)
	}
	var updated struct {
		RetentionMonths int     `json:"retentionMonths"`
		RowVersion      int64   `json:"rowVersion"`
		UpdatedAt       *string `json:"updatedAt"`
		UpdatedBy       *string `json:"updatedBy"`
	}
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatalf("PATCH body malformed: %s", body)
	}
	if updated.RetentionMonths != 12 || updated.RowVersion != 2 || updated.UpdatedAt == nil || updated.UpdatedBy == nil || *updated.UpdatedBy != "admin" {
		t.Fatalf("updated settings mismatch: %+v", updated)
	}
	var months int
	if err := surface.db.QueryRow(`SELECT retention_months FROM audit_retention WHERE id=1`).Scan(&months); err != nil || months != 12 {
		t.Fatalf("retention row not updated: months=%d err=%v", months, err)
	}
	var successEvents, ledgerRows int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='audit.retention.update' AND outcome='success'`).Scan(&successEvents); err != nil {
		t.Fatal(err)
	}
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE client_command_id='audit-patch-01' AND outcome='committed'`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if successEvents != 1 || ledgerRows != 1 {
		t.Fatalf("runner must record one success audit and one committed ledger row, got audit=%d ledger=%d", successEvents, ledgerRows)
	}
	var actorType string
	var actorID int64
	var clientCommand string
	if err := surface.db.QueryRow(`SELECT actor_type, actor_id, client_command_id FROM audit_events WHERE action='audit.retention.update'`).Scan(&actorType, &actorID, &clientCommand); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != surface.admin.id || clientCommand != "audit-patch-01" {
		t.Fatalf("audit row must carry the admission actor and command id, got %s/%d cmd=%s", actorType, actorID, clientCommand)
	}

	// A byte-identical retry replays the stored outcome: same body, no new
	// audit row, no new ledger row.
	status, replayed := surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie, patch)
	if status != http.StatusOK || replayed != body {
		t.Fatalf("replay must return the identical body:\nfirst=%s\nsecond=%s (status=%d)", body, replayed, status)
	}
	var eventsAfterReplay int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='audit.retention.update'`).Scan(&eventsAfterReplay); err != nil {
		t.Fatal(err)
	}
	if eventsAfterReplay != 1 {
		t.Fatalf("replay must not add audit rows, got %d", eventsAfterReplay)
	}

	// Stale optimistic version conflicts with the authoritative row version in
	// the frozen envelope, and the rejection is durably recorded.
	status, body = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":24,"expectedRowVersion":1,"clientCommandId":"audit-patch-02"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"row_version_conflict"`) || !strings.Contains(body, `"audit_retention"`) {
		t.Fatalf("stale version must conflict: status=%d body=%s", status, body)
	}
	var rejectedAudit, rejectedLedger int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='audit.retention.update' AND outcome='rejected'`).Scan(&rejectedAudit); err != nil {
		t.Fatal(err)
	}
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE client_command_id='audit-patch-02' AND outcome='rejected_known'`).Scan(&rejectedLedger); err != nil {
		t.Fatal(err)
	}
	if rejectedAudit != 1 || rejectedLedger != 1 {
		t.Fatalf("deterministic rejection must be recorded, got audit=%d ledger=%d", rejectedAudit, rejectedLedger)
	}
	if err := surface.db.QueryRow(`SELECT retention_months FROM audit_retention WHERE id=1`).Scan(&months); err != nil || months != 12 {
		t.Fatalf("rejected change must not land: months=%d err=%v", months, err)
	}

	// The same command id with a different request is a reuse conflict.
	status, body = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":24,"expectedRowVersion":2,"clientCommandId":"audit-patch-01"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"command_id_reused"`) {
		t.Fatalf("reused command id must conflict: status=%d body=%s", status, body)
	}

	// Huma rejects the body before any command runs: nothing is recorded.
	var ledgerBefore int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}
	status, _ = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":5,"expectedRowVersion":2,"clientCommandId":"audit-patch-03"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("months below minimum must fail validation, got %d", status)
	}
	var ledgerAfter int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM client_commands`).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if ledgerAfter != ledgerBefore {
		t.Fatalf("validation failure must not touch the ledger: before=%d after=%d", ledgerBefore, ledgerAfter)
	}

	// The operator is stopped at the handler boundary; no command survives.
	status, body = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.operator.cookie,
		`{"retentionMonths":24,"expectedRowVersion":2,"clientCommandId":"audit-patch-04"}`)
	if status != http.StatusForbidden {
		t.Fatalf("operator PATCH must be forbidden, got %d body=%s", status, body)
	}
}

func TestAuditSettingsUpdateRejectsWhileCleanupRunning(t *testing.T) {
	surface := newAuditSurface(t)
	// Arm a fresh cleanup permit exactly the way the retention controller
	// does: cutoff inside the configured window so the schema trigger accepts
	// it, acquired_at current so the running guard treats it as live.
	armedCutoff := audit.CanonicalTimestamp(audit.CutoffUTC(time.Now(), 6).Add(-time.Hour))
	nowText := audit.CanonicalTimestamp(time.Now())
	if _, err := surface.db.Exec(`UPDATE audit_cleanup_permits
		SET active=1, cutoff_at=?, upper_event_id=COALESCE((SELECT MAX(id) FROM audit_events),0), acquired_at=? WHERE id=1`,
		armedCutoff, nowText); err != nil {
		t.Fatalf("arming the permit must satisfy the schema guard: %v", err)
	}
	status, body := surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":12,"expectedRowVersion":1,"clientCommandId":"audit-patch-running-01"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"cleanup_running"`) {
		t.Fatalf("policy change during a live run must conflict: status=%d body=%s", status, body)
	}
	var months int
	if err := surface.db.QueryRow(`SELECT retention_months FROM audit_retention WHERE id=1`).Scan(&months); err != nil || months != 6 {
		t.Fatalf("rejected change must not land: months=%d err=%v", months, err)
	}
	var rejected int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='audit.retention.update' AND outcome='rejected'`).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 {
		t.Fatalf("the cleanup_running rejection must be audited, got %d", rejected)
	}
	// The stale (past stale window) permit no longer blocks policy changes.
	stale := audit.CanonicalTimestamp(time.Now().Add(-2 * time.Hour))
	if _, err := surface.db.Exec(`UPDATE audit_cleanup_permits SET acquired_at=? WHERE id=1`, stale); err != nil {
		t.Fatal(err)
	}
	status, _ = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":12,"expectedRowVersion":1,"clientCommandId":"audit-patch-running-02"}`)
	if status != http.StatusOK {
		t.Fatalf("stale permit must not block the change, got status=%d", status)
	}
}

func TestAuditSettingsUpdateCleanupEnabledPassThrough(t *testing.T) {
	surface := newAuditSurface(t)
	// First disable: cleanupEnabled participates in the command digest.
	status, _ := surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":6,"expectedRowVersion":1,"clientCommandId":"audit-cleanup-01","cleanupEnabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("disable PATCH failed with status %d", status)
	}
	var enabled int
	if err := surface.db.QueryRow(`SELECT cleanup_enabled FROM audit_retention WHERE id=1`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("cleanup must be disabled, got enabled=%d err=%v", enabled, err)
	}
	// Same command id, now WITHOUT the flag: different semantic request, so
	// the digest differs and the reuse guard fires.
	status, body := surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":6,"expectedRowVersion":2,"clientCommandId":"audit-cleanup-01"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"command_id_reused"`) {
		t.Fatalf("flag change under the same id must reuse-conflict: status=%d body=%s", status, body)
	}
	// The confirmed initial enable flips the flag back on and records it.
	status, _ = surface.do(t, http.MethodPatch, "/api/v1/admin/audit-settings", surface.admin.cookie,
		`{"retentionMonths":6,"expectedRowVersion":2,"clientCommandId":"audit-cleanup-02","cleanupEnabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("enable PATCH failed with status %d", status)
	}
	if err := surface.db.QueryRow(`SELECT cleanup_enabled FROM audit_retention WHERE id=1`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("cleanup must be enabled again, got enabled=%d err=%v", enabled, err)
	}
}

func TestVerifyAuditAdminSessionInsideTransaction(t *testing.T) {
	surface := newAuditSurface(t)
	// Build a runner-owned transaction through a real execution so the
	// Authorize callback runs against the exact transaction type the runner
	// passes, with the same admission-shaped metadata the PATCH handler uses.
	registry := execution.NewRegistry()
	op, err := registry.Register(execution.Operation{
		Name: "audit.test.probe", Class: execution.ClassWrite, ObjectType: "audit_retention",
		Authorize: func(ctx context.Context, tx *execution.Tx) error {
			return auth.VerifyExecutionSession(ctx, tx, "admin")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := func(actorID, sessionID, revision int64) error {
		ctx, err := execution.WithMetadata(context.Background(), execution.Metadata{
			CorrelationID: "corr-revalidate",
			Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: actorID},
			Source:        execution.Source{Kind: execution.SourceHTTP, RequestID: "req-revalidate"},
			Session:       execution.SessionRef{ID: sessionID, AuthRevision: revision},
		})
		if err != nil {
			return err
		}
		_, err = execution.Run(ctx, execution.NewRunner(surface.db, registry, nil), op,
			execution.Command{
				PrincipalType: "user", PrincipalID: actorID,
				ClientCommandID: fmt.Sprintf("revalidate-%d", time.Now().UnixNano()), Digest: strings.Repeat("a", 64),
			},
			func(tx *execution.Tx) (struct{}, execution.Change, error) {
				return struct{}{}, execution.Unchanged, nil
			},
			func(struct{}) int64 { return 1 })
		return err
	}
	if err := probe(surface.admin.id, surface.admin.session, 1); err != nil {
		t.Fatalf("current admin session must pass revalidation: %v", err)
	}

	// Session-state conditions run on a fresh session each: revocation is
	// terminal, and the schema's expiry/revision fields only move forward, so
	// no condition may reuse a previous session's state.
	sessionCondition := func(name string, windows []time.Duration, setup func(t *testing.T, sessionID int64)) {
		t.Helper()
		_, sessionID, err := surface.newSession(surface.admin.id, windows...)
		if err != nil {
			t.Fatal(err)
		}
		if setup != nil {
			setup(t, sessionID)
		}
		if err := probe(surface.admin.id, sessionID, 1); err == nil {
			t.Fatalf("%s must fail revalidation", name)
		} else if !errors.Is(err, auth.ErrActorChanged) {
			t.Fatalf("%s: want ErrActorChanged, got %v", name, err)
		}
	}
	nowText := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	sessionCondition("revoked session", nil, func(t *testing.T, sessionID int64) {
		if _, err := surface.db.Exec(`UPDATE sessions SET revoked_at=? WHERE id=?`, nowText(), sessionID); err != nil {
			t.Fatal(err)
		}
	})
	sessionCondition("idle-expired session", []time.Duration{-time.Minute}, nil)
	sessionCondition("absolute-expired session", []time.Duration{time.Hour, -time.Minute}, nil)
	// A stale proof revision in the admission metadata: the session row itself
	// is valid, so only the over-claimed proof must be rejected — and the same
	// session with the matching proof passes (the helper compares meta.Session
	// against the row, not only the DB join's internal consistency).
	_, staleID, err := surface.newSession(surface.admin.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe(surface.admin.id, staleID, 2); err == nil {
		t.Fatal("stale proof revision must fail revalidation")
	} else if !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("stale proof revision: want ErrActorChanged, got %v", err)
	}
	if err := probe(surface.admin.id, staleID, 1); err != nil {
		t.Fatalf("matching proof on a valid session must pass: %v", err)
	}
	// A non-admin session must not satisfy the admin requirement.
	if err := probe(surface.operator.id, surface.operator.session, 1); err == nil {
		t.Fatal("operator session must fail the admin revalidation")
	} else if !errors.Is(err, auth.ErrActorChanged) {
		t.Fatalf("operator probe: want ErrActorChanged, got %v", err)
	}

	// A fresh, current session passes again after every rejection above.
	_, fresh, err := surface.newSession(surface.admin.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe(surface.admin.id, fresh, 1); err != nil {
		t.Fatalf("fresh current session must pass revalidation: %v", err)
	}
	// The plain-error semantics of a failed Authorize: rejected sessions must
	// not leave ledger traces — only the three successful probes committed
	// (initial, matching-proof, fresh final).
	var traces int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM client_commands WHERE command_type='audit.test.probe'`).Scan(&traces); err != nil {
		t.Fatal(err)
	}
	if traces != 3 {
		t.Fatalf("only the successful probes may be recorded, got %d ledger rows", traces)
	}
}

func TestAuditCleanupPassScopeRestoresSystemMetadata(t *testing.T) {
	ctx, acting, err := auditCleanupPassScope(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := execution.FromContext(ctx)
	if !ok {
		t.Fatal("cleanup pass context must carry execution metadata")
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 || meta.Initiator != meta.Actor {
		t.Fatalf("cleanup pass must act as the system principal, got %+v", meta)
	}
	if meta.Source.Kind != execution.SourceScheduler || meta.CorrelationID == "" {
		t.Fatalf("cleanup pass must declare its scheduler source and correlation, got %+v", meta)
	}
	// The acting record is mapped from the same metadata because audit cannot
	// read execution's context; the controller rejects anything else.
	if acting.ActorType != audit.ActorSystem || acting.ActorID != 0 || acting.InitiatorType != audit.ActorSystem || acting.InitiatorID != 0 {
		t.Fatalf("acting record must be the explicit system principal, got %+v", acting)
	}
	if acting.CorrelationID != meta.CorrelationID {
		t.Fatalf("acting record correlation must match the pass context, got %q vs %q", acting.CorrelationID, meta.CorrelationID)
	}
	// Re-rooting replaces a leaked user context: the pass never inherits a
	// user identity.
	userCtx, err := execution.WithMetadata(context.Background(), execution.Metadata{
		CorrelationID: "corr-user", Actor: execution.Principal{Kind: execution.PrincipalUser, ID: 7},
		Source: execution.Source{Kind: execution.SourceHTTP},
	})
	if err != nil {
		t.Fatal(err)
	}
	restoredCtx, restoredRecord, err := auditCleanupPassScope(userCtx)
	if err != nil {
		t.Fatal(err)
	}
	if meta, _ := execution.FromContext(restoredCtx); meta.Actor.Kind != execution.PrincipalSystem {
		t.Fatalf("system restore must replace user metadata, got %+v", meta)
	}
	if restoredRecord.CorrelationID == "corr-user" || restoredRecord.ActorType != audit.ActorSystem {
		t.Fatalf("restored scope must not inherit the user correlation, got %+v", restoredRecord)
	}
}

func TestRunAuditCleanupPassExpiresOnlyOldEvents(t *testing.T) {
	surface := newAuditSurface(t)
	// The expired event is inserted directly with an old created_at:
	// audit_events is append-only, so backdating a written row is impossible
	// by schema design.
	oldResult, err := surface.db.Exec(
		`INSERT INTO audit_events(actor_type,actor_id,action,outcome,correlation_id,created_at)
		 VALUES('user',?,'old.action','success','corr-old','2020-01-01T00:00:00.000000000Z')`, surface.admin.id)
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := oldResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	recentID := seedAuditEvent(t, surface.db, audit.Record{
		ActorType: audit.ActorSystem, ActorID: 0, Action: "recent.action", Outcome: audit.OutcomeSuccess,
		CorrelationID: "corr-recent", InitiatorType: audit.ActorSystem,
	})
	controller, err := audit.NewController(surface.db, audit.NewWriter(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runAuditCleanupPass(context.Background(), controller)

	var oldCount, recentCount int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id=?`, oldID).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id=?`, recentID).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || recentCount != 1 {
		t.Fatalf("only the expired event may be cleaned: old=%d recent=%d", oldCount, recentCount)
	}
	var batches int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_cleanup_batches WHERE final=1`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 1 {
		t.Fatalf("the run must record its final batch, got %d", batches)
	}
	var permitActive int
	if err := surface.db.QueryRow(`SELECT active FROM audit_cleanup_permits WHERE id=1`).Scan(&permitActive); err != nil {
		t.Fatal(err)
	}
	if permitActive != 0 {
		t.Fatal("the cleanup permit must be released after the run")
	}
	// The batch record is an explicit system audit event, never a user action.
	var systemEvent int
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='audit.retention.cleanup' AND actor_type='system' AND actor_id=0 AND correlation_id<>''`).Scan(&systemEvent); err != nil {
		t.Fatal(err)
	}
	if systemEvent == 0 {
		t.Fatal("cleanup batches must carry a system audit event with a correlation")
	}

	// Disabled cleanup keeps every event and stays quiet (configured state).
	if _, err := surface.db.Exec(`UPDATE audit_retention SET cleanup_enabled=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	runAuditCleanupPass(context.Background(), controller)
	if err := surface.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE id=?`, recentID).Scan(&recentCount); err != nil {
		t.Fatal(err)
	}
	if recentCount != 1 {
		t.Fatal("disabled cleanup must keep all history")
	}
}

func TestStartAuditCleanupStopsWithCancelledContext(t *testing.T) {
	surface := newAuditSurface(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		StartAuditCleanup(ctx, surface.db)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartAuditCleanup must return immediately and the loop must stop with a cancelled context")
	}
}
