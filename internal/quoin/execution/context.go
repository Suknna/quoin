package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrMissingContext marks an audited operation invoked without execution
// metadata. Callers must fail closed: no synthetic context and no anonymous
// "assume system" fallback may mask the gap (ADR-0006).
var ErrMissingContext = errors.New("execution: execution context with correlation metadata is required")

// PrincipalKind is the type of an acting or initiating principal. Values
// mirror the audit_events.actor_type CHECK constraint and (since the
// client_commands principal_type CHECK gained 'system') the command ledger.
type PrincipalKind string

const (
	PrincipalUser    PrincipalKind = "user"
	PrincipalService PrincipalKind = "service"
	PrincipalSystem  PrincipalKind = "system"
)

// Principal identifies who acts. ID is the principal's database row id; the
// system principal owns no row and uses ID 0.
type Principal struct {
	Kind PrincipalKind
	ID   int64
}

// SourceKind states where an operation entered Quoin.
type SourceKind string

const (
	SourceHTTP      SourceKind = "http"
	SourceScheduler SourceKind = "scheduler"
	SourceCLI       SourceKind = "cli"
	SourceTask      SourceKind = "task"
	SourceInternal  SourceKind = "internal"
)

// Source records the entry channel and the per-request identity. RequestID is
// independent of CorrelationID: idempotent replays keep the original
// business correlation while each request keeps its own identity.
type Source struct {
	Kind      SourceKind
	RequestID string
}

// SessionRef is the verified session proof reference carried by user-origin
// metadata: the authenticated session's row id and the auth revision it was
// issued at. It is a reference that lets in-transaction re-authorization
// (e.g. VerifyExecutionSession on the runner's guarded Executor) confirm the
// session is still unrevoked, unexpired, at the current revision, and
// permitted for the required role. It is never a credential: no token,
// bearer, digest or secret travels in it. A user authentication flow that
// has no session yet carries the zero value and authorizes through its own
// flow checks instead.
type SessionRef struct {
	ID           int64
	AuthRevision int64
}

// Metadata is the typed, immutable correlation metadata carried through
// context.Context: one CorrelationID for the whole business operation, the
// current Actor, the original Initiator (equal to Actor for direct execution;
// preserved when background execution acts on the initiator's behalf), the
// entry Source, and the optional Session reference of user-origin operations.
// It carries request-scoped facts only — never passwords, OTPs, tokens,
// permission claims or dependency handles.
type Metadata struct {
	CorrelationID string
	Actor         Principal
	Initiator     Principal
	Source        Source
	Session       SessionRef
}

var (
	principalKinds = map[PrincipalKind]bool{PrincipalUser: true, PrincipalService: true, PrincipalSystem: true}
	sourceKinds    = map[SourceKind]bool{SourceHTTP: true, SourceScheduler: true, SourceCLI: true, SourceTask: true, SourceInternal: true}
)

// maxIdentityLength bounds propagated identifier fields.
const maxIdentityLength = 128

type contextKey struct{}

// NewCorrelationID creates a fresh 128-bit correlation identifier for a
// trusted entry point. Public clients can never forge internal correlations:
// propagation across processes happens through authenticated, validated
// channels, never through client-supplied headers.
func NewCorrelationID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("execution: create correlation id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// WithMetadata validates metadata and attaches it under a private, typed key.
// Entry points call it once per business operation on a context that does not
// carry metadata yet; sub-steps inherit the returned context. Replacing the
// correlation metadata of an existing context fails here — deliberate
// re-rooting (background task restore, flow restart) must go through
// ReplaceMetadata so it cannot happen by accident.
func WithMetadata(ctx context.Context, meta Metadata) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("execution: nil context")
	}
	if _, exists := FromContext(ctx); exists {
		return nil, errors.New("execution: context already carries execution metadata; sub-steps must not replace the correlation id (use ReplaceMetadata for deliberate re-rooting)")
	}
	meta = meta.normalized()
	if err := meta.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, meta), nil
}

