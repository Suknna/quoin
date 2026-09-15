package execution

// The command runner (ADR-0006): registered write operations execute inside
// one runner-owned BEGIN IMMEDIATE transaction that also persists the durable
// command ledger row and the success audit. Business code never calls audit,
// never commits, and cannot exceed its guarded Tx. Deterministic rejections
// roll the business stage back to a savepoint before the rejection is
// recorded, so no unexpected business modification can ride along;
// infrastructure and audit failures leave no durable trace at all — no forged
// success, no forged rejection. Idempotent replays read the stored outcome and
// the original correlation from the command ledger without producing new
// audit rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
)

// Ledger outcomes mirror the frozen client_commands.outcome CHECK constraint.
const (
	LedgerCommitted     = "committed"
	LedgerRejectedKnown = "rejected_known"
)

// savepointName is the runner-owned savepoint around the business stage.
const savepointName = "quoin_command"

// ErrCommandReused marks a client command id replayed with a different
// request digest. The stored ledger row stays authoritative and nothing new
// is recorded.
var ErrCommandReused = errors.New("execution: client command id reused with a different request")

// Command identifies the durable idempotency key of one external command.
// Digest is the non-secret canonical request digest (64 lowercase hex
// characters, compatible with auth.DigestCommand).
type Command struct {
	PrincipalType   string // "user", "service" or "system" (client_commands CHECK)
	PrincipalID     int64  // principal row id; the system principal uses 0
	ClientCommandID string
	Digest          string
}

func (cmd Command) validate() error {
	switch cmd.PrincipalType {
	case string(PrincipalUser), string(PrincipalService):
		if cmd.PrincipalID < 1 {
			return errors.New("execution: command principal id must be positive")
		}
	case string(PrincipalSystem):
		if cmd.PrincipalID != 0 {
			return errors.New("execution: the system command principal uses id 0")
		}
	default:
		return fmt.Errorf("execution: command principal type must be user, service or system, got %q", cmd.PrincipalType)
	}
	if cmd.ClientCommandID == "" {
		return errors.New("execution: client command id is required")
	}
	if len(cmd.Digest) != 64 {
		return errors.New("execution: request digest must contain 64 characters")
	}
	return nil
}

// Change declares whether the business stage modified state. Unchanged
// commands are still durably recorded — the runner classifies them in the
// ledger payload; business code has no switch to turn auditing off.
type Change int

const (
	Changed Change = iota
	Unchanged
)

// Outcome reports the result of one runner execution. On first execution
// CorrelationID is the current metadata correlation; on a replay it is the
// original business correlation read from the persisted command record, while
// the current request keeps its own independent identity.
type Outcome[T any] struct {
	Result        T
	Change        Change
	Replayed      bool
	CorrelationID string
}

// Rejection is a deterministic, non-secret business rejection (validation,
// conflict, missing object). The runner persists it as a rejected_known
// ledger row plus a rejected audit event in a clean transaction and surfaces
// the same error to the caller.
type Rejection struct {
	Code     string // stable machine code, e.g. "validation_failed"
	Detail   string // human-readable, non-secret detail
	ObjectID int64  // optional target object id
	// Raw is the stored ledger payload verbatim. Domains that persisted
	// rejections before the unified runner (their own payload shapes) decode
	// it to preserve the original typed outcome instead of misreporting the
	// replay as a command conflict. Never a secret: ledger payloads are
	// schema-gated non-secret JSON.
	Raw string
}

func (e *Rejection) Error() string {
	if e.Detail == "" {
		return "execution: command rejected: " + e.Code
	}
	return "execution: command rejected: " + e.Code + ": " + e.Detail
}

