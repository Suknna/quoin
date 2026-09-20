package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
)

// runMigrate implements `quoin migrate [preflight] --config <path>`: the
// coordinated-upgrade schema gate. The first release has no predecessor
// conversions: preflight verifies the database is exactly the current
// canonical schema, and the plain form only re-verifies that fact and exits
// an active Upgrade maintenance window. Any divergent digest belongs to an
// unreleased build and is rejected with no migration path.
func runMigrate(arguments []string) {
	preflight := len(arguments) > 0 && arguments[0] == "preflight"
	if preflight {
		arguments = arguments[1:]
	}
	config, options := parseMigrateArguments(arguments, "migrate")
	ctx := context.Background()
	if preflight {
		if version, digest, ok := bootstrap.PeekSchemaState(config.DataDirectory); ok {
			if reason, mismatch := schemaMismatchReason(version, digest); mismatch {
				failStable(errors.New(reason), reason)
			}
		}
	}
	// The migration command alone may open a root-key-authenticated legacy
	// database. Normal bootstrap deliberately rejects it until this exclusive
	// upgrade completes, so an old schema never serves application traffic.
	database, err := bootstrap.OpenMigrationDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		failStable(err, "schema_open_failed")
	}
	defer database.Close()
	if preflight {
		result, err := upgrade.PreflightWithOptions(ctx, database.SQL, options)
		if err != nil {
			failStable(err, stableCode(err))
		}
		emitMigrateSummary("preflight-verified", result)
		return
	}
	result, err := upgrade.MigrateWithOptions(ctx, database.SQL, options)
	if err != nil {
		failStable(err, stableCode(err))
	}
	emitMigrateSummary("migrated", result)
}

// parseMigrateArguments is the migrate-only flag surface. It duplicates
// parseConfig on purpose: that helper belongs to main.go's shared commands and
// must not grow migration-specific flags.
func parseMigrateArguments(arguments []string, command string) (contract.QuoinConfig, upgrade.Options) {
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
	return config, upgrade.Options{}
}

func schemaMismatchReason(version, digest string) (string, bool) {
	if version != "v1" {
		return "unsupported_schema_version", true
	}
	expected := sha256.Sum256([]byte(gencontracts.SchemaSQL))
	if digest != hex.EncodeToString(expected[:]) {
		return "schema_digest_mismatch", true
	}
	return "", false
}

func stableCode(err error) string {
	switch {
	case errors.Is(err, upgrade.ErrUnsupportedSchema):
		return "unsupported_schema_version"
	case errors.Is(err, upgrade.ErrSchemaDigestMismatch):
		return "schema_digest_mismatch"
	case errors.Is(err, upgrade.ErrSchemaHistoryPresent):
		return "schema_history_present"
	case errors.Is(err, upgrade.ErrNotUpgradeMaintenance):
		return "upgrade_maintenance_not_active"
	case errors.Is(err, upgrade.ErrChecklistBlocking):
		return "upgrade_checklist_blocking"
	case errors.Is(err, upgrade.ErrNoUpgradeBackup):
		return "upgrade_backup_missing"
	default:
		return "schema_open_failed"
	}
}

func emitMigrateSummary(stage string, result upgrade.PreflightResult) {
	body := map[string]any{"stage": stage, "release": buildinfo.Release, "maintenanceRevision": result.Revision, "backupId": result.BackupID, "manifestSha256": result.ManifestSHA256, "schemaVersion": result.SchemaVersion, "migrationHistory": result.MigrationHistory}
	serialized, err := json.Marshal(body)
	if err != nil {
		failStable(err, "schema_open_failed")
	}
	fmt.Println(string(serialized))
}

// failStable exits non-zero with the machine-readable stable code; the
// deployment helper records this exact token in its report. The underlying
// error travels on stderr for the operator transcript without changing the
// stable code contract.
func failStable(err error, code string) {
	sharedops.LogEvent("quoin", "error", "migrate."+code, fmt.Sprintf("upgrade schema gate rejected the database: %v", err))
	fmt.Fprintf(os.Stderr, "quoin migrate: %s: %v\n", code, err)
	os.Exit(1)
}
