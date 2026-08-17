package main

import (
	"flag"
	"fmt"
	"os"

	sharedops "github.com/Suknna/quoin/internal/ops"
	steleops "github.com/Suknna/quoin/internal/stele/ops"
)

func main() {
	configPath := flag.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	flag.Parse()
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := steleops.Run(ctx, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "stele:", err)
		os.Exit(1)
	}
}
