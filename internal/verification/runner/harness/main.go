// Command harness is the deterministic executor behind the ci/verify-*
// contract-gate entrypoints. Each entrypoint maps to a frozen table of real
// `go test` groups selected per catalog cell; the harness runs them as real
// subprocesses, records per-group outcomes, and projects the catalog's cell
// assertions into machine facts for the runner to compare. It never derives
// a verdict: the runner owns verdicts (VERIFY-VERDICT-004).
//
// Phase contract (invoked by the runner through the ci/verify-* scripts):
//
//	--phase setup   go vet the union of every group's packages
//	--phase action  run each group's go test set, persist results in $QUOIN_VERIFY_WORKDIR
//	--phase assert  re-verify the recorded results and write $QUOIN_VERIFY_FACTS
//
// DATA-VALIDATION-008 (deployment-acceptance manifest adversarial) is owned
// by the Deployment Acceptance closure ticket; its group joins the
// interleavings table when that implementation lands.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const factsSchemaKind = "quoin-verify-facts-v1"

type group struct {
	Name string
	Pkg  string
	Run  string // optional -run regex
}

type cellPlan struct {
	// Assertions maps each catalog assertion id to the group names that
	// prove it; the fact's actual is the catalog vocabulary string when all
	// of its groups passed and a failure marker otherwise.
	Assertions map[string][]string
	Groups     []group
}

type plan struct {
	Name  string
	Cells map[string]cellPlan // catalog cell id -> plan
}

