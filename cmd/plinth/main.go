package main

import (
	"flag"
	"fmt"
	"os"

	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthops "github.com/Suknna/quoin/internal/plinth/ops"
	"github.com/Suknna/quoin/internal/plinth/worker"
)

// main dispatches the one-shot `register` subcommand (attached-stdin
// one-time registration), the per-attempt `worker` subcommand (spawned by
// the supervisor, ARCH-WORKER-001) and the long-lived serve path. The
// subcommand may appear before flags, so parsing restarts after it.
func main() {
	args := os.Args[1:]
	register := false
	workerMode := false
	if len(args) > 0 && args[0] == "register" {
		register = true
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "worker" {
		workerMode = true
		args = args[1:]
	}
	if workerMode {
		flags := flag.NewFlagSet("plinth-worker", flag.ExitOnError)
		workspace := flags.String("workspace", "", "attempt workspace directory (only writable tree)")
		supervisorPID := flags.Int("supervisor-pid", 0, "supervisor pid for the isolation self-check")
		if err := flags.Parse(args); err != nil {
			fmt.Fprintln(os.Stderr, "plinth-worker:", err)
			os.Exit(1)
		}
		if *workspace == "" || *supervisorPID <= 0 {
			fmt.Fprintln(os.Stderr, "plinth-worker: --workspace and --supervisor-pid are required")
			os.Exit(1)
		}
		ctx, cancel := sharedops.SignalContext()
		defer cancel()
		if err := worker.Run(ctx, worker.Config{
			WorkspaceDir: *workspace, SupervisorPID: *supervisorPID,
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "plinth-worker:", err)
			os.Exit(1)
		}
		return
	}
	flags := flag.NewFlagSet("plinth", flag.ExitOnError)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "plinth:", err)
		os.Exit(1)
	}
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	var err error
	if register {
		err = plinthops.RunRegister(ctx, *configPath)
	} else {
		err = plinthops.Run(ctx, *configPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plinth:", err)
		os.Exit(1)
	}
}
