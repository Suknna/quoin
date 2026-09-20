package connections

// The connections command runner (ADR-0006 phase 5 adoption): every active
// mutation of the connection domain — create, rotate, enable, disable, probe
// lifecycle and model discovery — executes through the shared execution
// runner inside one runner-owned IMMEDIATE transaction that also persists the
// automatic audit row (and, for the replayable create/rotate commands, the
// durable command ledger row). Business code never calls audit, never
// commits, and cannot exceed the guarded Tx. User-origin operations
// re-verify the admin session inside the transaction (auth.
// VerifyExecutionSession); the runtime-driven probe lifecycle is confined to
// the explicit system task scope.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// Stable operation identities. connection.create and connection.rotate keep
// the historic ledger command types so replay of pre-migration rows still
// resolves.
const (
	opCreate         = "connection.create"
	opRotate         = "connection.rotate"
	opEnable         = "connection.enable"
	opDisable        = "connection.disable"
	opProbeStart     = "connection.probe.start"
	opProbeCancel    = "connection.probe.cancel"
	opProbeAccept    = "connection.probe.accept"
	opProbeResult    = "connection.probe.result_commit"
	opProbeCancelAck = "connection.probe.cancel_ack"
	opProbeInterrupt = "connection.probe.interrupt"
	opProbeBind      = "connection.probe.bind"
	opGrantFulfill   = "connection.grant.fulfill"
	opModelDiscovery = "connection.model_discovery"
	opMetricsAcquire = "connection.metrics.acquire"

	objectConnection = "connection"
	// objectProbeAttempt locates attempt-scoped probe operations in audits.
	objectProbeAttempt  = "connection_probe_attempt"
	objectModelProvider = "model_provider"
	// objectGrantFulfill locates the audited credential grant reveals
	// (DATA-CONN-002: the sensitive read is recorded before any secret
	// leaves storage; the payload itself never enters the audit).
	objectGrantFulfill = "connection_credential_grant"
	// objectMetricsAcquire locates the audited Stele 网关连接材料投递
	// （ADR-0011）：与 grant reveal 同级的敏感读，解密仅为了按需投递给
	// Stele，Quoin 自身不使用、不缓存。
	objectMetricsAcquire = "connection_metrics_acquire"
)

// adminRole is the only role allowed to execute interactive connection
// commands.
const adminRole = "admin"

// Deterministic rejection codes persisted with rejected audits and ledger
// rows; domainError maps them back to the exported sentinels on replay.
const (
	codeValidation     = "validation_failed"
	codeRowVersion     = "row_version_conflict"
	codeActiveConflict = "active_conflict"
	codeNameTaken      = "name_taken"
	codeSingleEnabled  = "single_enabled"
	codeTypeMismatch   = "type_mismatch"
	codeNotFound       = "not_found"
)

// commandRunner owns this family's shared execution runner and the operation
// declarations. Every active connection mutation is declared here — an
// undeclared operation cannot execute, and each write carries its mandatory
// authorization callback re-verified inside the runner transaction before
// replay lookup and business execution.
type commandRunner struct {
	runner         *execution.Runner
	audit          *audit.Writer
	create         *execution.Operation
	rotate         *execution.Operation
	enable         *execution.Operation
	disable        *execution.Operation
	probeStart     *execution.Operation
	probeCancel    *execution.Operation
	probeAccept    *execution.Operation
	probeResult    *execution.Operation
	probeCancelAck *execution.Operation
	probeInterrupt *execution.Operation
	probeBind      *execution.Operation
	grantFulfill   *execution.Operation
	modelDiscovery *execution.Operation
	metricsAcquire *execution.Operation
}

