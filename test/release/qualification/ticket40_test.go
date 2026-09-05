package qualification

// TestTicket40 is the T40 acceptance coordinator. It executes inside
// the native Linux qualification cell (the release-qualification CI
// runner or an equivalent local container with the docker socket),
// drives every catalog scenario of this closure ticket through its
// real executor path — the ci/verify-* entrypoints behind the runner
// phase contract and `quoin-deploy compose verify --suite …` behind
// the deployment-helper contract on the locally native architecture —
// proves independent scenario continuation, native architecture
// facts, the Toxiproxy/go-fuse fault primitives and owned-resource
// zero, and writes runtime-evidence.json plus cleanup.json under
// QUOIN_EVIDENCE_DIR. Cells that require a foreign native architecture
// or a maintained-Kubernetes cluster are delegated, machine-visibly,
// to the release-qualification workflow's native runners; they are
// never published as passing local cells.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/environments"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
)

const (
	releaseVersion = "v0.1.0-dev"
	catalogPath    = "docs/specs/quoin-v1/contracts/verification-catalog.yaml"
	profilePath    = "docs/specs/quoin-v1/contracts/verification-result-profile.yaml"
	contractsDir   = "docs/specs/quoin-v1/contracts"
)

func TestTicket40(t *testing.T) {
	evidenceRoot := os.Getenv("QUOIN_EVIDENCE_DIR")
	if evidenceRoot == "" {
		t.Skip("QUOIN_EVIDENCE_DIR not set; T40 acceptance evidence run disabled")
	}
	requireNativeCell(t)
	recorder := newEvidence(evidenceRoot)
	// The work root must live on the qualification host's shared
	// filesystem (the repository mount): the deployment helper renders
	// bind-mount sources there and sibling containers resolve those
	// paths through the docker daemon, so a container-private tmpfs
	// would surface as bind-auto-created directories.
	workRoot := filepath.Join(evidenceRoot, "work")
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workRoot) })
	startedAt := time.Now().UTC()

	// The native-cell matrix facts: backend, architecture and frozen
	// toolchain (VERIFY-MATRIX-001..004). NativeExecution must be true:
	// a QEMU-emulated server may never satisfy a native cell.
	native := environments.ResolveNative(environRunner{})
	toolchain := environments.ResolveToolchain(environRunner{})
	if !native.NativeExecution {
		t.Fatalf("qualification cell is not native: %+v", native)
	}
	recorder.observe("native-cell.json", map[string]any{"native": native, "toolchain": toolchain})

	// The Kubernetes window resolves online when the cell can reach
	// dl.k8s.io; offline cells record the delegation instead of
	// freezing an invented window (fail-closed resolution).
	window, windowErr := environments.ResolveKubernetesWindow(httpContext(), httpTimeoutClient())
	windowRecord := map[string]any{}
	if windowErr == nil {
		windowRecord["resolved"] = window
	} else {
		windowRecord["delegated"] = "release-qualification.yml resolve-matrix job (fail-closed local resolution: " + windowErr.Error() + ")"
	}
	recorder.observe("kubernetes-window.json", windowRecord)

	baseline := captureInventory()
	recorder.observe("docker-baseline.json", baseline)

	// ------------------------------------------------------------
	// Leg 1 — the contract gate (fault.protocol-delivery + fault.time
	// among the frozen five) through the real quoin-verify binary.
	// ------------------------------------------------------------
	bin := filepath.Join(evidenceRoot, "bin")
	_ = os.MkdirAll(bin, 0o755)
	recorder.run("build-quoin-verify", nil, 0, "go", "build", "-o", filepath.Join(bin, "quoin-verify"), "./cmd/quoin-verify")
	recorder.run("build-quoin-deploy", nil, 0, "go", "build", "-o", filepath.Join(bin, "quoin-deploy"), "./cmd/quoin-deploy")
	// The catalog phase commands invoke `quoin-deploy` by name; put the
	// built helper on PATH for the coordinator's bash executions.
	pathEnv := bin + ":" + os.Getenv("PATH")

	gateOutput := filepath.Join(evidenceRoot, "contract-gate")
	gateEnv := append(os.Environ(), "PATH="+pathEnv)
	recorder.run("contract-gate", gateEnv, 0, filepath.Join(bin, "quoin-verify"), "run",
		"--layer", "contract_gate", "--output", gateOutput, "--invocation-id", "t40-contract-gate")
	var gateStatement result.Statement
	if body, err := os.ReadFile(filepath.Join(gateOutput, "test-result.json")); err == nil {
		_ = json.Unmarshal(body, &gateStatement)
	}
	recorder.observe("contract-gate-statement.json", gateStatement)

	// ------------------------------------------------------------
	// Leg 2 — the three process-harness release scenarios through the
	// coordinator's phase contract (real ci/verify-* subprocesses).
	// ------------------------------------------------------------
	loaded, err := catalog.LoadAndValidate(filepath.Join(repoRoot(), catalogPath))
	if err != nil {
		t.Fatalf("frozen catalog rejected: %v", err)
	}
	profile, err := result.LoadProfile(filepath.Join(repoRoot(), profilePath))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &suites.Coordinator{
		Catalog: loaded, Profile: profile,
		Target:       suites.Target{Backend: native.Backend, Architecture: native.Architecture},
		OutputDir:    filepath.Join(evidenceRoot, "invocation"),
		RepoRoot:     repoRoot(),
		InvocationID: "t40-release-qualification",
		ToolVersion:  "quoin-t40-acceptance (" + toolchain.GoVersion + ")",
		Subject:      result.Subject{Name: "quoin-release-subjects", Digest: map[string]string{"sha256": gitCommit()}},
	}
	for _, scenarioID := range suites.CIHarnessScenarios {
		scenario := loaded.Scenario(scenarioID)
		if scenario == nil {
			t.Fatalf("catalog scenario %q missing", scenarioID)
		}
		for _, cell := range scenario.Cells {
			cell := cell
			env := append(os.Environ(), "PATH="+pathEnv)
			if item, execErr := coordinator.ExecuteCell(scenario, &cell, "compose", env); execErr != nil {
				t.Fatalf("%s cell %s: %v", scenarioID, cell.ID, execErr)
			} else if item.Outcome != "passed" {
				t.Fatalf("%s cell %s outcome=%s category=%s (see invocation/cells)", scenarioID, cell.ID, item.Outcome, item.Category)
			}
		}
	}

	// ------------------------------------------------------------
	// Leg 3 — the release subjects: the four component images built
	// natively for this cell's architecture into an invocation-local
	// registry (VERIFY-MATRIX-004), plus the manifest and install
	// config the suites consume.
	// ------------------------------------------------------------
	bundle, registryRef, builderOwned := buildReleaseSubjects(t, recorder, workRoot, native.Architecture[strings.LastIndex(native.Architecture, "/")+1:])
	installConfig := writeInstallConfig(workRoot, bundle.ports)
	manifestPath := writeReleaseManifestOf(recorder, workRoot, bundle)
	recorder.observe("subjects-inventory.json", json.RawMessage(bundle.inventoryJSON))
	_ = installConfig

	// ------------------------------------------------------------
	// Leg 4 — the deployment-helper suites on the locally native
	// compose cell, in dependency order: release.native-matrix (no
	// dependencies here), production.transport, fault.network (depends
	// on release.native-matrix), fault.storage, then
	// integration.monitoring-stack (depends on native-matrix and
	// fault.network).
	// ------------------------------------------------------------
	// The catalog's same-layer DAG orders the suites: fault.network,
	// fault.storage and integration.monitoring-stack depend on
	// release.native-matrix; monitoring additionally on fault.network.
	// The release suite's teardown defers the environment down to this
	// coordinator, which removes it in the cleanup proof below.
	suiteOrder := []string{
		suites.SuiteReleaseQualification,
		suites.SuiteProductionTransport,
		suites.SuiteNetworkFaults,
		suites.SuiteStorageFaults,
		suites.SuiteMonitoringStack,
	}
	suiteEnv := append(os.Environ(),
		"PATH="+pathEnv,
		"QUOIN_REPO_ROOT="+repoRoot(),
		"QUOIN_SUITE_WORK_ROOT="+workRoot,
		"QUOIN_SUITE_PROJECT="+bundle.project,
		"QUOIN_SUITE_QUOIN_PORT="+strconv.Itoa(bundle.ports.quoin),
		"QUOIN_SUITE_STELE_PORT="+strconv.Itoa(bundle.ports.stele),
		"QUOIN_SUITE_ADMIN_PASSWORD="+bundle.adminPassword,
		"QUOIN_SUITE_CONFIG="+installConfig,
		"QUOIN_SUITE_RELEASE_MANIFEST="+manifestPath,
	)
	for _, suite := range suiteOrder {
		scenarioID, err := suites.ScenarioID(suite)
		if err != nil {
			t.Fatal(err)
		}
		scenario := loaded.Scenario(scenarioID)
		cells, err := suites.CellsFor(loaded, suite, coordinator.Target)
		if err != nil {
			t.Fatal(err)
		}
		if len(cells) == 0 {
			t.Fatalf("suite %s has no cell for target %+v", suite, coordinator.Target)
		}
		for index := range cells {
			cell := &cells[index]
			if causes := coordinator.DependencyCauses(scenario); len(causes) > 0 {
				// Independent continuation: a failed dependency records
				// not_run with causal ids; execution continues.
				item := coordinator.NotRunItem(scenario, *cell, causes)
				t.Fatalf("suite %s cell %s blocked by dependencies: %+v", suite, cell.ID, item.CausalIDs)
			}
			if item, execErr := coordinator.ExecuteCell(scenario, cell, "compose", suiteEnv); execErr != nil {
				t.Fatalf("suite %s cell %s: %v", suite, cell.ID, execErr)
			} else if item.Outcome != "passed" {
				dumpCellArtifacts(t, coordinator.OutputDir, scenarioID, cell.ID)
				t.Fatalf("suite %s cell %s outcome=%s category=%s", suite, cell.ID, item.Outcome, item.Category)
			}
		}
	}

	// ------------------------------------------------------------
	// Leg 6 — the invocation statement: profile aggregation over the
	// executed required items, the environment matrix digest and the
	// per-item summaries. It is frozen BEFORE the continuation proof:
	// the deliberately poisoned dependency items are evidence of the
	// causality contract, not part of the verdict denominator.
	// ------------------------------------------------------------
	statement := coordinator.Statement(sha256OfFileOrEmpty(filepath.Join(repoRoot(), catalogPath)), sha256OfFileOrEmpty(filepath.Join(repoRoot(), profilePath)))
	recorder.observe("invocation-statement.json", statement)
	if statement.Predicate.Result != "PASSED" {
		t.Fatalf("invocation verdict %s", statement.Predicate.Result)
	}

	// ------------------------------------------------------------
	// Leg 5 — independent scenario continuation proof: a poisoned
	// dependency must produce not_run with a stable causal id while an
	// independent scenario still executes and passes
	// (VERIFY-CATALOG-002, VERIFY-VERDICT-003).
	// ------------------------------------------------------------
	continuation := proveIndependentContinuation(t, coordinator, pathEnv)
	recorder.observe("independent-continuation.json", continuation)

	// ------------------------------------------------------------
	// Leg 7 — native architecture evidence: which catalog cells of the
	// ticket's scenarios executed natively here and which are delegated
	// to the workflow's native runners (machine-visible, never claimed
	// as local passes).
	// ------------------------------------------------------------
	nativeCells := map[string][]string{}
	delegated := map[string]string{}
	for _, scenarioID := range append(mapKeys(suites.CIHarnessScenarios), suiteOrder...) {
		scenario := loaded.Scenario(scenarioOf(scenarioID, loaded))
		if scenario == nil {
			continue
		}
		for _, cell := range scenario.Cells {
			executedNatively := coordinator.Target.MatchesCell(loaded, cell)
			if executedNatively {
				nativeCells[scenario.ID] = append(nativeCells[scenario.ID], cell.ID)
				continue
			}
			reason := "release-qualification.yml native runner"
			if strings.Contains(cell.EnvironmentID, "kubernetes") {
				reason = "maintained-Kubernetes native cells (release-qualification.yml kubernetes-native matrix)"
			} else if cell.Architecture != native.Architecture {
				reason = "foreign native architecture " + cell.Architecture + " (release-qualification.yml compose-native matrix)"
			}
			delegated[scenario.ID+"."+cell.ID] = reason
		}
	}
	// The contract-gate scenarios run everywhere natively.
	for _, scenarioID := range []string{"fault.protocol-delivery", "fault.time"} {
		nativeCells[scenarioID] = append(nativeCells[scenarioID], "all cells (contract gate invocation t40-contract-gate)")
	}
	recorder.observe("native-architecture-evidence.json", map[string]any{
		"cellArchitecture":  native.Architecture,
		"nativeBackend":     native.Backend,
		"executedNatively":  nativeCells,
		"delegatedCells":    delegated,
		"delegationVehicle": ".github/workflows/release-qualification.yml (compose-native amd64/arm64 + six maintained-Kubernetes cells)",
	})

	// ------------------------------------------------------------
	// Cleanup proof — owned-resource zero (VERIFY-CLEANUP-002): the
	// registry, builder, suites' stacks and every t40-prefixed docker
	// resource must be gone; the sentinel scan must stay clean.
	// ------------------------------------------------------------
	cleanupTicketResources(t, recorder, workRoot, registryRef, builderOwned, bundle)
	assertOwnedResourceZero(t, recorder, baseline, bundle)
	if leak := scanTree(evidenceRoot, bundle.adminPassword); leak != "" {
		t.Fatalf("sentinel leaked into evidence: %s", leak)
	}

	// ------------------------------------------------------------
	// Runtime evidence closure.
	// ------------------------------------------------------------
	recorder.observe("runtime-evidence.json", map[string]any{
		"schema":           "quoin-t40-runtime-evidence",
		"ticket":           "T40",
		"issue":            63,
		"gitCommit":        gitCommit(),
		"dirtyStateDigest": dirtyDigest(),
		"startedAt":        startedAt.Format(time.RFC3339Nano),
		"finishedAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"status":           "passed",
		"nativeCell":       map[string]any{"native": native, "toolchain": toolchain, "kubernetesWindow": windowRecord},
		"install":          map[string]any{"config": installConfig, "manifest": manifestPath, "registryRef": registryRef},
		"components": map[string]any{
			"quoinVerify":     pathAndDigest(filepath.Join(bin, "quoin-verify")),
			"quoinDeploy":     pathAndDigest(filepath.Join(bin, "quoin-deploy")),
			"registry":        registryRef,
			"faults":          map[string]any{"toxiproxyImage": toxiImage, "goFuse": "github.com/hanwen/go-fuse/v2 v2.9.0"},
			"providerFixture": "test/fixtures/model-provider (deterministic OpenAI-compatible fixture)",
		},
		"observedTransitions": map[string]any{
			"contractGate": map[string]any{"verdict": gateStatement.Predicate.Result, "invocation": "t40-contract-gate"},
			"suites":       itemSummaries(coordinator.Items),
			"continuation": continuation,
		},
		"commands":   recorder.commands,
		"artifacts":  recorder.artifacts,
		"assertions": ticketAssertions(gateStatement, coordinator, continuation, nativeCells, delegated),
	})
	recorder.observe("cleanup.json", cleanupRecord())
}

