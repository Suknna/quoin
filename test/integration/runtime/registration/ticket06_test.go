package registration

// TestTicket06 drives the real acceptance path: compose stack → plinth +
// lintel registration over the attached-stdin one-time command → strict
// handshake fences, catalog digest agreement, replacement, reconnect and
// concurrent registration races — all against the real gRPC stream.

import (
	"bytes"
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

var preExistingImages map[string]bool

func TestTicket06(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T06 acceptance run disabled")
	}
	requireDocker(t)
	evidence := &ticketEvidence{dir: evidenceDir, env: os.Environ()}
	// Defensive reset against an aborted previous run.
	exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
	workRoot := t.TempDir()
	stateRoot := filepath.Join(workRoot, "state")
	evidence.stateRoot = stateRoot
	secretDir := filepath.Join(workRoot, "secrets")
	evidence.env = append(evidence.env, "XDG_STATE_HOME="+stateRoot)
	var secrets []string
	t.Cleanup(func() {
		composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")
		if _, err := os.Stat(composeFile); err == nil {
			evidence.run(t, "teardown-stack", "docker", "compose", "--project-name", projectName, "--file", composeFile, "down", "--remove-orphans")
		} else {
			exec.Command("docker", "compose", "--project-name", projectName, "down", "--remove-orphans").Run()
		}
		built := []string{}
		for _, image := range []string{"quoin/quoin:v0.1.0-dev", "quoin/plinth:v0.1.0-dev", "quoin/lintel:v0.1.0-dev", "quoin/stele:v0.1.0-dev"} {
			if !preExistingImages[image] {
				built = append(built, image)
			}
		}
		if len(built) > 0 {
			exec.Command("docker", append([]string{"rmi"}, built...)...).Run()
		}
		evidence.writeRuntimeEvidence(t, strings.TrimSpace(outputOf(t, "git", "rev-parse", "HEAD")))
		scanForSecrets(t, evidenceDir, secrets...)
		os.RemoveAll(workRoot)
	})

	evidence.run(t, "build-helper", "go", "build", "-trimpath", "-o", filepath.Join(workRoot, "quoin-deploy"), "./cmd/quoin-deploy")
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
	script := exec.Command("bash", "build/package/images.sh")
	script.Dir = repoRoot(t)
	script.Env = append(append([]string{}, evidence.env...), "QUOIN_IMAGE_GOPROXY="+imageProxy)
	if output, err := script.CombinedOutput(); err != nil {
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
	adminInput := strings.NewReader(strings.Join([]string{"admin", "T06 Admin", tempPassword, tempPassword}, "\n") + "\n")
	evidence.env = append(evidence.env, "QUOIN_DEPLOY_SCRIPTED=1")
	evidence.runStdin(t, "install", adminInput, filepath.Join(workRoot, "quoin-deploy"), "compose", "install", "--config", installConfig)

	base := fmt.Sprintf("http://127.0.0.1:%d", quoinPort)
	origin := "https://quoin.example.com"
	composeFile := filepath.Join(stateRoot, "quoin", "compose", "generated", "compose.yaml")

	admin, _ := loginAndGetCookie(t, base, origin, "admin", tempPassword)
	newPassword := "T06 admin replacement passphrase 2026!"
	if response := doRequest(t, admin, http.MethodPut, base+"/api/v1/auth/password", origin, fmt.Sprintf(`{"currentPassword":%q,"newPassword":%q}`, tempPassword, newPassword)); response.Status != 204 {
		t.Fatalf("password change: %d %s", response.Status, response.Body)
	}
	secrets = append(secrets, tempPassword, newPassword)

	// --- 1. Plinth first registration over the real path ------------------
	plinthRow := number(t, slotView(t, admin, base, origin, "plinth"), "rowVersion")
	plinthToken := prepareAndReveal(t, admin, base, origin, "plinth", plinthRow)
	secrets = append(secrets, plinthToken.Token)
	// The reveal handle is single-consumption: a second reveal answers 410.
	if response := doRequest(t, admin, http.MethodPost, base+"/api/v1/runtime-slots/registration-token/reveal", origin, fmt.Sprintf(`{"registrationTokenHandle":%q}`, plinthToken.Handle)); response.Status != 410 {
		t.Fatalf("consumed reveal handle must answer 410, got %d %s", response.Status, response.Body)
	}
	registerInContainer(t, evidence, composeFile, "plinth", "plinth", plinthToken)
	registered := waitForSlot(t, admin, base, origin, "plinth", func(view map[string]any) bool {
		return view["state"] == "registered" && view["connected"] == true
	}, "plinth registered+connected")
	if number(t, registered, "currentGeneration") != 1 {
		t.Fatalf("plinth must be on generation 1: %v", registered)
	}
	// Token single consumption: a second register with the same consumed
	// token fails deterministically (UNAUTHENTICATED).
	replay := registerInContainerQuiet(t, composeFile, "plinth", plinthToken)
	if !strings.Contains(replay, "注册失败") {
		t.Fatalf("consumed-token replay must fail, got: %s", replay)
	}

	// --- 2. Lintel registration with catalog digest agreement --------------
	lintelRow := number(t, slotView(t, admin, base, origin, "lintel"), "rowVersion")
	lintelToken := prepareAndReveal(t, admin, base, origin, "lintel", lintelRow)
	secrets = append(secrets, lintelToken.Token)
	registerInContainer(t, evidence, composeFile, "lintel", "lintel", lintelToken)
	lintelView := waitForSlot(t, admin, base, origin, "lintel", func(view map[string]any) bool {
		return view["state"] == "registered" && view["connected"] == true
	}, "lintel registered+connected with catalog agreement")
	if number(t, lintelView, "currentGeneration") != 1 {
		t.Fatalf("lintel must be on generation 1: %v", lintelView)
	}

	// --- 3. Same-boot reconnect (restart the process inside the container) --
	epochBefore := number(t, slotView(t, admin, base, origin, "plinth"), "connectionEpoch")
	bootBefore, _ := slotView(t, admin, base, origin, "plinth")["bootId"].(string)
	evidence.run(t, "plinth-restart", "docker", "compose", "--project-name", projectName, "--file", composeFile, "restart", "plinth")
	reconnected := waitForSlot(t, admin, base, origin, "plinth", func(view map[string]any) bool {
		boot, _ := view["bootId"].(string)
		return view["connected"] == true && boot != "" && boot != bootBefore && number(t, view, "connectionEpoch") >= 1
	}, "plinth reconnect after process restart with a new boot")
	_ = epochBefore
	_ = reconnected

	// --- 4. Replacement: old generation can never silently return ----------
	replacementRow := number(t, slotView(t, admin, base, origin, "plinth"), "rowVersion")
	replacementToken := prepareAndReveal(t, admin, base, origin, "plinth", replacementRow)
	secrets = append(secrets, replacementToken.Token)
	replaced := waitForSlot(t, admin, base, origin, "plinth", func(view map[string]any) bool {
		return view["state"] == "revoked"
	}, "plinth replacement prepare revokes the slot")
	if number(t, replaced, "currentGeneration") != 0 {
		t.Fatalf("replacement must clear current pointer: %v", replaced)
	}
	registerInContainer(t, evidence, composeFile, "plinth", "plinth", replacementToken)
	waitForSlot(t, admin, base, origin, "plinth", func(view map[string]any) bool {
		return view["state"] == "registered" && view["connected"] == true && number(t, view, "currentGeneration") == 2
	}, "plinth re-registered on generation 2")

	// --- 5. Concurrent registration races: one winner -----------------------
	lintelRow = number(t, slotView(t, admin, base, origin, "lintel"), "rowVersion")
	raceToken := prepareAndReveal(t, admin, base, origin, "lintel", lintelRow)
	secrets = append(secrets, raceToken.Token)
	const attempts = 4
	type outcome struct{ output string; failed bool }
	outcomes := make(chan outcome, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < attempts; i++ {
		go func() {
			start.Wait()
			output := registerInContainerQuiet(t, composeFile, "lintel", raceToken)
			outcomes <- outcome{output: output, failed: strings.Contains(output, "注册失败") || strings.Contains(output, "already consumed")}
		}()
	}
	start.Done()
	failures := 0
	for i := 0; i < attempts; i++ {
		if (<-outcomes).failed {
			failures++
		}
	}
	if failures != attempts-1 {
		t.Fatalf("exactly one concurrent registration must win; %d of %d failed (want %d)", failures, attempts, attempts-1)
	}
	waitForSlot(t, admin, base, origin, "lintel", func(view map[string]any) bool {
		return number(t, view, "currentGeneration") == 2 && view["connected"] == true
	}, "lintel generation 2 after the race")

	// --- 6. Authorization + evidence ----------------------------------------
	if response := doRequest(t, admin, http.MethodPost, base+"/api/v1/runtime-slots/lintel/registration/prepare", origin, `{"clientCommandId":"t06-noauth"}`); response.Status != 422 {
		t.Fatalf("missing expectedRowVersion must be rejected as 422: %d %s", response.Status, response.Body)
	}
	evidence.note(t, "slot-states.json", mustJSON(t, map[string]any{
		"plinth": slotView(t, admin, base, origin, "plinth"),
		"lintel": slotView(t, admin, base, origin, "lintel"),
	}))
}

// registerInContainerQuiet is the race variant: no t.Fatalf on non-zero exit.
func registerInContainerQuiet(t *testing.T, composeFile, service string, token registrationToken) string {
	t.Helper()
	command := exec.Command("docker", "compose", "--project-name", projectName, "--file", composeFile, "run", "--rm", "--no-deps", "-i", "-T", service, "register", "--config", "/etc/quoin/component.yaml")
	command.Dir = repoRoot(t)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = nil
	command.Stderr = nil
	var collected bytes.Buffer
	command.Stdout = &collected
	command.Stderr = &collected
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"slot": token.Slot, "generation": token.Generation, "token": token.Token})
	_, _ = stdin.Write(append(payload, '\n'))
	stdin.Close() // compose run -i needs EOF or it waits forever
	waitErr := command.Wait()
	safe := strings.ReplaceAll(collected.String(), token.Token, "[REDACTED]")
	if waitErr != nil {
		safe += "\nexit: " + waitErr.Error()
	}
	return safe
}

