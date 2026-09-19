// Package businesssystem owns the Business System aggregate (T16): the
// parse-once upload that creates a Disabled system with its first immutable
// draft in one transaction, the append-only immutable configuration versions
// with their typed projections, and the single-UPDATE publish command that
// moves business_systems.current_config_version_id under the frozen
// concurrency fence (DATA-CONFIG-001/003/004, HTTP-CONFIG-001/002).
package businesssystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// ErrNotFound reports a missing business system or version.
var ErrNotFound = errors.New("business system or config version not found")

// ErrCommandReused reports a client command id replayed with a different
// request digest (HTTP-COMMAND-003).
var ErrCommandReused = errors.New("client command id reused with a different request")

// ConflictError carries the frozen publish conflict codes.
type ConflictError struct {
	Code           string // row_version_conflict | current_pointer_conflict | active_conflict
	Detail         string
	SystemKey      string
	ObjectID       int64
	CurrentVersion *int64 // actual current published version id (nil = none)
}

func (err *ConflictError) Error() string { return err.Detail }

// UploadInput carries the raw declaration.
type UploadInput struct {
	YAMLBody []byte
}

// Service owns the Business System state changes. Every mutation runs through
// the family's shared execution runner (ADR-0006): the runner owns the
// transaction, the command ledger and the automatic audit row, so business
// code never calls the audit writer or manages BEGIN/COMMIT itself.
type Service struct {
	db *sql.DB
	// reader is the composition layer's read-only query surface
	// (bootstrap mode=ro/query_only); until injected it falls back to the
	// write pool (compose-time compatibility, same as the other families).
	reader audit.Reader
	now    func() time.Time

	runner *execution.Runner
	// Interactive admin commands (durable, replayable ledger commands).
	opVerificationRun    *execution.Operation
	opVerificationCancel *execution.Operation
	// Runtime/system machine stages (non-ledger executions).
	opDiscoveryResult *execution.Operation
	opRunResult       *execution.Operation
	opRunGap          *execution.Operation
	opRefreshGap      *execution.Operation
}

// Stable operation identities.
const (
	opVerificationRunName    = "config_verification.run"
	opVerificationCancelName = "config_verification.cancel"
	opDiscoveryResultName    = "config_verification.result.discovery"
	opRunResultName          = "config_verification.result.commit"
	opRunGapName             = "config_verification.gap.settle"
	opRefreshGapName         = "resource_refresh.gap.settle"

	objectVerificationRun = "config_verification_run"
	objectRefreshRun      = "resource_refresh_run"
)

func NewService(db *sql.DB) *Service {
	now := func() time.Time { return time.Now().UTC() }
	service := &Service{db: db, now: now}
	service.runner = execution.NewRunnerWithClock(db, execution.NewRegistry(), audit.NewWriterWithClock(now), now)
	service.registerOperations()
	return service
}

// registerOperations declares every active Business System mutation. An
// undeclared operation cannot execute; each carries its mandatory
// authorization callback re-verified inside the runner transaction.
func (service *Service) registerOperations() {
	register := func(op execution.Operation) *execution.Operation {
		declared, err := service.runner.Register(op)
		if err != nil {
			panic("businesssystem: register " + op.Name + ": " + err.Error())
		}
		return declared
	}
	admin := execution.Operation{Class: execution.ClassWrite, ObjectType: objectVerificationRun, Authorize: authorizeVerificationAdmin}
	service.opVerificationRun = register(execution.Operation{Name: opVerificationRunName, Class: admin.Class, ObjectType: admin.ObjectType, Authorize: admin.Authorize})
	service.opVerificationCancel = register(execution.Operation{Name: opVerificationCancelName, Class: admin.Class, ObjectType: admin.ObjectType, Authorize: admin.Authorize})
	machine := execution.Operation{Class: execution.ClassWrite, ObjectType: objectVerificationRun, Authorize: requireSystemResultWork}
	service.opDiscoveryResult = register(execution.Operation{Name: opDiscoveryResultName, Class: machine.Class, ObjectType: machine.ObjectType, Authorize: machine.Authorize})
	service.opRunResult = register(execution.Operation{Name: opRunResultName, Class: machine.Class, ObjectType: machine.ObjectType, Authorize: machine.Authorize})
	service.opRunGap = register(execution.Operation{Name: opRunGapName, Class: machine.Class, ObjectType: machine.ObjectType, Authorize: machine.Authorize})
	service.opRefreshGap = register(execution.Operation{Name: opRefreshGapName, Class: execution.ClassWrite, ObjectType: objectRefreshRun, Authorize: requireSystemResultWork})
}

