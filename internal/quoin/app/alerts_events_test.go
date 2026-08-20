package app_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// sseStack is a real public handler + real SQLite + real admin session over
// httptest, with a second alerts.Service handle on the same database to
// drive deliveries (the same writes Stele's relay path commits).
type sseStack struct {
	server  *httptest.Server
	db      *sql.DB
	alerts  *alerts.Service
	auth    *auth.Service
	cookie  string
	source  int64
	creds   int64
	rootDir string
}

func newSSEStack(t *testing.T) *sseStack {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	config := contract.QuoinConfig{
		Component: "quoin", PublicOrigin: "https://quoin.example.com",
		DataDirectory:             filepath.Join(root, "data"),
		BackupDirectory:           filepath.Join(root, "backup"),
		RootKeyFile:               filepath.Join(secrets, "root-key"),
		RuntimeTLSCertificateFile: filepath.Join(secrets, "runtime-tls.crt"),
		RuntimeTLSPrivateKeyFile:  filepath.Join(secrets, "runtime-tls.key"),
		SteleServiceTokenFile:     filepath.Join(secrets, "stele-service-token"),
	}
	if _, err := bootstrap.BootstrapSecrets(config); err != nil {
		t.Fatal(err)
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	authService, err := auth.NewService(database.SQL)
	if err != nil {
		t.Fatal(err)
	}
	password := "SSE stack horse battery 2026!"
	if _, err := authService.CreateFirstAdmin(ctx, "admin", "SSE Admin", password); err != nil {
		t.Fatal(err)
	}
	application := app.NewAPIServer(authService, database.SQL)
	handler, err := app.NewHandler(application, config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Session: login over the real handler, then clear the forced password
	// change through the real endpoint so the cookie is a normal session.
	loginRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
	loginRequest.Header.Set("Origin", config.PublicOrigin)
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse, err := server.Client().Do(loginRequest)
	if err != nil {
		t.Fatal(err)
	}
	loginBody, _ := io.ReadAll(loginResponse.Body)
	loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", loginResponse.StatusCode, loginBody)
	}
	setCookie := loginResponse.Header.Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "__Host-quoin-session=") {
		t.Fatalf("no session cookie: %q", setCookie)
	}
	cookieValue := strings.Split(strings.Split(setCookie, ";")[0], "=")[1]
	newPassword := "SSE stack horse battery 2027!"
	changeRequest, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/auth/password", strings.NewReader(fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, password, newPassword)))
	changeRequest.Header.Set("Cookie", "__Host-quoin-session="+cookieValue)
	changeRequest.Header.Set("Origin", config.PublicOrigin)
	changeRequest.Header.Set("Content-Type", "application/json")
	changeResponse, err := server.Client().Do(changeRequest)
	if err != nil {
		t.Fatal(err)
	}
	changeBody, _ := io.ReadAll(changeResponse.Body)
	changeResponse.Body.Close()
	if changeResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("password change: %d %s", changeResponse.StatusCode, changeBody)
	}

	stack := &sseStack{server: server, db: database.SQL, alerts: alerts.NewService(database.SQL), auth: authService, cookie: cookieValue, rootDir: root}
	digest := make([]byte, 32)
	rand.Read(digest)
	result, err := stack.alerts.CreateSource(ctx, "sse", "alertmanager", digest, 1, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	stack.source, stack.creds = result.SourceID, result.CredentialID
	return stack
}

// deliver pushes one webhook through the real alert transaction so triggers
// derive change events in the same commit.
func (stack *sseStack) deliver(t *testing.T, relayID, alertname, status string) {
	t.Helper()
	labels := map[string]string{"alertname": alertname}
	sum := alerts.FingerprintOf(labels)
	fingerprint := fmt.Sprintf("%016x", uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]))
	body := []byte(fmt.Sprintf(`{"status":"firing","alerts":[{"status":%q,"labels":{"alertname":%q},"startsAt":"2026-08-18T14:00:00Z","fingerprint":"%s"}],"truncatedAlerts":0}`, status, alertname, fingerprint))
	if _, err := stack.alerts.Deliver(context.Background(), relayID, stack.source, stack.creds, 1, body, time.Now().UTC()); err != nil {
		t.Fatalf("deliver %s: %v", relayID, err)
	}
}

type sseEvent struct {
	event string
	id    string
	data  string
}

