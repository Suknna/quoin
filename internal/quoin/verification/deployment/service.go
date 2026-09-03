// Package deployment owns the Deployment Acceptance invocation lifecycle:
// the immutable manifest/item/locator freeze, append-only results, helper
// exchange, typed observations, subject drift, and the single finalization
// receipt (verification.md §11–§12). There is deliberately no mutable
// invocation status or current pointer; the receipt's existence is the only
// completion fact.
package deployment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/contract"
)

// Service owns the Deployment Acceptance SQLite transactions.
type Service struct {
	db               *sql.DB
	now              func() time.Time
	binding          *contract.DeploymentBinding
	publicOrigin     string
	artifacts        artifactStore
	browserCanceller func(ctx context.Context, invocationID int64)
	execution        ExecutionHooks
}

// artifactStore is the narrow surface the finalizer/helper import need from
// the artifact store. It is wired by app.Run once the store exists; before
// that, finalization is refused rather than silently skipped.
type artifactStore interface {
	// CommitVerificationArtifact stages canonical bytes durably. createdAt is
	// the finalizer's frozen commit instant so the receipt's time closure and
	// the artifact rows never disagree.
	CommitVerificationArtifact(ctx context.Context, kind, mediaType, retentionKind string, ownerID int64, body []byte, createdAt time.Time) (int64, string, error)
}

// NewService constructs the Deployment Acceptance authority. The deployment
// binding may be nil (local development projection); starting an invocation
// is then refused — the deployment has no release subject to accept against.
func NewService(db *sql.DB, now func() time.Time, binding *contract.DeploymentBinding, publicOrigin string) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now, binding: binding, publicOrigin: publicOrigin}
}

// SetArtifactStore wires the durable artifact store. It is called by Run
// after handler construction; the store is request-time state, not
// constructor state, mirroring the application artifact wiring.
func (service *Service) SetArtifactStore(store artifactStore) {
	service.artifacts = store
}

// SetBrowserCanceller wires the runtime stop dispatcher for started
// deployment verification operations. Cancellation of a physically started
// operation must flow through the real stop path, never a local overwrite.
func (service *Service) SetBrowserCanceller(canceller func(ctx context.Context, invocationID int64)) {
	service.browserCanceller = canceller
}

// Now exposes the deterministic service clock (test/adapter seam).
func (service *Service) Now() time.Time { return service.now() }

// Binding exposes the immutable deployment binding (nil on dev deployments).
func (service *Service) Binding() *contract.DeploymentBinding {
	return service.binding
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// sha256Text is the digest of a plain string authority (public Origin).
func sha256Text(value string) string {
	return sha256Hex([]byte(value))
}

// canonicalJSON renders a value with sorted keys and no insignificant
// whitespace; every frozen digest in this package is over these bytes.
func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize verification document: %w", err)
	}
	return encoded, nil
}

func canonicalDigest(value any) (string, error) {
	body, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(body), nil
}

// errorFamily maps service failures onto the HTTP problem space.
type errorFamily string

const (
	errInvalid     errorFamily = "invalid"
	errState       errorFamily = "state"
	errConflict    errorFamily = "conflict"
	errUnavailable errorFamily = "unavailable"
	errNotFound    errorFamily = "not_found"
)

// Error is a typed service failure carrying its HTTP-facing family.
type Error struct {
	Family  errorFamily
	Message string
}

func (e *Error) Error() string { return e.Message }

func (service *Service) fail(family errorFamily, format string, arguments ...any) error {
	return &Error{Family: family, Message: fmt.Sprintf(format, arguments...)}
}

// ErrBindingUnavailable reports that this deployment carries no frozen
// release subject (local development projection).
func (service *Service) ErrBindingUnavailable() error {
	return service.fail(errUnavailable, "deployment acceptance requires a release-manifest deployment; this deployment has no frozen deployment binding")
}

// withConn runs fn on one serialized writer connection (BEGIN IMMEDIATE),
// the transaction discipline shared with the other command authorities.
func (service *Service) withConn(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	return nil
}
