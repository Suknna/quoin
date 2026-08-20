// Package lifecycle hosts the T04 ticket acceptance run: the real compose
// stack (Quoin + Stele over the SteleRelay gRPC path) driven through the
// complete alert lifecycle — first firing, repeat firing, resolved,
// resolved-first, late firing after resolved, duplicate relay replay,
// truncated and fingerprint-mismatch intake issues — while a real SSE
// consumer records the change stream, then reconnects with Last-Event-ID.
// Runtime and cleanup evidence lands under .artifacts/tickets/T04/.
package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	projectName = "quoin"
	quoinPort   = 18080
	stelePort   = 18081
)

type ticketEvidence struct {
	dir       string
	commands  []map[string]any
	artifacts []map[string]any
	env       []string
	stateRoot string
	// imageIDs captured right after the build: the teardown removes the
	// locally built images, so digests must not be read at evidence time.
	imageIDs map[string]string
}

// sseFrame is one recorded SSE event frame from the real stream.
type sseFrame struct {
	Event string `json:"event"`
	ID    string `json:"id"`
	Data  string `json:"data"`
}

// streamRecorder is a real SSE consumer: one long-lived GET on
// /api/v1/alerts/events that records every frame verbatim.
type streamRecorder struct {
	mu     sync.Mutex
	frames []sseFrame
	notify map[string]chan struct{}
	body   bytes.Buffer
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{notify: map[string]chan struct{}{}}
}

func (recorder *streamRecorder) record(frame sseFrame) {
	recorder.mu.Lock()
	recorder.frames = append(recorder.frames, frame)
	waiters := recorder.notify[frame.ID]
	recorder.mu.Unlock()
	if waiters != nil {
		close(waiters)
	}
}

func (recorder *streamRecorder) snapshot() []sseFrame {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]sseFrame{}, recorder.frames...)
}

// waitFor blocks until a change frame with the given id: line was recorded.
func (recorder *streamRecorder) waitFor(t *testing.T, id string, timeout time.Duration) sseFrame {
	t.Helper()
	recorder.mu.Lock()
	for _, frame := range recorder.frames {
		if frame.ID == id {
			recorder.mu.Unlock()
			return frame
		}
	}
	channel := make(chan struct{})
	recorder.notify[id] = channel
	recorder.mu.Unlock()
	select {
	case <-channel:
	case <-time.After(timeout):
		t.Fatalf("SSE frame id=%s never arrived; recorded=%+v", id, recorder.snapshot())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for _, frame := range recorder.frames {
		if frame.ID == id {
			return frame
		}
	}
	t.Fatalf("frame %s disappeared", id)
	return sseFrame{}
}

func (recorder *streamRecorder) countChanges() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	count := 0
	for _, frame := range recorder.frames {
		if frame.Event == "change" {
			count++
		}
	}
	return count
}

