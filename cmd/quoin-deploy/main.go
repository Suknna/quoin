package main

import (
	"fmt"
	"os"

	"github.com/Suknna/quoin/cmd/quoin-deploy/compose"
	"github.com/Suknna/quoin/cmd/quoin-deploy/kubernetes"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	switch os.Args[1] {
	case "compose":
		compose.Main(os.Args[2], os.Args[3:])
	case "kubernetes":
		kubernetes.Main(os.Args[2], os.Args[3:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: quoin-deploy <compose|kubernetes> <command>; Kubernetes supports only catalog-driven verify --suite <name> --phase <phase>")
	os.Exit(2)
}