// newCommandRunner registers the connection operations and builds the runner
// over db. now is the service's deterministic clock shared with the ledger
// and audit writers. Registration of these static, uniquely named
// declarations cannot fail; a failure is a programming error and stops the
// process instead of degrading auditing.
func newCommandRunner(db *sql.DB, now func() time.Time) *commandRunner {
	registry := execution.NewRegistry()
	commands := &commandRunner{audit: audit.NewWriterWithClock(now)}
	for _, declaration := range []struct {
		target    **execution.Operation
		name      string
		object    string
		authorize func(context.Context, *execution.Tx) error
	}{
		{&commands.create, opCreate, objectConnection, authorizeAdmin},
		{&commands.rotate, opRotate, objectConnection, authorizeAdmin},
		{&commands.enable, opEnable, objectConnection, authorizeAdmin},
		{&commands.disable, opDisable, objectConnection, authorizeAdmin},
		{&commands.probeStart, opProbeStart, objectProbeAttempt, authorizeAdmin},
		{&commands.probeCancel, opProbeCancel, objectProbeAttempt, authorizeAdmin},
		{&commands.probeAccept, opProbeAccept, objectProbeAttempt, requireProbeSystem},
		{&commands.probeResult, opProbeResult, objectProbeAttempt, requireProbeSystem},
		{&commands.probeCancelAck, opProbeCancelAck, objectProbeAttempt, requireProbeSystem},
		{&commands.probeInterrupt, opProbeInterrupt, objectProbeAttempt, requireProbeSystem},
		{&commands.probeBind, opProbeBind, objectProbeAttempt, requireProbeSystem},
		{&commands.grantFulfill, opGrantFulfill, objectGrantFulfill, requireProbeSystem},
		{&commands.modelDiscovery, opModelDiscovery, objectModelProvider, authorizeAdmin},
		{&commands.metricsAcquire, opMetricsAcquire, objectMetricsAcquire, requireProbeSystem},
	} {
		operation, err := registry.Register(execution.Operation{
			Name: declaration.name, Class: execution.ClassWrite,
			ObjectType: declaration.object, Authorize: declaration.authorize,
		})
		if err != nil {
			panic(fmt.Sprintf("connections: register operation %q: %v", declaration.name, err))
		}
		*declaration.target = operation
	}
	commands.runner = execution.NewRunnerWithClock(db, registry, commands.audit, now)
	return commands
}

// authorizeAdmin re-verifies inside the runner transaction that the context
// actor is a user whose session proof still names a current, enabled,
// initialized admin session at an unchanged auth revision. A plain error
// aborts with a clean rollback and no durable record — the correct fate for a
// session revoked or demoted between entry authentication and the commit.
func authorizeAdmin(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, adminRole)
}

// requireProbeSystem confines the runtime-driven probe lifecycle to the
// explicit system task scope: the supervisor stream is a trusted entry point,
// never a client channel, and no session fallback exists to fake.
func requireProbeSystem(ctx context.Context, _ *execution.Tx) error {
	return requireProbeSystemActor(ctx)
}

// requireProbeSystemActor is the context-level half of requireProbeSystem,
// shared with the lifecycle scope resolution below.
func requireProbeSystemActor(ctx context.Context) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("connections: probe lifecycle operation requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("connections: probe lifecycle operation cannot arrive from the %s channel", meta.Source.Kind)
	}
	return nil
}

// probeLifecycleContext resolves the execution scope of one runtime-driven
// probe transition (accept, bind, cancel ack, interrupt, result commit) on
// the given attempt. The attempt's PERSISTED association is the correlation
// authority (ADR-0006: create task/attempt and save correlation atomically;
// replies join the attempt by id and never trust caller-supplied identity):
//
//   - A context without execution metadata — the normal case once the
//     originating request has ended (queued-dispatch sweep, lease sweeper,
//     unwired stream handler) — is deliberately re-rooted onto the persisted
//     correlation and original initiator with the system task actor, so the
//     transition stays provably associated with the operation that created
//     the attempt instead of starting a fresh root. ReplaceMetadata is the
//     sanctioned re-rooting primitive; the caller's scope must be a fresh
//     background one, never the finished request.
//   - An inherited context is accepted only when its actor is the system
//     task principal AND its correlation equals the attempt's persisted one;
//     anything else is unrelated inherited actor metadata and is rejected
//     instead of passed through blindly — a long-lived stream or scheduler
//     scope must never launder its own correlation onto this attempt.
//   - An attempt that predates correlation (persisted association NULL) has
//     nothing to restore: unknown lineage stays visibly unknown. Unwired
//     callers get an explicit fresh system scope and wired system callers
//     pass through; no correlation is ever fabricated onto the attempt row.
func (service *Service) probeLifecycleContext(ctx context.Context, attemptID int64) (context.Context, error) {
	stored, found, err := attempt.LoadCorrelation(ctx, service.db, attemptID)
	if errors.Is(err, attempt.ErrAttemptMissing) {
		return nil, fmt.Errorf("connections: probe lifecycle attempt %d does not exist", attemptID)
	}
	if err != nil {
		return nil, err
	}
	meta, wired := execution.FromContext(ctx)
	if wired {
		if err := requireProbeSystemActor(ctx); err != nil {
			return nil, err
		}
		if found && stored.OperationCorrelationID != meta.CorrelationID {
			return nil, fmt.Errorf("connections: inherited metadata carries unrelated correlation %q for attempt %d (persisted %q)", meta.CorrelationID, attemptID, stored.OperationCorrelationID)
		}
		return ctx, nil
	}
	if found && restorableAttemptInitiator(stored) {
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: stored.OperationCorrelationID,
			Actor:         execution.Principal{Kind: execution.PrincipalSystem},
			Initiator:     execution.Principal{Kind: execution.PrincipalKind(stored.InitiatorType), ID: stored.InitiatorID},
			Source:        execution.Source{Kind: execution.SourceTask},
		})
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceTask},
	})
}