// authorizeVerificationAdmin re-verifies inside the runner transaction that
// the acting principal is an enabled, initialized admin session at its
// issuing auth revision. The principal and session proof come from the
// execution context, never from client-controlled arguments.
func authorizeVerificationAdmin(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "admin")
}

// requireSystemResultWork confines the runtime-driven verification lifecycle
// to the system task executor arriving through a trusted background source.
func requireSystemResultWork(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("businesssystem: result work requires the system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind == execution.SourceHTTP {
		return fmt.Errorf("businesssystem: result work cannot arrive from the http channel")
	}
	return nil
}

// resultContext resolves the system scope for one runtime result stage:
// inherited when the caller already carries execution metadata, restored from
// the attempt row's persisted correlation when a restart lost the request
// scope, or established fresh for a legacy correlation-less row — a
// deliberate machine operation under its own identity; a historical origin
// that is unknown stays unknown and is never fabricated onto the stage.
func (service *Service) resultContext(ctx context.Context, attemptID int64) (context.Context, error) {
	if _, ok := execution.FromContext(ctx); ok {
		return ctx, nil
	}
	correlation, found, err := attempt.LoadCorrelation(ctx, service.db, attemptID)
	if err != nil && !errors.Is(err, attempt.ErrAttemptMissing) {
		return nil, err
	}
	// A missing attempt row (e.g. the admission sweep has no attempt yet)
	// carries no correlation to restore: the fresh explicit scope below is
	// the stage's only provable identity.
	if errors.Is(err, attempt.ErrAttemptMissing) {
		found = false
	}
	actor := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
	source := execution.Source{Kind: execution.SourceTask}
	if found && correlation.OperationCorrelationID != "" {
		initiator := execution.Principal{Kind: execution.PrincipalSystem, ID: 0}
		if correlation.InitiatorType != "" {
			initiator = execution.Principal{Kind: execution.PrincipalKind(correlation.InitiatorType), ID: correlation.InitiatorID}
		}
		return execution.ReplaceMetadata(ctx, execution.Metadata{
			CorrelationID: correlation.OperationCorrelationID,
			Actor:         actor,
			Initiator:     initiator,
			Source:        source,
		})
	}
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.ReplaceMetadata(ctx, execution.Metadata{CorrelationID: correlationID, Actor: actor, Source: source})
}

// SetReader injects the composition layer's real read-only query surface.
func (service *Service) SetReader(reader audit.Reader) {
	if reader != nil {
		service.reader = reader
	}
}

// Reader serves the app layer's read-only locator queries. Unwired it
// returns the zero-value execution.Reader, which fails closed — the writer
// database is never a read fallback.
func (service *Service) Reader() audit.Reader {
	if service.reader != nil {
		return service.reader
	}
	return execution.Reader{}
}

func (service *Service) nowText() string { return service.now().UTC().Format(time.RFC3339Nano) }

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func idString(value *int64) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(*value, 10)
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeStored(payload string, target any) error {
	if payload == "" {
		return errors.New("empty stored result")
	}
	return json.Unmarshal([]byte(payload), target)
}

// replayCommandResult is the only route from a committed command ledger row
// back to a command response. A malformed ledger row cannot fall through into
// a second business execution.
func replayCommandResult[T any](record auth.CommandRecord, found bool, digest string, valid func(T) bool) (T, bool, error) {
	var zero T
	if !found {
		return zero, false, nil
	}
	if record.RequestDigest != digest {
		return zero, true, ErrCommandReused
	}
	var replayed T
	if err := decodeStored(record.ResultPayload, &replayed); err != nil {
		return zero, true, fmt.Errorf("decode committed business-system command: %w", err)
	}
	if !valid(replayed) {
		return zero, true, errors.New("committed business-system command has no result")
	}
	return replayed, true, nil
}
