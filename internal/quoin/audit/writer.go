// Persistence half of the audit module (docs/audit-design.md §6): this file
// owns the single INSERT path for audit_events and audit_event_targets plus
// the audit_cleanup_batches record, so every producer — the execution runner,
// HTTP access facts, auth flows, retention cleanup — stores the same
// whitelisted fields. Business code never inserts audit rows directly.
// Persistence happens on the caller's open transaction; the writer never
// opens connections, commits, or triggers further auditing on its own.
//
// Same-package ownership (coordinate before adding types): query.go owns the
// read model (Event, Target, Filter, Page, Reader, QueryEvents/GetEvent);
// writer.go owns the write input (Record) and the batch persistence method;
// retention.go owns the CleanupBatch type, retention settings and cleanup
// controller. The writer fails closed until the coordinated schema extension
// exists — it never silently drops correlation fields.
package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Outcome and phase vocabularies mirror the audit_events CHECK constraints
// and the coordinated phase extension. Shared package vocabulary lives here:
// the persistence core validates every value before insert.
const (
	OutcomeSuccess  = "success"
	OutcomeFailure  = "failure"
	OutcomeRejected = "rejected"
	OutcomeUnknown  = "unknown"

	// PhaseExecute is the schema default for events without a finer phase.
	PhaseExecute = "execute"
	// PhaseAccess marks an authorized-access fact recorded before content is
	// released.
	PhaseAccess = "access"
)

// maxIdentityLength bounds correlation and request identifiers. It matches
// the execution package's bound; duplicated here because audit must not
// import execution (execution imports audit).
const maxIdentityLength = 128

// DB is the minimal write surface the writer needs. Producers pass their open
// transaction so audit rows commit — or roll back — atomically with the
// domain write; passing a bare *sql.DB would compile but bypass
// same-transaction auditing and must not be used by producers.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordTarget is one object reference to persist with a record (an
// audit_event_targets row). Version is nil when no applicable version exists
// (persisted as NULL); the read-side projection is query.go's TargetView,
// whose Version 0 corresponds to this nil.
type RecordTarget struct {
	Type    string
	ID      int64
	Version *int64
}

// Record is one audit record to persist — the write-side input, distinct from
// query.go's EventView read model. Fields are a deliberate whitelist: request
// or response bodies, headers, prompts, credentials, passwords, OTPs and
// vendor responses must never be added here. ActorType/ActorID is the current
// acting principal; InitiatorType/InitiatorID preserve the original initiator
// when a background execution acts on someone's behalf, and default to the
// actor for direct executions.
type Record struct {
	ActorType       string
	ActorID         int64
	Action          string
	Outcome         string
	Phase           string
	ClientCommandID string
	DomainRefType   string
	DomainRefID     int64
	CorrelationID   string
	RequestID       string
	InitiatorType   string
	InitiatorID     int64
	Targets         []RecordTarget
}

// Writer persists audit records inside an existing transaction. Create
// writers with NewWriter.
type Writer struct {
	now func() time.Time
}

// NewWriter returns the canonical audit writer using server-side UTC time.
func NewWriter() *Writer {
	return &Writer{now: time.Now}
}

// NewWriterWithClock is NewWriter with an injectable clock, so a service
// owning a deterministic process clock produces audit created_at values on
// that same clock. The clock must return UTC-representable instants; values
// are canonicalized exactly like the default writer's.
func NewWriterWithClock(now func() time.Time) *Writer {
	if now == nil {
		return NewWriter()
	}
	return &Writer{now: now}
}

// Write validates the whole record, inserts its audit_events row and one
// audit_event_targets row per target, and returns the new event id. Targets
// are validated before the event INSERT so an invalid record cannot leave a
// partial row even inside a transaction that a caller fails to roll back.
// Any error must abort the caller's transaction at its boundary — audit
// failures are never swallowed and never release content.
func (w *Writer) Write(ctx context.Context, db DB, record Record) (int64, error) {
	if err := record.validate(); err != nil {
		return 0, err
	}
	phase := record.Phase
	if phase == "" {
		phase = PhaseExecute
	}
	result, err := db.ExecContext(ctx, `
		INSERT INTO audit_events(
			actor_type,actor_id,action,outcome,client_command_id,
			domain_ref_type,domain_ref_id,created_at,
			correlation_id,request_id,phase,initiator_type,initiator_id
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.ActorType, record.ActorID, record.Action, record.Outcome, nonEmpty(record.ClientCommandID),
		nonEmpty(record.DomainRefType), record.DomainRefID, CanonicalTimestamp(w.now()),
		record.CorrelationID, nonEmpty(record.RequestID), phase,
		nonEmpty(record.InitiatorType), record.InitiatorID)
	if err != nil {
		return 0, fmt.Errorf("audit: persist event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("audit: read event id: %w", err)
	}
	for _, target := range record.Targets {
		var version any
		if target.Version != nil {
			version = *target.Version
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO audit_event_targets(audit_event_id,target_type,target_id,target_version) VALUES(?,?,?,?)`,
			id, target.Type, target.ID, version); err != nil {
			return 0, fmt.Errorf("audit: persist event target: %w", err)
		}
	}
	return id, nil
}