// RecordedFailure marks an EXPECTED durable recorded attempt — one that
// legitimately ends in a non-committed outcome and must be recorded as such:
// the business stage's intended state changes (e.g. an authentication failure
// counter or a delivery status row) commit and the automatic audit records
// the attempt. Only Execute (non-ledger mutations) accepts it; Run forbids it
// because the frozen client_commands outcomes only know
// committed/rejected_known, and a recorded business attempt must not
// masquerade as a committed command result. It must never wrap arbitrary
// errors: committing on an unexpected error would forge a "recorded attempt"
// fact. Code is a stable machine code and Detail is human-readable and
// non-secret — codes, tokens and secrets never belong in either.
type RecordedFailure struct {
	Code     string
	Detail   string
	ObjectID int64
	// Outcome classifies the attempt in the audit. Empty means
	// audit.OutcomeFailure (a definitive failed attempt). The only other
	// accepted value is audit.OutcomeUnknown — an external outcome that is
	// genuinely unavailable (e.g. a delivery whose provider status was never
	// confirmed); it must never be used for definite failures. Any other
	// value is rejected and rolls the attempt back.
	Outcome string
}

func (e *RecordedFailure) Error() string {
	if e.Detail == "" {
		return "execution: recorded failure: " + e.Code
	}
	return "execution: recorded failure: " + e.Code + ": " + e.Detail
}

// ErrNoTransition marks a business stage that examined the current state and
// deliberately performed no state transition (a lifecycle compare-and-set
// miss: an already-terminal attempt, an already-fenced cancellation). The
// stage must return it BEFORE its first write. The runner always rolls the
// savepoint back first — even a mistakenly partial business write is
// discarded and can never commit — then commits the clean transaction,
// records no ledger row and no audit event (a missed transition is not a
// success fact), and surfaces ErrNoTransition so the caller can answer from
// the authoritative state. It exists for transient, non-ledger executions
// only: Run (ledger commands) rejects it because their unchanged outcomes
// are declared through the typed Change classification and stay durably
// recorded — a human no-op command is never silent. Authorization and any
// replay pre-stage always run before the business stage can return it.
// Missing execution metadata and infrastructure failures keep failing the
// execution; the sentinel is the centrally owned condition for "no state
// transition happened", never a business-side audit switch.
var ErrNoTransition = errors.New("execution: no state transition")

// Runner executes registered write operations on a database it never lets
// business code commit.
type Runner struct {
	db *sql.DB
	// reader is the composition layer's injected real read-only pool
	// (bootstrap mode=ro/query_only). Reader() fails closed until it is
	// wired: a read seam that silently wraps the write pool would defeat the
	// split Reader() exists to enforce.
	reader *sql.DB
	ops    *Registry
	audit  *audit.Writer
	now    func() time.Time
}

// NewRunner returns a runner over db. A nil writer falls back to the default
// audit writer; a nil registry accepts nothing.
func NewRunner(db *sql.DB, ops *Registry, writer *audit.Writer) *Runner {
	if ops == nil {
		ops = NewRegistry()
	}
	if writer == nil {
		writer = audit.NewWriter()
	}
	return &Runner{db: db, ops: ops, audit: writer, now: time.Now}
}

// NewRunnerWithClock is NewRunner with an injectable clock for the command
// ledger's created_at, so a service owning a deterministic process clock
// keeps ledger timestamps on that same clock. The audit writer keeps its own
// clock; pair with audit.NewWriterWithClock to align both. Nil now falls back
// to the UTC wall clock.
func NewRunnerWithClock(db *sql.DB, ops *Registry, writer *audit.Writer, now func() time.Time) *Runner {
	runner := NewRunner(db, ops, writer)
	if now != nil {
		runner.now = now
	}
	return runner
}

// Register adds one operation declaration to the runner's own registry so a
// composed application can keep a single registry — shared with admission and
// operation-coverage checks — while each service registers the operations it
// owns. Registering the same name twice fails, so two modules can never
// silently claim one operation identity.
func (r *Runner) Register(op Operation) (*Operation, error) {
	return r.ops.Register(op)
}

// SetReader accepts only capabilities created by OpenReadOnly or another
// runner. A connection-local PRAGMA cannot prove an arbitrary pool read-only.
func (r *Runner) SetReader(reader audit.Reader) error {
	trusted, ok := reader.(Reader)
	if !ok || trusted.db == nil {
		return errors.New("execution: reader must be created by OpenReadOnly")
	}
	if err := trusted.db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("execution: read-only reader is unavailable: %w", err)
	}
	r.reader = trusted.db
	return nil
}

