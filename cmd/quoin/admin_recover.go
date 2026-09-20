package main

// `quoin admin recover` — the offline administrator recovery command
// (docs/authentication-design.md §6, ADR-0010). Like the other admin
// lifecycle commands it requires the long-running Quoin to be stopped:
// OpenDatabase takes the exclusive data-directory lock and verifies the
// release schema, so a live server makes the command fail instead of racing
// it. Two shapes share one command:
//
//   - The deployment is still pending bootstrap (the initial random password
//     expired or was lost): recover mints a fresh random credential, rewrites
//     the 0600 initial-admin-password file in the data directory and re-arms
//     the 24-hour deadline.
//   - The administrator is initialized but locked out: the operator chooses a
//     temporary password through the attached TTY; no deadline applies.
//
// Passwords never enter logs, environment variables or arguments.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"golang.org/x/term"
)

func runAdminRecover(arguments []string) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fail("administrator recovery requires an attached TTY")
	}
	config := parseConfig(arguments, "admin recover")
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
	// Pure reads fail closed without the read-only pool; wire the same
	// handle production installs before serving.
	if err := service.SetReader(database.Reader); err != nil {
		fail(err.Error())
	}
	pending, err := service.HasPendingBootstrapAdmin(ctx)
	if err != nil {
		fail(err.Error())
	}
	if pending {
		rearmInitialPassword(ctx, service, config.DataDirectory)
		return
	}
	recoverAdminPassword(ctx, service)
}

// rearmInitialPassword regenerates the bootstrap credential file path: a
// fresh random password, the same 0600 file and a new 24-hour deadline on
// the users row.
func rearmInitialPassword(ctx context.Context, service *auth.Service, dataDirectory string) {
	password, err := auth.GenerateInitialPassword()
	if err != nil {
		fail(err.Error())
	}
	path := filepath.Join(dataDirectory, "initial-admin-password")
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(password+"\n"), 0o600); err != nil {
		fail(fmt.Sprintf("write %s: %v", temporary, err))
	}
	if err := os.Rename(temporary, path); err != nil {
		fail(fmt.Sprintf("publish %s: %v", path, err))
	}
	deadline := time.Now().UTC().Add(auth.InitialPasswordLifetime)
	if _, err := service.BeginRecovery(ctx, password, &deadline); err != nil {
		fail(err.Error())
	}
	fmt.Fprintf(os.Stdout, "Initial administrator password regenerated (shown only once):\n%s\n", password)
	fmt.Fprintln(os.Stderr, "File: "+path)
	fmt.Fprintln(os.Stderr, "Deadline: 24 hours. Start Quoin, sign in, and set the formal password.")
}

// recoverAdminPassword resets the initialized administrator's password to a
// temporary credential chosen through the attached TTY. The administrator
// then boots the service, signs in with it and completes the forced password
// change.
func recoverAdminPassword(ctx context.Context, service *auth.Service) {
	password := promptPassword("Temporary administrator password: ")
	confirmation := promptPassword("Confirm temporary administrator password: ")
	if password != confirmation {
		fail("passwords do not match")
	}
	if _, err := service.BeginRecovery(ctx, password, nil); err != nil {
		fail(err.Error())
	}
	fmt.Fprintln(os.Stderr, "Administrator password reset. Start Quoin, sign in with the temporary password, and set the formal password.")
}
