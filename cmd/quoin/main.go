package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/app"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: quoin serve|admin recover|backup --offline|restore --backup <backup-id>|root-key rebind|migrate [preflight]")
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "admin":
		if len(os.Args) < 3 || os.Args[2] != "recover" {
			fail("usage: quoin admin recover --config <path>")
		}
		runAdminRecover(os.Args[3:])
	case "backup":
		runBackup(os.Args[2:])
	case "restore":
		if len(os.Args) >= 3 && os.Args[2] == "finalize" {
			runRestoreFinalize(os.Args[3:])
		} else {
			runRestore(os.Args[2:])
		}
	case "root-key":
		if len(os.Args) < 3 || os.Args[2] != "rebind" {
			fail("usage: quoin root-key rebind --config <path>")
		}
		runRootKeyRebind(os.Args[3:])
	case "migrate":
		runMigrate(os.Args[2:])
	default:
		fail("usage: quoin serve|admin recover|backup --offline|restore --backup <backup-id>|root-key rebind|migrate [preflight]")
	}
}

func runServe(arguments []string) {
	config := parseConfig(arguments, "serve")
	ctx, cancel := sharedops.SignalContext()
	defer cancel()
	if err := app.Run(ctx, config); err != nil {
		fail(err.Error())
	}
}

func parseConfig(arguments []string, command string) contract.QuoinConfig {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path := flags.String("config", "/etc/quoin/component.yaml", "strict generated component configuration")
	if err := flags.Parse(arguments); err != nil {
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		fail("unexpected positional arguments")
	}
	var config contract.QuoinConfig
	if err := contract.DecodeFile(*path, &config); err != nil {
		fail(err.Error())
	}
	if config.Component != "quoin" {
		fail("configuration component must be quoin")
	}
	return config
}

func promptLine(reader *bufio.Reader, label string) string {
	fmt.Fprint(os.Stderr, label)
	value, err := reader.ReadString('\n')
	if err != nil {
		fail("could not read attached TTY input")
	}
	return strings.TrimSpace(value)
}

func promptPassword(label string) string {
	fmt.Fprint(os.Stderr, label)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fail("could not read password from attached TTY")
	}
	return trimTerminalPassword(value)
}

// trimTerminalPassword removes only the Enter sequence supplied by a terminal;
// spaces remain part of a password and must not be silently normalized.
func trimTerminalPassword(value []byte) string {
	return strings.TrimRight(string(value), "\r\n")
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "quoin:", message)
	os.Exit(1)
}
