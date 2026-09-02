package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/recovery"
	"golang.org/x/term"
)

// runRestore is intentionally an offline command. Recovery acquires the same
// data lock as Quoin before reading the backup or changing live files, so it
// cannot race a running service. The temporary password is read only from the
// attached TTY and never reaches argv, environment, logs, or reports.
func runRestore(arguments []string) {
	if len(arguments) > 0 && arguments[0] == "preflight" {
		runRestorePreflight(arguments[1:])
		return
	}
	if len(arguments) > 0 && arguments[0] == "continue" {
		runRestoreContinue(arguments[1:])
		return
	}
	backupID, configArguments := restoreBackupArgument(arguments)
	if backupID == "" {
		fail("usage: quoin restore --backup <backup-id> --config <path>")
	}
	config := parseConfig(configArguments, "restore")
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fail("restore requires an attached TTY for recovery administrator selection and temporary password")
	}
	reader := bufio.NewReader(os.Stdin)
	adminUsername := promptRestoreLine(reader, "Recovery administrator username: ")
	password := promptRestorePassword("Temporary password: ")
	confirmation := promptRestorePassword("Confirm temporary password: ")
	if password != confirmation {
		fail("passwords do not match")
	}
	result, err := recovery.Restore(context.Background(), recovery.Request{
		DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, BackupID: backupID,
		RootKeyFile: config.RootKeyFile, AdminUsername: adminUsername, TemporaryPassword: password,
		RollbackDirectory: ".restore-rollback-" + backupID,
	})
	if err != nil {
		fail(err.Error())
	}
	sharedops.LogEvent("quoin", "info", "restore.completed", "maintenance revision="+strconv.FormatInt(result.MaintenanceRevision, 10)+" rollback="+filepath.Base(result.RollbackDirectory))
}

// Restore prompts use stdout because Kubernetes TTY attach relays the terminal
// output stream there. Password bytes remain read from the raw terminal and are
// never written back to either stream.
func promptRestoreLine(reader *bufio.Reader, label string) string {
	fmt.Fprint(os.Stdout, label)
	value, err := reader.ReadString('\n')
	if err != nil {
		fail("could not read attached TTY input")
	}
	return strings.TrimSpace(value)
}

func promptRestorePassword(label string) string {
	fmt.Fprint(os.Stdout, label)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	if err != nil {
		fail("could not read password from attached TTY")
	}
	return trimTerminalPassword(value)
}

func runRestorePreflight(arguments []string) {
	backupID, configArguments := restoreBackupArgument(arguments)
	if backupID == "" {
		fail("usage: quoin restore preflight --backup <backup-id> --config <path>")
	}
	config := parseConfig(configArguments, "restore preflight")
	result, err := recovery.Preflight(config.BackupDirectory, backupID)
	if err != nil {
		fail(err.Error())
	}
	fmt.Printf("backup=%s release=%s manifest_sha256=%s\n", result.BackupID, result.Release, result.ManifestSHA256)
}

func runRestoreContinue(arguments []string) {
	backupID, configArguments := restoreBackupArgument(arguments)
	if backupID == "" {
		fail("usage: quoin restore continue --backup <backup-id> --config <path>")
	}
	config := parseConfig(configArguments, "restore continue")
	result, err := recovery.Continue(context.Background(), recovery.Request{
		DataDirectory: config.DataDirectory, BackupID: backupID, RootKeyFile: config.RootKeyFile,
		RollbackDirectory: ".restore-rollback-" + backupID,
	})
	if errors.Is(err, recovery.ErrNoContinuation) {
		fmt.Println("restore_continuation=absent")
		os.Exit(3)
	}
	if err != nil {
		fail(err.Error())
	}
	fmt.Printf("restore_continuation=published maintenance_revision=%d rollback=%s\n", result.MaintenanceRevision, filepath.Base(result.RollbackDirectory))
}

func runRestoreFinalize(arguments []string) {
	rollback, configArguments := restoreRollbackArgument(arguments)
	if rollback == "" {
		fail("usage: quoin restore finalize --rollback <directory> --config <path>")
	}
	config := parseConfig(configArguments, "restore finalize")
	if err := recovery.Finalize(config.DataDirectory, rollback); err != nil {
		fail(err.Error())
	}
}

func restoreRollbackArgument(arguments []string) (string, []string) {
	filtered := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--rollback" {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				return "", nil
			}
			return arguments[index+1], append(filtered, arguments[index+2:]...)
		}
		if strings.HasPrefix(argument, "--rollback=") {
			return strings.TrimPrefix(argument, "--rollback="), append(filtered, arguments[index+1:]...)
		}
		filtered = append(filtered, argument)
	}
	return "", filtered
}

func restoreBackupArgument(arguments []string) (string, []string) {
	filtered := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--backup" {
			if index+1 >= len(arguments) || arguments[index+1] == "" {
				return "", nil
			}
			return arguments[index+1], append(filtered, arguments[index+2:]...)
		}
		if strings.HasPrefix(argument, "--backup=") {
			return strings.TrimPrefix(argument, "--backup="), append(filtered, arguments[index+1:]...)
		}
		filtered = append(filtered, argument)
	}
	return "", filtered
}
