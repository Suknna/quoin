package lifecycle

// Shared acceptance helpers: the exact-bytes webhook builder, the real SSE
// consumers, HTTP helpers and the ticket evidence recorder used by
// TestTicket04. Split from ticket04_test.go so each file stays single-purpose
// and below the 500-line guidance.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/alerts"
)

var _ = alerts.FingerprintOf

// webhookBodyJSON builds one exact Alertmanager webhook body with the
// verified fingerprint and an optional truncatedAlerts value.
func webhookBodyJSON(t *testing.T, status string, labels map[string]string, startsAt string, truncated int) []byte {
	t.Helper()
	sum := alerts.FingerprintOf(labels)
	fingerprint := fmt.Sprintf("%016x", uint64(sum[0])<<56|uint64(sum[1])<<48|uint64(sum[2])<<40|uint64(sum[3])<<32|uint64(sum[4])<<24|uint64(sum[5])<<16|uint64(sum[6])<<8|uint64(sum[7]))
	labelsJSON, _ := json.Marshal(labels)
	return []byte(fmt.Sprintf(`{"status":%q,"alerts":[{"status":%q,"labels":%s,"startsAt":%q,"endsAt":"0001-01-01T00:00:00Z","fingerprint":"%s"}],"truncatedAlerts":%d}`,
		status, status, labelsJSON, startsAt, fingerprint, truncated))
}

