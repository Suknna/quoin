// Package runtime owns the Quoin-side Runtime slot authority (T06):
// one-time registration tokens in memory, the Register RPC transition, the
// in-process control-stream projection, and Hello handshake adjudication for
// Plinth and Lintel. Persistent authority lives in runtime_slots /
// runtime_credentials per the frozen schema; online connection state
// (connected/boot/epoch/lastSeen) is memory-only (DATA-RUNTIME-001).
package runtime

import (
	"context"
	rand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

const (
	SlotPlinth = "plinth"
	SlotLintel = "lintel"
	// registrationTokenTTL bounds the one-time reveal window (SEC-REVEAL-002).
	registrationTokenTTL = 60 * time.Second
)

// SlotState enumerates runtime_slots.state.
type SlotState string

const (
	StateUnregistered SlotState = "unregistered"
	StateRegistered   SlotState = "registered"
	StateRevoked      SlotState = "revoked"
)

// SlotView is the RuntimeSlot HTTP projection (persistent + transient).
type SlotView struct {
	Slot                 string    `json:"slot"`
	State                SlotState `json:"state"`
	CurrentGeneration    int64     `json:"currentGeneration"`
	PendingGeneration    *int64    `json:"pendingGeneration,omitempty"`
	RetiringGeneration   *int64    `json:"retiringGeneration,omitempty"`
	RetirementState      *string   `json:"retirementState,omitempty"`
	RowVersion           int64     `json:"rowVersion"`
	Connected            bool      `json:"connected"`
	BootID               string    `json:"bootId,omitempty"`
	ConnectionEpoch      *uint64   `json:"connectionEpoch,omitempty"`
	LastSeenAt           *string   `json:"lastSeenAt,omitempty"`
	FirstAuthenticatedAt *string   `json:"currentFirstAuthenticatedAt,omitempty"`
}

var (
	ErrNotFound       = errors.New("runtime slot not found")
	ErrRowVersion     = errors.New("expected row version does not match")
	ErrActiveConflict = errors.New("slot is not in the required state")
	// ErrTokenGone maps to 410 for unknown/expired/consumed reveal handles.
	ErrTokenGone = errors.New("registration token handle is invalid")
)

// RowVersionError carries the authoritative runtime_slots.row_version.
type RowVersionError struct{ Current int64 }

func (e *RowVersionError) Error() string { return ErrRowVersion.Error() }
func (e *RowVersionError) Unwrap() error { return ErrRowVersion }

// registrationToken is the in-memory one-time token (RUNTIME-REG-001): the
// raw 32-byte value exists only here and in the single reveal response; the
// database later sees its digest only.
type registrationToken struct {
	slot       string
	generation int64
	raw        string
	digest     [32]byte
	session    [32]byte // creating admin session digest
	expiresAt  time.Time
	consumed   bool
}

// connection is the transient control-stream projection (RUNTIME-CTRL-001).
// closing carries the stop signal for the handler loop: cancelling the
// stream context makes gRPC terminate the RPC on both ends.
type connection struct {
	bootID               string
	epoch                uint64
	updated              time.Time
	closing              chan struct{}
	once                 sync.Once
	sender               StreamSender
	outbound             uint64
	browserCapacitySlots uint32
}

func (connection *connection) close() {
	if connection.closing != nil {
		connection.once.Do(func() { close(connection.closing) })
	}
}

// Service wires the slot authority together.
type Service struct {
	db  *sql.DB
	now func() time.Time

	mu      sync.Mutex
	tokens  map[string]*registrationToken // handle -> token
	pending map[string]*registrationToken // slot+"\x00"+generation -> token awaiting Register
	conns   map[string]*connection        // slot -> active stream
	// bootEpochs remembers the highest accepted epoch per (slot, boot) so
	// stale reconnects are rejected with EPOCH_STALE (RUNTIME-CTRL-004).
	bootEpochs map[string]uint64
}

func NewService(db *sql.DB) *Service {
	return &Service{
		db:         db,
		now:        time.Now,
		tokens:     map[string]*registrationToken{},
		pending:    map[string]*registrationToken{},
		conns:      map[string]*connection{},
		bootEpochs: map[string]uint64{},
	}
}

func ValidSlot(slot string) bool { return slot == SlotPlinth || slot == SlotLintel }

// View projects one slot for HTTP.
func (service *Service) View(ctx context.Context, slot string) (SlotView, error) {
	if !ValidSlot(slot) {
		return SlotView{}, ErrNotFound
	}
	row := service.db.QueryRowContext(ctx, `
		SELECT s.state, s.row_version,
		       COALESCE((SELECT generation FROM runtime_credentials WHERE id = s.current_credential_id), 0),
		       (SELECT generation FROM runtime_credentials WHERE id = s.pending_credential_id),
		       (SELECT generation FROM runtime_credentials WHERE id = s.retiring_credential_id),
		       (SELECT first_authenticated_at FROM runtime_credentials WHERE id = s.current_credential_id)
		FROM runtime_slots s WHERE slot = ?`, slot)
	view := SlotView{Slot: slot}
	var pendingGen, retiringGen sql.NullInt64
	var firstAuth sql.NullString
	if err := row.Scan(&view.State, &view.RowVersion, &view.CurrentGeneration, &pendingGen, &retiringGen, &firstAuth); err != nil {
		return SlotView{}, err
	}
	if pendingGen.Valid {
		value := pendingGen.Int64
		view.PendingGeneration = &value
	}
	if retiringGen.Valid {
		value := retiringGen.Int64
		view.RetiringGeneration = &value
		state := "AwaitingFirstUse"
		if firstAuth.Valid {
			state = "PendingRetirement"
		}
		view.RetirementState = &state
	}
	if firstAuth.Valid {
		view.FirstAuthenticatedAt = &firstAuth.String
	}
	service.mu.Lock()
	conn, live := service.conns[slot]
	service.mu.Unlock()
	if live {
		view.Connected = true
		view.BootID = conn.bootID
		epoch := conn.epoch
		view.ConnectionEpoch = &epoch
		seen := conn.updated.UTC().Format(time.RFC3339Nano)
		view.LastSeenAt = &seen
	}
	return view, nil
}

// viewFromConn renders the same projection from an already-open connection,
// so a caller holding a write transaction (single-connection pool) can read
// its own committed state without deadlocking.
func (service *Service) viewFromConn(ctx context.Context, conn *sql.Conn, slot string) (SlotView, error) {
	row := conn.QueryRowContext(ctx, `
		SELECT s.state, s.row_version,
		       COALESCE((SELECT generation FROM runtime_credentials WHERE id = s.current_credential_id), 0),
		       (SELECT generation FROM runtime_credentials WHERE id = s.pending_credential_id),
		       (SELECT generation FROM runtime_credentials WHERE id = s.retiring_credential_id),
		       (SELECT first_authenticated_at FROM runtime_credentials WHERE id = s.current_credential_id)
		FROM runtime_slots s WHERE slot = ?`, slot)
	view := SlotView{Slot: slot}
	var pendingGen, retiringGen sql.NullInt64
	var firstAuth sql.NullString
	if err := row.Scan(&view.State, &view.RowVersion, &view.CurrentGeneration, &pendingGen, &retiringGen, &firstAuth); err != nil {
		return SlotView{}, err
	}
	if pendingGen.Valid {
		value := pendingGen.Int64
		view.PendingGeneration = &value
	}
	if retiringGen.Valid {
		value := retiringGen.Int64
		view.RetiringGeneration = &value
		state := "AwaitingFirstUse"
		if firstAuth.Valid {
			state = "PendingRetirement"
		}
		view.RetirementState = &state
	}
	if firstAuth.Valid {
		view.FirstAuthenticatedAt = &firstAuth.String
	}
	service.mu.Lock()
	connView, live := service.conns[slot]
	service.mu.Unlock()
	if live {
		view.Connected = true
		view.BootID = connView.bootID
		epoch := connView.epoch
		view.ConnectionEpoch = &epoch
		seen := connView.updated.UTC().Format(time.RFC3339Nano)
		view.LastSeenAt = &seen
	}
	return view, nil
}

// PrepareRegistration implements prepareRuntimeRegistration
// (HTTP-COMMAND-012): first registration keeps unregistered and prepares
// generation 1; replacement confirms the DATA-RUNTIME-001b fence (no active
// execution attempts bound to the slot, no un-stopped lintel browser
// operations), then revokes the slot; the schema trigger retires its
// credentials. The one-time token is created only after the transaction
// commits and lives in memory for 60 seconds bound to the creating admin
// session. Returns (view, handle, available).
func (service *Service) PrepareRegistration(ctx context.Context, slot string, expectedRowVersion int64, sessionDigest [32]byte) (SlotView, string, bool, error) {
	if !ValidSlot(slot) {
		return SlotView{}, "", false, ErrNotFound
	}
	// Same-session replay while the original handle is still valid returns
	// the same handle without re-preparing (SEC-REVEAL-003).
	if handle, _, ok := service.LookupToken(sessionDigest, slot); ok {
		view, viewErr := service.View(ctx, slot)
		return view, handle, true, viewErr
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return SlotView{}, "", false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return SlotView{}, "", false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var state string
	var rowVersion int64
	if err := conn.QueryRowContext(ctx, `SELECT state,row_version FROM runtime_slots WHERE slot=?`, slot).Scan(&state, &rowVersion); err != nil {
		return SlotView{}, "", false, err
	}
	if rowVersion != expectedRowVersion {
		return SlotView{}, "", false, &RowVersionError{Current: rowVersion}
	}
	if state != string(StateUnregistered) {
		// Replacement: DATA-RUNTIME-001b fence first — conflicts must not
		// revoke anything (HTTP-RUNTIME-002).
		var activeAttempts int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE runtime_slot=? AND state IN ('Queued','Assigned','Running','Cancelling')`, slot).Scan(&activeAttempts); err != nil {
			return SlotView{}, "", false, err
		}
		if activeAttempts > 0 {
			return SlotView{}, "", false, fmt.Errorf("%w: slot has active execution attempts", ErrActiveConflict)
		}
		if slot == SlotLintel {
			var activeOps int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE stop_confirmed_at IS NULL`).Scan(&activeOps); err != nil {
				return SlotView{}, "", false, err
			}
			if activeOps > 0 {
				return SlotView{}, "", false, fmt.Errorf("%w: lintel has active browser operations", ErrActiveConflict)
			}
		}
		// Single UPDATE: revoke + clear pointers; the AFTER trigger
		// retires every unretired credential of the slot.
		result, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET state='revoked',current_credential_id=NULL,pending_credential_id=NULL,retiring_credential_id=NULL,row_version=row_version+1 WHERE slot=? AND row_version=?`, slot, rowVersion)
		if err != nil {
			return SlotView{}, "", false, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return SlotView{}, "", false, &RowVersionError{Current: rowVersion}
		}
	} else {
		result, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET row_version=row_version+1 WHERE slot=? AND row_version=? AND state='unregistered'`, slot, rowVersion)
		if err != nil {
			return SlotView{}, "", false, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return SlotView{}, "", false, &RowVersionError{Current: rowVersion}
		}
	}
	var nextGeneration int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM runtime_credentials WHERE slot=?`, slot).Scan(&nextGeneration); err != nil {
		return SlotView{}, "", false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return SlotView{}, "", false, err
	}
	committed = true
	// Replacement revoked the slot: close the old control stream so the
	// replaced runtime loses its epoch immediately (RUNTIME-REVOKE-001).
	if state != string(StateUnregistered) {
		service.CloseSlot(slot)
	}

	// Token creation after commit; raw value exists only in memory.
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return SlotView{}, "", false, err
	}
	handleBytes := make([]byte, 24)
	if _, err := rand.Read(handleBytes); err != nil {
		return SlotView{}, "", false, err
	}
	handle := base64.RawURLEncoding.EncodeToString(handleBytes)
	token := &registrationToken{
		slot: slot, generation: nextGeneration,
		raw:    base64.RawURLEncoding.EncodeToString(rawBytes),
		digest: sha256.Sum256(rawBytes), session: sessionDigest,
		expiresAt: service.now().Add(registrationTokenTTL),
	}
	service.mu.Lock()
	service.tokens[handle] = token
	service.pending[slot+"\x00"+itoa64(nextGeneration)] = token
	service.mu.Unlock()
	// Read through the already-open transaction connection (the pool holds
	// exactly one connection; a second read here would deadlock).
	view, err := service.viewFromConn(ctx, conn, slot)
	if err != nil {
		return SlotView{}, "", false, err
	}
	return view, handle, true, nil
}

// LookupToken serves same-session command replays (SEC-REVEAL-003): the
// original still-valid unconsumed handle is returned, else unavailable.
func (service *Service) LookupToken(sessionDigest [32]byte, slot string) (handle string, generation int64, ok bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.evictLocked()
	for candidate, token := range service.tokens {
		if token.session == sessionDigest && token.slot == slot && !token.consumed {
			return candidate, token.generation, true
		}
	}
	return "", 0, false
}

// RevealToken consumes the reveal handle once (revealRuntimeRegistrationToken,
// SEC-REVEAL-002: the HANDLE is single-consumption): it returns the raw
// registration token and drops the handle. The underlying one-time
// registration token itself is consumed later by Register
// (RUNTIME-REG-002); until then the same session may re-prepare and reveal
// only through a new prepare replay of the same still-valid command.
func (service *Service) RevealToken(handle string, sessionDigest [32]byte) (raw, slot string, generation int64, err error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.evictLocked()
	token, exists := service.tokens[handle]
	if !exists || token.session != sessionDigest || token.consumed || !service.now().Before(token.expiresAt) {
		return "", "", 0, ErrTokenGone
	}
	// Handle consumed: drop it so a second redemption answers 410; the raw
	// token remains valid for exactly one Register (tracked in pending).
	delete(service.tokens, handle)
	return token.raw, token.slot, token.generation, nil
}

func itoa64(value int64) string {
	return strconv.FormatInt(value, 10)
}

// InvalidateSession drops every token bound to a session (logout/revoke).
func (service *Service) InvalidateSession(sessionDigest [32]byte) {
	service.mu.Lock()
	defer service.mu.Unlock()
	for handle, token := range service.tokens {
		if token.session == sessionDigest {
			delete(service.tokens, handle)
		}
	}
}

func (service *Service) evictLocked() {
	now := service.now()
	for handle, token := range service.tokens {
		if token.consumed || !now.Before(token.expiresAt) {
			delete(service.tokens, handle)
		}
	}
}
