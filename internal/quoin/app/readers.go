package app

import (
	"context"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/alerts"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/businessview"
	"github.com/Suknna/quoin/internal/quoin/execution"
	"github.com/Suknna/quoin/internal/quoin/feedback"
	"github.com/Suknna/quoin/internal/quoin/knowledge"
	qruntime "github.com/Suknna/quoin/internal/quoin/runtime"
)

// readAuthority serves pure read routes from the injected trusted read-only
// pool. Unwired it returns the zero-value execution.Reader: query-only by
// type and every statement fails closed — the writer database is never a
// read fallback.
func (application *apiServer) readAuthority() audit.Reader {
	if application.readerWired {
		return application.reader
	}
	return execution.Reader{}
}

func (application *apiServer) configureReadOnly(reader execution.Reader) error {
	// The pool must be alive before anything binds to it: a dead trusted
	// reader is a wiring failure, not a silent degradation.
	if err := reader.PingContext(context.Background()); err != nil {
		return fmt.Errorf("read-only pool unreachable: %w", err)
	}
	// The trusted execution.Reader type is the only accepted source; the
	// runner re-validates and fails closed when unwired.
	runner := execution.NewRunner(application.db, execution.NewRegistry(), audit.NewWriter())
	if err := runner.SetReader(reader); err != nil {
		return err
	}
	// The maintenance boot's readiness/about surfaces read through the same
	// trusted pool, and the upgrade legacy-replay authority reads its
	// history through it as well.
	if application.maintenance != nil {
		if err := application.maintenance.SetReader(reader); err != nil {
			return err
		}
	}
	if application.upgradeService != nil {
		if err := application.upgradeService.SetReader(reader); err != nil {
			return err
		}
	}
	views, err := businessview.NewServiceWithReader(reader, runner)
	if err != nil {
		return err
	}
	feedbackService, err := feedback.NewServiceWithReader(reader, runner)
	if err != nil {
		return err
	}
	knowledgeService, err := knowledge.NewServiceWithReader(reader, application.db, runner)
	if err != nil {
		return err
	}
	// Alert-source management and the runtime slot authority join the shared
	// runner registry and the real read-only query surface; the write
	// authority stays application.db for their runner transactions and the
	// not-yet-migrated ingestion paths.
	alertService, err := alerts.NewServiceWithReader(application.db, reader, runner)
	if err != nil {
		return err
	}
	slotService := qruntime.NewService()
	if err := application.auth.SetReader(reader); err != nil {
		return err
	}
	if err := application.analyses.SetReader(reader); err != nil {
		return err
	}
	if err := application.investigations.SetReader(reader); err != nil {
		return err
	}
	if err := application.inspections.SetReader(reader); err != nil {
		return err
	}
	if err := application.observations.SetReader(reader); err != nil {
		return err
	}
	if err := application.connections.SetReader(reader); err != nil {
		return err
	}
	application.systems.SetReader(reader)
	if application.backups != nil {
		if err := application.backups.SetReader(reader); err != nil {
			return err
		}
	}
	application.knowledgeService = knowledgeService
	application.views = views
	application.feedbackService = feedbackService
	application.alerts = alertService
	application.platformFaults = alerts.NewPlatformFaultReporter(alertService)
	application.runtime = slotService
	// Everything above succeeded: only now does the wired state go live.
	application.reader = reader
	application.readerWired = true
	return nil
}

// SetReadOnlyReader installs the trusted read-only pool from external
// composition (test harnesses and future exported wiring): the accepted
// type is the opaque execution.Reader produced by execution.OpenReadOnly.
func (application *apiServer) SetReadOnlyReader(reader execution.Reader) error {
	return application.configureReadOnly(reader)
}