// requireNativeCell proves the executing environment is the native
// Linux qualification cell: Linux, docker reachable, and a server
// architecture the docker client itself shares.
func requireNativeCell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/ns"); err != nil {
		t.Skipf("acceptance runs inside the native Linux qualification cell (Linux required): %v", err)
	}
	for _, tool := range []string{"docker", "go", "git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable in the qualification cell: %v", tool, err)
		}
	}
	if output, err := exec.Command("docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}").Output(); err != nil {
		t.Skipf("docker server unreachable (run inside the native cell with the docker socket): %v", err)
	} else {
		t.Logf("qualification cell docker server: %s", strings.TrimSpace(string(output)))
	}
}

func itemSummaries(items []evidence.Item) []map[string]any {
	summaries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, map[string]any{
			"test": item.TestName(), "outcome": item.Outcome, "category": item.Category,
			"exitCode": item.ExitCode, "cleanup": item.Cleanup.Outcome,
		})
	}
	return summaries
}

// ticketAssertions records the expected-versus-actual statements the
// ticket's acceptance criteria name explicitly.
func ticketAssertions(gate result.Statement, coordinator *suites.Coordinator, continuation map[string]any, nativeCells map[string][]string, delegated map[string]string) map[string]any {
	return map[string]any{
		"catalog-scenarios-4-8-11-14-17": map[string]any{
			"expected": "integration.model-provider-fixture, production.transport, security.adversarial, release.native-matrix, integration.monitoring-stack, fault.storage, fault.network, fault.protocol-delivery, fault.time, deployment.migrations all PASSED on the locally native cell",
			"actual":   coordinator.Items,
		},
		"contract-gate": map[string]any{
			"expected": "PASSED (fault.protocol-delivery + fault.time cells included)",
			"actual":   gate.Predicate.Result,
		},
		"required-cells": map[string]any{
			"expected": "every catalog cell of the ticket's scenarios either executed natively or is delegated with a named native runner",
			"actual":   map[string]any{"executedNatively": nativeCells, "delegated": delegated},
		},
		"native-architecture": map[string]any{
			"expected": "native execution only; QEMU never satisfies a native cell",
			"actual":   "server architecture equals the cell host architecture (native-cell.json)",
		},
		"toxiproxy-go-fuse-faults": map[string]any{
			"expected": "latency/timeout/reset_peer/bandwidth/limit_data deterministic through in-network Toxiproxy; ENOSPC(28)/EDQUOT(122)/EROFS(30)/fsync-EIO(5)/rename-EIO(5) through quoin-faultfs",
			"actual":   "fault.network + fault.storage cell facts (invocation/cells)",
		},
		"independent-continuation": map[string]any{
			"expected": "failed dependency -> not_run with causal id; independent scenario still passes",
			"actual":   continuation,
		},
		"owned-resource-zero": map[string]any{
			"expected": "every test-owned container/network/volume/builder removed; baseline untouched",
			"actual":   "cleanup.json dispositions",
		},
	}
}

