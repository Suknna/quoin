// Package install runs `quoin-deploy compose install`: the staged, retryable
// Compose installation (OPS-HELPER-002/003, OPS-PACKAGE-003).
package install

import (
	"os"

	deploycompose "github.com/Suknna/quoin/internal/deploy/compose"
)

// Run executes the install command with the already-parsed flag surface.
func Run(configPath, releaseManifestPath, reportPath string) {
	os.Exit(deploycompose.Install(deploycompose.Request{
		ConfigPath:          configPath,
		ReleaseManifestPath: releaseManifestPath,
		ReportPath:          reportPath,
		Stdin:               os.Stdin,
		Stdout:              os.Stdout,
		Stderr:              os.Stderr,
	}))
}
