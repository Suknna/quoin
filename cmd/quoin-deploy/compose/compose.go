// Package compose is the deployment helper's Compose backend command
// surface (OPS-HELPER-001): `quoin-deploy compose install|verify`.
package compose

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Suknna/quoin/cmd/quoin-deploy/install"
	deployupgrade "github.com/Suknna/quoin/cmd/quoin-deploy/upgrade"
	recovery "github.com/Suknna/quoin/cmd/quoin-deploy/recover_lintel"
	"github.com/Suknna/quoin/cmd/quoin-deploy/verify"
	deploycompose "github.com/Suknna/quoin/internal/deploy/compose"
	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Main dispatches one Compose backend command. Unsupported commands exit 2
// without any deployment side effect.
func Main(command string, arguments []string) {
	switch command {
	case "install":
		flags := Parse("compose install", arguments)
		install.Run(flags.ConfigPath, flags.ReleaseManifestPath, flags.ReportPath)
	case "verify":
		flags := Parse("compose verify", arguments)
		verify.Run(flags.ConfigPath, flags.ReleaseManifestPath, flags.ReportPath)
	case "backup":
		flags := Parse("compose backup", arguments)
		os.Exit(deploycompose.Backup(deploycompose.Request{ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, flags.Offline))
	case "restore":
		flags := Parse("compose restore", arguments)
		os.Exit(deploycompose.Restore(deploycompose.Request{ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, flags.BackupID))
	case "upgrade":
		flags := deployupgrade.Parse("compose upgrade", arguments)
		os.Exit(deploycompose.Upgrade(deploycompose.Request{
			ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath,
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}, flags))
	case "recover-lintel":
		flags := recovery.Parse("compose recover-lintel", arguments)
		os.Exit(deploycompose.RecoverLintel(deploycompose.Request{ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, flags))
	default:
		fmt.Fprintln(os.Stderr, "usage: quoin-deploy compose <install|upgrade|verify|backup|restore|recover-lintel> --config <path> [--release-manifest <path>] [--report <path>] [--offline] [--backup <backup-id>]")
		os.Exit(2)
	}
}

// Flags carries the stable helper flag set shared by the Compose
// subcommands: --config and --report form the primary invocation surface;
// --release-manifest selects the digest-pinned formal release artifacts.
type Flags struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	Offline             bool
	BackupID            string
}

// Parse returns the parsed flags, exiting 2 on malformed invocations. Every
// exit path writes a minimal invalid-input report first (OPS-HELPER-003: all
// paths produce verification-report.json).
func Parse(name string, arguments []string) Flags {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "strict compose-install YAML")
	releaseManifestPath := flags.String("release-manifest", "", "release-manifest.json with the digest-pinned four-component artifacts")
	reportPath := flags.String("report", "", "where the atomic verification-report.json is written")
	offline := flags.Bool("offline", false, "stop workloads and run offline fallback")
	backupID := flags.String("backup", "", "published backup identifier for restore")
	parseErr := flags.Parse(arguments)
	invalid := parseErr != nil || *configPath == "" || flags.NArg() != 0
	if !invalid {
		return Flags{ConfigPath: *configPath, ReleaseManifestPath: *releaseManifestPath, ReportPath: *reportPath, Offline: *offline, BackupID: *backupID}
	}
	brief := report.New("compose", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH), name, *configPath, "")
	brief.MarkFailed("invalid_invocation", "malformed command line", "fix the command line; no deployment side effect has occurred")
	brief.ExitCode = 2
	path := *reportPath
	if path == "" {
		if stateDir, err := deployconfig.StateDirectory(); err == nil {
			path = filepath.Join(stateDir, "verification-report.json")
		} else {
			path = "verification-report.json"
		}
	}
	_ = brief.Finish(path)
	if parseErr != nil {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "quoin-deploy:", name, "requires --config and takes no positional arguments")
	os.Exit(2)
	return Flags{}
}