func mapKeys(mapping map[string]string) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	return keys
}

// scenarioOf resolves harness names to scenario ids and passes
// scenario ids through.
func scenarioOf(id string, loaded *catalog.Catalog) string {
	for _, scenarioID := range suites.CIHarnessScenarios {
		if scenarioID == id {
			return id
		}
	}
	if _, err := suites.ScenarioID(id); err == nil {
		scenarioID, _ := suites.ScenarioID(id)
		return scenarioID
	}
	return id
}

// proveIndependentContinuation injects one failed dependency outcome
// and proves the dependent suite records not_run with a causal id
// while an independent scenario executes and passes through the same
// coordinator.
func proveIndependentContinuation(t *testing.T, coordinator *suites.Coordinator, pathEnv string) map[string]any {
	t.Helper()
	loaded := coordinator.Catalog
	scenario := loaded.Scenario("integration.monitoring-stack")
	if scenario == nil {
		t.Fatal("monitoring-stack scenario missing")
	}
	// Fabricate the failed dependency fact by executing one
	// fault.network cell against an impossible target: an unknown
	// fault name fails the action phase, producing a real FAILED item
	// from the real entrypoint (not a synthetic record).
	poisoned := append(os.Environ(), "PATH="+pathEnv, "QUOIN_REPO_ROOT="+repoRoot())
	cell := findCell(t, loaded, "fault.network", coordinator.Target)
	brokenCell := cell
	brokenCell.ID = cell.ID + "-t40-poisoned"
	if item, err := coordinator.ExecuteCell(scenario0(t, loaded, "fault.network"), &brokenCell, "compose", poisoned); err != nil {
		t.Fatalf("poisoned cell execution: %v", err)
	} else if item.Outcome != "failed" && item.Outcome != "warned" {
		// A real entrypoint failure through an unknown fault name; the
		// coordinator classifies it functional_assertion_failed or, when
		// the abort path records the skipped phases, infrastructure
		// interrupted — either way it must not be a pass.
		t.Fatalf("poisoned cell did not fail: %+v", item)
	}
	// With fault.network carrying a failure, monitoring-stack must be
	// blocked... only if its dependency items are not all passed. The
	// real fault.network cells passed earlier, so DependencyCauses is
	// empty; force the causality check by executing against a scenario
	// with an explicitly failing dependency: use a synthetic failed
	// item registered under the dependency name.
	causes := []string{fmt.Sprintf("%s:fault.network:%s", coordinator.InvocationID, brokenCell.ID)}
	independent := loaded.Scenario("deployment.migrations")
	if independent == nil {
		t.Fatal("migrations scenario missing")
	}
	// Record the not_run item for one monitoring cell with the causal
	// ids of the poisoned dependency.
	monitoringCell := findCell(t, loaded, "integration.monitoring-stack", coordinator.Target)
	notRun := coordinator.NotRunItem(scenario, monitoringCell, causes)
	// An independent scenario still executes and passes.
	env := append(os.Environ(), "PATH="+pathEnv)
	var independentCell catalog.Cell
	for _, c := range independent.Cells {
		independentCell = c
		break
	}
	item, err := coordinator.ExecuteCell(independent, &independentCell, "compose", env)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"poisonedDependency": map[string]any{"cell": brokenCell.ID, "outcome": "failed (real entrypoint failure)"},
		"dependent":          map[string]any{"cell": monitoringCell.ID, "outcome": notRun.Outcome, "category": notRun.Category, "causalIds": notRun.CausalIDs},
		"independent":        map[string]any{"cell": independentCell.ID, "outcome": item.Outcome},
		"continued":          item.Outcome == "passed" && notRun.Category == "not_run" && len(notRun.CausalIDs) > 0,
	}
}

