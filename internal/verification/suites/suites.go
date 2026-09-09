// Package suites owns the Release Qualification suite vocabulary and the
// per-cell phase coordinator (T40). The frozen verification catalog is
// the single scenario authority: this package maps suite names to
// scenario IDs, resolves the catalog's `<compose|kubernetes>` phase templates
// against the concrete deployment backend, and executes one cell's
// setup/action/assert/teardown phases through real commands using the
// same environment contract the contract-gate runner uses
// (QUOIN_VERIFY_*). Verdicts stay with the frozen result profile; this
// package never invents outcome classes.
package suites

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Suknna/quoin/internal/verification/catalog"
)

// Suite names as frozen in the catalog executor entrypoints.
const (
	SuiteProductionTransport  = "production-transport"
	SuiteReleaseQualification = "release-qualification"
	SuiteMonitoringStack      = "monitoring-stack"
	SuiteStorageFaults        = "storage-faults"
	SuiteNetworkFaults        = "network-faults"
	// Dedicated acceptance suites are Kubernetes-native adapters for catalog
	// scenarios whose established Compose commands predate the common verifier.
	SuiteRestoreIsolation = "restore-isolation"
	SuiteLintelRecovery   = "lintel-recovery"
)

// table maps each suite name to its owning catalog scenario. The
// scenario ID — not a copy of its assertions — is the authority.
var table = map[string]string{
	SuiteProductionTransport:  "production.transport",
	SuiteReleaseQualification: "release.native-matrix",
	SuiteMonitoringStack:      "integration.monitoring-stack",
	SuiteStorageFaults:        "fault.storage",
	SuiteNetworkFaults:        "fault.network",
	SuiteRestoreIsolation:     "deployment.restore-isolation",
	SuiteLintelRecovery:       "browser.lintel-recovery",
}

// ScenarioID returns the catalog scenario one suite executes.
func ScenarioID(suite string) (string, error) {
	scenarioID, known := table[suite]
	if !known {
		return "", fmt.Errorf("unknown suite %q (closed vocabulary: %s)", suite, strings.Join(SuiteNames(), ", "))
	}
	return scenarioID, nil
}

// SuiteNames lists the suite vocabulary in stable order.
func SuiteNames() []string {
	return []string{SuiteProductionTransport, SuiteReleaseQualification, SuiteMonitoringStack, SuiteStorageFaults, SuiteNetworkFaults, SuiteRestoreIsolation, SuiteLintelRecovery}
}

// CIHarness names map the ci/verify-* entrypoints to harness tables.
var CIHarnessScenarios = map[string]string{
	"model-provider-fixture": "integration.model-provider-fixture",
	"security":               "security.adversarial",
	"migrations":             "deployment.migrations",
}

// ResolvePhase replaces the catalog's `<compose|kubernetes>` placeholder with
// the concrete backend of this target. Kubernetes phases invoke kubectl-native
// verification rather than a deployment helper subcommand. Unresolved
// placeholders would otherwise reach bash literally, so they are errors.
func ResolvePhase(command, backend string) (string, error) {
	resolved := strings.ReplaceAll(command, "<compose|kubernetes>", backend)
	if strings.Contains(resolved, "<") && strings.Contains(resolved, ">") {
		return "", fmt.Errorf("phase command %q still carries an unresolved placeholder", command)
	}
	// Compose owns its established operational commands. Kubernetes deliberately
	// exposes lifecycle verification only through verify --suite, which enters
	// the native Stack and cannot fall through to an unsupported dispatcher verb.
	if backend == BackendKubernetes {
		switch {
		case strings.HasPrefix(resolved, "quoin-deploy kubernetes restore --verify-isolation"):
			return strings.Replace(resolved, "quoin-deploy kubernetes restore --verify-isolation", "quoin-deploy kubernetes verify --suite restore-isolation", 1), nil
		case strings.HasPrefix(resolved, "quoin-deploy kubernetes recover-lintel"):
			return strings.Replace(resolved, "quoin-deploy kubernetes recover-lintel", "quoin-deploy kubernetes verify --suite lintel-recovery", 1), nil
		}
	}
	return resolved, nil
}

// Target describes the deployment target one qualification host
// executes cells for.
type Target struct {
	Backend      string // compose | kubernetes
	Architecture string // linux/amd64 | linux/arm64
	K8sSelector  string // optional maintained_minor_N.latest_patch for k8s cells
}

// MatchesCell reports whether a catalog cell belongs to this target:
// same environment kind (compose-native vs kubernetes-native) and same
// architecture. `always`-mode deployment cells are partitioned across
// hosts by their environment; each native runner executes exactly its
// own cells (VERIFY-CATALOG-005: the full matrix is the union of all
// native runners, never one host pretending to be many).
func (target Target) MatchesCell(loaded *catalog.Catalog, cell catalog.Cell) bool {
	environment := loaded.Environment(cell.EnvironmentID)
	if environment == nil {
		return false
	}
	kindMatches := (target.Backend == "compose" && environment.Kind == "docker_compose") ||
		(target.Backend == "kubernetes" && environment.Kind == "kubernetes")
	if !kindMatches || cell.Architecture != target.Architecture {
		return false
	}
	return true
}

// CellsFor returns the scenario's cells this target executes, in
// catalog order.
func CellsFor(loaded *catalog.Catalog, suite string, target Target) ([]catalog.Cell, error) {
	scenarioID, err := ScenarioID(suite)
	if err != nil {
		return nil, err
	}
	scenario := loaded.Scenario(scenarioID)
	if scenario == nil {
		return nil, fmt.Errorf("catalog scenario %q missing", scenarioID)
	}
	var selected []catalog.Cell
	for _, cell := range scenario.Cells {
		if target.MatchesCell(loaded, cell) {
			selected = append(selected, cell)
		}
	}
	return selected, nil
}

// LoadCatalogAssertionIDs maps "<scenario>/<cell>" to the frozen
// assertion IDs of that cell, from the frozen verification catalog.
func LoadCatalogAssertionIDs(repoRoot string) (map[string][]string, error) {
	loaded, err := catalog.LoadAndValidate(filepath.Join(repoRoot, "docs", "specs", "quoin-v1", "contracts", "verification-catalog.yaml"))
	if err != nil {
		return nil, err
	}
	index := map[string][]string{}
	for _, scenario := range loaded.Scenarios {
		for _, cell := range scenario.Cells {
			ids := make([]string, 0, len(cell.Assertions))
			for _, assertion := range cell.Assertions {
				ids = append(ids, assertion.ID)
			}
			index[scenario.ID+"/"+cell.ID] = ids
		}
	}
	return index, nil
}
