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

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// ErrNotFound reports a missing business system or version.
var ErrNotFound = errors.New("business system or config version not found")

// ErrCommandReused reports a client command id replayed with a different
// request digest (HTTP-COMMAND-003).
var ErrCommandReused = errors.New("client command id reused with a different request")

// ErrBrowserIdentityMissing reports a draft whose browser checks cannot run
// because the business system has no Browser Identity configured yet.
var ErrBrowserIdentityMissing = errors.New("business system has no browser identity")

// ConflictError carries the frozen publish conflict codes.
type ConflictError struct {
	Code           string // row_version_conflict | current_pointer_conflict | active_conflict
	Detail         string
	SystemKey      string
	ObjectID       int64
	CurrentVersion *int64 // actual current published version id (nil = none)
}

func (err *ConflictError) Error() string { return err.Detail }

// UploadInput carries the raw declaration and optional embedded catalog digest.
type UploadInput struct {
	YAMLBody             []byte
	JourneyCatalogDigest string
}

// Service owns the Business System SQLite transactions.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service                   { return &Service{db: db, now: time.Now} }
func (service *Service) UseClock(now func() time.Time) { service.now = now }
func (service *Service) DB() *sql.DB                   { return service.db }
func (service *Service) nowText() string               { return service.now().UTC().Format(time.RFC3339Nano) }

// Upload parses a quoin/v1 declaration once, resolves stable reference names in
// the serialized writer transaction, and stores that fully resolved declaration
// as immutable JSON. A Label Contract is not an active upload/publish dependency.
func (service *Service) audit(ctx context.Context, conn *sql.Conn, principalID int64, action string, systemID int64, timestamp string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,'success','business_system',?,?)`,
		principalID, action, systemID, timestamp)
	return err
}


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
