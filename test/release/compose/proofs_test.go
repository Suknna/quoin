package release_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type helperReport struct {
	Command  string `json:"command"`
	Release  string `json:"release"`
	Backend  string `json:"backend"`
	ExitCode int    `json:"exitCode"`
	Stages   []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"stages"`
	Checks []struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	} `json:"checks"`
	Failure *struct {
		Code       string `json:"code"`
		NextAction string `json:"nextAction"`
	} `json:"failure"`
}

func loadReport(t *testing.T, path string) *helperReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("report %s missing: %v", path, err)
	}
	var report helperReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report %s is not structured JSON: %v", path, err)
	}
	return &report
}

func assertInstallReport(t *testing.T, recorder *evidence, path string, images map[string]*releaseImages) {
	t.Helper()
	report := loadReport(t, path)
	if report.ExitCode != 0 || report.Failure != nil {
		t.Fatalf("install report must be a success envelope: %+v", report)
	}
	stages := map[string]string{}
	for _, stage := range report.Stages {
		stages[stage.Name] = stage.Status
	}
	for _, stage := range []string{"preflight", "secret-bootstrap", "admin-bootstrap", "workloads", "verify"} {
		if stages[stage] != "completed" {
			t.Fatalf("install stage %s is %q, want completed", stage, stages[stage])
		}
	}
	checks := map[string]bool{}
	for _, check := range report.Checks {
		checks[check.ID] = check.Result == "passed"
	}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		for _, prefix := range []string{"readiness-", "metrics-", "logs-", "image-digest-"} {
			if !checks[prefix+component] {
				t.Fatalf("install report missing passed check %s%s", prefix, component)
			}
		}
	}
	if !checks["topology"] {
		t.Fatal("install report missing passed topology check")
	}
	if report.Release != "v0.1.0-dev" {
		t.Fatalf("report release %q does not match the manifest", report.Release)
	}
	recorder.note("install-report.json", mustJSON(t, report))
}

// assertPinnedContainers independently proves the running containers use the
// digest-pinned references, without trusting the helper's own verdict.
func assertPinnedContainers(t *testing.T, recorder *evidence, composeFile string, images map[string]*releaseImages) {
	t.Helper()
	observed := map[string]string{}
	for _, component := range []string{"quoin", "plinth", "lintel", "stele"} {
		output := recorder.output(append(composeFileArguments(composeFile), "ps", "-aq", component)...)
		container := strings.Fields(strings.TrimSpace(output))[0]
		reference := strings.TrimSpace(recorder.output("docker", "inspect", container, "--format", "{{.Config.Image}}"))
		pinned := images[component].Repository + "@" + images[component].Index
		if reference != pinned {
			t.Fatalf("%s container runs %q, want pinned %q", component, reference, pinned)
		}
		observed[component] = reference
	}
	recorder.observe("container-image-references.json", observed)
}

// proveProcessLocks shows the fixed state-directory flock arbitration
// (OPS-TOPOLOGY-002): a second process against the live quoin-data and
// plinth-state fails with the stable already-owned error instead of starting.
func proveProcessLocks(t *testing.T, recorder *evidence, composeFile string) {
	t.Helper()
	observations := map[string]map[string]any{}
	// OPS-TOPOLOGY-002: Quoin, Plinth and Lintel each own a fixed state
	// lock; Stele mounts no persistent state.
	for _, component := range []string{"quoin", "plinth", "lintel"} {
		output := recorder.run("lock-contest-"+component, nil, nil, -1, append(composeFileArguments(composeFile), "run", "--rm", "--no-deps", "-T", component)...)
		code := lastExitCode(t, recorder, "lock-contest-"+component)
		if !strings.Contains(output, "state directory is already owned") {
			t.Fatalf("%s lock contest did not surface the stable ownership error:\n%s", component, output)
		}
		observations[component] = map[string]any{"exitCode": code, "stableError": "state directory is already owned"}
	}
	recorder.observe("process-locks.json", observations)
}

func lastExitCode(t *testing.T, recorder *evidence, name string) int {
	t.Helper()
	for index := len(recorder.commands) - 1; index >= 0; index-- {
		if recorder.commands[index].Name == name {
			return recorder.commands[index].ExitCode
		}
	}
	t.Fatalf("command %s not recorded", name)
	return -1
}
