// Package upgrade owns the shared invocation surface and the credential-free
// prepared-observation state machine of `quoin-deploy <compose|helm> upgrade`
// (T36, OPS-UPGRADE-002): the helper never receives Web credentials or calls
// a product HTTP write interface; it only polls the unauthenticated ops
// metrics of the not-publicly-exposed listener.
package upgrade

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Flags is the shared upgrade invocation surface.
type Flags struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	PreparedWait        time.Duration
}

// Parse returns the parsed flags; malformed invocations exit 2 after writing
// the minimal invalid-input report (OPS-HELPER-003).
func Parse(name string, arguments []string) Flags {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "strict install YAML")
	releaseManifestPath := flags.String("release-manifest", "", "release-manifest.json with the digest-pinned four-component artifacts")
	reportPath := flags.String("report", "", "where the atomic verification-report.json is written")
	preparedWait := flags.Duration("prepared-wait", 30*time.Minute, "bound for the Admin-side prepare/drain/backup observation")
	parseErr := flags.Parse(arguments)
	invalid := parseErr != nil || *configPath == "" || *releaseManifestPath == "" || flags.NArg() != 0
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
		return Flags{ConfigPath: *configPath, ReleaseManifestPath: *releaseManifestPath, ReportPath: *reportPath, PreparedWait: *preparedWait}
	}
	brief.MarkFailed("invalid_invocation", "malformed command line", "fix the command line; no deployment side effect has occurred")
	brief.ExitCode = 2
	_ = brief.Finish(path)
	if parseErr != nil {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "quoin-deploy:", name, "requires --config and --release-manifest")
	os.Exit(2)
	return Flags{}
}

func backendOf(name string) string {
	if len(name) >= 4 && name[:4] == "helm" {
		return "helm"
	}
	return "compose"
}

// PreparedObservation is the closed projection of the Quoin ops metrics the
// upgrade decision needs.
type PreparedObservation struct {
	Accepting            bool
	MaintenanceUpgrade   bool
	Prepared             bool
	ProcessStart         float64
}

// ParsePrepared parses the metrics exposition body.
func ParsePrepared(body string) (PreparedObservation, error) {
	var value PreparedObservation
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		number, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		name := fields[0]
		labels := ""
		if index := strings.IndexByte(name, '{'); index >= 0 {
			labels = name[index:]
			name = name[:index]
		}
		switch name {
		case "quoin_accepting_work":
			value.Accepting, seen["accepting"] = number == 1, true
		case "quoin_maintenance":
			if strings.Contains(labels, `maintenance_reason="upgrade"`) {
				value.MaintenanceUpgrade = value.MaintenanceUpgrade || number != 0
				seen["upgrade"] = true
			}
		case "quoin_upgrade_prepared":
			value.Prepared, seen["prepared"] = number == 1, true
		case "process_start_time_seconds":
			value.ProcessStart, seen["start"] = number, true
		}
	}
	for _, key := range []string{"accepting", "upgrade", "prepared", "start"} {
		if !seen[key] {
			return PreparedObservation{}, fmt.Errorf("ops metrics missing %s", key)
		}
	}
	return value, nil
}

// PreparedOptions provides the backend-specific transport only; every state
// transition and failure ordering lives in ObservePrepared.
type PreparedOptions struct {
	Read    func(label string) (PreparedObservation, error)
	Now     func() time.Time
	Sleep   func(time.Duration)
	OnEnter func()
	WaitFor time.Duration
	PollEvery time.Duration
}

// PreparedError carries the stable report reason and operator action.
type PreparedError struct{ Code, Message, NextAction string }

func (err *PreparedError) Error() string { return err.Message }

// ObservePrepared waits for the Admin-driven preparation exactly once: the
// Upgrade maintenance must be entered and stay active, and the
// quoin_upgrade_prepared gauge must flip to 1 without a process reset.
func ObservePrepared(options PreparedOptions) error {
	if options.Read == nil {
		return &PreparedError{Code: "upgrade_prepared_unavailable", Message: "metrics reader is unavailable", NextAction: "repair the deployment verifier"}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = time.Sleep
	}
	if options.WaitFor == 0 {
		options.WaitFor = 30 * time.Minute
	}
	if options.PollEvery == 0 {
		options.PollEvery = 5 * time.Second
	}
	first, err := options.Read("upgrade-metrics-baseline")
	if err != nil {
		return &PreparedError{Code: "upgrade_prepared_unavailable", Message: err.Error(), NextAction: "ensure the verifier can reach the Quoin ops listener"}
	}
	processStart := first.ProcessStart
	announced := false
	deadline := options.Now().Add(options.WaitFor)
	for {
		current, err := options.Read("upgrade-metrics-observe")
		if err != nil {
			return &PreparedError{Code: "upgrade_prepared_unavailable", Message: err.Error(), NextAction: "ensure the verifier can reach the Quoin ops listener"}
		}
		if current.ProcessStart != processStart {
			return &PreparedError{Code: "upgrade_observation_reset", Message: "Quoin restarted during the upgrade observation", NextAction: "rerun the upgrade command after Quoin is stably ready"}
		}
		if current.MaintenanceUpgrade && !current.Accepting && current.Prepared {
			return nil
		}
		if !announced && !current.MaintenanceUpgrade {
			if options.OnEnter != nil {
				options.OnEnter()
			}
			announced = true
		}
		if !options.Now().Before(deadline) {
			return &PreparedError{Code: "upgrade_prepared_timeout", Message: "the Upgrade maintenance never reached quoin_upgrade_prepared=1", NextAction: "complete prepareUpgrade, drain active work and the pre-upgrade backup in the Admin Web UI, then rerun the upgrade command"}
		}
		options.Sleep(options.PollEvery)
	}
}
