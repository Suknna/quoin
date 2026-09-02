package main

// `quoin maintenance recover-lintel` — the stopped, same-Release one-shot
// command the deployment helper drives through the three offline phases
// (T35, OPS-HELPER-005). serve/finalize hold the exclusive data-directory
// lock for their whole lifetime; hold only keeps a distroless recovery Pod
// alive so the serve phase can run through an attached exec stream.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/maintenance"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

func runLintelRecovery(arguments []string) {
	flags := flag.NewFlagSet("maintenance recover-lintel", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	phase := flags.String("phase", "", "issue | await | finalize | hold")
	backend := flags.String("backend", "", "compose | helm")
	disposition := flags.String("storage-disposition", "", "exclusively_reattached | retired")
	dispositionDigest := flags.String("disposition-digest", "", "sha256 of the storage disposition evidence report")
	fenceReportDigest := flags.String("fence-report-digest", "", "sha256 of the workload fence report")
	recoveryReportDigest := flags.String("recovery-report-digest", "", "sha256 of the recovery report (finalize)")
	postVerifyDigest := flags.String("post-verify-digest", "", "sha256 of the pre-transaction post-verify proof report (finalize)")
	oldGeneration := flags.Int64("old-token-generation", 0, "pin the receipt replay to this old generation (finalize replay)")
	firstAuthTimeout := flags.Duration("first-auth-timeout", 15*time.Minute, "bound for the replacement first Hello (serve)")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	config := parseConfig([]string{"--config", *configPath}, "maintenance recover-lintel")
	ctx, cancel := sharedops.SignalContext()
	defer cancel()

	switch *phase {
	case "hold":
		if err := app.HoldLintelRecovery(ctx); err != nil {
			fail(err.Error())
		}
		return
	case "issue":
		fence := qruntime.LintelRecoveryFence{
			Backend: *backend, Disposition: *disposition,
			DispositionDigest: *dispositionDigest, FenceReportDigest: *fenceReportDigest,
		}
		if err := app.LintelRecoveryIssue(ctx, config, fence, *firstAuthTimeout, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "quoin: recover-lintel issue failed: %v\n", err)
			os.Exit(1)
		}
		return
	case "await":
		fence := qruntime.LintelRecoveryFence{
			Backend: *backend, Disposition: *disposition,
			DispositionDigest: *dispositionDigest, FenceReportDigest: *fenceReportDigest,
		}
		if err := app.LintelRecoveryAwait(ctx, config, fence, *firstAuthTimeout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "quoin: recover-lintel await failed: %v\n", err)
			os.Exit(1)
		}
		return
	case "finalize":
		result, err := maintenance.FinalizeLintelRecovery(context.Background(), maintenance.LintelRecoveryFinalizeRequest{
			DataDirectory: config.DataDirectory, RootKeyFile: config.RootKeyFile, Disposition: *disposition,
			OldGeneration:        *oldGeneration,
			DispositionDigest:    *dispositionDigest,
			FenceReportDigest:    *fenceReportDigest,
			RecoveryReportDigest: *recoveryReportDigest,
			PostVerifyDigest:     *postVerifyDigest,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "quoin: recover-lintel finalize failed: %v\n", err)
			os.Exit(1)
		}
		status := "finalized"
		if result.AlreadyFinalized {
			status = "already_finalized"
		}
		fmt.Fprintf(os.Stderr, "lintel_recovery_finalize=%s maintenance_revision=%d old_generation=%d replacement_generation=%d closed_operations=%d demoted_identities=%d\n",
			status, result.MaintenanceRevision, result.OldGeneration, result.ReplacementGeneration, result.ClosedOperations, result.DemotedIdentities)
		return
	default:
		fmt.Fprintln(os.Stderr, "quoin: --phase must be issue, await, finalize or hold")
		os.Exit(2)
	}
}
