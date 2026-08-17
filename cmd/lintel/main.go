package main

import (
	"flag"
	"fmt"
	"os"

	lintelops "github.com/Suknna/quoin/internal/lintel/ops"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

func main() {
	configPath := flag.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	flag.Parse()
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := lintelops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "lintel:", err)
		os.Exit(1)
	}
}