// Reader is the composition read surface: query-only by construction — the
// type exposes no Exec, so domain writes have no path through it. Without an
// injected read-only pool every query fails closed; it never falls back to
// the writer.
type Reader struct {
	db *sql.DB
}

// Close releases the pool owned by the OpenReadOnly caller. Shared consumers
// must leave its lifetime to their composition root.
func (r Reader) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r Reader) PingContext(ctx context.Context) error {
	if r.db == nil {
		return errors.New("execution: no read-only reader is configured")
	}
	return r.db.PingContext(ctx)
}

// QueryContext runs one read query on the injected read-only pool.
func (r Reader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if r.db == nil {
		return nil, errors.New("execution: no read-only reader is configured for this runner")
	}
	if err := guardReadStatement(query); err != nil {
		return nil, err
	}
	return r.db.QueryContext(ctx, query, args...)
}

// QueryRowContext runs one read single-row query on the injected read-only
// pool.
func (r Reader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if r.db == nil {
		return failClosedRow()
	}
	if err := guardReadStatement(query); err != nil {
		return failClosedRow()
	}
	return r.db.QueryRowContext(ctx, query, args...)
}

// Reader returns the composition-injected read-only query surface for
// cross-domain SELECT queries. The runner never closes the injected pool —
// its ownership stays with the composition layer.
func (r *Runner) Reader() Reader {
	return Reader{db: r.reader}
}

