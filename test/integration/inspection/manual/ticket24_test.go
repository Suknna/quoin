// Package manual holds the opt-in, real-process acceptance entrypoint for T24.
package manual

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

type commandEvidence struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
	ExitCode  int      `json:"exitCode"`
	Log       string   `json:"log"`
	SHA256    string   `json:"sha256"`
}
type observations struct {
	RunID                      string   `json:"runId"`
	State                      string   `json:"state"`
	PreparedConfigVersionID    string   `json:"preparedConfigVersionId"`
	PublishedConfigVersionID   string   `json:"publishedConfigVersionId"`
	SupersedingConfigVersionID string   `json:"supersedingConfigVersionId"`
	ExecutedCheckKeys          []string `json:"executedCheckKeys"`
	ReportRereadIdentical      bool     `json:"reportRereadIdentical"`
	Checks                     []struct {
		CheckKey   string `json:"checkKey"`
		Status     string `json:"status"`
		EvidenceID string `json:"evidenceId"`
		GapReason  string `json:"gapReason"`
	} `json:"checks"`
	Report struct {
		Version        int      `json:"version"`
		EvidenceIDs    []string `json:"evidenceIds"`
		EvidenceDigest string   `json:"evidenceDigest"`
	} `json:"report"`
	NavigationReturned bool             `json:"navigationReturned"`
	Events             []map[string]any `json:"events"`
}

