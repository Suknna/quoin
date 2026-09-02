// Package recover_lintel owns the shared, catalog-bound invocation surface
// of `quoin-deploy <compose|helm> recover-lintel` (T35): the frozen
// verification-catalog entrypoint binds the internal orchestration flags
// (OPS-HELPER-001), so both backends parse one flag set instead of keeping
// parallel lists.
package recover_lintel

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Phases are the frozen catalog orchestration phases; teardown is a no-op.
const (
	PhaseSetup  = "setup"
	PhaseAction = "action"
	PhaseAssert = "assert"
)

// Dispositions are the frozen storage dispositions (OPS-HELPER-005).
const (
	DispositionExclusivelyReattached = "exclusively_reattached"
	DispositionRetired               = "retired"
)

// Flags is the shared recover-lintel invocation surface.
type Flags struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	Phase               string
	StorageDisposition  string
	FirstAuthTimeout    time.Duration
}

// Parse returns the parsed flags; malformed invocations exit 2 after writing
// the minimal invalid-input report (OPS-HELPER-003).
func Parse(name string, arguments []string) Flags {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "strict install YAML")
	releaseManifestPath := flags.String("release-manifest", "", "release-manifest.json with the digest-pinned four-component artifacts")
	reportPath := flags.String("report", "", "where the atomic verification-report.json is written")
	phase := flags.String("phase", PhaseAction, "catalog orchestration phase: setup | action | assert")
	disposition := flags.String("storage-disposition", "", "required storage disposition: exclusively_reattached | retired")
	firstAuthTimeout := flags.Duration("first-auth-timeout", 15*time.Minute, "bound for the replacement Lintel first Hello")
	parseErr := flags.Parse(arguments)
	// The frozen catalog entrypoint binds only --phase (OPS-HELPER-001); the
	// storage disposition is required exactly where it is consumed — the
	// action phase. Setup preflights and the read-only assert phase stay
	// callable exactly as the catalog spells them.
	invalid := parseErr != nil || *configPath == "" || flags.NArg() != 0 ||
		(*phase != PhaseSetup && *phase != PhaseAction && *phase != PhaseAssert) ||
		(*phase == PhaseAction && *disposition != DispositionExclusivelyReattached && *disposition != DispositionRetired)
	brief := report.New(backendOf(name), fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH), name, *configPath, "")
	path := *reportPath
	if path == "" {
		if stateDir, err := deployconfig.StateDirectoryFor(backendOf(name)); err == nil {
			path = filepath.Join(stateDir, "verification-report.json")
		} else {
			path = "verification-report.json"
		}
	}
	if !invalid {
		return Flags{
			ConfigPath: *configPath, ReleaseManifestPath: *releaseManifestPath, ReportPath: *reportPath,
			Phase: *phase, StorageDisposition: *disposition, FirstAuthTimeout: *firstAuthTimeout,
		}
	}
	reason := "malformed command line"
	if parseErr == nil && *phase == PhaseAction && *disposition != DispositionExclusivelyReattached && *disposition != DispositionRetired {
		reason = "--storage-disposition must be exclusively_reattached or retired"
	}
	brief.MarkFailed("invalid_invocation", reason, "fix the command line; no deployment side effect has occurred")
	brief.ExitCode = 2
	_ = brief.Finish(path)
	if parseErr != nil {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "quoin-deploy:", name, "requires --config; --storage-disposition is required for --phase action")
	os.Exit(2)
	return Flags{}
}

func backendOf(name string) string {
	if len(name) >= 4 && name[:4] == "helm" {
		return "helm"
	}
	return "compose"
}
