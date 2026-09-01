package main

import (
	"context"

	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
)

// runBackup is deliberately an offline-only command: OpenDatabase takes the
// data-directory lock, so it refuses to race a running Quoin process.
func runBackup(arguments []string) {
	offline := false
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument == "--offline" {
			offline = true
		} else {
			filtered = append(filtered, argument)
		}
	}
	if !offline {
		fail("usage: quoin backup --offline --config <path>")
	}
	config := parseConfig(filtered, "backup")
	database, err := bootstrap.OpenDatabase(context.Background(), config.DataDirectory, config.RootKeyFile)
	if err != nil {
		fail(err.Error())
	}
	defer database.Close()
	service, err := backup.NewService(database.SQL, backup.Config{DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory, ArtifactDirectory: config.DataDirectory + "/artifacts"})
	if err != nil {
		fail(err.Error())
	}
	// OpenDatabase holds the same data lock as Quoin. Reconcile only after it
	// succeeds, so abandoned active rows are closed by the exclusive owner.
	if err = service.Reconcile(context.Background()); err != nil {
		fail(err.Error())
	}
	if _, err = service.RunOffline(context.Background()); err != nil {
		fail(err.Error())
	}
}
