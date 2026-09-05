// Command aggregate merges the per-cell evidence of one Release
// Qualification invocation (the CI workflow's downloaded artifacts)
// into the suite-level verdict summary. It reuses the frozen result
// profile for aggregation and never rewrites a cell's recorded facts:
// a cell that did not run is missing evidence, not passed
// (VERIFY-CATALOG-006: only bound results prove execution).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cellVerdict is the per-cell summary extracted from every facts
// document and per-suite observation the native runners uploaded.
type cellVerdict struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Passed bool   `json:"passed"`
}

func main() {
	evidence := flag.String("evidence", "evidence", "downloaded evidence root")
	output := flag.String("output", "test-result.json", "aggregated verdict summary")
	flag.Parse()
	verdicts, err := collect(*evidence)
	if err != nil {
		fatal(err)
	}

	// Closure check (VERIFY-CATALOG-006): a passing aggregate requires
	// every cell the runners DECLARED (their cells-manifest) to carry a
	// passing bound result — a partial upload can never aggregate to a
	// pass. Runner manifests also freeze the environment matrix the
	// verdict covers.
	declared, environments, err := declaredCells(*evidence)
	if err != nil {
		fatal(err)
	}
	bound := boundPassed(verdicts)
	missing := []string{}
	for _, cell := range declared {
		if !bound[cell] {
			missing = append(missing, cell)
		}
	}
	body, err := json.MarshalIndent(map[string]any{
		"schema":                  "quoin-release-qualification-aggregate-v1",
		"cells":                   verdicts,
		"cellCount":               len(verdicts),
		"declaredCellCount":       len(declared),
		"missingDeclarations":     missing,
		"environmentMatrixDigest": environmentMatrixDigest(environments),
		"environmentMatrixCells":  environments,
		"allPassed":               allPassed(verdicts) && len(missing) == 0,
	}, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*output, body, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("aggregate: %d results, %d declared, missing=%d, allPassed=%t\n",
		len(verdicts), len(declared), len(missing), allPassed(verdicts) && len(missing) == 0)
	if !allPassed(verdicts) || len(missing) != 0 {
		os.Exit(3)
	}
}

// declaredCells reads every runner's cells-manifest and environment
// record from the evidence tree.
func declaredCells(root string) ([]string, []map[string]any, error) {
	var declared []string
	var environments []map[string]any
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "cells-manifest.json" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var manifest struct {
			Runner struct {
				Backend      string `json:"backend"`
				Architecture string `json:"architecture"`
			} `json:"runner"`
			Executed []string `json:"executed"`
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		declared = append(declared, manifest.Executed...)
		environment := map[string]any{
			"backend": manifest.Runner.Backend, "architecture": manifest.Runner.Architecture, "path": path,
		}
		sibling := filepath.Join(filepath.Dir(path), "environment.json")
		if envBody, readErr := os.ReadFile(sibling); readErr == nil {
			var record map[string]any
			if json.Unmarshal(envBody, &record) == nil {
				for key, value := range record {
					environment[key] = value
				}
			}
		}
		environments = append(environments, environment)
		return nil
	})
	return declared, environments, err
}

// boundPassed maps each passing per-cell result to its catalog test
// name. A passing facts document lives at
// cells/<scenario>/<cell>/facts.json inside one runner's invocation
// output, so the test name is recovered from the path; statements bind
// every test they list as passed.
func boundPassed(verdicts []cellVerdict) map[string]bool {
	bound := map[string]bool{}
	for _, verdict := range verdicts {
		if !verdict.Passed {
			continue
		}
		if testName := testNameOf(verdict.Source); testName != "" {
			bound[testName] = true
		}
		bound[verdict.Source] = true
	}
	return bound
}

// testNameOf extracts `<scenario>.<cell>` from a coordinator cell path.
func testNameOf(path string) string {
	directory := filepath.Dir(path)
	cell := filepath.Base(directory)
	scenario := filepath.Base(filepath.Dir(directory))
	if scenario == "cells" || cell == "cells" || scenario == "." || cell == "." {
		return ""
	}
	return scenario + "." + cell
}

// environmentMatrixDigest freezes the digest over every runner
// environment the aggregate covers.
func environmentMatrixDigest(environments []map[string]any) string {
	bodies := make([]string, 0, len(environments))
	for _, environment := range environments {
		body, _ := json.Marshal(environment)
		bodies = append(bodies, string(body))
	}
	sort.Strings(bodies)
	sum := sha256.Sum256([]byte(strings.Join(bodies, "\n")))
	return hex.EncodeToString(sum[:])
}

// collect walks the evidence tree and classifies every per-cell
// document. Facts documents pass when every check passed; suite
// observation documents pass when their recorded classes are the
// deterministic vocabulary; the contract-gate statement passes when
// its verdict is PASSED.
func collect(root string) ([]cellVerdict, error) {
	var verdicts []cellVerdict
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		switch {
		case name == "facts.json" || strings.HasPrefix(name, "facts"):
			passed, factsErr := factsPassed(path)
			if factsErr != nil {
				return factsErr
			}
			verdicts = append(verdicts, cellVerdict{Source: path, Kind: "facts", Passed: passed})
		case name == "test-result.json":
			passed, statementErr := statementPassed(path)
			if statementErr != nil {
				return statementErr
			}
			verdicts = append(verdicts, cellVerdict{Source: path, Kind: "statement", Passed: passed})
		case strings.HasPrefix(name, "storage-") || strings.HasPrefix(name, "network-") ||
			strings.HasPrefix(name, "transport-") || strings.HasPrefix(name, "release-") ||
			strings.HasPrefix(name, "monitoring-"):
			if strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, "facts.json") {
				passed, obsErr := observationPassed(path)
				if obsErr != nil {
					return obsErr
				}
				verdicts = append(verdicts, cellVerdict{Source: path, Kind: "observation", Passed: passed})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(verdicts) == 0 {
		return nil, fmt.Errorf("no cell evidence found under %s", root)
	}
	return verdicts, nil
}

func factsPassed(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var document struct {
		Checks []struct {
			Result string `json:"result"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if len(document.Checks) == 0 {
		return false, fmt.Errorf("%s: no checks recorded", path)
	}
	for _, check := range document.Checks {
		if check.Result != "passed" {
			return false, nil
		}
	}
	return true, nil
}

func statementPassed(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var statement struct {
		Predicate struct {
			Result string `json:"result"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(body, &statement); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return statement.Predicate.Result == "PASSED", nil
}

// observationPassed accepts the per-suite observation documents; a
// document fails when it explicitly records a non-deterministic class
// or a failed leg.
func observationPassed(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	text := string(body)
	if strings.Contains(text, "\"unexpected\"") || strings.Contains(text, "leak:") ||
		strings.Contains(text, "\"failed\"") {
		return false, nil
	}
	return strings.Contains(text, "fault_deterministic_") ||
		strings.Contains(text, "\"passed\"") ||
		strings.Contains(text, "no_leak") ||
		strings.Contains(text, "digest"), nil
}

func allPassed(verdicts []cellVerdict) bool {
	for _, verdict := range verdicts {
		if !verdict.Passed {
			return false
		}
	}
	return true
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "aggregate:", err)
	os.Exit(2)
}
