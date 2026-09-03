package app

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/backup"
	"github.com/Suknna/quoin/internal/quoin/upgrade"
	"path/filepath"
)

// backupUpgradeRunner adapts the backup authority to the reconciler's
// execution seam. The reconciler creates the queued upgrade row itself
// inside its projection transaction; only the stage-machine execution is
// delegated.
type backupUpgradeRunner struct{ service *backup.Service }

func (runner *backupUpgradeRunner) RunUpgrade(ctx context.Context, id int64) error {
	_, err := runner.service.Run(ctx, strconv.FormatInt(id, 10))
	return err
}

// maintenanceAdmissionChecker is the SQL-authoritative scheduling gate: any
// active maintenance revision stops new scheduled work (OPS-UPGRADE-003).
func maintenanceAdmissionChecker(ctx context.Context, db *sql.DB) func(context.Context) bool {
	return func(queryContext context.Context) bool {
		var active int
		if err := db.QueryRowContext(queryContext, `SELECT active FROM maintenance_state WHERE id=1`).Scan(&active); err != nil {
			// Fail closed: an unreadable gate must not admit new work.
			return true
		}
		return active == 1
	}
}

// startUpgradeMaintenanceRuntime equips a maintenance-mode boot inside an
// active Upgrade revision with the durable convergence machinery: the drain
// checklist reconciler with its verified pre-upgrade backup, and a
// dispatch-free lease sweeper that closes attempts whose runtime disappeared
// with the previous process.
func (application *apiServer) startUpgradeMaintenanceRuntime(ctx context.Context, config contract.QuoinConfig, serverSet *servers) error {
	artifactStore, err := artifact.NewStore(application.db, filepath.Join(config.DataDirectory, "artifacts"))
	if err != nil {
		return err
	}
	backupService, err := backup.NewService(application.db, backup.Config{
		DataDirectory: config.DataDirectory, BackupDirectory: config.BackupDirectory,
		ArtifactDirectory: filepath.Join(config.DataDirectory, "artifacts"), ArtifactStore: artifactStore,
		// No scheduled backup can admit inside maintenance; the reconciler's
		// upgrade run is the only legitimate backup in this window.
		ScheduleAdmission: func() bool { return false },
	})
	if err != nil {
		return err
	}
	application.upgradeReconciler = upgrade.NewReconciler(application.db, &backupUpgradeRunner{service: backupService})
	projector, err := serverSet.ops.UpgradePreparedProjector()
	if err != nil {
		return err
	}
	application.upgradeReconciler.SetPrepared(projector)
	go application.upgradeReconciler.Run(ctx)
	sweeper := &RuntimeService{
		Slots: application.runtime, Analyses: application.analyses,
		Investigations: application.investigations, Knowledge: application.knowledgeService,
		BusinessSystems: application.systems, Inspections: application.inspections,
		Browsers: application.browsers,
	}
	go sweeper.RunLeaseSweeper(ctx)
	sharedops.LogEvent("quoin", "info", "upgrade.maintenance_boot", "durable upgrade reconciliation started")
	return nil
}
