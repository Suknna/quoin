package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/app"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: quoin serve|secrets bootstrap|admin create|backup --offline|restore --backup <backup-id>")
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "secrets":
		if len(os.Args) < 3 || os.Args[2] != "bootstrap" {
			fail("usage: quoin secrets bootstrap --config <path>")
		}
		runSecrets(os.Args[3:])
	case "admin":
		if len(os.Args) < 3 || os.Args[2] != "create" {
			fail("usage: quoin admin create --config <path>")
		}
		runAdmin(os.Args[3:])
	case "backup":
		runBackup(os.Args[2:])
	case "restore":
		if len(os.Args) >= 3 && os.Args[2] == "finalize" {
			runRestoreFinalize(os.Args[3:])
		} else {
			runRestore(os.Args[2:])
		}
	default:
		fail("usage: quoin serve|secrets bootstrap|admin create|backup --offline|restore --backup <backup-id>")
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

func runSecrets(arguments []string) {
	secretName, configArguments := kubernetesSecretArgument(arguments)
	config := parseConfig(configArguments, "secrets bootstrap")
	created, err := bootstrap.BootstrapSecrets(config)
	if err != nil {
		fail(err.Error())
	}
	if secretName != "" {
		if err := bootstrap.PublishKubernetesSecret(config, secretName); err != nil {
			fail(err.Error())
		}
	}
	if created {
		sharedops.LogEvent("quoin", "info", "secrets.bootstrap.created", "deployment secret set created")
	} else {
		sharedops.LogEvent("quoin", "info", "secrets.bootstrap.already_valid", "existing deployment secret set verified")
	}
}

// kubernetesSecretArgument removes the bootstrap Job-only destination flag
// before the shared strict component-config parser sees its arguments.
func kubernetesSecretArgument(arguments []string) (string, []string) {
	filtered := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == "--kubernetes-secret" {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				fail("--kubernetes-secret requires a fixed Secret name")
			}
			return arguments[index+1], append(filtered, arguments[index+2:]...)
		}
		if strings.HasPrefix(arguments[index], "--kubernetes-secret=") {
			return strings.TrimPrefix(arguments[index], "--kubernetes-secret="), append(filtered, arguments[index+1:]...)
		}
		filtered = append(filtered, arguments[index])
	}
	return "", filtered
}

func runAdmin(arguments []string) {
	config := parseConfig(arguments, "admin create")
	ctx := context.Background()
	// Read-only existence probe first: when an administrator already exists
	// (for example a Compose dependency rerun against a live Quoin), report
	// and exit without ever touching the exclusive data-directory lock.
	if bootstrap.PeekHasUsers(config.DataDirectory) {
		sharedops.LogEvent("quoin", "info", "admin.bootstrap.already_exists", "administrator presence confirmed; creation skipped")
		return
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		fail(err.Error())
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		fail(err.Error())
	}
	exists, err := service.HasUsers(ctx)
	if err != nil {
		fail(err.Error())
	}
	if exists {
		sharedops.LogEvent("quoin", "info", "admin.bootstrap.already_exists", "administrator presence confirmed; creation skipped")
		return
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fail("first administrator creation requires an attached TTY")
	}
	reader := bufio.NewReader(os.Stdin)
	username := promptLine(reader, "Username: ")
	displayName := promptLine(reader, "Display name: ")
	password := promptPassword("Temporary password: ")
	confirmation := promptPassword("Confirm temporary password: ")
	if password != confirmation {
		fail("passwords do not match")
	}
	created, err := service.CreateFirstAdmin(ctx, username, displayName, password)
	if err != nil {
		fail(err.Error())
	}
	if !created {
		sharedops.LogEvent("quoin", "info", "admin.bootstrap.already_exists", "administrator presence confirmed; creation skipped")
		return
	}
	sharedops.LogEvent("quoin", "info", "admin.bootstrap.created", "first administrator created; password change required at first login")
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
