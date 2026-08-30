package main

import (
	"fmt"
	"os"

	"github.com/Suknna/quoin/cmd/quoin-deploy/compose"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "compose" {
		usage()
	}
	compose.Main(os.Args[2], os.Args[3:])
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: quoin-deploy compose <install|verify> --config <path> [--release-manifest <path>] [--report <path>]")
	os.Exit(2)
}
