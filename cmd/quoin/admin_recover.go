package main

// `quoin admin recover` — the offline administrator recovery command
// (docs/authentication-design.md §6). Like the other admin lifecycle
// commands it requires the long-running Quoin to be stopped: OpenDatabase
// takes the exclusive data-directory lock and verifies the release schema,
// so a live server makes the command fail instead of racing it. Passwords
// are read through the attached TTY only; the generated temporary password
// of the factors mode is printed to the attached TTY exactly once and never
// enters logs, environment variables or files. After recovery the service
// starts again and the administrator signs in through the ordinary login
// into the same unified initialization flow as a first install.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"golang.org/x/term"
)

const adminRecoverUsage = "usage: quoin admin recover --config <path> --mode password|factors"

func runAdminRecover(arguments []string) {
	mode, configArguments := recoveryModeArgument(arguments)
	if mode != string(auth.RecoveryModePassword) && mode != string(auth.RecoveryModeFactors) {
		fail(adminRecoverUsage + "; --mode must be password or factors")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fail("administrator recovery requires an attached TTY")
	}
	config := parseConfig(configArguments, "admin recover")
	ctx := context.Background()
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		fail(err.Error())
	}
	defer database.Close()
	service, err := auth.NewService(database.SQL)
	if err != nil {
		fail(err.Error())
	}
	if mode == string(auth.RecoveryModePassword) {
		recoverAdminPassword(ctx, service)
		return
	}
	recoverAdminFactors(ctx, service)
}

// recoveryModeArgument removes --mode before the shared strict config parser
// sees the arguments (same pattern as kubernetesSecretArgument).
func recoveryModeArgument(arguments []string) (string, []string) {
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == "--mode" {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				fail("--mode requires password or factors")
			}
			return arguments[index+1], append(arguments[:index:index], arguments[index+2:]...)
		}
		if strings.HasPrefix(arguments[index], "--mode=") {
			return strings.TrimPrefix(arguments[index], "--mode="), append(arguments[:index:index], arguments[index+1:]...)
		}
	}
	return "", arguments
}

// recoverAdminPassword resets the administrator password to a temporary
// credential chosen through the attached TTY. The administrator then boots the
// service, signs in with it, and completes the same unified initialization
// flow as a first install (formal password plus verified contact).
func recoverAdminPassword(ctx context.Context, service *auth.Service) {
	password := promptPassword("Temporary administrator password: ")
	confirmation := promptPassword("Confirm temporary administrator password: ")
	if password != confirmation {
		fail("passwords do not match")
	}
	if _, err := service.BeginRecovery(ctx, auth.RecoveryModePassword, password); err != nil {
		fail(err.Error())
	}
	fmt.Fprintln(os.Stderr, "Administrator password reset. Start Quoin, sign in with the temporary password, and complete initialization.")
}

// recoverAdminFactors resets every factor and the password. After starting the
// service, the administrator signs in with the temporary password printed here
// and completes the unified initialization flow (formal password plus a new
// verified contact).
func recoverAdminFactors(ctx context.Context, service *auth.Service) {
	credential, err := service.BeginRecovery(ctx, auth.RecoveryModeFactors, "")
	if err != nil {
		fail(err.Error())
	}
	fmt.Fprintf(os.Stdout, "Temporary administrator password (shown only once):\n%s\n", credential.TemporaryPassword)
	fmt.Fprintln(os.Stderr, "All administrator factors were reset. Start Quoin, sign in with the temporary password above, and complete initialization.")
}