func (stack *sseStack) openStream(t *testing.T, lastEventID, after string) (*http.Response, *bufio.Reader) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, stack.server.URL+"/api/v1/alerts/events", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+stack.cookie)
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	if after != "" {
		request.URL.RawQuery = "after=" + after
	}
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response, bufio.NewReader(response.Body)
}

// readFrame reads one event frame; heartbeat comment frames return ok=false
// only on error/EOF, a comment-only frame is skipped.
func readFrame(t *testing.T, reader *bufio.Reader) (sseEvent, bool) {
	t.Helper()
	var frame sseEvent
	sawContent := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, false
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if sawContent {
				return frame, true
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
			sawContent = true
		case strings.HasPrefix(line, "id: "):
			frame.id = strings.TrimPrefix(line, "id: ")
			sawContent = true
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
			sawContent = true
		case strings.HasPrefix(line, ":"):
			// transport comment (heartbeat): not content
		default:
			sawContent = true
		}
	}
}

// TestSSEFramingReplayAndCursorMatrix covers the HTTP-side contract in one
// real-handler pass: exact framing, headers, after-replay, Last-Event-ID
// priority over after, empty-database cursor=0, pre-head 410 problem+json
// after retention GC.
func TestSSEFramingReplayAndCursorMatrix(t *testing.T) {
	stack := newSSEStack(t)

	// Empty database: cursor 0 is current, stream opens with SSE headers.
	response, reader := stack.openStream(t, "", "0")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty-db stream status=%d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content-type=%q", contentType)
	}
	if cache := response.Header.Get("Cache-Control"); cache != "no-cache" {
		t.Fatalf("cache-control=%q", cache)
	}
	if buffering := response.Header.Get("X-Accel-Buffering"); buffering != "no" {
		t.Fatalf("x-accel-buffering=%q", buffering)
	}

	// Two distinct alerts land while the stream is open.
	stack.deliver(t, "sse-r1", "SSEOne", "firing")
	stack.deliver(t, "sse-r2", "SSETwo", "firing")

	first, ok := readFrame(t, reader)
	if !ok || first.event != "change" || first.id != "1" {
		t.Fatalf("first frame=%+v ok=%v", first, ok)
	}
	var payload struct {
		Seq          string `json:"seq"`
		Type         string `json:"type"`
		OccurrenceID string `json:"occurrenceId"`
		RowVersion   int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(first.data), &payload); err != nil {
		t.Fatalf("payload %q: %v", first.data, err)
	}
	if payload.Seq != "1" || payload.Type != "created" || payload.OccurrenceID != "1" || payload.RowVersion != 1 {
		t.Fatalf("payload=%+v frame=%+v", payload, first)
	}
	second, ok := readFrame(t, reader)
	if !ok || second.id != "2" || second.event != "change" {
		t.Fatalf("second frame=%+v ok=%v", second, ok)
	}
	response.Body.Close()

	// Replay from after=1 returns exactly event 2.
	response, reader = stack.openStream(t, "", "1")
	replayed, ok := readFrame(t, reader)
	if !ok || replayed.id != "2" {
		t.Fatalf("replay frame=%+v ok=%v", replayed, ok)
	}
	response.Body.Close()

	// Last-Event-ID wins over after when both are present.
	response, reader = stack.openStream(t, "0", "2")
	fromHeader, ok := readFrame(t, reader)
	if !ok || fromHeader.id != "1" {
		t.Fatalf("Last-Event-ID must win: frame=%+v ok=%v", fromHeader, ok)
	}
	response.Body.Close()

	// Retention GC removes the oldest derived row; after=0 is now below
	// oldest-1 and must get a pre-head 410 problem+json (no stream bytes).
	if _, err := stack.db.Exec(`DELETE FROM alert_change_log WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, stack.server.URL+"/api/v1/alerts/events", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+stack.cookie)
	request.URL.RawQuery = "after=0"
	expiredResponse, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(expiredResponse.Body)
	expiredResponse.Body.Close()
	if expiredResponse.StatusCode != http.StatusGone {
		t.Fatalf("stale cursor before head: status=%d body=%s", expiredResponse.StatusCode, raw)
	}
	if contentType := expiredResponse.Header.Get("Content-Type"); !strings.Contains(contentType, "application/problem+json") {
		t.Fatalf("410 content-type=%q", contentType)
	}
	var problem struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil || problem.Status != 410 {
		t.Fatalf("410 problem=%s err=%v", raw, err)
	}
}

// TestSSEInStreamResyncAfterGC proves the second expiry boundary: a cursor
// that expires after the stream is established gets one in-stream
// resync_required event and a close — never a late 410. Construction: the
// stream is current (cursor == high); two further events commit and
// retention GC removes every derived row below the new high within one poll
// tick, so the handler's next expiry check (which runs BEFORE replay) sees
// cursor ≤ oldest-2 and resyncs instead of silently skipping the GC'd row.
func TestSSEInStreamResyncAfterGC(t *testing.T) {
	stack := newSSEStack(t)
	stack.deliver(t, "resync-r1", "ResyncOne", "firing")

	for attempt := 0; attempt < 5; attempt++ {
		var high int64
		if err := stack.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM alert_change_log`).Scan(&high); err != nil {
			t.Fatal(err)
		}
		response, reader := stack.openStream(t, "", fmt.Sprintf("%d", high))
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("stream status=%d", response.StatusCode)
		}
		// Let the handler complete its immediate first (empty) poll, then
		// commit two more events and GC everything below the new high within
		// its 300ms sleep tick (these writes take single-digit milliseconds).
		time.Sleep(50 * time.Millisecond)
		stack.deliver(t, fmt.Sprintf("resync-a%d", attempt), fmt.Sprintf("ResyncA%d", attempt), "firing")
		stack.deliver(t, fmt.Sprintf("resync-b%d", attempt), fmt.Sprintf("ResyncB%d", attempt), "firing")
		if _, err := stack.db.Exec(`DELETE FROM alert_change_log WHERE id < (SELECT MAX(id) FROM alert_change_log)`); err != nil {
			t.Fatal(err)
		}
		frame, ok := readFrame(t, reader)
		if ok && frame.event == "change" {
			// Lost the race this round (handler polled between our writes and
			// the GC): it replayed a change event. Retry with the new cursor.
			response.Body.Close()
			continue
		}
		if !ok || frame.event != "resync_required" || frame.data != `{"type":"resync_required"}` {
			t.Fatalf("resync frame=%+v ok=%v", frame, ok)
		}
		if _, err := reader.ReadByte(); err == nil {
			t.Fatal("stream must close after resync_required")
		}
		response.Body.Close()
		return
	}
	t.Fatal("in-stream resync never observed after 5 attempts")
}

