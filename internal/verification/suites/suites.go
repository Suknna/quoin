// Package suites owns the Release Qualification suite vocabulary and the
// per-cell phase coordinator (T40). The frozen verification catalog is
// the single scenario authority: this package maps suite names to
// scenario IDs, resolves the catalog's `<compose|helm>` phase templates
// against the concrete deployment backend, and executes one cell's
// setup/action/assert/teardown phases through real commands using the
// same environment contract the contract-gate runner uses
// (QUOIN_VERIFY_*). Verdicts stay with the frozen result profile; this
// package never invents outcome classes.
package suites

import (
	"fmt"
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
)

// table maps each suite name to its owning catalog scenario. The
// scenario ID — not a copy of its assertions — is the authority.
var table = map[string]string{
	SuiteProductionTransport:  "production.transport",
	SuiteReleaseQualification: "release.native-matrix",
	SuiteMonitoringStack:      "integration.monitoring-stack",
	SuiteStorageFaults:        "fault.storage",
	SuiteNetworkFaults:        "fault.network",
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
	return []string{SuiteProductionTransport, SuiteReleaseQualification, SuiteMonitoringStack, SuiteStorageFaults, SuiteNetworkFaults}
}

// CIHarness names map the ci/verify-* entrypoints to harness tables.
var CIHarnessScenarios = map[string]string{
	"model-provider-fixture": "integration.model-provider-fixture",
	"security":               "security.adversarial",
	"migrations":             "deployment.migrations",
}

// ResolvePhase replaces the catalog's `<compose|helm>` placeholder with
// the concrete backend of this target. Unresolved placeholders would
// otherwise reach bash literally, so a template that still carries one
// after resolution is an error, not a silent skip.
func ResolvePhase(command, backend string) (string, error) {
	resolved := strings.ReplaceAll(command, "<compose|helm>", backend)
	if strings.Contains(resolved, "<") && strings.Contains(resolved, ">") {
		return "", fmt.Errorf("phase command %q still carries an unresolved placeholder", command)
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