// plans is the frozen execution table of the contract-gate layer.
var plans = map[string]plan{
	"interleavings": {
		Name: "protocol.interleavings",
		Cells: map[string]cellPlan{
			"default": {
				Groups: []group{
					{Name: "plinth-adversarial", Pkg: "./internal/plinth/..."},
					{Name: "http-matrix", Pkg: "./internal/quoin/app ./test/contract/contracts"},
					{Name: "dispatch-closure", Pkg: "./internal/quoin/analysis ./internal/quoin/attempt ./internal/quoin/connections"},
					{Name: "persistence-adversarial", Pkg: "./internal/quoin/alerts ./internal/quoin/feedback ./internal/quoin/investigation ./internal/quoin/labelcontract ./internal/quoin/knowledge ./internal/quoin/artifact"},
					{Name: "security-adversarial", Pkg: "./internal/quoin/auth ./internal/quoin/maintenance ./internal/quoin/recovery"},
					{Name: "runtime-protocol", Pkg: "./internal/quoin/runtime ./internal/quoin/browser"},
				},
			},
		},
	},
	"config-pipeline": {
		Name: "config.pipeline",
		Cells: map[string]cellPlan{
			"default": {
				Groups: []group{
					{Name: "strict-yaml", Pkg: "./internal/quoin/config ./internal/quoin/labelcontract"},
					{Name: "promql-and-projection", Pkg: "./internal/quoin/businesssystem"},
					{Name: "config-verification-runs", Pkg: "./internal/quoin/inspection"},
				},
			},
		},
	},
	"protocol-delivery-faults": {
		Name: "fault.protocol-delivery",
		Cells: map[string]cellPlan{
			"reorder": {
				Groups: []group{
					{Name: "cancel-result-order", Pkg: "./internal/quoin/attempt", Run: "^TestCancelFenceCommitOrder$|^TestInterruptConvergesActiveAndRespectsCancelFence$|^TestBeginModelCallLostAckRetryAliasesRunningPredecessor$"},
					{Name: "stream-fence-order", Pkg: "./internal/quoin/runtime", Run: "^TestReplacementRetiresAndFences$|^TestWithCurrentRejectsSupersededStream$|^TestWithCurrentClosingFencesSupersededOwner$"},
					{Name: "delivery-barrier-interleavings", Pkg: "./internal/quoin/alerts", Run: "^TestResolvedDeliveryBarrierInterleavings$|^TestConcurrentDistinctOccurrencesGrowSeqMonotonically$"},
				},
				Assertions: map[string][]string{
					"fault-deterministic":       {"cancel-result-order", "delivery-barrier-interleavings"},
					"idempotent-reconciliation": {"cancel-result-order", "delivery-barrier-interleavings"},
					"no-false-terminal-success": {"stream-fence-order"},
				},
			},
			"duplicate": {
				Groups: []group{
					{Name: "relay-replay-idempotency", Pkg: "./internal/quoin/alerts", Run: "^TestDeliverRelayReplayIsIdempotent$|^TestResolvedClosesOccurrenceWithoutReopen$|^TestResolvedFirstCreatesClosedOccurrence$"},
					{Name: "command-replay-over-http", Pkg: "./internal/quoin/app", Run: "^TestAdminUserCommandReplayOverHTTP$|^TestSSEFramingReplayAndCursorMatrix$|^TestBrowserExplorationRejectsConflictingActionResultReplay$"},
				},
				Assertions: map[string][]string{
					"fault-deterministic":       {"relay-replay-idempotency"},
					"idempotent-reconciliation": {"relay-replay-idempotency", "command-replay-over-http"},
					"no-false-terminal-success": {"relay-replay-idempotency", "command-replay-over-http"},
				},
			},
		},
	},
	"time-boundaries": {
		Name: "fault.time",
		Cells: map[string]cellPlan{
			"session-idle-expiry": {
				Groups: []group{
					{Name: "session-idle-envelope", Pkg: "./test/contract/verification", Run: "^TestBoundarySessionIdleExpiry$"},
					{Name: "auth-session-core", Pkg: "./internal/quoin/auth"},
				},
				Assertions: map[string][]string{
					"before-boundary": {"session-idle-envelope", "auth-session-core"},
					"at-boundary":     {"session-idle-envelope"},
					"after-boundary":  {"session-idle-envelope", "auth-session-core"},
				},
			},
			"attempt-lease-expiry": {
				Groups: []group{
					{Name: "lease-sweep", Pkg: "./internal/quoin/attempt", Run: "^TestSweepExpiredInterruptsAndConvergesCancelling$"},
					{Name: "lease-renewal", Pkg: "./internal/quoin/attempt", Run: "^TestRenewLeaseForBootExtendsOnlyLiveLeases$"},
				},
				Assertions: map[string][]string{
					"before-boundary": {"lease-renewal"},
					"at-boundary":     {"lease-sweep", "lease-renewal"},
					"after-boundary":  {"lease-sweep"},
				},
			},
			"reconnect-grace": {
				Groups: []group{
					{Name: "grace-transitions", Pkg: "./internal/quoin/browser", Run: "^TestManualLoginReconnectGraceTransitionsAndExpires$"},
					{Name: "grace-completion", Pkg: "./internal/quoin/browser", Run: "^TestPublishResultCompletesAnAwaitingReconnectManualLogin$"},
				},
				Assertions: map[string][]string{
					"before-boundary": {"grace-transitions"},
					"at-boundary":     {"grace-transitions", "grace-completion"},
					"after-boundary":  {"grace-transitions"},
				},
			},
			"reveal-handle-expiry": {
				Groups: []group{
					{Name: "reveal-ttl-boundary", Pkg: "./internal/quoin/secrets"},
					{Name: "reveal-single-consume", Pkg: "./internal/quoin/app", Run: "^TestAlertSourceRevealLifecycleOverRealServer$"},
				},
				Assertions: map[string][]string{
					"before-boundary": {"reveal-ttl-boundary"},
					"at-boundary":     {"reveal-ttl-boundary"},
					"after-boundary":  {"reveal-ttl-boundary", "reveal-single-consume"},
				},
			},
			"verification-eight-hour-deadline": {
				Groups: []group{
					{Name: "time-closure-arithmetic", Pkg: "./internal/verification/result", Run: "^TestTimeClosureBoundaries$|^TestProfileMapsFrozenCategories$"},
				},
				Assertions: map[string][]string{
					"before-boundary": {"time-closure-arithmetic"},
					"at-boundary":     {"time-closure-arithmetic"},
					"after-boundary":  {"time-closure-arithmetic"},
				},
			},
		},
	},
}

