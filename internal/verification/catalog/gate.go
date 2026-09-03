package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// CrossContractViolations implements the build-gate negatives the JSON Schema
// cannot express (VERIFY-COVERAGE-002): duplicate IDs, dangling or illegal
// dependency/proof references, cycles, cross-layer edges, root coverage,
// capability/environment closure and dependency applicability coverage.
func (c *Catalog) CrossContractViolations() []Violation {
	var violations []Violation
	seen := map[string]bool{}
	for i := range c.Scenarios {
		id := c.Scenarios[i].ID
		if seen[id] {
			violations = append(violations, Violation{"duplicate_scenario_id", id})
		}
		seen[id] = true
	}
	for i := range c.Scenarios {
		scenario := &c.Scenarios[i]
		violations = append(violations, c.scenarioViolations(scenario)...)
	}
	violations = append(violations, c.rootCoverageViolations()...)
	violations = append(violations, c.applicabilityClosureViolations()...)
	violations = append(violations, c.dependencyCycleViolations()...)
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Code != violations[j].Code {
			return violations[i].Code < violations[j].Code
		}
		return violations[i].Detail < violations[j].Detail
	})
	return violations
}

func (c *Catalog) scenarioViolations(scenario *Scenario) []Violation {
	var violations []Violation
	for _, root := range scenario.ValidationRoots {
		if !c.rootIDs[root] {
			violations = append(violations, Violation{"dangling_validation_root",
				fmt.Sprintf("%s references undeclared root %s", scenario.ID, root)})
		}
	}
	for _, capability := range scenario.RequiredCapabilities {
		if !c.capabilityIDs[capability] {
			violations = append(violations, Violation{"unknown_capability",
				fmt.Sprintf("%s references undeclared capability %s", scenario.ID, capability)})
		}
	}
	for i := range scenario.Cells {
		cell := &scenario.Cells[i]
		if !c.environmentIDs[cell.EnvironmentID] {
			violations = append(violations, Violation{"unknown_environment",
				fmt.Sprintf("%s.%s references undeclared environment %s", scenario.ID, cell.ID, cell.EnvironmentID)})
		}
		for _, capability := range cell.RequiredCapabilities {
			if !c.capabilityIDs[capability] {
				violations = append(violations, Violation{"unknown_capability",
					fmt.Sprintf("%s.%s references undeclared capability %s", scenario.ID, cell.ID, capability)})
			}
		}
	}
	for _, dependency := range scenario.DependsOn {
		if dependency == scenario.ID {
			violations = append(violations, Violation{"self_dependency", scenario.ID})
			continue
		}
		target, ok := c.scenarioByID[dependency]
		if !ok {
			violations = append(violations, Violation{"dangling_dependency",
				fmt.Sprintf("%s depends on unknown scenario %s", scenario.ID, dependency)})
			continue
		}
		if target.Layer != scenario.Layer {
			violations = append(violations, Violation{"cross_layer_dependency",
				fmt.Sprintf("%s (%s) depends on %s (%s)", scenario.ID, scenario.Layer, dependency, target.Layer)})
		}
	}
	for _, proof := range scenario.ProofRefs {
		target, ok := c.scenarioByID[proof]
		if !ok {
			violations = append(violations, Violation{"dangling_proof_ref",
				fmt.Sprintf("%s proof-references unknown scenario %s", scenario.ID, proof)})
			continue
		}
		if layerRank[target.Layer] >= layerRank[scenario.Layer] {
			violations = append(violations, Violation{"proof_ref_not_lower_layer",
				fmt.Sprintf("%s (%s) proof-references %s (%s)", scenario.ID, scenario.Layer, proof, target.Layer)})
		}
	}
	if scenario.Status == "retired" && scenario.Successor == "" {
		violations = append(violations, Violation{"retired_without_successor", scenario.ID})
	}
	return violations
}

// rootCoverageViolations enforces VERIFY-COVERAGE-001/003: every declared
// validation root must be covered by at least one required scenario, and
// scenarios may only reference declared roots.
func (c *Catalog) rootCoverageViolations() []Violation {
	covered := map[string]bool{}
	for i := range c.Scenarios {
		scenario := &c.Scenarios[i]
		if scenario.Requirement != "required" {
			continue
		}
		for _, root := range scenario.ValidationRoots {
			covered[root] = true
		}
	}
	var violations []Violation
	for _, root := range c.ValidationRoots {
		if !covered[root.ID] {
			violations = append(violations, Violation{"uncovered_validation_root", root.ID})
		}
	}
	return violations
}