// restorableAttemptInitiator reports whether a persisted association carries
// a complete, valid initiator (same shape as backup's runIdentity.restorable):
// incomplete rows are treated as unknown lineage, never half-restored.
func restorableAttemptInitiator(stored attempt.Correlation) bool {
	if stored.OperationCorrelationID == "" {
		return false
	}
	switch stored.InitiatorType {
	case string(execution.PrincipalSystem):
		return stored.InitiatorID == 0
	case string(execution.PrincipalUser), string(execution.PrincipalService):
		return stored.InitiatorID > 0
	default:
		return false
	}
}

// requireActor fails closed when an explicit caller-provided principal does
// not match the execution context actor: stale handler state can never
// attribute a mutation to a different user.
func requireActor(ctx context.Context, actorID int64) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalUser || meta.Actor.ID != actorID {
		return fmt.Errorf("connections: caller principal %d does not match the execution context actor %s/%d", actorID, meta.Actor.Kind, meta.Actor.ID)
	}
	return nil
}

// domainRejection couples an exported sentinel with the deterministic runner
// rejection: the runner records the rejection (and the ledger row on replay)
// while callers keep matching the stable sentinel through errors.Is.
type domainRejection struct {
	cause     error
	rejection execution.Rejection
}

func (e *domainRejection) Error() string { return e.cause.Error() }
func (e *domainRejection) Unwrap() error { return e.cause }
func (e *domainRejection) As(target any) bool {
	if target, ok := target.(**execution.Rejection); ok {
		*target = &e.rejection
		return true
	}
	return false
}

// rejectionOf builds a deterministic rejection carrying a domain sentinel.
func rejectionOf(cause error, code, detail string, objectID int64) error {
	return &domainRejection{cause: cause, rejection: execution.Rejection{Code: code, Detail: detail, ObjectID: objectID}}
}

// versionRejection is the row-version fence rejection that also carries the
// authoritative current row version, so the HTTP conflict payload keeps its
// refresh hint across the runner round-trip.
type versionRejection struct {
	current   int64
	id        int64
	rejection execution.Rejection
}

func (e *versionRejection) Error() string        { return ErrRowVersion.Error() }
func (e *versionRejection) Is(target error) bool { return target == ErrRowVersion }
func (e *versionRejection) As(target any) bool {
	switch t := target.(type) {
	case **RowVersionError:
		*t = &RowVersionError{Current: e.current, ID: e.id}
		return true
	case **execution.Rejection:
		*t = &e.rejection
		return true
	}
	return false
}

// domainError keeps coupled rejections verbatim and translates bare replayed
// rejections (rebuilt from the ledger payload without domain coupling) back
// to their stable sentinel so errors.Is contracts hold on both paths.
func domainError(err error) error {
	var coupledVersion *versionRejection
	if errors.As(err, &coupledVersion) {
		return err
	}
	var coupled *domainRejection
	if errors.As(err, &coupled) {
		return err
	}
	var rejection *execution.Rejection
	if !errors.As(err, &rejection) {
		return err
	}
	switch rejection.Code {
	case codeNameTaken:
		return ErrNameTaken
	case codeSingleEnabled:
		return ErrSingleEnabled
	case codeActiveConflict:
		return ErrActiveConflict
	case codeTypeMismatch:
		return ErrTypeMismatch
	case codeRowVersion:
		return &RowVersionError{ID: rejection.ObjectID}
	case codeNotFound:
		return ErrNotFound
	case codeValidation:
		return fmt.Errorf("%w: %s", ErrValidation, rejection.Detail)
	default:
		return err
	}
}
