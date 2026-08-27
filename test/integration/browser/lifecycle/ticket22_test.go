package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// init lifts only Go's implicit package deadline for this opt-in test. The
// real stack's documented cold-start readiness interval exceeds `go test`'s
// injected ten-minute default; an explicit caller-supplied timeout is retained.
func init() {
	for index, argument := range os.Args {
		if argument == "-test.timeout=10m" || argument == "-test.timeout=10m0s" {
			os.Args[index] = "-test.timeout=30m"
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// acquireTicket22Lock serializes the fixed-name T22 Docker project, TLS port,
// and evidence directory. A second opt-in invocation must wait rather than
// deleting the first invocation's generated Compose projection during startup.
func acquireTicket22Lock(t *testing.T, root string) func() {
	t.Helper()
	lockPath := filepath.Join(root, ".artifacts", "tickets", ".browser-e2e.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("create T22 lock directory: %v", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open T22 lock: %v", err)
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			lock.Close()
			t.Fatalf("lock T22 acceptance: %v", err)
		}
		if deadline, ok := t.Deadline(); ok && time.Until(deadline) <= time.Minute {
			lock.Close()
			t.Fatal("T22 acceptance remained locked until its test deadline")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return func() {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
			t.Errorf("unlock T22 acceptance: %v", err)
		}
		if err := lock.Close(); err != nil {
			t.Errorf("close T22 lock: %v", err)
		}
	}
}

// TestTicket22 is the opt-in real-process Exploration acceptance entrypoint.
// It runs a real Investigation through Plinth's model turn, Quoin's outbound
// Runtime stream, Lintel, and Chromium. A published read-only profile is only
// setup; the asserted operation is never a manual-login operation.
func TestTicket22(t *testing.T) {
	evidenceDir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceDir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T22 real-process acceptance is disabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skipf("pnpm unavailable: %v", err)
	}
	root := repositoryRoot(t)
	releaseTicket22Lock := acquireTicket22Lock(t, root)
	defer releaseTicket22Lock()
	wanted := filepath.Join(root, ".artifacts", "tickets", "T22")
	actual := evidenceDir
	if !filepath.IsAbs(actual) {
		actual = filepath.Join(root, actual)
	}
	actual, err := filepath.Abs(actual)
	if err != nil || actual != wanted {
		t.Fatalf("T22 evidence directory must be %s, got %s (err=%v)", wanted, actual, err)
	}
	if err := os.RemoveAll(actual); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}

	sourceBefore := workspaceDigest(t)
	type evidence struct {
		Name string `json:"name"`
		Exit int    `json:"exitCode"`
		Log  string `json:"log"`
		SHA  string `json:"sha256"`
	}
	commands := make([]evidence, 0, 4)
	run := func(name, command string, args ...string) {
		t.Helper()
		cmd := exec.Command(command, args...)
		cmd.Dir = root
		env := make([]string, 0, len(os.Environ())+2)
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "QUOIN_TICKET=") && !strings.HasPrefix(item, "QUOIN_EVIDENCE_DIR=") {
				env = append(env, item)
			}
		}
		if name == "ticket22-playwright" {
			env = append(env, "QUOIN_TICKET=T22", "QUOIN_EVIDENCE_DIR="+actual, "QUOIN_BROWSER_E2E_LOCK_HELD=1")
		} else {
			env = append(env, "QUOIN_TICKET=", "QUOIN_EVIDENCE_DIR=")
		}
		cmd.Env = env
		started := time.Now()
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		runErr := cmd.Run()
		exit := 0
		if runErr != nil {
			if process, ok := runErr.(*exec.ExitError); ok {
				exit = process.ExitCode()
			} else {
				exit = -1
			}
		}
		path := filepath.Join(actual, name+".log")
		if err := os.WriteFile(path, output.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(output.Bytes())
		commands = append(commands, evidence{Name: name, Exit: exit, Log: path, SHA: hex.EncodeToString(digest[:])})
		if runErr != nil {
			t.Fatalf("%s exited %d after %s:\n%s", name, exit, time.Since(started).Round(time.Millisecond), output.String())
		}
	}

	run("verify-contracts", "go", "run", "./ci/contracts/verify")
	run("go-test-browser-runtime", "go", "test", "./internal/lintel/browser", "./internal/lintel/runtime", "./internal/quoin/app", "./internal/quoin/browser")
	run("ticket22-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-22", "--project=chromium")
	if sourceBefore != workspaceDigest(t) {
		t.Fatal("verified source changed during T22 acceptance")
	}
	observationPath := filepath.Join(actual, "t22-runtime-observations.json")
	body, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatalf("T22 real-runtime observation missing: %v", err)
	}
	var observation map[string]any
	if json.Unmarshal(body, &observation) != nil {
		t.Fatal("T22 real-runtime observation is invalid JSON")
	}
	if observation["runtimePath"] != "Investigation HTTP -> Plinth model -> Quoin Runtime gRPC -> Lintel -> Chromium" {
		t.Fatalf("unexpected T22 runtime path: %#v", observation)
	}
	if calls, ok := observation["browserToolCalls"].(float64); !ok || calls != 2 {
		t.Fatalf("T22 must observe exactly open+close Browser Tool Calls, got %#v", observation["browserToolCalls"])
	}
	operation, ok := observation["operation"].(map[string]any)
	if !ok || operation["kind"] != "exploration" || operation["state"] != "Succeeded" || operation["traceIntegrity"] != "complete" {
		t.Fatalf("T22 operation is not a completed Exploration with complete trace: %#v", observation["operation"])
	}
	if _, ok := operation["traceArtifactId"].(string); !ok {
		t.Fatalf("T22 complete trace Artifact id missing: %#v", operation)
	}
	if digest, ok := operation["completionDigest"].(string); !ok || len(digest) != sha256.Size*2 {
		t.Fatalf("T22 completion digest missing or malformed: %#v", operation["completionDigest"])
	}
	if _, ok := operation["stopConfirmedAt"].(string); !ok {
		t.Fatalf("T22 browser Stop confirmation missing: %#v", operation)
	}
	if released, ok := observation["slotReleased"].(bool); !ok || !released {
		t.Fatalf("T22 identity slot was not released: %#v", observation["slotReleased"])
	}
	// A successful close must not pass through cancellation convergence at its
	// StopAck. Besides being semantically wrong, that used to leave a real
	// reconcile.cancel_converge error in the production-process evidence despite
	// an otherwise successful operation.
	runtimeLog, err := os.ReadFile(filepath.Join(actual, "runtime-process.log"))
	if err != nil {
		t.Fatalf("T22 runtime process log missing: %v", err)
	}
	if bytes.Contains(runtimeLog, []byte(`"code":"reconcile.cancel_converge"`)) {
		t.Fatal("successful T22 close emitted cancellation convergence error")
	}
	writeJSON(t, filepath.Join(actual, "runtime-evidence.json"), map[string]any{
		"gitCommit":          strings.TrimSpace(commandOutput(t, "git", "rev-parse", "HEAD")),
		"sourceDigestBefore": sourceBefore,
		"sourceDigestAfter":  workspaceDigest(t),
		"commands":           commands,
		"realRuntime":        "Investigation HTTP -> Plinth model -> Quoin Runtime gRPC -> Lintel -> Chromium",
		"rawObservation":     sha256Text(body),
	})
}