// failClosedRow builds a row whose Scan always fails: the query runs against
// a throwaway gate handle under an already-canceled context, so the error
// surfaces on the caller's Scan without touching any database. An unwired
// reader must report its configuration gap, never serve the write pool.
func failClosedRow() *sql.Row {
	readerGateOnce.Do(func() {
		var err error
		readerGateDB, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			panic("execution: open reader gate: " + err.Error())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return readerGateDB.QueryRowContext(ctx, `SELECT 1`)
}

var (
	readerGateOnce sync.Once
	readerGateDB   *sql.DB
)

// traceKind tells the recording closure and the audit writer which fact to
// persist for a finished transaction body.
type traceKind int

const (
	traceSuccess traceKind = iota
	traceRejected
)

// Run executes op as a durable, replayable command: required execution
// metadata, principal match, authorization inside the transaction, replay
// lookup, business stage on the guarded Tx, command ledger row plus success
// audit in the same transaction, commit. The business result must marshal to
// a JSON object and carry no secrets — it is persisted for replay. Results
// containing transient secrets (bearer tokens, one-time codes) must use
// Execute instead.
//
// objectID extracts the typed result's object id for the ledger and audit
// domain reference. run returning a *Rejection marks a deterministic
// rejection: partial writes are rolled back to a savepoint and the rejection
// is recorded in the clean transaction. A *RecordedFailure is forbidden on
// ledger commands — durable business failures belong to Execute. Any other
// error — including audit failure — rolls everything back and surfaces
// verbatim.
func Run[T any](ctx context.Context, r *Runner, op *Operation, cmd Command, run func(tx *Tx) (T, Change, error), objectID func(T) int64) (Outcome[T], error) {
	var zero Outcome[T]
	meta, err := Require(ctx)
	if err != nil {
		return zero, err
	}
	if err = r.checkOperation(op); err != nil {
		return zero, err
	}
	if err = cmd.validate(); err != nil {
		return zero, err
	}
	if run == nil {
		return zero, errors.New("execution: business function is required")
	}
	if objectID == nil {
		return zero, errors.New("execution: object id accessor is required")
	}
	if string(meta.Actor.Kind) != cmd.PrincipalType || meta.Actor.ID != cmd.PrincipalID {
		return zero, errors.New("execution: command principal does not match the context actor")
	}
	var outcome Outcome[T]
	var resultPayload string
	pre := func(tx *Tx) (bool, error) {
		stored, found, err := lookupCommand(ctx, tx.conn, cmd)
		if err != nil || !found {
			return false, err
		}
		if stored.commandType != op.Name || stored.digest != cmd.Digest {
			return false, ErrCommandReused
		}
		// Replay: the stored outcome and the original business correlation
		// are authoritative; no new ledger row and no new audit row.
		outcome.Replayed = true
		outcome.CorrelationID = stored.correlationID
		if stored.outcome == LedgerRejectedKnown {
			return true, stored.rejection()
		}
		result, change, err := decodeLedgerPayload[T](stored.payload)
		if err != nil {
			return false, err
		}
		outcome.Result, outcome.Change = result, change
		return true, nil
	}
	business := func(tx *Tx) (int64, error) {
		value, change, err := run(tx)
		if err != nil {
			return 0, err
		}
		payload, err := encodeLedgerPayload(value, change)
		if err != nil {
			return 0, err
		}
		resultPayload = payload
		outcome.Result = value
		outcome.Change = change
		outcome.CorrelationID = meta.CorrelationID
		return objectID(value), nil
	}
	record := func(ctx context.Context, conn *sql.Conn, kind traceKind, recordID int64, rejection *Rejection) error {
		if kind == traceRejected {
			return insertCommand(ctx, conn, r.now, meta, cmd, op, LedgerRejectedKnown, recordID, marshalRejectionPayload(rejection))
		}
		return insertCommand(ctx, conn, r.now, meta, cmd, op, LedgerCommitted, recordID, resultPayload)
	}
	if err = r.transact(ctx, op, &cmd, meta, pre, business, record); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// Execute runs op as one audited, transactional mutation WITHOUT the durable
// command ledger: no client_commands row, no replay, no synthetic client
// command key. It is the non-replayable mutation path for steps that already
// own their atomic one-time identifiers and for results containing transient
// secrets — bearer tokens, one-time codes, reveal handles — which must never
// enter the ledger; such results stay in memory and are returned only after
// the commit. Authorization, execution metadata, savepoint rejection
// classification and automatic audit behave exactly like Run.
func Execute[T any](ctx context.Context, r *Runner, op *Operation, run func(tx *Tx) (T, error), objectID func(T) int64) (T, error) {
	var zero T
	meta, err := Require(ctx)
	if err != nil {
		return zero, err
	}
	if err = r.checkOperation(op); err != nil {
		return zero, err
	}
	if run == nil {
		return zero, errors.New("execution: business function is required")
	}
	if objectID == nil {
		return zero, errors.New("execution: object id accessor is required")
	}
	var result T
	business := func(tx *Tx) (int64, error) {
		value, err := run(tx)
		if err != nil {
			return 0, err
		}
		result = value
		return objectID(value), nil
	}
	nothing := func(context.Context, *sql.Conn, traceKind, int64, *Rejection) error { return nil }
	if err = r.transact(ctx, op, nil, meta, nil, business, nothing); err != nil {
		return zero, err
	}
	return result, nil
}

// checkOperation enforces that op is the registered declaration of a write
// operation with its authorization callback declared.
func (r *Runner) checkOperation(op *Operation) error {
	if op == nil {
		return errors.New("execution: operation is required")
	}
	registered, ok := r.ops.Lookup(op.Name)
	if !ok || registered != op {
		return fmt.Errorf("execution: operation %q is not registered", op.Name)
	}
	if op.Class != ClassWrite {
		return fmt.Errorf("execution: operation %q is %s and must not run through the command runner", op.Name, op.Class)
	}
	if op.Authorize == nil {
		return fmt.Errorf("execution: operation %q must declare an authorization callback", op.Name)
	}
	return nil
}

// transact is the shared transaction core behind Run and Execute: one
// runner-owned IMMEDIATE transaction holding, in order, the operation's
// authorization callback, the optional command replay pre-stage, the business
// stage inside a savepoint, the unified rejection classification, the
// caller's durable traces and the shared audit row, then commit. All cleanup
// statements run on context.Background so an expired or canceled context can
// never leave the transaction dangling.
func (r *Runner) transact(
	ctx context.Context,
	op *Operation,
	cmd *Command,
	meta Metadata,
	pre func(tx *Tx) (skip bool, err error),
	business func(tx *Tx) (int64, error),
	record func(ctx context.Context, conn *sql.Conn, kind traceKind, objectID int64, rejection *Rejection) error,
) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	committed := false
	tx := newTx(conn)
	defer func() {
		// Expire the business handle first: after the transaction ends, a
		// retained reference must never check the pooled connection back out.
		tx.finish()
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = conn.Close()
	}()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "SAVEPOINT "+savepointName); err != nil {
		return err
	}
	// Authorization comes before replay and business inside every
	// transaction: a since-revoked principal can neither re-execute nor read
	// a stored replay result.
	if err = op.Authorize(ctx, tx); err != nil {
		committed, err = r.classify(ctx, tx, op, cmd, meta, err, record)
		return err
	}
	if pre != nil {
		skip, preErr := pre(tx)
		if skip {
			// The replay pre-stage finished the decision: commit the
			// read-only transaction and return the stored outcome (or stored
			// rejection) without recording anything new. The handle expires
			// before the commit itself closes the write window.
			tx.finish()
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return preErr
		}
		if preErr != nil {
			committed, preErr = r.classify(ctx, tx, op, cmd, meta, preErr, record)
			return preErr
		}
	}
	objectID, businessErr := business(tx)
	if businessErr != nil {
		committed, businessErr = r.classify(ctx, tx, op, cmd, meta, businessErr, record)
		return businessErr
	}
	if err = record(ctx, conn, traceSuccess, objectID, nil); err != nil {
		return err
	}
	if err = r.writeAudit(ctx, tx, meta, op, cmd, audit.OutcomeSuccess, objectID); err != nil {
		return err
	}
	// Expire the business handle before the commit itself: after this point
	// no business call can initiate anything inside the transaction.
	tx.finish()
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// classify implements the unified end-of-business rule set:
//
//   - *Rejection (deterministic command rejection): roll the business stage
//     back to the savepoint, persist the rejection traces and the rejected
//     audit event in the now-clean transaction, commit, and surface the
//     rejection.
//   - *RecordedFailure (expected durable failure, Execute only): keep the
//     business stage's intended state changes by releasing the savepoint,
//     write the failure audit, commit, and surface the failure. On ledger
//     commands (cmd != nil) it is forbidden — nothing durable is recorded and
//     the caller receives the misuse error.
//
// Any other error records nothing: the deferred full rollback discards the
// transaction and the caller reports only the verifiable failure. No generic
// error ever commits.
func (r *Runner) classify(
	ctx context.Context,
	tx *Tx,
	op *Operation,
	cmd *Command,
	meta Metadata,
	businessErr error,
	record func(context.Context, *sql.Conn, traceKind, int64, *Rejection) error,
) (bool, error) {
	conn := tx.conn
	var rejection *Rejection
	var failure *RecordedFailure
	switch {
	case errors.Is(businessErr, ErrNoTransition):
		if cmd != nil {
			// Ledger commands must not go silent: their unchanged outcomes
			// are declared through the typed Change classification and stay
			// durably recorded. A sentinel here is runner misuse; nothing
			// durable is recorded and the caller receives the error.
			return false, fmt.Errorf("execution: %w is not supported by ledger commands; declare the unchanged classification instead", ErrNoTransition)
		}
		// A missed transition: discard any stray business write at the
		// savepoint (registered audit projections are never invoked on this
		// path), then commit the clean transaction and record nothing.
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK TO SAVEPOINT "+savepointName); err != nil {
			return false, err
		}
	case errors.As(businessErr, &rejection):
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK TO SAVEPOINT "+savepointName); err != nil {
			return false, err
		}
		if err := record(ctx, conn, traceRejected, rejection.ObjectID, rejection); err != nil {
			return false, err
		}
		if err := r.writeAudit(ctx, tx, meta, op, cmd, audit.OutcomeRejected, rejection.ObjectID); err != nil {
			return false, err
		}
	case errors.As(businessErr, &failure):
		if cmd != nil {
			// Ledger commands must not commit business failures: the frozen
			// ledger has no failed-outcome row, and a committed domain change
			// must never ride a failed result. Full rollback (deferred).
			return false, fmt.Errorf("execution: recorded failure %q is not supported by ledger commands; use Rejection for deterministic rejections", failure.Code)
		}
		outcome, outcomeErr := recordedFailureOutcome(failure)
		if outcomeErr != nil {
			return false, outcomeErr
		}
		// Keep the intended business state: release (not roll back) the
		// savepoint, then record the attempt with its classified outcome.
		if _, err := conn.ExecContext(context.Background(), "RELEASE SAVEPOINT "+savepointName); err != nil {
			return false, err
		}
		if err := r.writeAudit(ctx, tx, meta, op, cmd, outcome, failure.ObjectID); err != nil {
			return false, err
		}
	default:
		return false, businessErr
	}
	// Expire the business handle before the commit itself: after this point
	// no business call can initiate anything inside the transaction.
	tx.finish()
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	return true, businessErr
}

