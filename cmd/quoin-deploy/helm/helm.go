// Package helm dispatches one Kubernetes backend command. Unsupported
// commands exit 2 without any deployment side effect.
package helm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	deployconfig "github.com/Suknna/quoin/internal/deploy/config"
	deployhelm "github.com/Suknna/quoin/internal/deploy/helm"
	"github.com/Suknna/quoin/internal/deploy/report"
)

// Main dispatches one Helm backend command.
func Main(command string, arguments []string) {
	switch command {
	case "install":
		flags := Parse("helm install", arguments)
		os.Exit(deployhelm.Install(deployhelm.Request{
			ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath,
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}))
	case "verify":
		flags := Parse("helm verify", arguments)
		os.Exit(deployhelm.Verify(deployhelm.Request{
			ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath,
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}))
	case "backup":
		flags := Parse("helm backup", arguments)
		os.Exit(deployhelm.Backup(deployhelm.Request{ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, flags.Offline))
	case "restore":
		flags := Parse("helm restore", arguments)
		os.Exit(deployhelm.Restore(deployhelm.Request{ConfigPath: flags.ConfigPath, ReleaseManifestPath: flags.ReleaseManifestPath, ReportPath: flags.ReportPath, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, flags.BackupID))
	default:
		fmt.Fprintln(os.Stderr, "usage: quoin-deploy helm <install|verify|backup|restore> --config <path> --release-manifest <path> [--report <path>] [--offline] [--backup <backup-id>]")
		os.Exit(2)
	}
}

// Flags carries the stable helper flag set shared by the Helm subcommands.
type Flags struct {
	ConfigPath          string
	ReleaseManifestPath string
	ReportPath          string
	Offline             bool
	BackupID            string
}

// Parse returns the parsed flags, exiting 2 on malformed invocations. Every
// exit path writes a minimal invalid-input report first (OPS-HELPER-003).
func Parse(name string, arguments []string) Flags {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "strict helm-install YAML")
	releaseManifestPath := flags.String("release-manifest", "", "release-manifest.json with the digest-pinned images and OCI chart")
	reportPath := flags.String("report", "", "where the atomic verification-report.json is written")
	offline := flags.Bool("offline", false, "stop workloads and run offline fallback")
	backupID := flags.String("backup", "", "published backup identifier for restore")
	parseErr := flags.Parse(arguments)
	invalid := parseErr != nil || *configPath == "" || flags.NArg() != 0
	if !invalid {
		return Flags{ConfigPath: *configPath, ReleaseManifestPath: *releaseManifestPath, ReportPath: *reportPath, Offline: *offline, BackupID: *backupID}
	}
	brief := report.New("helm", fmt.Sprintf("%s/%s", os.Getenv("GOOS"), os.Getenv("GOARCH")), name, *configPath, "")
	brief.MarkFailed("invalid_invocation", "malformed command line", "fix the command line; no deployment side effect has occurred")
	brief.ExitCode = 2
	path := *reportPath
	if path == "" {
		if stateDir, err := deployconfig.StateDirectoryFor("helm"); err == nil {
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
