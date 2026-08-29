// Package schedule owns the opt-in real-process acceptance entrypoint for T25.
package schedule

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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

// TestTicket25 combines deterministic package seams with the actual compose,
// Quoin, Runtime, external-fixture, Chromium/UI path. It never writes product
// tables directly: the browser scenario uses only authenticated HTTP APIs.
func TestTicket25(t *testing.T) {
	dir := os.Getenv("QUOIN_EVIDENCE_DIR")
	if dir == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T25 real-process acceptance is disabled")
	}
	for _, command := range []string{"docker", "pnpm"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s unavailable: %v", command, err)
		}
	}
	repo := root(t)
	actual, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != filepath.Join(repo, ".artifacts", "tickets", "T25") {
		t.Fatalf("T25 evidence directory must be canonical: %s", actual)
	}
	if err := os.MkdirAll(actual, 0o755); err != nil {
		t.Fatal(err)
	}
	before := sourceDigest(t, repo)
	commands := []commandEvidence{}
	manifest := func() {
		var observed map[string]any
		if body, readErr := os.ReadFile(filepath.Join(actual, "t25-schedule-observations.json")); readErr == nil {
			_ = json.Unmarshal(body, &observed)
		}
		writeJSON(t, filepath.Join(actual, "runtime-evidence.json"), map[string]any{
			"gitCommit":           strings.TrimSpace(output(t, repo, "git", "rev-parse", "HEAD")),
			"sourceDigestBefore":  before,
			"sourceDigestAfter":   sourceDigest(t, repo),
			"commands":            commands,
			"componentVersions":   componentVersions(repo),
			"observedTransitions": observed["events"],
			"observations":        observed,
			"expectedActual": []map[string]string{
				{"case": "package-private Clock", "expected": "controllable clock drives exact minute scheduling", "actual": "see scheduler-unit.log TestTickSchedulesCurrentBoundaryInPlanTimezone"},
				{"case": "DST/timezone/boundary", "expected": "local cron maps to canonical UTC; repeated and nonexistent wall times are deterministic", "actual": "see scheduler-unit.log DST and timezone tests"},
				{"case": "duplicate/restart race", "expected": "one durable (business_system, plan, scheduled_for) key across repeated scheduler work", "actual": "see inspection-unit.log deterministic-key replay and playwright observation sameUTCKeyRows"},
				{"case": "real timer smoke", "expected": "a published each-minute plan creates a visible scheduled Run", "actual": "see t25-schedule-observations.json and ticket25-playwright.log"},
				{"case": "missed-run behavior", "expected": "startup and a late wakeup create no historical Run", "actual": "see scheduler-unit.log TestRunDoesNotBackfillStartupMinute and TestRunSkipsLateWakeupWithoutBackfill"},
			},
			"rawEvidence": evidenceDigests(t, actual),
		})
	}
	defer manifest()
	run := func(name string, argv ...string) {
		t.Helper()
		started := time.Now().UTC()
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = repo
		cmd.Env = ticketEnv(actual)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		runErr := cmd.Run()
		ended := time.Now().UTC()
		code := 0
		if runErr != nil {
			if exit, ok := runErr.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				code = -1
			}
		}
		log := filepath.Join(actual, name+".log")
		if err := os.WriteFile(log, output.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(output.Bytes())
		commands = append(commands, commandEvidence{Name: name, Argv: argv, StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano), ExitCode: code, Log: log, SHA256: hex.EncodeToString(sum[:])})
		manifest()
		if runErr != nil {
			t.Fatalf("%s exited %d:\n%s", name, code, output.String())
		}
	}
	run("scheduler-unit", "go", "test", "./internal/quoin/inspection/scheduler", "-count=1")
	run("inspection-unit", "go", "test", "./internal/quoin/inspection", "-run", "TestCreateScheduledInspectionRun|TestScheduledPlansIgnorePlansWithoutChecks", "-count=1")
	run("ticket25-playwright", "pnpm", "--dir", "web", "exec", "playwright", "test", "--grep", "@ticket-25", "--project=chromium")

	if before != sourceDigest(t, repo) {
		t.Fatalf("verified source changed during T25 acceptance")
	}
	body, err := os.ReadFile(filepath.Join(actual, "t25-schedule-observations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		RunID           string `json:"runId"`
		TriggerKind     string `json:"triggerKind"`
		ScheduledFor    string `json:"scheduledFor"`
		SameUTCKeyRows  int    `json:"sameUTCKeyRows"`
		NoLateBackfills bool   `json:"noLateBackfills"`
		State           string `json:"state"`
	}
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.RunID == "" || observed.TriggerKind != "schedule" || observed.ScheduledFor == "" || observed.SameUTCKeyRows != 1 || !observed.NoLateBackfills || (observed.State != "Completed" && observed.State != "CompletedWithGaps") {
		t.Fatalf("scheduled real-timer proof missing: %#v", observed)
	}
	cleanup, err := os.ReadFile(filepath.Join(actual, "cleanup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cleaned struct {
		Phases []struct {
			Resources []struct {
				Name               string `json:"name"`
				ObservedFinalState string `json:"observedFinalState"`
			} `json:"resources"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(cleanup, &cleaned); err != nil {
		t.Fatal(err)
	}
	if len(cleaned.Phases) == 0 {
		t.Fatal("cleanup.json has no teardown phases")
	}
	for _, phase := range cleaned.Phases {
		for _, resource := range phase.Resources {
			if resource.ObservedFinalState != "absent" {
				t.Fatalf("cleanup not proven absent: %s", resource.Name)
			}
		}
	}
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
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatal("repository root not found")
		}
		d = parent
	}
}

func ticketEnv(dir string) []string {
	env := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "QUOIN_TICKET=") && !strings.HasPrefix(value, "QUOIN_EVIDENCE_DIR=") {
			env = append(env, value)
		}
	}
	return append(env, "QUOIN_TICKET=T25", "QUOIN_EVIDENCE_DIR="+dir)
}

func output(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	body, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func sourceDigest(t *testing.T, repo string) string {
	t.Helper()
	patch := output(t, repo, "git", "diff", "--binary", "HEAD")
	untracked := strings.Fields(output(t, repo, "git", "ls-files", "--others", "--exclude-standard"))
	sort.Strings(untracked)
	hash := sha256.New()
	_, _ = hash.Write([]byte("patch\x00" + patch))
	for _, path := range untracked {
		body, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = hash.Write([]byte(path + "\x00"))
		_, _ = hash.Write(body)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
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
	return versions
}

func runOutput(repo, command string, args ...string) string {
	cmd := exec.Command(command, args...)
	cmd.Dir = repo
	body, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(body)
}

func evidenceDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "runtime-evidence.json" {
			return err
		}
		body, err := os.ReadFile(path)
		if err == nil {
			relative, _ := filepath.Rel(dir, path)
			sum := sha256.Sum256(body)
			got[relative] = hex.EncodeToString(sum[:])
		}
		return err
	})
	return got
}
