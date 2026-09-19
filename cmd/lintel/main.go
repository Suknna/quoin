package main

import (
	"flag"
	"fmt"
	"os"

	lintelops "github.com/Suknna/quoin/internal/lintel/ops"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

func main() {
	args := os.Args[1:]
	flags := flag.NewFlagSet("lintel", flag.ExitOnError)
	configPath := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "lintel:", err)
		os.Exit(1)
	}
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := lintelops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "lintel:", err)
		os.Exit(1)
	}
}
