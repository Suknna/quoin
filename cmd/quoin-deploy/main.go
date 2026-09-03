package main

import (
	"fmt"
	"os"

	"github.com/Suknna/quoin/cmd/quoin-deploy/compose"
	"github.com/Suknna/quoin/cmd/quoin-deploy/helm"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	switch os.Args[1] {
	case "compose":
		compose.Main(os.Args[2], os.Args[3:])
	case "helm":
		helm.Main(os.Args[2], os.Args[3:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: quoin-deploy <compose|helm> <install|upgrade|verify|backup|restore|recover-lintel> --config <path> [--release-manifest <path>] [--report <path>]")
	os.Exit(2)
}