// recordedFailureOutcome resolves the audit outcome of a recorded attempt:
// empty defaults to failure; only failure and unknown are accepted, so a
// caller can never forge a success through the recorded-attempt path.
func recordedFailureOutcome(failure *RecordedFailure) (string, error) {
	switch failure.Outcome {
	case "":
		return audit.OutcomeFailure, nil
	case audit.OutcomeFailure, audit.OutcomeUnknown:
		return failure.Outcome, nil
	default:
		return "", errors.New("execution: recorded failure outcome must be empty, failure or unknown")
	}
}

// writeAudit persists the automatic audit row for a finished execution.
// cmd may be nil for non-ledger executions; their audit rows carry no client
// command id.
func (r *Runner) writeAudit(ctx context.Context, tx *Tx, meta Metadata, op *Operation, cmd *Command, auditOutcome string, objectID int64) error {
	event := audit.Record{
		ActorType:     string(meta.Actor.Kind),
		ActorID:       meta.Actor.ID,
		Action:        op.Name,
		Outcome:       auditOutcome,
		Phase:         audit.PhaseExecute,
		DomainRefType: op.ObjectType,
		DomainRefID:   objectID,
		CorrelationID: meta.CorrelationID,
		RequestID:     meta.Source.RequestID,
		InitiatorType: string(meta.Initiator.Kind),
		InitiatorID:   meta.Initiator.ID,
		Targets:       []audit.RecordTarget{{Type: op.ObjectType, ID: objectID}},
	}
	if cmd != nil {
		event.ClientCommandID = cmd.ClientCommandID
	}
	if op.TargetVersion != nil {
		if version, ok := op.TargetVersion(ctx, tx, objectID); ok {
			event.Targets[0].Version = &version
		}
	}
	eventID, err := r.audit.Write(ctx, tx, event)
	if err != nil {
		return fmt.Errorf("execution: persist audit event: %w", err)
	}
	if auditOutcome == audit.OutcomeSuccess {
		if err := tx.projectAudit(ctx, eventID); err != nil {
			return fmt.Errorf("execution: project audited outcome: %w", err)
		}
	}
	return nil
}