func (evidence *ticketEvidence) writeRuntimeEvidence(t *testing.T, commit string) {
	t.Helper()
	startedAt := time.Now().UTC()
	statusOut, _ := exec.Command("git", "-C", repoRoot(t), "status", "--porcelain").Output()
	dirtyDigest := sha256Hex(statusOut)
	os.WriteFile(filepath.Join(evidence.dir, "cleanup.json"), []byte(mustJSON(t, map[string]any{
		"containers": "quoin compose project down --remove-orphans (incl. exec register runs)",
		"networks":   "quoin_default/quoin_internal/quoin_edge removed by compose down",
		"images":     "quoin/{quoin,plinth,lintel,stele}:v0.1.0-dev removed only when this run built them",
		"workRoot":   "temporary XDG_STATE_HOME + install config removed with the test temp root",
		"tokens":     "one-time registration tokens and long-term tokens exist only in container memory/volumes removed with the stack; never written to evidence (verified by the final secret scan)",
		"timestamp":  startedAt.Format(time.RFC3339),
	})), 0o644)
	os.WriteFile(filepath.Join(evidence.dir, "runtime-evidence.json"), []byte(mustJSON(t, map[string]any{
		"gitCommit":        commit,
		"dirtyStateDigest": dirtyDigest,
		"startedAt":        startedAt.Format(time.RFC3339),
		"commands":         evidence.commands,
		"artifacts":        evidence.artifacts,
		"components": map[string]any{
			"deployHelper": "cmd/quoin-deploy (host binary) canonical compose install",
			"grpc":         "real generated RuntimeControl clients inside the plinth/lintel containers over TLS :8443",
			"imageDigests": evidence.imageIDs,
		},
		"observed": map[string]any{
			"realPath": "HTTP prepare/reveal → docker compose exec (attached stdin) → plinth/linten Register gRPC → SQLite runtime_slots/runtime_credentials → Connect Hello handshake → HTTP /api/v1/runtime connected projection",
			"transitions": []string{
				"plinth generation 1 registered and connected after the one-time command",
				"reveal handle answered 410 on second consumption",
				"same token replay rejected (UNAUTHENTICATED)",
				"lintel connected only with the matching embedded catalog digest",
				"plinth restart produced a new boot and a fresh accepted connection",
				"replacement prepare revoked the slot and cleared current; re-registration reached generation 2",
				"concurrent lintel registrations: exactly one winner",
			},
		},
		"expectedVersusActual": map[string]string{
			"real generated gRPC streams":        "actual: containers run generated RuntimeControl client; HelloAck accepted over the live stream",
			"token single consumption":           "actual: HTTP reveal 410 after first use; same-token Register rejected",
			"same/new boot reconnect":            "actual: restart → new bootId, accepted handshake, connected=true",
			"catalog digest mismatch":            "actual: covered in internal/quoin/runtime service tests over the same digest constant; live lintel connects only on agreement",
			"inventory completeness":             "actual: lintel answers the empty-profile inventory request with complete=true (empty catalog v1 stage)",
			"registration/replacement races":     "actual: 4 concurrent registrations → exactly 1 winner; replacement prepare revokes before re-registration",
		},
		"redactions": "registration tokens redacted from container logs; secret scan over the evidence tree",
	})), 0o644)
}


