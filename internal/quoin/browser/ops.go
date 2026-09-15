package browser

// Declared write operations of the retained browser authority (ADR-0006
// automatic audit). Only the write paths that remain reachable after the
// controlled-browser retirement execute through the shared runner:
//
//   - the session-revocation drain (RevokeSession), invoked from every
//     auth-service revocation path and from the Upgrade-maintenance entry;
//   - the standalone operation cancel (CancelStandalone), retained as the
//     cancelStandaloneBrowserOperation command on the Upgrade-drain
//     maintenance allowlist.
//
// The Lintel-frame writers are retired reference code (ADR-0007): their only
// entries are structurally closed because RuntimeService.slotName no longer
// maps the Lintel slot, so Connect and Register reject it before any
// credential check, and the normal standalone surface and workbench
// WebSocket are never registered. They keep their enumerated execution
// architecture baseline entries instead of a blanket package exemption and
// must not be reactivated for tests.

import (
	"context"
	"fmt"
	"sync"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Stable operation identities of the retained browser write surface.
const (
	// OpRevokeSession cancels every live manual-login operation of one
	// revoked session. The audited object is the revoked auth session: it
	// is the fact that drives the drain.
	OpRevokeSession = "browser.revoke_session"
	// OpCancelOperation is the durable user cancel command of the retained
	// Upgrade-drain surface. It runs through the command ledger, so its
	// idempotent replay is decided by the shared client_commands authority
	// instead of the local replay helper.
	OpCancelOperation = "browser.cancel_operation"
)

var (
	opsOnce     sync.Once
	opsRegistry *execution.Registry
	opRevoke    *execution.Operation
	opCancel    *execution.Operation
)

// browserOperations declares the retained browser write operations once per
// process. A registration failure is a declaration conflict and panics like
// every other compose-time registry.
func browserOperations() (*execution.Registry, *execution.Operation, *execution.Operation) {
	opsOnce.Do(func() {
		registry := execution.NewRegistry()
		register := func(op execution.Operation) *execution.Operation {
			declared, err := registry.Register(op)
			if err != nil {
				panic("browser: register " + op.Name + ": " + err.Error())
			}
			return declared
		}
		opRevoke = register(execution.Operation{
			Name: OpRevokeSession, Class: execution.ClassWrite, ObjectType: "auth_session",
			Authorize: authorizeSystemDrain,
		})
		opCancel = register(execution.Operation{
			Name: OpCancelOperation, Class: execution.ClassWrite, ObjectType: "browser_operation",
			Authorize: authorizeOwningUser,
		})
		opsRegistry = registry
	})
	return opsRegistry, opRevoke, opCancel
}

// authorizeSystemDrain confines the revocation drain to the system
// principal. The drain legitimately runs inside an HTTP request (logout,
// password change, admin session revocation) and in background paths
// (Upgrade maintenance entry); callers Delegate, so the audit records the
// original initiator separately while the executing principal is always the
// system drain.
func authorizeSystemDrain(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("browser: revocation drain requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	return nil
}

// authorizeOwningUser confines the cancel command to a user principal. The
// per-operation ownership check (the operation belongs to the scoped
// identity and to this actor) re-verifies inside the transaction's business
// stage, which holds the same BEGIN IMMEDIATE lock as the state change.
func authorizeOwningUser(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID < 1 {
		return fmt.Errorf("browser: cancel requires the owning user principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	return nil
}

// drainAuthority roots one revocation-drain execution. A context that
// already carries execution metadata delegates the system actor while
// correlation and the original initiator are preserved verbatim; a bare
// background context (Upgrade maintenance entry) becomes an explicit fresh
// internal scope under its own correlation. There is never an anonymous
// fallback.
func drainAuthority(ctx context.Context) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return execution.Delegate(ctx, execution.Principal{Kind: execution.PrincipalSystem, ID: 0})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem, ID: 0},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
}

// runner composes the retained browser operations over the authority
// database. The runner is a value object: building it per call keeps the
// drain and the cancel command composable without shared mutable state.
func (service *Service) runner() *execution.Runner {
	registry, _, _ := browserOperations()
	return execution.NewRunner(service.db, registry, nil)
}