// storedCommand is one persisted ledger row as the replay path reads it.
type storedCommand struct {
	commandType   string
	digest        string
	outcome       string
	objectType    string
	objectID      int64
	payload       string
	correlationID string
}

// rejection rebuilds the deterministic rejection persisted with a
// rejected_known ledger row.
func (stored storedCommand) rejection() error {
	var payload rejectionPayload
	if err := json.Unmarshal([]byte(stored.payload), &payload); err != nil {
		return fmt.Errorf("execution: stored rejected command is invalid: %w", err)
	}
	return &Rejection{Code: payload.Code, Detail: payload.Detail, ObjectID: payload.ObjectID, Raw: stored.payload}
}

// lookupCommand reads the ledger key on the open transaction so a same-key
// request that waited for the writer lock is decided deterministically. This
// is the execution package's own access to the frozen client_commands schema
// concept — deliberately not imported from auth to avoid an import cycle.
func lookupCommand(ctx context.Context, conn *sql.Conn, cmd Command) (storedCommand, bool, error) {
	row := conn.QueryRowContext(ctx, `
		SELECT command_type, request_digest, outcome,
		       COALESCE(result_object_type,''), COALESCE(result_object_id,0),
		       COALESCE(result_payload_json,''), COALESCE(correlation_id,'')
		FROM client_commands
		WHERE principal_type=? AND principal_id=? AND client_command_id=?`,
		cmd.PrincipalType, cmd.PrincipalID, cmd.ClientCommandID)
	var stored storedCommand
	if err := row.Scan(&stored.commandType, &stored.digest, &stored.outcome, &stored.objectType, &stored.objectID, &stored.payload, &stored.correlationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storedCommand{}, false, nil
		}
		return storedCommand{}, false, fmt.Errorf("execution: read command ledger: %w", err)
	}
	return stored, true, nil
}