// TestSSEAuthAndInvalidCursor covers 401 without a session, 400 on a
// non-decimal cursor, and silent close on session revocation (Q214).
func TestSSEAuthAndInvalidCursor(t *testing.T) {
	stack := newSSEStack(t)

	request, _ := http.NewRequest(http.MethodGet, stack.server.URL+"/api/v1/alerts/events", nil)
	request.URL.RawQuery = "after=0"
	response, err := stack.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), "请重新登录") {
		t.Fatalf("no-cookie: %d %s", response.StatusCode, raw)
	}

	badResponse, _ := stack.openStream(t, "", "not-a-number")
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor status=%d", badResponse.StatusCode)
	}
	badResponse.Body.Close()

	// Live stream closes silently once the session is revoked.
	stack.deliver(t, "auth-r1", "AuthOne", "firing")
	response, reader := stack.openStream(t, "", "0")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", response.StatusCode)
	}
	if frame, ok := readFrame(t, reader); !ok || frame.event != "change" {
		t.Fatalf("expected change frame first, got %+v ok=%v", frame, ok)
	}
	session, err := stack.auth.Authenticate(context.Background(), stack.cookie)
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.auth.Logout(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("stream must close after session revocation")
	}
	response.Body.Close()
}

// TestSSELiveStateChange proves Firing→Resolved emits state_changed with
// rowVersion 2 on an open stream.
func TestSSELiveStateChange(t *testing.T) {
	stack := newSSEStack(t)
	stack.deliver(t, "live-r1", "LiveOne", "firing")

	response, reader := stack.openStream(t, "", "1")
	defer response.Body.Close()
	stack.deliver(t, "live-r2", "LiveOne", "resolved")
	frame, ok := readFrame(t, reader)
	if !ok || frame.event != "change" || frame.id == "" {
		t.Fatalf("state frame=%+v ok=%v", frame, ok)
	}
	var payload struct {
		Type       string `json:"type"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal([]byte(frame.data), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Type != "state_changed" || payload.RowVersion != 2 {
		t.Fatalf("payload=%+v", payload)
	}
}

// TestIntakeIssueAcknowledgeAndHistoryView covers the Admin one-way
// acknowledge over the real HTTP surface (204 on success, 409 on stale
// expectedRowVersion, 403 for Operator) and the state=Resolved history
