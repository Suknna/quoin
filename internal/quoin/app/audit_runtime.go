package app

// Audit retention cleanup runtime (docs/audit-design.md §7): the startup loop
// that drives the audit retention Controller. This file owns only WHEN a
// cleanup pass is attempted and under which execution identity; every delete
// decision stays inside the controller and the schema's permit/cutoff guards.
// Each pass restores an explicit system execution context on its own scope —
// admission-style metadata with the system principal (id 0, never a user) —
// and maps that identity into the acting record the controller requires,
// because audit cannot read execution's context (execution imports audit), so
// the scheduled root's system identity is passed explicitly and never
// defaulted anywhere.
//
// No initialization gate is needed here: bootstrap.initializeDatabase seeds
// the retention singleton and cleanup permit atomically at database creation,
// and a freshly bootstrapped deployment simply holds no expired events — a
// pass right after bootstrap is always safe and typically a no-op.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// auditCleanupInterval is how often a cleanup pass is attempted after the
// initial startup pass. Failed or disabled passes are re-evaluated on the next
// tick; the retention controller and schema guards make every pass safe to
// retry.
const auditCleanupInterval = 24 * time.Hour

// StartAuditCleanup launches the retention cleanup loop and returns
// immediately. Call it once at normal-process startup with the long-lived
// process context; maintenance surfaces never start it.
func StartAuditCleanup(ctx context.Context, db *sql.DB) {
	go runAuditCleanup(ctx, db)
}

// runAuditCleanup attempts one cleanup pass immediately (so an armed backlog
// drains without waiting a day) and then once per interval until ctx is
// cancelled. The loop keeps no state: a failed pass is reported and the next
// pass retries through the controller's own permit/batching recovery.
func runAuditCleanup(ctx context.Context, db *sql.DB) {
	controller, err := audit.NewController(db, audit.NewWriter(), nil)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "audit.cleanup_controller_failed", err.Error())
		return
	}
	for {
		runAuditCleanupPass(ctx, controller)
		select {
		case <-ctx.Done():
			return
		case <-time.After(auditCleanupInterval):
		}
	}
}

// runAuditCleanupPass attempts one retention cleanup run. A disabled cleanup
// flag is the configured state (existing history stays safe), not a failure;
// anything else surfaces a bounded ops-log diagnostic while the durable
// last_failure_at/last_error_code columns carry the admin-visible outcome.
func runAuditCleanupPass(ctx context.Context, controller *audit.Controller) {
	passCtx, acting, err := auditCleanupPassScope(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "audit.cleanup_context_failed", err.Error())
		return
	}
	result, err := controller.Cleanup(passCtx, acting)
	switch {
	case err == nil:
		if result.Batches > 0 {
			sharedops.LogEvent("quoin", "info", "audit.retention_cleanup_completed",
				fmt.Sprintf("cutoff=%s batches=%d deletedEvents=%d deletedTargets=%d",
					result.CutoffAt, result.Batches, result.DeletedEvents, result.DeletedTargets))
		}
	case errors.Is(err, audit.ErrCleanupDisabled):
		// cleanup_enabled=0 keeps all history; quietly wait for the next tick.
	case errors.Is(err, audit.ErrCleanupRecordRequired):
		// The scope contract of this file was violated — a wiring bug, never a
		// transient condition; audit refused to invent the identity.
		sharedops.LogEvent("quoin", "error", "audit.cleanup_record_rejected", err.Error())
	default:
		sharedops.LogEvent("quoin", "error", "audit.retention_cleanup_failed", err.Error())
	}
}

// auditCleanupPassScope builds one pass's execution scope: an explicit system
// execution context plus the acting audit record mapped from the very same
// metadata — system principal as actor and initiator (id 0, no user exists
// behind a cleanup run), the scheduler source kind and a fresh per-pass
// correlation shared by the context and every batch record of the run.
// ReplaceMetadata is deliberate: the loop scope is a background task context,
// so re-rooting must work even if a future caller hands in a context that
// already carries metadata, and it must never inherit that identity.
func auditCleanupPassScope(ctx context.Context) (context.Context, audit.Record, error) {
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, audit.Record{}, fmt.Errorf("create audit cleanup correlation: %w", err)
	}
	system := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	metadata := execution.Metadata{
		CorrelationID: correlation,
		Actor:         system,
		Initiator:     system,
		Source:        execution.Source{Kind: execution.SourceScheduler},
	}
	passCtx, err := execution.ReplaceMetadata(ctx, metadata)
	if err != nil {
		return nil, audit.Record{}, fmt.Errorf("restore audit cleanup metadata: %w", err)
	}
	// audit cannot read execution's context (execution imports audit), so the
	// scheduled root's system identity is mapped into the explicit acting
	// record the controller validates — never synthesized inside audit.
	acting := audit.Record{
		ActorType:     audit.ActorSystem,
		ActorID:       0,
		CorrelationID: metadata.CorrelationID,
		InitiatorType: audit.ActorSystem,
		InitiatorID:   0,
	}
	return passCtx, acting, nil
}