func TestTicket04(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T04 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	for _, stale := range []string{"t04-relay-helper"} {
		exec.Command("docker", "rm", "-f", stale).Run()
	}
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)

	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	preExisting := map[string]bool{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		preExisting[image] = exec.Command("docker", "image", "inspect", image).Run() == nil
	}
	// Image build: restricted networks pass their module mirror through the
	// canonical Dockerfile's GOPROXY build argument (QUOIN_IMAGE_GOPROXY,
	// falling back to `go env GOPROXY` — the same authority the host build
	// resolves modules through — because the host proxy often lives in the
	// go env file, not the shell environment).
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		if out, err := exec.Command("go", "env", "GOPROXY").Output(); err == nil {
			imageProxy = strings.TrimSpace(string(out))
		}
	}
	imageEnv := append(append([]string{}, evidence.env...), "QUOIN_IMAGE_GOPROXY="+imageProxy)
	imagesScript := exec.Command("bash", "build/package/images.sh")
	imagesScript.Dir = repoRoot(t)
	imagesScript.Env = imageEnv
	if output, err := imagesScript.CombinedOutput(); err != nil {
		t.Fatalf("images: %v\n%s", err, output)
	}
	evidence.imageIDs = map[string]string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{.Id}}").Output(); err == nil {
			evidence.imageIDs[image] = strings.TrimSpace(string(out))
		}
	}
	evidence.run(t, "build-relayclient-host", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "relayclient-host"), "./cmd/relayclient")
	relayLinux := filepath.Join(workRoot, "relayclient-linux")
	buildCmd := exec.Command("go", "build", "-trimpath", "-o", relayLinux, "./cmd/relayclient")
	buildCmd.Dir = repoRoot(t)
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build linux relayclient: %v\n%s", err, out)
	}

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: %d\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, stelePort, secretDir))

	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "Ticket Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"

	client := cookieClient(t)
	login := httpPost(t, client, base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"admin","password":"%s"}`, tempPassword))
	if !strings.Contains(login, `"passwordChangeRequired":true`) {
		t.Fatalf("first login must require password change: %.300s", login)
	}
	newPassword := randomSecret(t, 24)
	httpPut(t, client, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"%s","newPassword":"%s"}`, tempPassword, newPassword))

	commandID := randomSecret(t, 18)
	createBody := fmt.Sprintf(`{"key":"t04-am","protocol":"alertmanager","clientCommandId":"%s"}`, commandID)
	metadata := httpPost(t, client, base+"/api/v1/alert-sources", origin, jsonMap(createBody))
	var metadataObj struct {
		SourceKey    string `json:"sourceKey"`
		CredentialID string `json:"credentialId"`
		RevealHandle string `json:"revealHandle"`
	}
	if err := json.Unmarshal([]byte(metadata), &metadataObj); err != nil {
		t.Fatalf("create metadata parse: %v\n%s", err, metadata)
	}
	reveal := httpPost(t, client, base+"/api/v1/alert-sources/credentials/reveal", origin, fmt.Sprintf(`{"revealHandle":"%s"}`, metadataObj.RevealHandle))
	var revealObj struct {
		CredentialID string `json:"credentialId"`
		BearerToken  string `json:"bearerToken"`
	}
	if err := json.Unmarshal([]byte(reveal), &revealObj); err != nil {
		t.Fatalf("reveal parse: %v\n%s", err, reveal)
	}
	bearer := revealObj.BearerToken
	sourceID := sourceIDFromMetadata(t, client, base, origin, metadataObj.SourceKey)
	credentialID := revealObj.CredentialID

	// Relay helper: every lifecycle delivery flows through the same
	// SteleRelay gRPC path Stele uses, with the test choosing the relay id
	// (duplicate replay) and the exact body bytes (reorder/subset faults).
	relay := func(name, relayID string, body []byte) string {
		bodyPath := filepath.Join(workRoot, name+".json")
		os.WriteFile(bodyPath, body, 0o600)
		return evidence.run(t, name, nil, "docker", "run", "--rm",
			"--network", "quoin_internal",
			"-v", relayLinux+":/relayclient:ro",
			"-v", filepath.Join(secretDir, "runtime-ca.pem")+":/ca.pem:ro",
			"-v", filepath.Join(secretDir, "stele-service-token")+":/token:ro",
			"-v", bodyPath+":/body.json:ro",
			"node:22-bookworm-slim", "/relayclient",
			"-endpoint", "quoin:8443", "-ca", "/ca.pem", "-token", "/token",
			"-relay-id", relayID, "-source", sourceID,
			"-credential", credentialID, "-snapshot", "1", "-body", "/body.json")
	}

	// The real SSE consumer connects BEFORE any lifecycle delivery runs, so
	// every derived event must arrive over the live stream.
	cookieValue := sessionCookieOf(t, client, base)
	recorder := newStreamRecorder()
	streamCtx, cancelStream := context.WithCancel(context.Background())
	go consumeSSE(streamCtx, t, base, origin, cookieValue, "0", recorder)

	occurrence := "T04Life"
	starts := "2026-08-18T20:00:00Z"
	firingLabels := map[string]string{"alertname": occurrence, "severity": "critical", "instance": "t04-1"}

	// 1. First firing: occurrence created (SSE created, rowVersion 1).
	out := relay("t04-first-firing", "t04-r1", webhookBodyJSON(t, "firing", firingLabels, starts, 0))
	mustContain(t, out, "status=DELIVERY_STATUS_ACCEPTED", "first firing")
	waitSnapshotRow(t, client, base, origin, occurrence, "Firing")
	recorder.waitFor(t, "1", 30*time.Second)

	// 2. Repeat firing (new relay id, same identity): one more observation,
	// no state change, NO new change event (DATA-SSE-001).
	before := recorder.countChanges()
	relay("t04-repeat-firing", "t04-r2", webhookBodyJSON(t, "firing", firingLabels, starts, 0))
	time.Sleep(1500 * time.Millisecond)
	if after := recorder.countChanges(); after != before {
		t.Fatalf("repeat firing must not emit a change event: %d -> %d", before, after)
	}

	// 3. Resolved: state_changed with rowVersion 2.
	relay("t04-resolved", "t04-r3", webhookBodyJSON(t, "resolved", firingLabels, starts, 0))
	waitSnapshotRow(t, client, base, origin, occurrence, "Resolved")
	frame := recorder.waitFor(t, "2", 30*time.Second)
	if !strings.Contains(frame.Data, `"type":"state_changed"`) || !strings.Contains(frame.Data, `"rowVersion":2`) {
		t.Fatalf("resolved frame payload wrong: %+v", frame)
	}

	// 4. Late firing after resolved: observation recorded, occurrence stays
	// Resolved, no reopen, no new change event.
	lateBefore := recorder.countChanges()
	relay("t04-late-firing", "t04-r4", webhookBodyJSON(t, "firing", firingLabels, starts, 0))
	time.Sleep(1500 * time.Millisecond)
	if after := recorder.countChanges(); after != lateBefore {
		t.Fatalf("late firing must not emit a change event: %d -> %d", lateBefore, after)
	}
	occurrenceID := occurrenceIDFromList(t, client, base, origin, occurrence)
	detail := httpGet(t, client, base+"/api/v1/alerts/"+occurrenceID, origin)
	if !strings.Contains(detail, `"state":"Resolved"`) {
		t.Fatalf("late firing must not reopen: %s", detail)
	}
	observations := httpGet(t, client, base+"/api/v1/alerts/"+occurrenceID+"/observations", origin)
	if !strings.Contains(observations, "late_firing_after_resolved") {
		t.Fatalf("late firing observation missing: %s", observations)
	}

	// 5. Resolved-first: a fresh identity first seen as resolved creates a
	// closed occurrence (created event, state Resolved).
	resolvedFirstLabels := map[string]string{"alertname": "T04ResolvedFirst", "instance": "t04-2"}
	relay("t04-resolved-first", "t04-r5", webhookBodyJSON(t, "resolved", resolvedFirstLabels, "2026-08-18T20:05:00Z", 0))
	waitSnapshotRow(t, client, base, origin, "T04ResolvedFirst", "Resolved")
	frame = recorder.waitFor(t, "3", 30*time.Second)
	if !strings.Contains(frame.Data, `"type":"created"`) {
		t.Fatalf("resolved-first must emit created: %+v", frame)
	}

	// 6. Duplicate relay_id replay of the FIRST delivery: deduplicated, no
	// state change, no new event.
	dupBefore := recorder.countChanges()
	out = relay("t04-duplicate-relay", "t04-r1", webhookBodyJSON(t, "firing", firingLabels, starts, 0))
	mustContain(t, out, "already committed", "duplicate relay replay")
	time.Sleep(1500 * time.Millisecond)
	if after := recorder.countChanges(); after != dupBefore {
		t.Fatalf("duplicate relay replay must not emit events: %d -> %d", dupBefore, after)
	}

	// 7. Truncated delivery (truncatedAlerts=1): delivery accepted, one
	// delivery_truncated intake issue, no lifecycle inference.
	truncLabels := map[string]string{"alertname": "T04Truncated", "instance": "t04-3"}
	relay("t04-truncated", "t04-r6", webhookBodyJSON(t, "firing", truncLabels, "2026-08-18T20:10:00Z", 1))
	waitIntakeIssue(t, client, base, origin, "delivery_truncated")

	// 8. Fingerprint mismatch: declared fingerprint not reproducible from
	// the attached labels → fingerprint_mismatch intake issue, the item
	// never enters the occurrence list.
	mismatchBody := []byte(fmt.Sprintf(`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"T04Mismatch"},"startsAt":"2026-08-18T20:15:00Z","fingerprint":"0123456789abcdef"}],"truncatedAlerts":0}`))
	relay("t04-fingerprint-mismatch", "t04-r7", mismatchBody)
	waitIntakeIssue(t, client, base, origin, "fingerprint_mismatch")

	// 9. History view: the resolved occurrences are filterable, the Firing
	// list no longer contains them.
	history := httpGet(t, client, base+"/api/v1/alerts?state=Resolved", origin)
	if !strings.Contains(history, occurrence) || !strings.Contains(history, "T04ResolvedFirst") {
		t.Fatalf("resolved history missing occurrences: %s", history)
	}
	if strings.Contains(httpGet(t, client, base+"/api/v1/alerts", origin), occurrence) {
		t.Fatalf("resolved occurrence must leave the Firing snapshot")
	}

	// 10. SSE replay: reconnect with Last-Event-ID=2 and require the later
	// events first; the header must also win over a stale after parameter.
	replayFrames := replayWithLastEventID(t, base, origin, cookieValue, "2", "0")
	if len(replayFrames) == 0 || replayFrames[0].ID != "3" {
		t.Fatalf("Last-Event-ID replay must resume at id 3: %+v", replayFrames)
	}
	evidence.note(t, "sse-replay-last-event-id.json", mustJSON(t, map[string]any{
		"lastEventID": "2", "after": "0 (must lose to the header)",
		"frames":   replayFrames,
		"expected": "first replayed frame id=3 (header wins over after=0)",
	}))

	cancelStream()
	allFrames := recorder.snapshot()
	evidence.note(t, "sse-live-frames.json", mustJSON(t, map[string]any{
		"frames":      allFrames,
		"expectation": "id 1 created / id 2 state_changed rv=2 / id 3 created (resolved-first); repeat firing, late firing and duplicate relay replay add no frames",
	}))

	// Teardown: only resources this run owns.
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
	evidence.run(t, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
	builtImages := []string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if !preExisting[image] {
			builtImages = append(builtImages, image)
		}
	}
	if len(builtImages) > 0 {
		arguments := append([]string{"rmi"}, builtImages...)
		evidence.run(t, "teardown-images", nil, "docker", arguments...)
	} else {
		evidence.note(t, "teardown-images.json", mustJSON(t, map[string]any{"conclusion": "all four images pre-existed; none removed"}))
	}

	commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
	evidence.writeRuntimeEvidence(t, commit, newPassword, tempPassword, bearer, allFrames)
	scanForSecrets(t, evidenceDir, newPassword, tempPassword, bearer)
	os.RemoveAll(workRoot)
}