// TestTicket24 is only the ticket-acceptance leg. The outer Issue #47 script
// owns generic gates and their tee logs; this test preserves those artifacts.
func TestTicket24(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T24 real-process acceptance is disabled")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skipf("pnpm unavailable: %v", err)
	}
	repo := root(t)
	actual, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T24") {
		t.Fatalf("T24 evidence directory must be canonical: %s", actual)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := lockTicket(t, filepath.Join(repo, ".artifacts", "tickets", ".browser-e2e.lock"))
	defer lock.release()
	before := sourceDigest(t, repo)
	commands := []commandEvidence{}
	manifest := func() {
		var observed map[string]any
		if body, err := os.ReadFile(filepath.Join(actual, "t24-inspection-observations.json")); err == nil {
			_ = json.Unmarshal(body, &observed)
		}
		writeJSON(t, filepath.Join(actual, "runtime-evidence.json"), map[string]any{"gitCommit": strings.TrimSpace(output(t, repo, "git", "rev-parse", "HEAD")), "sourceDigestBefore": before, "sourceDigestAfter": sourceDigest(t, repo), "commands": commands, "events": observed["events"], "observations": observed, "componentVersions": componentVersions(repo), "rawEvidence": evidenceDigests(t, actual)})
	}
	defer manifest() // failure leaves command metadata; ordinary outer-gate logs survive.
	run := func(name string, argv ...string) {
		t.Helper()
		started := time.Now().UTC()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		cmd.Env = ticketEnv(actual, true)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		err := cmd.Run()
		ended := time.Now().UTC()
		code := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				code = -1
			}
		}
		log := filepath.Join(actual, name+".log")
		if e := os.WriteFile(log, output.Bytes(), 0o644); e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(output.Bytes())
		commands = append(commands, commandEvidence{Name: name, Argv: argv, StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano), ExitCode: code, Log: log, SHA256: hex.EncodeToString(sum[:])})
		manifest()
		if err != nil {
			t.Fatalf("%s exited %d:\n%s", name, code, output.String())
		}
	}
	run("ticket24-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-24", "--project=chromium")
	after := sourceDigest(t, repo)
	if before != after {
		t.Fatalf("verified source changed during T24 acceptance: before=%s after=%s", before, after)
	}
	body, err := os.ReadFile(filepath.Join(actual, "t24-inspection-observations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got observations
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	ok, gap := false, false
	collected := map[string]bool{}
	for _, check := range got.Checks {
		if check.Status == "ok" && check.EvidenceID != "" {
			ok = true
			collected[check.EvidenceID] = true
		}
		if check.Status == "gap" {
			gap = true
		}
	}
	if got.RunID == "" || got.State != "CompletedWithGaps" || !ok || !gap {
		t.Fatalf("mixed Run proof missing: %#v", got)
	}
	if got.PublishedConfigVersionID == "" || got.SupersedingConfigVersionID == "" || got.SupersedingConfigVersionID == got.PublishedConfigVersionID {
		t.Fatalf("frozen published binding proof missing: %#v", got)
	}
	if len(got.ExecutedCheckKeys) != 2 || got.ExecutedCheckKeys[0] != "broken-page" || got.ExecutedCheckKeys[1] != "up-instant" {
		t.Fatalf("Run did not execute the frozen v1 check set: %#v", got.ExecutedCheckKeys)
	}
	if !got.ReportRereadIdentical {
		t.Fatalf("immutable report byte-identical re-read proof missing: %#v", got)
	}
	if !got.NavigationReturned || got.Report.Version != 1 || !isSHA256(got.Report.EvidenceDigest) {
		t.Fatalf("navigation/report proof missing: %#v", got)
	}
	for _, id := range got.Report.EvidenceIDs {
		if !collected[id] {
			t.Fatalf("report Evidence %s not collected", id)
		}
	}
	cleanupBody, err := os.ReadFile(filepath.Join(actual, "cleanup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cleanup struct {
		Phases []struct {
			Phase     string `json:"phase"`
			Resources []struct {
				Name               string `json:"name"`
				RemovalCommand     string `json:"removalCommand"`
				ObservedFinalState string `json:"observedFinalState"`
			} `json:"resources"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(cleanupBody, &cleanup); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.Phases) == 0 {
		t.Fatal("cleanup.json has no teardown phases")
	}
	// The bootstrap-exit phase owns the real dispositions: every host process
	// must carry an actual kill+absence probe, never a "not started" claim.
	hostProcesses := map[string]bool{"TLS proxy": false, "readiness server": false, "model fixture": false, "Thanos fixture": false}
	for _, phase := range cleanup.Phases {
		for _, resource := range phase.Resources {
			if resource.ObservedFinalState != "absent" {
				t.Fatalf("cleanup not proven absent: %s/%s", phase.Phase, resource.Name)
			}
			if _, expected := hostProcesses[resource.Name]; expected && strings.Contains(resource.RemovalCommand, "kill") {
				hostProcesses[resource.Name] = true
			}
		}
	}
	for name, proven := range hostProcesses {
		if !proven {
			t.Fatalf("cleanup.json lacks a real kill+absence probe for host process %s", name)
		}
	}
}

type ticketLock struct{ f *os.File }

func (l ticketLock) release() {
	if l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		_ = l.f.Close()
	}
}
func lockTicket(t *testing.T, path string) ticketLock {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return ticketLock{f}
}
func root(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("repository root not found")
		}
		d = p
	}
}
func ticketEnv(dir string, playwright bool) []string {
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "QUOIN_TICKET=") && !strings.HasPrefix(v, "QUOIN_EVIDENCE_DIR=") && !strings.HasPrefix(v, "QUOIN_BROWSER_E2E_LOCK_HELD=") {
			env = append(env, v)
		}
	}
	return append(env, "QUOIN_TICKET=T24", "QUOIN_EVIDENCE_DIR="+dir, "QUOIN_BROWSER_E2E_LOCK_HELD=1")
}
func output(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func sourceDigest(t *testing.T, repo string) string {
	t.Helper()
	patch := output(t, repo, "git", "diff", "--binary", "HEAD")
	untracked := strings.Fields(output(t, repo, "git", "ls-files", "--others", "--exclude-standard"))
	sort.Strings(untracked)
	h := sha256.New()
	_, _ = h.Write([]byte("patch\x00" + patch))
	for _, p := range untracked {
		b, err := os.ReadFile(filepath.Join(repo, p))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Write([]byte(p + "\x00"))
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
func componentVersions(repo string) map[string]string {
	versions := map[string]string{"quoinSource": strings.TrimSpace(runOutput(repo, "git", "rev-parse", "HEAD"))}
	for name, path := range map[string]string{"thanosFixture": "test/fixtures/thanos-query/main.go", "modelFixture": "test/fixtures/model-provider/main.go"} {
		if body, err := os.ReadFile(filepath.Join(repo, path)); err == nil {
			sum := sha256.Sum256(body)
			versions[name+"SHA256"] = hex.EncodeToString(sum[:])
		}
	}
	if value := strings.TrimSpace(runOutput(repo, "docker", "version", "--format", "{{.Server.Version}}")); value != "" {
		versions["dockerServer"] = value
	}
	return versions
}
func runOutput(repo, command string, args ...string) string {
	cmd := exec.Command(command, args...)
	cmd.Dir = repo
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(b)
}
func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func evidenceDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil {
			r, _ := filepath.Rel(dir, path)
			got[r] = fmt.Sprintf("%x", sha256.Sum256(b))
		}
		return err
	})
	return got
}