type groupResult struct {
	Name       string   `json:"name"`
	Args       []string `json:"args"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	Log        string   `json:"log"`
}

type actionResults struct {
	Cell    string        `json:"cell"`
	Started string        `json:"started"`
	Groups  []groupResult `json:"groups"`
}

func main() {
	if len(os.Args) < 3 || os.Args[2] != "--phase" || len(os.Args) < 4 {
		fatal(fmt.Errorf("usage: harness <name> --phase <setup|action|assert>"))
	}
	name := os.Args[1]
	phase := os.Args[3]
	selected, ok := plans[name]
	if !ok {
		fatal(fmt.Errorf("unknown harness %q", name))
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	cell := os.Getenv("QUOIN_VERIFY_CELL")
	cellPlans, ok := selected.Cells[cell]
	if !ok {
		fatal(fmt.Errorf("harness %q has no plan for cell %q", name, cell))
	}
	switch phase {
	case "setup":
		if err := runSetup(root, cellPlans); err != nil {
			fatal(err)
		}
	case "action":
		if err := runAction(root, cellPlans); err != nil {
			fatal(err)
		}
	case "assert":
		if err := runAssert(cellPlans); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown phase %q", phase))
	}
}

func runSetup(root string, selected cellPlan) error {
	packages := map[string]bool{}
	for _, grp := range selected.Groups {
		for _, pkg := range strings.Fields(grp.Pkg) {
			packages[pkg] = true
		}
	}
	ordered := make([]string, 0, len(packages))
	for pkg := range packages {
		ordered = append(ordered, pkg)
	}
	sort.Strings(ordered)
	args := append([]string{"vet"}, ordered...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	fmt.Printf("harness setup: go vet %s\n", strings.Join(ordered, " "))
	return cmd.Run()
}

func runAction(root string, selected cellPlan) error {
	workdir := os.Getenv("QUOIN_VERIFY_WORKDIR")
	if workdir == "" {
		return fmt.Errorf("QUOIN_VERIFY_WORKDIR not set")
	}
	logs := filepath.Join(workdir, "harness-logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return err
	}
	results := actionResults{Cell: os.Getenv("QUOIN_VERIFY_CELL"), Started: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, grp := range selected.Groups {
		args := []string{"test", "-count=1", "-timeout=25m"}
		if grp.Run != "" {
			args = append(args, "-run", grp.Run)
		}
		args = append(args, strings.Fields(grp.Pkg)...)
		started := time.Now()
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		body, err := cmd.CombinedOutput()
		result := groupResult{
			Name: grp.Name, Args: args,
			ExitCode: exitOf(err), DurationMS: time.Since(started).Milliseconds(),
			Log: filepath.Join("harness-logs", grp.Name+".log"),
		}
		if writeErr := os.WriteFile(filepath.Join(logs, grp.Name+".log"), body, 0o644); writeErr != nil {
			return writeErr
		}
		fmt.Printf("harness action %-30s exit=%d %dms\n", grp.Name, result.ExitCode, result.DurationMS)
		results.Groups = append(results.Groups, result)
		if err != nil {
			// Record and continue: the assert phase reports the complete
			// picture and the runner keeps executing independent scenarios.
			fmt.Printf("harness action %s failed; continuing without fail-fast\n", grp.Name)
		}
	}
	body, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workdir, "harness-results.json"), body, 0o644)
}

func runAssert(selected cellPlan) error {
	workdir := os.Getenv("QUOIN_VERIFY_WORKDIR")
	resultsPath := filepath.Join(workdir, "harness-results.json")
	body, err := os.ReadFile(resultsPath)
	if err != nil {
		return fmt.Errorf("action results missing: %w", err)
	}
	var results actionResults
	if err := json.Unmarshal(body, &results); err != nil {
		return err
	}
	passed := map[string]bool{}
	allPassed := true
	for _, result := range results.Groups {
		passed[result.Name] = result.ExitCode == 0
		if result.ExitCode != 0 {
			allPassed = false
			fmt.Printf("harness assert FAILED group %s (exit %d)\n", result.Name, result.ExitCode)
		}
	}
	// Every planned group must have actually run.
	for _, grp := range selected.Groups {
		if _, ok := passed[grp.Name]; !ok {
			allPassed = false
			fmt.Printf("harness assert MISSING group %s\n", grp.Name)
		}
	}
	facts := map[string]any{
		"schema_kind": factsSchemaKind,
		"assertions":  map[string]any{},
		"checks":      []any{},
	}
	assertions := facts["assertions"].(map[string]any)
	for assertionID, groupNames := range selected.Assertions {
		actual := expectedVocabulary(assertionID)
		for _, name := range groupNames {
			if !passed[name] {
				actual = "group_failed:" + name
			}
		}
		assertions[assertionID] = map[string]any{"actual": actual}
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
	facts["checks"] = checks
	factsBody, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	factsPath := os.Getenv("QUOIN_VERIFY_FACTS")
	if factsPath == "" {
		return fmt.Errorf("QUOIN_VERIFY_FACTS not set")
	}
	if err := os.WriteFile(factsPath, factsBody, 0o644); err != nil {
		return err
	}
	if !allPassed {
		return fmt.Errorf("harness assert: failing groups present")
	}
	fmt.Printf("harness assert: all %d groups passed\n", len(results.Groups))
	return nil
}

// expectedVocabulary is the closed per-assertion state vocabulary frozen by
// the catalog cells (before/at/after boundary states, boolean fault facts).
func expectedVocabulary(assertionID string) any {
	switch assertionID {
	case "before-boundary":
		return "not_expired"
	case "at-boundary":
		return "specified_transition"
	case "after-boundary":
		return "stable_terminal_or_expired"
	default:
		return true
	}
}

func exitOf(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errorsAs(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func errorsAs(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
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
			return "", fmt.Errorf("repository root not found")
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
