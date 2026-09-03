package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/buildinfo"
	gencontracts "github.com/Suknna/quoin/internal/gen/contracts"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/bootstrap"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
)

// runMigrate implements `quoin migrate [preflight] --config <path>`: the
// coordinated-upgrade schema gate. preflight is the same-Release read-only
// verification the deployment helper runs on the OLD image after stopping
// the stack; the plain form is the exclusive forward step run by the NEW
// image. Both enforce the first-release boundary: only an exact zero-history
// fresh-v1 schema may pass, and no N-1 migration evidence is ever invented.
func runMigrate(arguments []string) {
	preflight := len(arguments) > 0 && arguments[0] == "preflight"
	if preflight {
		arguments = arguments[1:]
	}
	config := parseConfig(arguments, "migrate")
	ctx := context.Background()
	if version, digest, ok := bootstrap.PeekSchemaState(config.DataDirectory); ok {
		if reason, mismatch := schemaMismatchReason(version, digest); mismatch {
			failStable(errors.New(reason), reason)
		}
	}
	database, err := bootstrap.OpenDatabase(ctx, config.DataDirectory, config.RootKeyFile)
	if err != nil {
		failStable(err, "schema_open_failed")
	}
	defer database.Close()
	if preflight {
		result, err := upgrade.Preflight(ctx, database.SQL)
		if err != nil {
			failStable(err, stableCode(err))
		}
		emitMigrateSummary("preflight-verified", result)
		return
	}
	result, err := upgrade.Migrate(ctx, database.SQL)
	if err != nil {
		failStable(err, stableCode(err))
	}
	emitMigrateSummary("migrated", result)
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
	body, err := json.Marshal(map[string]any{"stage": stage, "release": buildinfo.Release, "maintenanceRevision": result.Revision, "backupId": result.BackupID, "manifestSha256": result.ManifestSHA256, "schemaVersion": result.SchemaVersion, "migrationHistory": result.MigrationHistory})
	if err != nil {
		failStable(err, "schema_open_failed")
	}
	fmt.Println(string(body))
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