// CleanupBatch is the durable record of one executed retention cleanup batch
// (docs/audit-design.md §7): its cutoff point, scope summary and quantities —
// never the deleted content itself. Owned here because RecordCleanupBatch
// persists it; retention.go's controller fills it per batch.
type CleanupBatch struct {
	CutoffAt       string // deletion cutoff: events recorded before this instant
	UpperEventID   int64  // highest event id eligible in this batch
	DeletedEvents  int64
	DeletedTargets int64
	Final          bool   // batch reached the cutoff; no further eligible rows
	CreatedAt      string // server-side batch time; empty uses the writer clock
}

// RecordCleanupBatch persists one cleanup batch record and its audit event on
// the caller's open transaction, atomically with the batch's deletions: the
// event's domain reference is the new audit_cleanup_batches row. The record
// carries the controller's system principal and the cleanup run's correlation
// — the writer never invents identity to fill the gap. The audit sink is
// required — on any error the caller must roll the whole batch back, retain
// the expired data and alert; no fallback deletes without a record. Only
// successful batches are recorded: a failed batch must never be declared
// cleaned.
func (w *Writer) RecordCleanupBatch(ctx context.Context, tx *sql.Tx, record Record, batch CleanupBatch) error {
	if err := record.validate(); err != nil {
		return err
	}
	if batch.CutoffAt == "" || len(batch.CutoffAt) > 64 {
		return errors.New("audit: cleanup batch cutoff is required")
	}
	if batch.DeletedEvents < 0 || batch.DeletedTargets < 0 || batch.UpperEventID < 0 {
		return errors.New("audit: cleanup batch quantities must not be negative")
	}
	createdAt := batch.CreatedAt
	if createdAt == "" {
		createdAt = CanonicalTimestamp(w.now())
	}
	final := 0
	if batch.Final {
		final = 1
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO audit_cleanup_batches(
			cutoff_at,upper_event_id,deleted_events,deleted_targets,final,created_at
		) VALUES(?,?,?,?,?,?)`,
		batch.CutoffAt, batch.UpperEventID, batch.DeletedEvents, batch.DeletedTargets, final, createdAt)
	if err != nil {
		return fmt.Errorf("audit: persist cleanup batch: %w", err)
	}
	batchID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("audit: read cleanup batch id: %w", err)
	}
	// The batch row is the audit event's authoritative domain reference.
	record.DomainRefType = "audit_cleanup_batch"
	record.DomainRefID = batchID
	if _, err := w.Write(ctx, tx, record); err != nil {
		return err
	}
	return nil
}

// validate enforces the record whitelist and identity rules for new rows.
// Historical rows are the only records allowed to read as correlation-less;
// the writer never creates one. Validation errors name the offending field
// without echoing raw values.
func (record Record) validate() error {
	if !actorTypes[record.ActorType] {
		return errors.New("audit: invalid actor type")
	}
	if err := validatePrincipal("actor", record.ActorType, record.ActorID); err != nil {
		return err
	}
	if record.Action == "" {
		return errors.New("audit: action is required")
	}
	if !outcomes[record.Outcome] {
		return errors.New("audit: invalid outcome")
	}
	if record.Phase != "" && !phases[record.Phase] {
		return errors.New("audit: invalid phase")
	}
	if !validIdentity(record.CorrelationID) {
		return fmt.Errorf("audit: correlation id must contain 1 to %d printable ASCII characters", maxIdentityLength)
	}
	if record.RequestID != "" && !validIdentity(record.RequestID) {
		return fmt.Errorf("audit: request id must contain 1 to %d printable ASCII characters", maxIdentityLength)
	}
	if record.InitiatorType != "" {
		if err := validatePrincipal("initiator", record.InitiatorType, record.InitiatorID); err != nil {
			return err
		}
	} else if record.InitiatorID != 0 {
		return errors.New("audit: initiator id requires initiator type")
	}
	for _, target := range record.Targets {
		if target.Type == "" {
			return errors.New("audit: event target type is required")
		}
		if target.ID < 0 {
			return errors.New("audit: event target id must not be negative")
		}
		if target.Version != nil && *target.Version < 1 {
			return errors.New("audit: event target version must be positive when present")
		}
	}
	return nil
}

var (
	actorTypes = map[string]bool{ActorUser: true, ActorService: true, ActorSystem: true}
	outcomes   = map[string]bool{OutcomeSuccess: true, OutcomeFailure: true, OutcomeRejected: true, OutcomeUnknown: true}
	phases     = map[string]bool{PhaseAccess: true, PhaseExecute: true}
)

func validatePrincipal(role, kind string, id int64) error {
	if !actorTypes[kind] {
		return fmt.Errorf("audit: invalid %s type", role)
	}
	if kind == ActorSystem {
		if id != 0 {
			return fmt.Errorf("audit: the system %s uses id 0", role)
		}
		return nil
	}
	if id < 1 {
		return fmt.Errorf("audit: %s %s requires a positive id", kind, role)
	}
	return nil
}

// validIdentity accepts opaque bounded tokens: printable non-space ASCII,
// no control characters, no non-ASCII.
func validIdentity(value string) bool {
	if value == "" || len(value) > maxIdentityLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= 0x20 || value[i] >= 0x7F {
			return false
		}
	}
	return true
}

func nonEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