// applicabilityClosureViolations enforces VERIFY-CATALOG-002: a dependency's
// applicable cells must cover the dependent. Coverage is judged over the
// layer's canonical target universe — the contract gate runs without a
// target, release qualification runs the full native deployment matrix and
// deployment acceptance runs one site target that freezes current objects.
// Whenever the dependent has an applicable cell for a universe target, the
// dependency must have one too, so no invocation shape can run the dependent
// without its prerequisite.
func (c *Catalog) applicabilityClosureViolations() []Violation {
	var violations []Violation
	for i := range c.Scenarios {
		scenario := &c.Scenarios[i]
		if scenario.Status != "active" {
			continue
		}
		for _, dependency := range scenario.DependsOn {
			target, ok := c.scenarioByID[dependency]
			if !ok || target.Layer != scenario.Layer {
				continue // reported as dangling or cross-layer already
			}
			for _, universe := range layerTargetUniverse(scenario.Layer) {
				if len(ApplicableCells(scenario, universe)) == 0 {
					continue
				}
				if len(ApplicableCells(target, universe)) == 0 {
					violations = append(violations, Violation{"applicability_not_closed",
						fmt.Sprintf("%s is applicable in %s but dependency %s is not", scenario.ID, describeTarget(universe), dependency)})
				}
			}
		}
	}
	return violations
}

func layerTargetUniverse(layer string) []*Target {
	switch layer {
	case LayerReleaseQualification:
		return []*Target{
			{Backend: "compose", Architecture: "linux/amd64"},
			{Backend: "compose", Architecture: "linux/arm64"},
			{Backend: "kubernetes", Architecture: "linux/amd64"},
			{Backend: "kubernetes", Architecture: "linux/arm64"},
		}
	case LayerDeploymentAcceptance:
		return []*Target{
			{Backend: "compose", Architecture: "linux/amd64", CurrentObject: true},
			{Backend: "compose", Architecture: "linux/arm64", CurrentObject: true},
			{Backend: "kubernetes", Architecture: "linux/amd64", CurrentObject: true},
			{Backend: "kubernetes", Architecture: "linux/arm64", CurrentObject: true},
		}
	default:
		return []*Target{nil}
	}
}

func describeTarget(target *Target) string {
	if target == nil {
		return "no target"
	}
	return target.Backend + "/" + target.Architecture
}

// dependencyCycleViolations walks depends_on with DFS coloring; depends_on
// must form a same-layer DAG.
func (c *Catalog) dependencyCycleViolations() []Violation {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var violations []Violation
	var visit func(id string, path []string)
	visit = func(id string, path []string) {
		switch state[id] {
		case visiting:
			for i, node := range path {
				if node == id {
					violations = append(violations, Violation{"dependency_cycle",
						strings.Join(append(append([]string{}, path[i:]...), id), " -> ")})
				}
			}
			return
		case done:
			return
		}
		state[id] = visiting
		scenario := c.scenarioByID[id]
		if scenario != nil {
			for _, dependency := range scenario.DependsOn {
				if _, ok := c.scenarioByID[dependency]; ok {
					visit(dependency, append(path, id))
				}
			}
		}
		state[id] = done
	}
	for i := range c.Scenarios {
		visit(c.Scenarios[i].ID, nil)
	}
	return violations
}

// ApplicableCells resolves a scenario's cells against a target context. A nil
// target keeps only `always` cells, which is the contract-gate execution
// model; deployment_target modes need a target and for_each modes
// additionally need the frozen current-object set of a site.
func ApplicableCells(scenario *Scenario, target *Target) []Cell {
	var cells []Cell
	for _, cell := range scenario.Cells {
		switch cell.Applicability.Mode {
		case "always":
			cells = append(cells, cell)
		case "deployment_target":
			if target != nil && target.Backend == cell.Applicability.Backend && target.Architecture == cell.Applicability.Architecture {
				cells = append(cells, cell)
			}
		case "for_each_current_object":
			if target != nil && target.CurrentObject {
				cells = append(cells, cell)
			}
		case "deployment_target_for_each_current_object":
			if target != nil && target.CurrentObject && target.Backend == cell.Applicability.Backend && target.Architecture == cell.Applicability.Architecture {
				cells = append(cells, cell)
			}
		}
	}
	return cells
}

// ExecutionOrder returns the deterministic topological order of a layer's
// active scenarios. The catalog gate has already rejected cycles; here ties
// break alphabetically so repeated invocations plan identically.
func ExecutionOrder(c *Catalog, layer string) []string {
	ready := map[string]bool{}
	var order []string
	remaining := map[string]*Scenario{}
	for i := range c.Scenarios {
		scenario := &c.Scenarios[i]
		if scenario.Layer == layer && scenario.Status == "active" {
			remaining[scenario.ID] = scenario
		}
	}
	for len(remaining) > 0 {
		var candidates []string
		for id, scenario := range remaining {
			blocked := false
			for _, dependency := range scenario.DependsOn {
				if _, still := remaining[dependency]; still {
					blocked = true
					break
				}
			}
			if !blocked {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) == 0 {
			// Defensive: validated catalogs never reach this. Deterministic
			// failure beats an infinite loop on a hand-built catalog.
			ids := make([]string, 0, len(remaining))
			for id := range remaining {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return append(order, ids...)
		}
		sort.Strings(candidates)
		for _, id := range candidates {
			order = append(order, id)
			ready[id] = true
			delete(remaining, id)
		}
	}
	_ = ready
	return order
}
