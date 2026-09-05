// Package verify runs `quoin-deploy compose|helm verify`: the
// read-only, repeatable operational-surface verifier
// (OPS-HELPER-004, OPS-VERIFY-003) and, with --suite, one Release
// Qualification suite cell phase (T40). The suite mode executes the
// frozen catalog's deployment-helper entrypoints; verdicts stay with
// the qualification coordinator and the frozen result profile.
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	deploycompose "github.com/Suknna/quoin/internal/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/verification/suites"
)

// SuiteFlags carries the release-qualification suite surface.
type SuiteFlags struct {
	Suite    string
	Phase    string
	Config   string
	Manifest string
}

// RunSuite executes one suite phase for the backend and returns the
// process exit code. The QUOIN_VERIFY_* environment carries the
// coordinator's cell context (scenario, cell, parameters, workdir,
// facts path) when the phase runs inside a qualification invocation.
func RunSuite(backend string, flags SuiteFlags) int {
	if flags.Config == "" {
		flags.Config = os.Getenv("QUOIN_SUITE_CONFIG")
	}
	if flags.Manifest == "" {
		flags.Manifest = os.Getenv("QUOIN_SUITE_RELEASE_MANIFEST")
	}
	workdir := os.Getenv("QUOIN_VERIFY_WORKDIR")
	if workdir == "" {
		stateDir, err := stateDirectory()
		if err != nil {
			workdir = os.TempDir()
		} else {
			workdir = filepath.Join(stateDir, "suite", flags.Suite)
		}
	}
	if parameters := os.Getenv("QUOIN_VERIFY_PARAMETERS"); parameters != "" {
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "quoin-deploy: suite workdir %s: %v\n", workdir, err)
			os.Exit(2)
		}
		if err := os.WriteFile(filepath.Join(workdir, "cell-parameters.json"), []byte(parameters), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "quoin-deploy: persist cell parameters: %v\n", err)
			os.Exit(2)
		}
	}
	request := suites.DeploymentRequest{
		Backend: backend, Suite: flags.Suite, Phase: flags.Phase,
		Scenario: os.Getenv("QUOIN_VERIFY_SCENARIO"), Cell: os.Getenv("QUOIN_VERIFY_CELL"),
		Workdir: workdir, FactsPath: os.Getenv("QUOIN_VERIFY_FACTS"),
		ConfigPath: flags.Config, ReleaseManifestPath: flags.Manifest,
		RepoRoot: repoRoot(), Stdout: os.Stdout, Stderr: os.Stderr,
	}
	return suites.RunDeploymentSuite(request)
}

func stateDirectory() (string, error) {
	return deployconfig.StateDirectory()
}

func repoRoot() string {
	if root := os.Getenv("QUOIN_REPO_ROOT"); root != "" {
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// ParametersJSON is a small helper for callers composing cell parameter
// documents; it keeps the encoding in one place.
func ParametersJSON(parameters map[string]any) string {
	body, err := json.Marshal(parameters)
	if err != nil {
		return "{}"
	}
	return string(body)
}

// Run executes the operational verify command with the already-parsed
// flag surface.
func Run(configPath, releaseManifestPath, reportPath string) {
	os.Exit(deploycompose.Verify(deploycompose.Request{
		ConfigPath:          configPath,
		ReleaseManifestPath: releaseManifestPath,
		ReportPath:          reportPath,
		Stdin:               os.Stdin,
		Stdout:              os.Stdout,
		Stderr:              os.Stderr,
	}))
}
