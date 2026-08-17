package main

import (
	"flag"
	"fmt"
	"os"

	sharedops "github.com/Suknna/quoin/internal/ops"
	plinthops "github.com/Suknna/quoin/internal/plinth/ops"
)

func main() {
	configPath := flag.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	flag.Parse()
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := plinthops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "plinth:", err)
		os.Exit(1)
	}
}
