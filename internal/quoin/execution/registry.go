package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Class is the declared read/write category of an operation. Write operations
// may only execute through the runner's owned transactions; reads use the
// read-only database capability.
type Class string

const (
	ClassRead  Class = "read"
	ClassWrite Class = "write"
)

// Operation is the declared contract of one operation: a stable identity, its
// read/write class, the object location contract (the domain object type it
// acts on, used as audit domain_ref and ledger result_object_type), and the
// authorization callback.
type Operation struct {
	// Name is the stable operation identity, e.g. "backup.trigger". It must
	// never be guessed from URLs or SQL and never change meaning.
	Name string
	// Class distinguishes reads from authoritative state changes.
	Class Class
	// ObjectType locates the domain object the operation acts on.
	ObjectType string
	// Authorize re-verifies inside the runner's open transaction — before
	// replay lookup and before business — that the context actor may execute
	// this operation right now. It is mandatory: a nil callback fails
	// registration-time checks at Run/Execute, because replaying a stored
	// result for a since-revoked principal would leak. A plain error aborts
	// with a clean rollback and no durable record (e.g. a revoked session);
	// a *Rejection is recorded deterministically like a business rejection.
	Authorize func(ctx context.Context, tx *Tx) error
	// TargetVersion optionally resolves the authoritative version of the
	// audited object inside the same transaction, so the audit target pins
	// the applicable version (DATA-AUDIT-001). When absent or when it
	// reports false, the target records no version — absent stays an honest
	// fact instead of a fabricated one.
	TargetVersion func(ctx context.Context, tx *Tx, objectID int64) (int64, bool)
}

func (op Operation) validate() error {
	if op.Name == "" {
		return errors.New("execution: operation name is required")
	}
	if op.Class != ClassRead && op.Class != ClassWrite {
		return fmt.Errorf("execution: operation %q must declare class read or write, got %q", op.Name, op.Class)
	}
	if op.ObjectType == "" {
		return fmt.Errorf("execution: operation %q must declare its object type", op.Name)
	}
	return nil
}

// Registry holds the operation declarations. Missing declarations fail at
// Run/Execute and in registration checks — the runner never infers business
// semantics from URLs or SQL.
type Registry struct {
	mu  sync.RWMutex
	ops map[string]*Operation
}

// NewRegistry returns an empty operation registry.
func NewRegistry() *Registry {
	return &Registry{ops: map[string]*Operation{}}
}

// Register validates the declaration and stores it. The returned pointer is
// the canonical identity callers pass to Run/Execute; registering the same
// name twice fails.
func (r *Registry) Register(op Operation) (*Operation, error) {
	if err := op.validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.ops[op.Name]; exists {
		return nil, fmt.Errorf("execution: operation %q is already registered", op.Name)
	}
	stored := &Operation{
		Name:          op.Name,
		Class:         op.Class,
		ObjectType:    op.ObjectType,
		Authorize:     op.Authorize,
		TargetVersion: op.TargetVersion,
	}
	r.ops[op.Name] = stored
	return stored, nil
}

// Lookup returns the registered operation by name.
func (r *Registry) Lookup(name string) (*Operation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	op, ok := r.ops[name]
	return op, ok
}