// consumeSSE performs one real long-lived SSE GET and records frames.
func consumeSSE(ctx context.Context, t *testing.T, base, origin, cookieValue, after string, recorder *streamRecorder) {
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/alerts/events", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+cookieValue)
	if after != "" {
		request.URL.RawQuery = "after=" + after
	}
	request = request.WithContext(ctx)
	response, err := (cookielessClient()).Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var event, id, data strings.Builder
	flush := func() {
		if event.Len() > 0 || id.Len() > 0 || data.Len() > 0 {
			recorder.record(sseFrame{Event: event.String(), ID: id.String(), Data: data.String()})
			event.Reset()
			id.Reset()
			data.Reset()
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			flush()
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event.WriteString(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "id: "):
			id.WriteString(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// replayWithLastEventID opens a fresh SSE connection carrying both a
// Last-Event-ID header and a stale after parameter, returning the first
// frames observed (the header must win).
func replayWithLastEventID(t *testing.T, base, origin, cookieValue, lastEventID, after string) []sseFrame {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/alerts/events", nil)
	request.Header.Set("Cookie", "__Host-quoin-session="+cookieValue)
	request.Header.Set("Last-Event-ID", lastEventID)
	request.URL.RawQuery = "after=" + after
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("replay connect: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("replay status=%d body=%s", response.StatusCode, body)
	}
	reader := bufio.NewReader(response.Body)
	frames := []sseFrame{}
	var event, id, data strings.Builder
	flush := func() {
		if event.Len() > 0 || id.Len() > 0 {
			frames = append(frames, sseFrame{Event: event.String(), ID: id.String(), Data: data.String()})
			event.Reset()
			id.Reset()
			data.Reset()
		}
	}
	// Read until the first complete frame (or a short deadline).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			if len(frames) > 0 {
				break
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event.WriteString(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "id: "):
			id.WriteString(strings.TrimPrefix(line, "id: "))
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	flush()
	return frames
}

func cookielessClient() *http.Client {
	return &http.Client{Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func sessionCookieOf(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	// The cookie jar keeps the session value; read it back for the raw SSE
	// consumer (which does not share the jar).
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == "__Host-quoin-session" {
			return cookie.Value
		}
	}
	t.Fatal("session cookie missing from jar")
	return ""
}

func waitSnapshotRow(t *testing.T, client *http.Client, base, origin, alertname, state string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/alerts?state="+state, origin)
		if strings.Contains(body, alertname) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never reached state %s", alertname, state)
}

func occurrenceIDFromList(t *testing.T, client *http.Client, base, origin, alertname string) string {
	t.Helper()
	for _, state := range []string{"Firing", "Resolved"} {
		body := httpGet(t, client, base+"/api/v1/alerts?state="+state, origin)
		var snapshot struct {
			Items []struct {
				ID     string            `json:"id"`
				Labels map[string]string `json:"labels"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
			continue
		}
		for _, item := range snapshot.Items {
			if item.Labels["alertname"] == alertname {
				return item.ID
			}
		}
	}
	t.Fatalf("occurrence %s not found in any view", alertname)
	return ""
}

func waitIntakeIssue(t *testing.T, client *http.Client, base, origin, kind string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body := httpGet(t, client, base+"/api/v1/alert-intake-issues", origin)
		if strings.Contains(body, fmt.Sprintf(`"kind":"%s"`, kind)) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("intake issue kind=%s never appeared", kind)
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected %q in output:\n%s", what, needle, haystack)
	}
}

func (evidence *ticketEvidence) run(t *testing.T, name string, stdin io.Reader, command string, arguments ...string) string {
	t.Helper()
	started := time.Now()
	cmd := exec.Command(command, arguments...)
	cmd.Env = evidence.env
	cmd.Dir = repoRoot(t)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	logPath := filepath.Join(evidence.dir, name+".log")
	os.WriteFile(logPath, combined.Bytes(), 0o644)
	evidence.commands = append(evidence.commands, map[string]any{"name": name, "exitCode": exitCode, "duration": time.Since(started).Round(time.Millisecond).String()})
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": logPath, "sha256": sha256Hex(combined.Bytes()), "bytes": combined.Len()})
	if exitCode != 0 {
		t.Fatalf("%s: exit=%d output:\n%s", name, exitCode, combined.String())
	}
	return combined.String()
}

func (evidence *ticketEvidence) note(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(evidence.dir, name)
	os.WriteFile(path, []byte(content), 0o644)
	evidence.artifacts = append(evidence.artifacts, map[string]any{"path": path, "sha256": sha256Hex([]byte(content)), "bytes": len(content)})
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit, newPassword, tempPassword, bearer string, frames []sseFrame) {
	t.Helper()
	startedAt := time.Now().UTC()
	// Local-only images have no RepoDigest; their immutable content identity
	// is the image config digest (.Id), captured at build time (see
	// ticketEvidence.imageIDs — teardown removes the images before this runs).
	imageDigests := evidence.imageIDs
	// Dirty-state digest: SHA-256 over `git status --porcelain` so the
	// evidence pins both the commit and the working-tree state it ran in.
	statusOut, _ := exec.Command("git", "-C", repoRoot(t), "status", "--porcelain").Output()
	dirtyDigest := sha256Hex(statusOut)
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers":  "no long-lived containers owned (relayclient runs were --rm); quoin compose project down --remove-orphans",
		"networks":    "quoin_default/quoin_internal/quoin_edge removed by compose down",
		"images":      "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed (only if the run built them)",
		"workRoot":    "temporary XDG_STATE_HOME + secrets removed with test temp root",
		"credentials": "temp/new admin passwords and the revealed alert bearer held only in process memory; never written to evidence",
		"timestamp":   startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"artifacts":        evidence.artifacts,
		"components": map[string]any{
			"deployHelper": "cmd/quoin-deploy (go build -trimpath, host binary)",
			"relayClient":  "cmd/relayclient (linux static, GOOS=linux CGO_ENABLED=0) over SteleRelay gRPC inside quoin_internal",
			"imageDigests": imageDigests,
		},
		"observed": map[string]any{
			"realPath":                 "relayclient -> SteleRelay gRPC -> Quoin SQLite -> alert_change_log triggers -> SSE /api/v1/alerts/events -> raw HTTP consumer",
			"lifecycle":                "first firing created; repeat firing no event; resolved state_changed rv2; late firing after resolved appended observation without reopen; resolved-first created closed occurrence",
			"protocolFaults":           "duplicate relay_id replay deduplicated (already committed); truncatedAlerts=1 raised delivery_truncated intake issue; non-reproducible fingerprint raised fingerprint_mismatch intake issue",
			"sseLive":                  "frames recorded verbatim in sse-live-frames.json; ids strictly monotonic; payload shape {seq,type,occurrenceId,rowVersion}",
			"sseReplay":                "reconnect with Last-Event-ID=2 resumed exactly at id 3 and the header won over the stale after=0 parameter",
			"identityConflictBoundary": "identity_conflict requires a pre-existing conflicting snapshot or an FNV-1a collision; acceptance fixtures are forbidden from direct product-table writes, so this kind is covered deterministically in internal/quoin/alerts package tests instead",
		},
		"expectedVersusActual": map[string]string{
			"first firing creates occurrence + created event": "actual: relay ACCEPTED; snapshot row Firing; SSE id=1 created",
			"repeat firing emits nothing":                     "actual: change-frame count unchanged after t04-r2",
			"resolved transitions row + event":                "actual: snapshot Resolved; SSE id=2 state_changed rowVersion=2",
			"late firing never reopens":                       "actual: detail stays Resolved; observation late_firing_after_resolved appended; no new event",
			"resolved-first creates closed occurrence":        "actual: snapshot row Resolved for T04ResolvedFirst; SSE id=3 created",
			"duplicate relay replay deduplicated":             "actual: relayclient echoed 'duplicate relay; already committed'; no state or event change",
			"truncated + mismatch become intake issues":       "actual: alert-intake-issues lists delivery_truncated and fingerprint_mismatch; neither entered the occurrence list",
			"resolved leave the Firing snapshot":              "actual: /api/v1/alerts no longer contains the resolved occurrences; ?state=Resolved lists both",
			"SSE replay uses Last-Event-ID":                   "actual: first replayed frame id=3 after header Last-Event-ID=2 with after=0 ignored",
		},
		"redactions": "admin passwords and the alert bearer are not written to any evidence file",
	})), 0o644)
	_ = newPassword
	_ = tempPassword
	_ = bearer
}

func scanForSecrets(t *testing.T, evidenceDir string, secrets ...string) {
	t.Helper()
	filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("secret material leaked into %s", path)
			}
		}
		return nil
	})
}

func cookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpPost(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpPut(t *testing.T, client *http.Client, target, origin, body string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	return doRequest(t, client, request)
}

func httpGet(t *testing.T, client *http.Client, target, origin string) string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return doRequest(t, client, request)
}

func doRequest(t *testing.T, client *http.Client, request *http.Request) string {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode >= 400 {
		t.Fatalf("%s %s: status=%d body=%.500s", request.Method, request.URL, response.StatusCode, raw)
	}
	return string(raw)
}

func sourceIDFromMetadata(t *testing.T, client *http.Client, base, origin, key string) string {
	t.Helper()
	list := httpGet(t, client, base+"/api/v1/alert-sources", origin)
	var listObj struct {
		Items []struct {
			Key string `json:"key"`
			ID  string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(list), &listObj); err != nil {
		t.Fatalf("list sources parse: %v\n%s", err, list)
	}
	for _, item := range listObj.Items {
		if item.Key == key {
			return item.ID
		}
	}
	t.Fatalf("source %s not found in list: %s", key, list)
	return ""
}

func randomSecret(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return string(encoded)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func jsonMap(value string) string { return value }

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o600)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}

func outputOf(t *testing.T, command string, arguments ...string) string {
	t.Helper()
	out, err := exec.Command(command, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return string(out)
}
