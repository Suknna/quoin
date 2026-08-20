package main

import (
	"flag"
	"fmt"
	"os"

	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthops "github.com/Suknna/quoin/internal/plinth/ops"
)

// main dispatches the one-shot `register` subcommand (attached-stdin
// one-time registration) from the long-lived serve path. The subcommand
// may appear before flags, so parsing restarts after it.
func main() {
	args := os.Args[1:]
	register := false
	if len(args) > 0 && args[0] == "register" {
		register = true
		args = args[1:]
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