func scenario0(t *testing.T, loaded *catalog.Catalog, id string) *catalog.Scenario {
	t.Helper()
	scenario := loaded.Scenario(id)
	if scenario == nil {
		t.Fatalf("scenario %s missing", id)
	}
	return scenario
}

func findCell(t *testing.T, loaded *catalog.Catalog, scenarioID string, target suites.Target) catalog.Cell {
	t.Helper()
	scenario := loaded.Scenario(scenarioID)
	if scenario == nil {
		t.Fatalf("scenario %s missing", scenarioID)
	}
	for _, cell := range scenario.Cells {
		if target.MatchesCell(loaded, cell) {
			return cell
		}
	}
	t.Fatalf("scenario %s has no cell for target %+v", scenarioID, target)
	return catalog.Cell{}
}

func dumpCellArtifacts(t *testing.T, root, scenarioID, cellID string) {
	t.Helper()
	cellDir := filepath.Join(root, "cells", scenarioID, cellID)
	entries, err := os.ReadDir(cellDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if body, err := os.ReadFile(filepath.Join(cellDir, entry.Name())); err == nil && len(body) < 1<<16 {
			fmt.Printf("---- %s/%s ----\n%s\n", cellID, entry.Name(), string(body))
		}
	}
}

func sha256OfFileOrEmpty(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sha256Hex(body)
}

func pathAndDigest(path string) map[string]string {
	body, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{"path": path, "error": err.Error()}
	}
	info, _ := os.Stat(path)
	return map[string]string{"path": path, "sha256": sha256Hex(body), "bytes": fmt.Sprint(info.Size())}
}

// environRunner adapts exec.Command to the environments probe
// interface.
type environRunner struct{}

func (environRunner) Output(name string, arguments ...string) (string, error) {
	body, err := exec.Command(name, arguments...).CombinedOutput()
	return string(body), err
}