// ReplaceMetadata is the deliberate re-rooting helper for flows that restore
// persisted metadata — a background task rebuilt after restart, a persisted
// authentication flow reattached to a fresh request context. Unlike
// WithMetadata it may replace existing metadata, and it must only ever be
// called with the fresh request or task scope as parent so the finished
// request's cancellation and deadline do not leak into the new scope.
func ReplaceMetadata(ctx context.Context, meta Metadata) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("execution: nil context")
	}
	meta = meta.normalized()
	if err := meta.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, meta), nil
}

// FromContext returns the metadata when present.
func FromContext(ctx context.Context) (Metadata, bool) {
	if ctx == nil {
		return Metadata{}, false
	}
	meta, ok := ctx.Value(contextKey{}).(Metadata)
	return meta, ok
}

// Require returns the metadata or ErrMissingContext. Audited operations call
// this instead of guessing a default identity.
func Require(ctx context.Context) (Metadata, error) {
	meta, ok := FromContext(ctx)
	if !ok {
		return Metadata{}, ErrMissingContext
	}
	return meta, nil
}

// Delegate attaches the preserved correlation metadata to parent with the
// actor replaced by the executing principal, so audit records can state the
// original initiator and the actual executor separately. The session proof
// reference is cleared: background execution is authorized by the
// background principal's own identity, never by the user's session. Like
// every derived context it inherits parent's cancellation and deadline:
// callers must pass the fresh background task scope — not the finished HTTP
// request context. Rebuilding metadata from persisted task state after a
// process restart goes through ReplaceMetadata on that new task context
// instead.
func Delegate(parent context.Context, actor Principal) (context.Context, error) {
	meta, ok := FromContext(parent)
	if !ok {
		return nil, ErrMissingContext
	}
	meta = Metadata{
		CorrelationID: meta.CorrelationID,
		Actor:         actor,
		Initiator:     meta.Initiator,
		Source:        meta.Source,
	}.normalized()
	if err := meta.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(parent, contextKey{}, meta), nil
}

// normalized defaults a missing initiator to the actor: a directly executed
// operation initiates itself.
func (meta Metadata) normalized() Metadata {
	if meta.Initiator.Kind == "" {
		meta.Initiator = meta.Actor
	}
	return meta
}

func (meta Metadata) validate() error {
	if !validIdentity(meta.CorrelationID) {
		return fmt.Errorf("execution: correlation id must contain 1 to %d printable ASCII characters", maxIdentityLength)
	}
	if err := validatePrincipal("actor", meta.Actor.Kind, meta.Actor.ID); err != nil {
		return err
	}
	if err := validatePrincipal("initiator", meta.Initiator.Kind, meta.Initiator.ID); err != nil {
		return err
	}
	if !sourceKinds[meta.Source.Kind] {
		return fmt.Errorf("execution: invalid source kind %q", meta.Source.Kind)
	}
	if meta.Source.RequestID != "" && !validIdentity(meta.Source.RequestID) {
		return fmt.Errorf("execution: request id must contain 1 to %d printable ASCII characters", maxIdentityLength)
	}
	if err := validateSession(meta); err != nil {
		return err
	}
	return nil
}

// validateSession enforces the optional session proof reference: both fields
// zero (no session — flows and background execution) or both positive, and
// only on a user actor. A service or system actor never carries a user's
// session proof; their authorization is their own principal.
func validateSession(meta Metadata) error {
	session := meta.Session
	if session.ID == 0 && session.AuthRevision == 0 {
		return nil
	}
	if meta.Actor.Kind != PrincipalUser {
		return fmt.Errorf("execution: a %s actor cannot carry a session proof reference", meta.Actor.Kind)
	}
	if session.ID < 1 || session.AuthRevision < 1 {
		return errors.New("execution: session reference requires both session id and auth revision positive, or both zero")
	}
	return nil
}

func validatePrincipal(role string, kind PrincipalKind, id int64) error {
	if !principalKinds[kind] {
		return fmt.Errorf("execution: invalid %s kind %q", role, kind)
	}
	if kind == PrincipalSystem {
		if id != 0 {
			return fmt.Errorf("execution: the system %s uses id 0", role)
		}
		return nil
	}
	if id < 1 {
		return fmt.Errorf("execution: %s %s requires a positive principal id", kind, role)
	}
	return nil
}

// validIdentity accepts opaque bounded tokens: 1 to maxIdentityLength
// printable non-space ASCII characters, no control characters, no non-ASCII.
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
