// Package verify runs `quoin-deploy compose verify`: the read-only,
// repeatable operational-surface verifier (OPS-HELPER-004, OPS-VERIFY-003).
package verify

import (
	"os"

	deploycompose "github.com/Suknna/quoin/internal/deploy/compose"
)

// Run executes the verify command with the already-parsed flag surface.
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
