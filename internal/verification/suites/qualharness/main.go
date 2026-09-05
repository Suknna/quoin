// Command qualharness is the deterministic executor behind the
// ci/verify-model-provider-fixture, ci/verify-security and
// ci/verify-migrations release-qualification entrypoints (the catalog's
// process-harness scenarios). Like the contract-gate harness it runs
// real work as real subprocesses, records per-leg outcomes in
// $QUOIN_VERIFY_WORKDIR and projects the catalog's cell assertions
// into machine facts; it never derives a verdict — the coordinator and
// the frozen profile own verdicts (VERIFY-VERDICT-004).
//
// Phase contract (invoked through the catalog phase commands):
//
//	--phase setup   go vet the union of every leg's packages
//	--phase action  run each leg's real work, persist results in $QUOIN_VERIFY_WORKDIR
//	--phase assert  re-verify the recorded results and write $QUOIN_VERIFY_FACTS
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/verification/fixtures"
	"github.com/Suknna/quoin/internal/verification/suites"
)

// leg is one named unit of real work; a scenario's assertion set is
// proven when every planned leg ran and passed.
type leg struct {
	Name string
	Pkg  string // owning go package(s) for setup vetting (optional)
	Run  string // optional -run regex
	// work executes the leg's real action when it is not a go-test
	// group; nil means the leg is a plain go-test group.
	work func(workdir string) (string, error)
}

type harnessPlan struct {
	Name  string
	Legs  []leg
	Facts map[string]any // executor-reported actuals per catalog assertion id
}