// insertCommand persists the ledger row inside the runner transaction. The
// correlation id is written exactly once, on first execution; replays never
// rewrite it.
func insertCommand(ctx context.Context, conn *sql.Conn, now func() time.Time, meta Metadata, cmd Command, op *Operation, outcome string, objectID int64, payload string) error {
	var correlation any
	if meta.CorrelationID != "" {
		correlation = meta.CorrelationID
	}
	var payloadArg any
	if payload != "" {
		payloadArg = payload
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO client_commands(
			principal_type,principal_id,client_command_id,command_type,request_digest,
			outcome,result_object_type,result_object_id,result_payload_json,created_at,correlation_id
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		cmd.PrincipalType, cmd.PrincipalID, cmd.ClientCommandID, op.Name, cmd.Digest,
		outcome, op.ObjectType, objectID, payloadArg, timestamp(now), correlation); err != nil {
		return fmt.Errorf("execution: persist command ledger row: %w", err)
	}
	return nil
}

type rejectionPayload struct {
	Code     string `json:"code"`
	ObjectID int64  `json:"objectId,omitempty"`
	Detail   string `json:"detail"`
}

func marshalRejectionPayload(rejection *Rejection) string {
	body, err := json.Marshal(rejectionPayload{Code: rejection.Code, ObjectID: rejection.ObjectID, Detail: rejection.Detail})
	if err != nil {
		// String fields cannot fail to marshal; keep the row schema-valid
		// rather than panicking.
		body = []byte("{}")
	}
	return string(body)
}

// commandClassKey is the reserved ledger payload key marking the unified
// "no change" classification of a state-neutral command. The frozen outcome
// CHECKs only know committed/rejected_known, so the classification lives in
// the persisted payload; business code has no switch to suppress it.
const (
	commandClassKey = "quoinCommandClass"
	classUnchanged  = "unchanged"
)

type unchangedEnvelope[T any] struct {
	CommandClass string `json:"quoinCommandClass"`
	Result       T      `json:"result"`
}

func encodeLedgerPayload[T any](value T, change Change) (string, error) {
	if change == Unchanged {
		body, err := json.Marshal(unchangedEnvelope[T]{CommandClass: classUnchanged, Result: value})
		if err != nil {
			return "", fmt.Errorf("execution: encode command result: %w", err)
		}
		return string(body), nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("execution: encode command result: %w", err)
	}
	return string(body), nil
}

func decodeLedgerPayload[T any](payload string) (T, Change, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &probe); err == nil {
		if raw, ok := probe[commandClassKey]; ok && string(raw) == `"`+classUnchanged+`"` {
			var envelope unchangedEnvelope[T]
			if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
				var zero T
				return zero, Changed, fmt.Errorf("execution: decode unchanged command result: %w", err)
			}
			return envelope.Result, Unchanged, nil
		}
	}
	var value T
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		var zero T
		return zero, Changed, fmt.Errorf("execution: decode command result: %w", err)
	}
	return value, Changed, nil
}

func timestamp(now func() time.Time) string {
	return now().UTC().Format(time.RFC3339Nano)
}
