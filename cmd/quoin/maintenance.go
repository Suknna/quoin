package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/quoin/maintenance"
	"golang.org/x/term"
)

// runRootKeyRebind accepts no key bytes through argv, stdin or environment.
// The operator replaces the deployment-mounted root-key file before invoking
// this stopped, exclusive command and confirms the irreversible consequence on
// an attached TTY.
func runRootKeyRebind(arguments []string) {
	config := parseConfig(arguments, "root-key rebind")
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fail("root-key rebind requires an attached TTY for confirmation")
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintln(os.Stdout, "This binds Quoin to the replacement root key and permanently makes existing connection credential ciphertext unreadable.")
	fmt.Fprint(os.Stdout, "Type REBIND to continue: ")
	confirmation, err := reader.ReadString('\n')
	if err != nil {
		fail("could not read attached TTY confirmation")
	}
	if trimTerminalPassword([]byte(confirmation)) != "REBIND" {
		fail("root-key rebind cancelled")
	}
	result, err := maintenance.RebindRootKey(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		fail(err.Error())
	}
	status := "applied"
	if result.AlreadyRebound {
		status = "already_applied"
	}
	fmt.Printf("root_key_rebind=%s binding_revision=%d maintenance_revision=%d connections=%d\n", status, result.BindingRevision, result.MaintenanceRevision, result.ConnectionCount)
}