type legResult struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Args       []string `json:"args,omitempty"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Log        string   `json:"log"`
	Detail     string   `json:"detail,omitempty"`
}

type actionResults struct {
	Harness string      `json:"harness"`
	Started string      `json:"started"`
	Legs    []legResult `json:"legs"`
}

func main() {
	if len(os.Args) < 4 || os.Args[2] != "--phase" {
		fatal(errors.New("usage: qualharness <model-provider-fixture|security|migrations> --phase <setup|action|assert>"))
	}
	name, phase := os.Args[1], os.Args[3]
	plan, known := plans[name]
	if !known {
		fatal(fmt.Errorf("unknown harness %q", name))
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		fatal(err)
	}
	switch phase {
	case "setup":
		if err := runSetup(root, plan); err != nil {
			fatal(err)
		}
	case "action":
		if err := runAction(root, name, plan); err != nil {
			fatal(err)
		}
	case "assert":
		if err := runAssert(plan); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown phase %q", phase))
	}
}

// plans is the frozen execution table of the process-harness release
// scenarios. The security table maps each named adversarial suite to
// the packages that own its corpus; the migrations table pairs the
// product's migration gate tests with the frozen-schema sqlite_master
// comparison.
var plans = map[string]harnessPlan{
	"model-provider-fixture": {
		Name: "integration.model-provider-fixture",
		Legs: []leg{
			{Name: "fixture-blackbox-contract", Pkg: "./test/fixtures/model-provider ./internal/verification/fixtures",
				work: providerFixtureLeg},
		},
	},
	"security": {
		Name: "security.adversarial",
		Legs: []leg{
			{Name: "authentication-adversarial", Pkg: "./internal/quoin/auth"},
			{Name: "root-key-adversarial", Pkg: "./internal/quoin/bootstrap"},
			{Name: "reveal-adversarial", Pkg: "./internal/quoin/secrets"},
			{Name: "download-adversarial", Pkg: "./internal/quoin/artifact"},
			{Name: "sentinel-leak-adversarial", Pkg: "./internal/quoin/app ./internal/quoin/inspection"},
			{Name: "redaction-profile", Pkg: "./internal/verification/evidence"},
		},
	},
	"migrations": {
		Name: "deployment.migrations",
		Legs: []leg{
			{Name: "migration-gate-corpus", Pkg: "./internal/quoin/upgrade"},
			{Name: "frozen-schema-sqlite-master", Pkg: "./internal/verification/suites/qualharness",
				work: func(workdir string) (string, error) {
					detail, err := compareFrozenSchemaSQLiteMaster()
					if err != nil {
						return "", err
					}
					return detail, nil
				}},
		},
	},
}

func runSetup(root string, plan harnessPlan) error {
	packages := map[string]bool{}
	for _, l := range plan.Legs {
		for _, pkg := range strings.Fields(l.Pkg) {
			packages[pkg] = true
		}
	}
	ordered := make([]string, 0, len(packages))
	for pkg := range packages {
		ordered = append(ordered, pkg)
	}
	sort.Strings(ordered)
	command := exec.Command("go", append([]string{"vet"}, ordered...)...)
	command.Dir = root
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	fmt.Printf("qualharness setup: go vet %s\n", strings.Join(ordered, " "))
	return command.Run()
}

func runAction(root, harnessName string, plan harnessPlan) error {
	workdir := os.Getenv("QUOIN_VERIFY_WORKDIR")
	if workdir == "" {
		return errors.New("QUOIN_VERIFY_WORKDIR not set")
	}
	logs := filepath.Join(workdir, "harness-logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return err
	}
	results := actionResults{Harness: harnessName, Started: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, l := range plan.Legs {
		started := time.Now()
		var result legResult
		var err error
		if l.work != nil {
			detail, err := l.work(workdir)
			if err != nil {
				detail = detail + "\nerror: " + err.Error()
			}
			result = legResult{Name: l.Name, Kind: "work", ExitCode: exitOf(err), DurationMS: time.Since(started).Milliseconds(), Log: filepath.Join("harness-logs", l.Name+".log"), Detail: detail}
			_ = os.WriteFile(filepath.Join(logs, l.Name+".log"), []byte(detail), 0o644)
		} else {
			args := []string{"test", "-count=1", "-timeout=25m"}
			if l.Run != "" {
				args = append(args, "-run", l.Run)
			}
			args = append(args, strings.Fields(l.Pkg)...)
			command := exec.Command("go", args...)
			command.Dir = root
			body, err := command.CombinedOutput()
			result = legResult{Name: l.Name, Kind: "go-test", ExitCode: exitOf(err), DurationMS: time.Since(started).Milliseconds(), Log: filepath.Join("harness-logs", l.Name+".log")}
			_ = os.WriteFile(filepath.Join(logs, l.Name+".log"), body, 0o644)
			fmt.Printf("qualharness action %-32s exit=%d %dms\n", l.Name, result.ExitCode, result.DurationMS)
		}
		results.Legs = append(results.Legs, result)
		if err != nil {
			fmt.Printf("qualharness action %s failed; continuing without fail-fast\n", l.Name)
		}
	}
	body, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workdir, "harness-results.json"), body, 0o644)
}

func runAssert(plan harnessPlan) error {
	workdir := os.Getenv("QUOIN_VERIFY_WORKDIR")
	body, err := os.ReadFile(filepath.Join(workdir, "harness-results.json"))
	if err != nil {
		return fmt.Errorf("action results missing: %w", err)
	}
	var results actionResults
	if err := json.Unmarshal(body, &results); err != nil {
		return err
	}
	passed := map[string]bool{}
	allPassed := true
	for _, result := range results.Legs {
		passed[result.Name] = result.ExitCode == 0
		if result.ExitCode != 0 {
			allPassed = false
			fmt.Printf("qualharness assert FAILED leg %s (exit %d)\n", result.Name, result.ExitCode)
		}
	}
	for _, l := range plan.Legs {
		if _, ran := passed[l.Name]; !ran {
			allPassed = false
			fmt.Printf("qualharness assert MISSING leg %s\n", l.Name)
		}
	}
	checks := make([]map[string]string, 0, len(passed))
	names := make([]string, 0, len(passed))
	for name := range passed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		outcome := "failed"
		if passed[name] {
			outcome = "passed"
		}
		checks = append(checks, map[string]string{"name": name, "result": outcome})
	}
	if err := suites.WriteFacts(os.Getenv("QUOIN_VERIFY_FACTS"), map[string]any{}, checks); err != nil {
		return err
	}
	if !allPassed {
		return errors.New("qualharness assert: failing legs present")
	}
	fmt.Printf("qualharness assert: all %d legs passed\n", len(results.Legs))
	return nil
}

// providerFixtureLeg builds and runs the real deterministic fixture
// process with a cancellable completion delay and drives the frozen
// black-box contract through the shared probe.
func providerFixtureLeg(workdir string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	_ = listener.Close()
	binary := filepath.Join(workdir, "model-provider-fixture")
	build := exec.Command("go", "build", "-o", binary, "./test/fixtures/model-provider")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("fixture build: %v: %s", err, output)
	}
	fixture := exec.Command(binary, "-address", address, "-completion-delay", "3s")
	fixture.Dir = root
	if err := fixture.Start(); err != nil {
		return "", err
	}
	defer func() {
		_ = fixture.Process.Kill()
		_, _ = fixture.Process.Wait()
	}()
	base := "http://" + address
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if probe, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			probe.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	legs := fixtures.ProbeProviderFixture(defaultContext(), base)
	failed := []string{}
	detail := &strings.Builder{}
	for _, l := range legs {
		fmt.Fprintf(detail, "%s: passed=%t %s\n", l.Name, l.Passed, l.Detail)
		if !l.Passed {
			failed = append(failed, l.Name)
		}
	}
	_ = os.WriteFile(filepath.Join(workdir, "provider-fixture-legs.json"), []byte(toJSON(legs)), 0o644)
	if len(failed) != 0 {
		return detail.String(), fmt.Errorf("provider fixture legs failed: %s", strings.Join(failed, ","))
	}
	return detail.String(), nil
}

func exitOf(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root not found")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "qualharness:", err)
	os.Exit(1)
}

func toJSON(value any) string {
	body, _ := json.MarshalIndent(value, "", "  ")
	return string(body)
}
