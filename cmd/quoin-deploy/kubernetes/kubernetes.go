// Package kubernetes dispatches qualification verification phases for the
// ordinary kubectl manifest backend. It deliberately has no installer, backup,
// restore, upgrade, or recovery DSL: those lifecycle operations are performed
// by the native Stack against the real namespace and manifest.
package kubernetes

import (
	"fmt"
	"os"

	composecmd "github.com/Suknna/quoin/cmd/quoin-deploy/compose"
	"github.com/Suknna/quoin/cmd/quoin-deploy/verify"
)

// Main accepts the catalog's Kubernetes verify phases. The suite coordinator
// owns their lifecycle and invokes the native Stack rather than recursing into
// a second deployment helper implementation.
func Main(command string, arguments []string) {
	if command != "verify" {
		fmt.Fprintln(os.Stderr, "usage: quoin-deploy kubernetes verify --suite <name> --phase <phase> [--config <path>] [--release-manifest <path>]; restore and recover-lintel are catalog-routed through verify --suite")
		os.Exit(2)
	}
	flags := composecmd.Parse("kubernetes verify", arguments)
	if flags.Suite == "" {
		fmt.Fprintln(os.Stderr, "quoin-deploy: kubernetes verify requires --suite; operational verification is performed by the native qualification stack")
		os.Exit(2)
	}
	os.Exit(verify.RunSuite("kubernetes", verify.SuiteFlags{
		Suite: flags.Suite, Phase: flags.Phase, Config: flags.ConfigPath, Manifest: flags.ReleaseManifestPath,
	}))
}
