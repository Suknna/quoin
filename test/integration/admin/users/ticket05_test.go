// Package users hosts the T05 ticket acceptance run: the real compose stack
// (Quoin over HTTP) driven through the admin user-management surface —
// authorization matrix, login cooldown and dummy-hash paths, concurrent
// last-admin races over real HTTP, command replay, password resets with
// session revocation, and revoked/disabled session rejection. Evidence lands
// under .artifacts/tickets/T05/.
package users

import (
	"encoding/json"
	"fmt"
	"net/http"
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
)

// preExistingImages records which dev images existed before this run so the
// cleanup only removes what the run built.
var preExistingImages map[string]bool

func TestTicket05(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T05 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	// Defensive reset: a previous aborted run may have left the fixed-name
	// project up (which would also make images.sh skip rebuilding).
	exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)
	// Secrets guarded for the whole run so the final scan runs even on a
	// failed path (cleanup must fire from t.Cleanup, not the happy tail).
	var secrets []string
	t.Cleanup(func() {
		composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
		if _, err := os.Stat(composeFile); err == nil {
			evidence.run(t, "teardown-stack", nil, "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
		} else {
			exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
		}
		builtImages := []string{}
		for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
			if !preExistingImages[image] {
				builtImages = append(builtImages, image)
			}
		}
		if len(builtImages) > 0 {
			exec.Command("docker", append([]string{"rmi"}, builtImages...)...).Run()
		}
		commit := strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD"))
		evidence.writeRuntimeEvidence(t, commit)
		scanForSecrets(t, evidenceDir, secrets...)
		os.RemoveAll(workRoot)
	})

	evidence.run(t, "build-helper", nil, "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
	preExistingImages = map[string]bool{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		preExistingImages[image] = exec.Command("docker", "image", "inspect", image).Run() == nil
	}
	imageProxy := os.Getenv("QUOIN_IMAGE_GOPROXY")
	if imageProxy == "" {
		if out, err := exec.Command("go", "env", "GOPROXY").Output(); err == nil {
			imageProxy = strings.TrimSpace(string(out))
		}
	}
	imagesScript := exec.Command("bash", "build/package/images.sh")
	imagesScript.Dir = repoRoot(t)
	imagesScript.Env = append(append([]string{}, evidence.env...), "QUOIN_IMAGE_GOPROXY="+imageProxy)
	if output, err := imagesScript.CombinedOutput(); err != nil {
		t.Fatalf("images: %v\n%s", err, output)
	}
	evidence.imageIDs = map[string]string{}
	for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
		if out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{.Id}}").Output(); err == nil {
			evidence.imageIDs[image] = strings.TrimSpace(string(out))
		}
	}

	installConfig := filepath.Join(workRoot, "install.yaml")
	writeFile(t, installConfig, fmt.Sprintf("document: compose-install\npublicOrigin: https://quoin.example.com\npublishMode: loopback\nquoinPublicHostPort: %d\nsteleWebhookHostPort: 18081\nsecretDirectory: %s\nlintelBrowserSlots: 1\nlintelShmSizeBytes: 1073741824\n", quoinPort, secretDir))
	tempPassword := randomSecret(t, 24)
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T05 Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.run(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	secrets = append(secrets, tempPassword, "T05 admin replacement passphrase 2026!", "T05 operator initial passphrase 2026!", "T05 replacement passphrase 2027!", "T05 operator final passphrase 2028!", "T05 second admin passphrase 2026!")

	admin, _ := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T05 admin replacement passphrase 2026!"
	change := httpJSON(t, admin, "PUT", base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword))
	if change.Status != 204 {
		t.Fatalf("admin password change: status=%d body=%s", change.Status, change.Body)
	}

	// --- 1. Authorization matrix -----------------------------------------
	opPassword := "T05 operator initial passphrase 2026!"
	create := httpJSON(t, admin, "POST", base+"/api/v1/admin/users", origin, fmt.Sprintf(`{"clientCommandId":"t05-create-op","username":"op-t05","displayName":"T05 Operator","role":"operator","password":%q}`, opPassword))
	if create.Status != 201 {
		t.Fatalf("create operator: status=%d body=%s", create.Status, create.Body)
	}
	operator, _ := loginAndGetCookie(t, base, origin, "op-t05", opPassword)
	opForbidden := httpJSON(t, operator, "GET", base+"/api/v1/admin/users", origin, "")
	opWriteForbidden := httpJSON(t, operator, "POST", base+"/api/v1/admin/users", origin, fmt.Sprintf(`{"clientCommandId":"t05-op-write","username":"op2","displayName":"Two","role":"operator","password":%q}`, opPassword))
	opAudit := httpJSON(t, operator, "GET", base+"/api/v1/audit-events?action=user.create", origin, "")
	opSessions := httpJSON(t, operator, "GET", base+"/api/v1/auth/sessions", origin, "")
	if opForbidden.Status != 403 || !strings.Contains(opForbidden.Body, "forbidden") {
		t.Fatalf("operator reading users must be forbidden: %d %s", opForbidden.Status, opForbidden.Body)
	}
	if opWriteForbidden.Status != 403 {
		t.Fatalf("operator creating users must be forbidden: %d %s", opWriteForbidden.Status, opWriteForbidden.Body)
	}
	if opAudit.Status != 200 || !strings.Contains(opAudit.Body, "user.create") {
		t.Fatalf("operator must read audit events: %d %s", opAudit.Status, opAudit.Body)
	}
	if opSessions.Status != 200 || !strings.Contains(opSessions.Body, `"current":true`) {
		t.Fatalf("operator must read own sessions: %d %s", opSessions.Status, opSessions.Body)
	}
	evidence.note(t, "authorization-matrix.json", mustJSON(t, map[string]any{
		"operator_list_users":    map[string]any{"expected": 403, "actual": opForbidden.Status, "body": opForbidden.Body},
		"operator_create_user":   map[string]any{"expected": 403, "actual": opWriteForbidden.Status, "body": opWriteForbidden.Body},
		"operator_read_audit":    map[string]any{"expected": 200, "actual": opAudit.Status},
		"operator_read_sessions": map[string]any{"expected": 200, "actual": opSessions.Status},
	}))

	// Restricted session after an admin reset: everything except
	// me/password/logout answers 403 password_change_required.
	opRow := userByName(t, admin, base, origin, "op-t05")
	resetBody := fmt.Sprintf(`{"clientCommandId":"t05-reset-op","expectedRowVersion":%d,"newPassword":%q}`, opRow.RowVersion, "T05 replacement passphrase 2027!")
	reset := httpJSON(t, admin, "POST", base+"/api/v1/admin/users/"+opRow.ID+"/reset-password", origin, resetBody)
	if reset.Status != 200 || !strings.Contains(reset.Body, `"revokedSessionCount":1`) {
		t.Fatalf("reset operator password: status=%d body=%s", reset.Status, reset.Body)
	}
	if rejected := httpJSON(t, operator, "GET", base+"/api/v1/auth/sessions", origin, ""); rejected.Status != 401 {
		t.Fatalf("revoked session must be rejected on the next request: %d %s", rejected.Status, rejected.Body)
	}
	tempOp, tempOpCookie := loginAndGetCookie(t, base, origin, "op-t05", "T05 replacement passphrase 2027!")
	restricted := httpJSON(t, tempOp, "GET", base+"/api/v1/audit-events", origin, "")
	if restricted.Status != 403 || !strings.Contains(restricted.Body, "password_change_required") {
		t.Fatalf("restricted session must be blocked with password_change_required: %d %s", restricted.Status, restricted.Body)
	}
	finalOpPassword := "T05 operator final passphrase 2028!"
	if done := httpJSON(t, tempOp, "PUT", base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":"T05 replacement passphrase 2027!","newPassword":%q}`, finalOpPassword)); done.Status != 204 {
		t.Fatalf("restricted session must still change its own password: %d %s", done.Status, done.Body)
	}
	_ = tempOpCookie

	// --- 2. Login cooldown and dummy-hash paths ---------------------------
	ghost := "t05-ghost-user"
	ghostClient := bareClient()
	var wrongExisting statusResponse
	startExisting := time.Now()
	wrongExisting = doRequest(t, ghostClient, "POST", base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"op-t05","password":"definitely wrong passphrase!"}`))
	existingDuration := time.Since(startExisting)
	var wrongGhost statusResponse
	var ghostDuration time.Duration
	for attempt := 1; attempt <= 5; attempt++ {
		start := time.Now()
		wrongGhost = doRequest(t, ghostClient, "POST", base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":%q,"password":"definitely wrong passphrase!"}`, ghost))
		ghostDuration = time.Since(start)
		if wrongGhost.Status != 401 {
			t.Fatalf("ghost login %d: status=%d body=%s", attempt, wrongGhost.Status, wrongGhost.Body)
		}
	}
	cooldown := doRequest(t, ghostClient, "POST", base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":%q,"password":"another wrong passphrase!"}`, ghost))
	if cooldown.Status != 429 || cooldown.Headers.Get("Retry-After") == "" {
		t.Fatalf("sixth ghost failure must be rate limited with Retry-After: status=%d headers=%v body=%s", cooldown.Status, cooldown.Headers, cooldown.Body)
	}
	if wrongExisting.Status != 401 || wrongExisting.Body != wrongGhost.Body {
		t.Fatalf("unified failure message drift:\nexisting=%s\nghost=%s", wrongExisting.Body, wrongGhost.Body)
	}
	// Dummy-hash: an unknown user costs a comparable Argon2id verification
	// rather than returning early.
	if ghostDuration < 15*time.Millisecond {
		t.Fatalf("ghost-user login returned suspiciously fast (%s); dummy hash path missing", ghostDuration)
	}
	if existingDuration < 15*time.Millisecond {
		t.Fatalf("existing-user wrong password returned suspiciously fast (%s)", existingDuration)
	}
	evidence.note(t, "login-cooldown.json", mustJSON(t, map[string]any{
		"ghost_failures":         5,
		"sixth_attempt":          map[string]any{"expected": 429, "actual": cooldown.Status, "retryAfter": cooldown.Headers.Get("Retry-After")},
		"unified_message":        map[string]any{"expected": "identical body for unknown user / wrong password", "actual": wrongGhost.Body},
		"dummy_hash_timing":      map[string]any{"ghostOneFailureMs": ghostDuration.Milliseconds(), "existingOneFailureMs": existingDuration.Milliseconds(), "floorMs": 15},
		"cooldown_scope":         "per normalized username in process memory; the cooldown user is isolated from later flows",
	}))

	// --- 3. Command replay -------------------------------------------------
	replayCreate := fmt.Sprintf(`{"clientCommandId":"t05-create-op","username":"op-t05","displayName":"T05 Operator","role":"operator","password":%q}`, opPassword)
	replayed := httpJSON(t, admin, "POST", base+"/api/v1/admin/users", origin, replayCreate)
	if replayed.Status != 201 || replayed.Body != create.Body {
		t.Fatalf("replay must return the original 201 body: status=%d\nfirst=%s\nreplay=%s", replayed.Status, create.Body, replayed.Body)
	}
	reused := httpJSON(t, admin, "POST", base+"/api/v1/admin/users", origin, strings.Replace(replayCreate, "T05 Operator", "Renamed", 1))
	if reused.Status != 409 || !strings.Contains(reused.Body, "command_id_reused") {
		t.Fatalf("same command id with a different request must conflict: %d %s", reused.Status, reused.Body)
	}
	duplicate := httpJSON(t, admin, "POST", base+"/api/v1/admin/users", origin, strings.ReplaceAll(replayCreate, "t05-create-op", "t05-create-op-dup"))
	if duplicate.Status != 409 || !strings.Contains(duplicate.Body, "用户名已存在") {
		t.Fatalf("duplicate username must conflict with a human message: %d %s", duplicate.Status, duplicate.Body)
	}

	// --- 4. Concurrent last-admin races over real HTTP ----------------------
	admin2Password := "T05 second admin passphrase 2026!"
	admin2Create := httpJSON(t, admin, "POST", base+"/api/v1/admin/users", origin, fmt.Sprintf(`{"clientCommandId":"t05-create-admin2","username":"admin2","displayName":"T05 Second Admin","role":"admin","password":%q}`, admin2Password))
	if admin2Create.Status != 201 {
		t.Fatalf("create second admin: %d %s", admin2Create.Status, admin2Create.Body)
	}
	admin2, _ := loginAndGetCookie(t, base, origin, "admin2", admin2Password)
	adminRow := userByName(t, admin, base, origin, "admin")
	admin2Row := userByName(t, admin, base, origin, "admin2")
	const raceAttempts = 6
	results := make(chan statusResponse, raceAttempts*2)
	var ready sync.WaitGroup
	ready.Add(1)
	raceOne := func(client *http.Client, row userRow, who string) {
		ready.Wait()
		body := fmt.Sprintf(`{"clientCommandId":"t05-race-%s-%d","expectedRowVersion":%d,"enabled":false}`, who, time.Now().UnixNano(), row.RowVersion)
		results <- doRequest(t, client, "PATCH", base+"/api/v1/admin/users/"+row.ID, origin, body)
	}
	for i := 0; i < raceAttempts; i++ {
		go raceOne(admin, admin2Row, "a")
		go raceOne(admin2, adminRow, "b")
	}
	ready.Done()
	successes, conflicts, unauthorized := 0, 0, 0
	for i := 0; i < raceAttempts*2; i++ {
		result := <-results
		switch result.Status {
		case 200:
			successes++
		case 409:
			conflicts++
		case 401:
			unauthorized++
		default:
			t.Fatalf("race produced status=%d body=%s", result.Status, result.Body)
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one concurrent disable must win, got %d (conflicts=%d unauthorized=%d)", successes, conflicts, unauthorized)
	}
	// Whoever lost the race also lost their session: find the surviving
	// admin by trying both clients against the authoritative list.
	finalList := httpJSON(t, admin2, "GET", base+"/api/v1/admin/users", origin, "")
	if finalList.Status == 401 {
		finalList = httpJSON(t, admin, "GET", base+"/api/v1/admin/users", origin, "")
	}
	if finalList.Status != 200 {
		t.Fatalf("survivor user list: status=%d body=%s", finalList.Status, finalList.Body)
	}
	var parsedList struct {
		Items []userRow `json:"items"`
	}
	if err := json.Unmarshal([]byte(finalList.Body), &parsedList); err != nil {
		t.Fatalf("parse final users: %v", err)
	}
	enabledAdmins, survivorClient := 0, admin
	var survivorRow userRow
	for _, row := range parsedList.Items {
		if row.Role == "admin" && row.Enabled {
			enabledAdmins++
			survivorRow = row
			if row.Username == "admin2" {
				survivorClient = admin2
			}
		}
	}
	if enabledAdmins != 1 || survivorRow.ID == "" {
		t.Fatalf("exactly one effective admin must remain, got %d: %s", enabledAdmins, finalList.Body)
	}
	evidence.note(t, "last-admin-race.json", mustJSON(t, map[string]any{
		"attempts":       raceAttempts * 2,
		"successes":      map[string]any{"expected": 1, "actual": successes},
		"conflicts":      conflicts,
		"unauthorized":   unauthorized,
		"enabledAdmins":  map[string]any{"expected": 1, "actual": enabledAdmins},
	}))

	// Whoever survived still cannot remove themselves.
	selfDisable := httpJSON(t, survivorClient, "PATCH", base+"/api/v1/admin/users/"+survivorRow.ID, origin, fmt.Sprintf(`{"clientCommandId":"t05-self-disable","expectedRowVersion":%d,"enabled":false}`, survivorRow.RowVersion))
	if selfDisable.Status != 409 || !strings.Contains(selfDisable.Body, "active_conflict") {
		t.Fatalf("surviving admin self-disable must hit the last-admin guard: %d %s", selfDisable.Status, selfDisable.Body)
	}

	// --- 5. Disabled account rejects its sessions ---------------------------
	opFinalRow := userByName(t, survivorClient, base, origin, "op-t05")
	disabledOp := httpJSON(t, survivorClient, "PATCH", base+"/api/v1/admin/users/"+opFinalRow.ID, origin, fmt.Sprintf(`{"clientCommandId":"t05-disable-op","expectedRowVersion":%d,"enabled":false}`, opFinalRow.RowVersion))
	if disabledOp.Status != 200 {
		t.Fatalf("disable operator: %d %s", disabledOp.Status, disabledOp.Body)
	}
	if rejected := doRequest(t, bareClientWithCookie(tempOpCookie), "GET", base+"/api/v1/auth/me", origin, ""); rejected.Status != 401 {
		t.Fatalf("disabled account must reject existing sessions: %d %s", rejected.Status, rejected.Body)
	}
	if disabledLogin := doRequest(t, bareClient(), "POST", base+"/api/v1/auth/login", origin, fmt.Sprintf(`{"username":"op-t05","password":%q}`, finalOpPassword)); disabledLogin.Status != 401 {
		t.Fatalf("disabled account login must fail uniformly: %d %s", disabledLogin.Status, disabledLogin.Body)
	}

	// --- 6. Explicit revoke-sessions command + audit trail ----------------
	revokeTarget := userByName(t, survivorClient, base, origin, "admin2")
	if survivorRow.Username == "admin2" {
		revokeTarget = userByName(t, survivorClient, base, origin, "admin")
	}
	revoke := httpJSON(t, survivorClient, "POST", base+"/api/v1/admin/users/"+revokeTarget.ID+"/revoke-sessions", origin, `{"clientCommandId":"t05-revoke-sessions"}`)
	if revoke.Status != 200 {
		t.Fatalf("revoke sessions: %d %s", revoke.Status, revoke.Body)
	}
	audit := httpJSON(t, survivorClient, "GET", base+"/api/v1/audit-events?limit=100", origin, "")
	for _, action := range []string{"user.create", "user.update", "user.reset_password", "user.revoke_sessions"} {
		if !strings.Contains(audit.Body, `"`+action+`"`) {
			t.Fatalf("audit trail missing %s: %s", action, audit.Body)
		}
	}

	// Teardown happens in t.Cleanup (see top) so failure paths clean too.
}
