// Command qualify is the CI-facing Release Qualification driver. Where
// the local acceptance coordinator (test/release/qualification) drives
// one native cell end to end, this command drives the catalog matrix
// for one CI runner: it resolves the runner's target from the frozen
// catalog, executes every applicable suite cell through the same
// Coordinator used by the acceptance (real phase commands, real
// evidence items), and writes the runner's cell manifest for the
// aggregate job.
//
// Usage:
//
//	qualify prepare --work DIR --config PATH --manifest PATH \
//	    --inventory subjects-inventory.json --release v0.1.0-dev
//	qualify run --work DIR --config PATH --manifest PATH \
//	    --suite name [--suite name]... --backend compose --arch linux/arm64
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/verification/catalog"
	"github.com/Suknna/quoin/internal/verification/evidence"
	"github.com/Suknna/quoin/internal/verification/result"
	"github.com/Suknna/quoin/internal/verification/suites"
)

const defaultCatalog = "docs/specs/quoin-v1/contracts/verification-catalog.yaml"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "prepare":
		prepareCommand(os.Args[2:])
	case "run":
		runCommand(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: qualify <prepare|run> [flags]")
	os.Exit(2)
}

// prepareCommand projects a subjects inventory into the deployment
// inputs every suite consumes: the strict install config and the
// helper-shaped release manifest.
func prepareCommand(arguments []string) {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	work := flags.String("work", "", "qualification work root")
	configOut := flags.String("config", "", "where the install config is written")
	manifestOut := flags.String("manifest", "", "where the release manifest is written")
	inventoryPath := flags.String("inventory", "", "subjects inventory from the release builder")
	release := flags.String("release", "v0.1.0-dev", "release version of the built subjects")
	quoinPort := flags.Int("quoin-port", 20880, "published Quoin loopback port")
	stelePort := flags.Int("stele-port", 20881, "published Stele loopback port")
	if err := flags.Parse(arguments); err != nil || *work == "" || *inventoryPath == "" {
		usage()
	}
	body, err := os.ReadFile(*inventoryPath)
	if err != nil {
		fatal(err)
	}
	var inventory struct {
		Release string `json:"release"`
		Images  map[string]struct {
			Repository  string            `json:"repository"`
			IndexDigest string            `json:"index_digest"`
			Platforms   map[string]string `json:"platforms"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &inventory); err != nil {
		fatal(fmt.Errorf("inventory: %w", err))
	}
	images := map[string]suites.SubjectImage{}
	for component, image := range inventory.Images {
		images[component] = suites.SubjectImage{
			Repository: image.Repository, Index: image.IndexDigest, Platforms: image.Platforms,
		}
	}
	if len(images) == 0 {
		fatal(fmt.Errorf("inventory carries no images"))
	}
	version := inventory.Release
	if version == "" {
		version = *release
	}
	if err := os.MkdirAll(*work, 0o755); err != nil {
		fatal(err)
	}
	configPath, err := suites.WriteInstallConfig(*work, suites.InstallPorts{Quoin: *quoinPort, Stele: *stelePort})
	if err != nil {
		fatal(err)
	}
	commit, _ := execOutput("git", "rev-parse", "HEAD")
	manifestPath, err := suites.WriteReleaseManifest(*work, version, strings.TrimSpace(commit), images)
	if err != nil {
		fatal(err)
	}
	if *configOut != "" && *configOut != configPath {
		if err := os.Rename(configPath, *configOut); err != nil {
			fatal(err)
		}
		configPath = *configOut
	}
	if *manifestOut != "" && *manifestOut != manifestPath {
		if err := os.Rename(manifestPath, *manifestOut); err != nil {
			fatal(err)
		}
		manifestPath = *manifestOut
	}
	fmt.Printf("qualify prepare: config=%s manifest=%s\n", configPath, manifestPath)
}

// runCommand executes every applicable cell of the selected suites for
// this runner's target, then writes the evidence, the statement and the
// runner cell manifest.
func runCommand(arguments []string) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	work := flags.String("work", "", "qualification work root (from prepare)")
	configPath := flags.String("config", "", "install config (from prepare)")
	manifestPath := flags.String("manifest", "", "release manifest (from prepare)")
	backend := flags.String("backend", "compose", "deployment backend of this runner")
	arch := flags.String("arch", "", "native architecture of this runner (linux/amd64 | linux/arm64)")
	var suiteNames []string
	flags.Var(&suiteFlag{values: &suiteNames}, "suite", "suite to execute (repeatable)")
	adminPassword := flags.String("admin-password", "", "bootstrap admin password for stack-backed suites")
	quoinPort := flags.Int("quoin-port", 20880, "published Quoin loopback port")
	stelePort := flags.Int("stele-port", 20881, "published Stele loopback port")
	catalogPath := flags.String("catalog", defaultCatalog, "frozen verification catalog path")
	if err := flags.Parse(arguments); err != nil || *work == "" || *configPath == "" || *manifestPath == "" || *arch == "" || len(suiteNames) == 0 {
		usage()
	}
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	path := *catalogPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	loaded, err := catalog.LoadAndValidate(path)
	if err != nil {
		fatal(err)
	}
	profilePath := filepath.Join(filepath.Dir(path), "verification-result-profile.yaml")
	profile, err := result.LoadProfile(profilePath)
	if err != nil {
		fatal(err)
	}
	coordinator := &suites.Coordinator{
		Catalog: loaded, Profile: profile,
		Target:       suites.Target{Backend: *backend, Architecture: *arch},
		OutputDir:    filepath.Join(*work, "invocation"),
		RepoRoot:     root,
		InvocationID: fmt.Sprintf("rq-%s-%s-%d", *backend, strings.ReplaceAll(*arch, "/", "-"), time.Now().UTC().Unix()),
		ToolVersion:  "quoin-qualify/dev",
		Subject:      result.Subject{Name: "quoin-release-subjects", Digest: map[string]string{"sha256": subjectDigest(*manifestPath)}},
	}
	helper, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	pathEnv := filepath.Dir(helper) + ":" + os.Getenv("PATH")
	env := append(os.Environ(),
		"PATH="+pathEnv,
		"QUOIN_REPO_ROOT="+root,
		"QUOIN_SUITE_WORK_ROOT="+*work,
		"QUOIN_SUITE_PROJECT=rq-"+filepath.Base(*work),
		"QUOIN_SUITE_QUOIN_PORT="+strconv.Itoa(*quoinPort),
		"QUOIN_SUITE_STELE_PORT="+strconv.Itoa(*stelePort),
		"QUOIN_SUITE_CONFIG="+*configPath,
		"QUOIN_SUITE_RELEASE_MANIFEST="+*manifestPath,
	)
	if *adminPassword != "" {
		env = append(env, "QUOIN_SUITE_ADMIN_PASSWORD="+*adminPassword)
	}
	executed := []string{}
	failed := false
	for _, suite := range suiteNames {
		scenarioID, err := suites.ScenarioID(suite)
		if err != nil {
			fatal(err)
		}
		scenario := loaded.Scenario(scenarioID)
		if scenario == nil {
			fatal(fmt.Errorf("catalog scenario %q missing", scenarioID))
		}
		cells, err := suites.CellsFor(loaded, suite, coordinator.Target)
		if err != nil {
			fatal(err)
		}
		if len(cells) == 0 {
			fmt.Printf("qualify: suite %s has no cell for target %s/%s\n", suite, *backend, *arch)
			continue
		}
		for index := range cells {
			cell := &cells[index]
			if causes := coordinator.DependencyCauses(scenario); len(causes) > 0 {
				item := coordinator.NotRunItem(scenario, *cell, causes)
				fmt.Printf("qualify: %s.%s not_run (causes %v)\n", scenarioID, cell.ID, item.CausalIDs)
				continue
			}
			item, err := coordinator.ExecuteCell(scenario, cell, *backend, env)
			if err != nil {
				fatal(err)
			}
			executed = append(executed, item.TestName())
			fmt.Printf("qualify: %s -> %s (%s)\n", item.TestName(), item.Outcome, item.Category)
			if item.Outcome != "passed" {
				failed = true
			}
		}
	}
	// The runner cell manifest and environment record feed the aggregate
	// job's closure check: every declared executed cell must bind a
	// passing result, and the environment matrix digest is computed
	// over the runner's resolved backend/architecture/toolchain.
	if err := writeJSON(filepath.Join(*work, "cells-manifest.json"), map[string]any{
		"runner":   map[string]string{"backend": *backend, "architecture": *arch},
		"suites":   suiteNames,
		"executed": executed,
	}); err != nil {
		fatal(err)
	}
	if err := writeJSON(filepath.Join(*work, "environment.json"), map[string]any{
		"backend": *backend, "architecture": *arch,
	}); err != nil {
		fatal(err)
	}
	statement := coordinator.Statement(fileDigest(path), fileDigest(profilePath))
	if err := writeJSON(filepath.Join(*work, "test-result.json"), statement); err != nil {
		fatal(err)
	}
	fmt.Printf("qualify: verdict=%s executed=%d\n", statement.Predicate.Result, len(executed))
	if failed || statement.Predicate.Result != "PASSED" {
		os.Exit(3)
	}
}

type suiteFlag struct{ values *[]string }

func (flag *suiteFlag) String() string { return strings.Join(*flag.values, ",") }
func (flag *suiteFlag) Set(value string) error {
	*flag.values = append(*flag.values, value)
	return nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func fileDigest(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return evidence.Digest(body)
}

func subjectDigest(manifestPath string) string {
	if digest := fileDigest(manifestPath); digest != "" {
		return digest
	}
	return "unknown"
}

func execOutput(name string, arguments ...string) (string, error) {
	output, err := exec.Command(name, arguments...).Output()
	return string(output), err
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
	fmt.Fprintln(os.Stderr, "qualify:", err)
	os.Exit(2)
}
