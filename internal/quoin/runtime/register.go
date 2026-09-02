package runtime

// Register RPC transition and Connect-stream handshake (T06):
// RUNTIME-REG-002/003, RUNTIME-AUTH-003/004, RUNTIME-CTRL-002..004/010.

import (
	"context"
	rand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// RegisterError distinguishes the canonical gRPC statuses (RUNTIME-REG-003).
type RegisterError struct {
	Status string // "INVALID_ARGUMENT" | "UNAUTHENTICATED" | "FAILED_PRECONDITION"
	Detail string
}

func (e *RegisterError) Error() string { return fmt.Sprintf("%s: %s", e.Status, e.Detail) }

var (
	errInvalidArgument = &RegisterError{Status: "INVALID_ARGUMENT"}
	errUnauthenticated = &RegisterError{Status: "UNAUTHENTICATED"}
	errPrecondition    = &RegisterError{Status: "FAILED_PRECONDITION"}
)

// Register consumes a one-time token inside the registration window
// (RUNTIME-REG-002): within one IMMEDIATE transaction it creates the
// confirmed credential (generation bound to the token), sets current and
// moves the slot to registered, returning the fresh long-term token.
// releaseVersion must equal this Quoin exactly.
func (service *Service) Register(ctx context.Context, slotName, oneTimeToken string, generation int64, bootID, releaseVersion, currentRelease string) (longTermToken string, generationOut int64, err error) {
	if !ValidSlot(slotName) || generation == 0 || bootID == "" {
		return "", 0, &RegisterError{Status: "INVALID_ARGUMENT", Detail: "slot, generation and boot id are required"}
	}
	raw, err := base64.RawURLEncoding.DecodeString(oneTimeToken)
	if err != nil || len(raw) != 32 {
		return "", 0, &RegisterError{Status: "INVALID_ARGUMENT", Detail: "token must be 32 bytes base64url"}
	}
	if releaseVersion != currentRelease {
		return "", 0, &RegisterError{Status: "FAILED_PRECONDITION", Detail: "release version mismatch"}
	}
	// Single-consume under the service lock: the winner consumes the token
	// atomically; every other racer sees UNAUTHENTICATED (already consumed).
	digest := sha256.Sum256(raw)
	token, ok := service.consumeToken(digest, slotName, generation)
	if !ok {
		return "", 0, &RegisterError{Status: "UNAUTHENTICATED", Detail: "registration token unknown, expired or already consumed"}
	}
	_ = token
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return "", 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if token.recoveryRevision != 0 {
		// Helper-owned Lintel recovery (T35): rotate the current credential
		// through the frozen pending→confirmed→current/retiring protocol
		// instead of the unregistered/revoked registration window.
		longTerm, err := service.registerLintelRecoveryRotation(ctx, conn, token)
		if err != nil {
			return "", 0, err
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return "", 0, err
		}
		committed = true
		return longTerm, token.generation, nil
	}
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM runtime_slots WHERE slot=?`, slotName).Scan(&state); err != nil {
		return "", 0, err
	}
	// Registration window: unregistered (first) or revoked (replacement).
	if state != string(StateUnregistered) && state != string(StateRevoked) {
		return "", 0, &RegisterError{Status: "FAILED_PRECONDITION", Detail: "slot is not in a registration window (ALREADY_REGISTERED)"}
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM runtime_credentials WHERE slot=? AND generation=?`, slotName, generation).Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	longRaw := make([]byte, 32)
	if _, err := rand.Read(longRaw); err != nil {
		return "", 0, err
	}
	longDigest := sha256.Sum256(longRaw)
	var credentialID int64
	if exists == 1 {
		// Replacement retry for the same generation: refresh digest identity
		// is not allowed — a consumed token can never mint twice, so this
		// branch is unreachable in practice; fail closed.
		return "", 0, &RegisterError{Status: "FAILED_PRECONDITION", Detail: "generation already exists"}
	}
	insert, err := conn.ExecContext(ctx, `INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,row_version,created_at) VALUES(?,?,?,?,1,?)`,
		slotName, generation, longDigest[:], now, now)
	if err != nil {
		return "", 0, err
	}
	credentialID, err = insert.LastInsertId()
	if err != nil {
		return "", 0, err
	}
	// registered slot + current pointer in one UPDATE; schema triggers
	// verify the credential is confirmed, unretired and same-slot.
	result, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot=? AND state IN ('unregistered','revoked')`, credentialID, slotName)
	if err != nil {
		return "", 0, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return "", 0, &RegisterError{Status: "FAILED_PRECONDITION", Detail: "slot is not in a registration window (ALREADY_REGISTERED)"}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return "", 0, err
	}
	committed = true
	return base64.RawURLEncoding.EncodeToString(longRaw), generation, nil
}

// registerLintelRecoveryRotation executes the schema-owned two-phase
// rotation for the recovery registration: insert the confirmed replacement,
// set pending, then atomically promote it to current while the predecessor
// becomes retiring (DATA-RUNTIME-001b). The caller owns the open transaction;
// the slot stays registered the whole time — revocation is never used, so
// the old credential survives until the offline finalizer retires it after
// the replacement's first authenticated Hello.
func (service *Service) registerLintelRecoveryRotation(ctx context.Context, conn *sql.Conn, token *registrationToken) (string, error) {
	if token.slot != SlotLintel {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "recovery registration is lintel-only"}
	}
	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return "", err
	}
	if active != 1 || reason != "LintelRecovery" || revision != token.recoveryRevision {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "lintel recovery maintenance revision changed"}
	}
	var state string
	var currentID sql.NullInt64
	var pendingID, retiringID sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT state,current_credential_id,pending_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, SlotLintel).Scan(&state, &currentID, &pendingID, &retiringID); err != nil {
		return "", err
	}
	if state != string(StateRegistered) || !currentID.Valid || pendingID.Valid || retiringID.Valid {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "lintel slot is not in the recovery rotation window"}
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM runtime_credentials WHERE slot=? AND generation=?`, SlotLintel, token.generation).Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if exists == 1 {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "generation already exists"}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	longRaw := make([]byte, 32)
	if _, err := rand.Read(longRaw); err != nil {
		return "", err
	}
	longDigest := sha256.Sum256(longRaw)
	insert, err := conn.ExecContext(ctx, `INSERT INTO runtime_credentials(slot,generation,token_digest,row_version,created_at) VALUES(?,?,?,1,?)`,
		SlotLintel, token.generation, longDigest[:], now)
	if err != nil {
		return "", err
	}
	replacementID, err := insert.LastInsertId()
	if err != nil {
		return "", err
	}
	// The frozen two-phase sequence (DATA-RUNTIME-001b): pending pointer,
	// then the runtime-persisted confirmation, then the atomic promotion.
	pending, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET pending_credential_id=?,row_version=row_version+1 WHERE slot=? AND state='registered' AND current_credential_id=? AND pending_credential_id IS NULL AND retiring_credential_id IS NULL`,
		replacementID, SlotLintel, currentID.Int64)
	if err != nil {
		return "", err
	}
	if rows, _ := pending.RowsAffected(); rows != 1 {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "lintel slot is not in the recovery rotation window"}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE runtime_credentials SET confirmed_at=?,row_version=row_version+1 WHERE id=? AND confirmed_at IS NULL`, now, replacementID); err != nil {
		return "", err
	}
	promote, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET current_credential_id=?,pending_credential_id=NULL,retiring_credential_id=?,row_version=row_version+1 WHERE slot=? AND state='registered' AND current_credential_id=? AND pending_credential_id=? AND retiring_credential_id IS NULL`,
		replacementID, currentID.Int64, SlotLintel, currentID.Int64, replacementID)
	if err != nil {
		return "", err
	}
	if rows, _ := promote.RowsAffected(); rows != 1 {
		return "", &RegisterError{Status: "FAILED_PRECONDITION", Detail: "lintel slot is not in the recovery rotation window"}
	}
	return base64.RawURLEncoding.EncodeToString(longRaw), nil
}

// consumeToken atomically matches and consumes the in-memory registration
// token by digest+slot+generation. Exactly one concurrent Register wins;
// the winner's token leaves the pending map (single consumption,
// RUNTIME-REG-002).
func (service *Service) consumeToken(digest [32]byte, slotName string, generation int64) (*registrationToken, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	key := slotName + "\x00" + strconv.FormatInt(generation, 10)
	token, exists := service.pending[key]
	if !exists || token.digest != digest || token.consumed || !service.now().Before(token.expiresAt) {
		return nil, false
	}
	token.consumed = true
	delete(service.pending, key)
	return token, true
}

// HelloDecision is the adjudication result of a Connect handshake.
type HelloDecision struct {
	Accepted                 bool
	Reason                   string // empty when accepted; else TOKEN_INVALID | SLOT_REVOKED | VERSION_MISMATCH | EPOCH_STALE | CATALOG_MISMATCH
	LastConnectionEpoch      uint64
	ProfileReconcileRequired bool
	// generation advanced to first_authenticated_at by this handshake.
	MarkedFirstAuthenticated bool
}

// Adjudicate authenticates a Connect bearer and validates the Hello fields
// (RUNTIME-AUTH-003/004, RUNTIME-CTRL-002..004/010). catalogDigest is only
// required for lintel (empty expectation for plinth).
// ValidateBearer reports whether the raw bearer is the slot's current
// long-term token (used by non-stream RPCs such as FetchCredentialGrant).
func (service *Service) ValidateBearer(ctx context.Context, bearer string, slotName string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return false
	}
	digest := sha256.Sum256(raw)
	var state string
	var currentID int
	err = service.db.QueryRowContext(ctx, `SELECT s.state, s.current_credential_id FROM runtime_slots s JOIN runtime_credentials c ON c.id=s.current_credential_id WHERE s.slot=? AND c.token_digest=?`, slotName, digest[:]).Scan(&state, &currentID)
	if err != nil || state != string(StateRegistered) {
		return false
	}
	return true
}

func (service *Service) Adjudicate(ctx context.Context, bearer string, slotName, bootID string, epoch uint64, releaseVersion, currentRelease, expectedCatalogDigest, journeyCatalogDigest string) (HelloDecision, error) {
	decision := HelloDecision{}
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		decision.Reason = "TOKEN_INVALID"
		return decision, nil
	}
	digest := sha256.Sum256(raw)
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return decision, err
	}
	defer conn.Close()
	// Slot state first: a revoked slot rejects every bearer with
	// SLOT_REVOKED (RUNTIME-AUTH-003), even one whose credential was
	// auto-retired and no longer sits under any slot pointer.
	var state string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM runtime_slots WHERE slot=?`, slotName).Scan(&state); err != nil {
		return decision, err
	}
	if state != string(StateRegistered) {
		decision.Reason = "SLOT_REVOKED"
		return decision, nil
	}
	var role string
	var confirmedAt, retiredAt, firstAuth sql.NullString
	err = conn.QueryRowContext(ctx, `
		SELECT CASE WHEN s.current_credential_id = c.id THEN 'current' ELSE 'retiring' END,
		       c.confirmed_at, c.retired_at, c.first_authenticated_at
		FROM runtime_credentials c
		JOIN runtime_slots s ON s.slot = c.slot
		WHERE c.slot=? AND c.token_digest=? AND (c.id = s.current_credential_id OR c.id = s.retiring_credential_id)`, slotName, digest[:]).Scan(&role, &confirmedAt, &retiredAt, &firstAuth)
	if errors.Is(err, sql.ErrNoRows) {
		decision.Reason = "TOKEN_INVALID"
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	if !confirmedAt.Valid || retiredAt.Valid {
		decision.Reason = "TOKEN_INVALID"
		return decision, nil
	}
	if releaseVersion != currentRelease {
		decision.Reason = "VERSION_MISMATCH"
		return decision, nil
	}
	// Epoch monotonicity inside (slot, boot): new boots may restart at 1.
	service.mu.Lock()
	lastEpoch := service.bootEpochs[slotName+"\x00"+bootID]
	service.mu.Unlock()
	decision.LastConnectionEpoch = lastEpoch
	if epoch <= lastEpoch {
		decision.Reason = "EPOCH_STALE"
		return decision, nil
	}
	if slotName == SlotLintel && journeyCatalogDigest != expectedCatalogDigest {
		decision.Reason = "CATALOG_MISMATCH"
		return decision, nil
	}
	decision.Accepted = true
	// First successful authentication of the current generation flips the
	// retiring role into user-visible Pending Retirement (RUNTIME-REG-004 ⑤).
	if role == "current" && !firstAuth.Valid {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `UPDATE runtime_credentials SET first_authenticated_at=?,row_version=row_version+1 WHERE slot=? AND token_digest=? AND first_authenticated_at IS NULL`, now, slotName, digest[:]); err != nil {
			return decision, err
		}
		decision.MarkedFirstAuthenticated = true
	}
	if slotName == SlotLintel {
		decision.ProfileReconcileRequired = true
	}
	return decision, nil
}

// AttachStream records the accepted control stream as the slot's single
// active connection; a replaced stream is signalled to end
// (RUNTIME-CTRL-001).
func (service *Service) AttachStream(slotName, bootID string, epoch uint64) <-chan struct{} {
	service.mu.Lock()
	defer service.mu.Unlock()
	key := slotName + "\x00" + bootID
	if service.bootEpochs[key] < epoch {
		service.bootEpochs[key] = epoch
	}
	if old, live := service.conns[slotName]; live {
		old.close()
	}
	fresh := &connection{bootID: bootID, epoch: epoch, updated: time.Now(), closing: make(chan struct{})}
	service.conns[slotName] = fresh
	return fresh.closing
}

// Touch updates the transient lastSeen projection (Heartbeat) without any
// persistent write (RUNTIME-CTRL-005).
// WithCurrent serializes a stateful inbound message with control-stream
// replacement. The callback may commit durable state only while this exact
// boot/epoch remains the slot authority; a replacement either happens before
// the callback (and it is rejected) or after its transaction commits.
func (service *Service) WithCurrent(slotName, bootID string, epoch uint64, apply func() error) error {
	return service.WithCurrentClosing(slotName, bootID, epoch, func(_ <-chan struct{}) error {
		return apply()
	})
}

// WithCurrentClosing is the data-plane counterpart of WithCurrent. The
// callback can atomically bind transient state to the exact control owner;
// its returned fence closes when that owner is replaced or detached.
func (service *Service) WithCurrentClosing(slotName, bootID string, epoch uint64, apply func(closing <-chan struct{}) error) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	conn, live := service.conns[slotName]
	if !live || conn.bootID != bootID || conn.epoch != epoch {
		return ErrNotConnected
	}
	return apply(conn.closing)
}

func (service *Service) Touch(slotName string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live {
		conn.updated = time.Now()
	}
}

// DetachStream removes the projection only when the ending handler still owns
// it. A superseded stream must never erase its successor's live authority.
func (service *Service) DetachStream(slotName, bootID string, epoch uint64) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live && conn.bootID == bootID && conn.epoch == epoch {
		conn.close()
		delete(service.conns, slotName)
	}
}

// CloseSlot signals the slot's live control stream to end (replacement/
// revoke: the old epoch must never keep authority after the slot leaves
// registered). Idempotent.
func (service *Service) CloseSlot(slotName string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if conn, live := service.conns[slotName]; live {
		conn.close()
	}
}

// CloseAll signals every live control stream to end (shutdown / revoke).
func (service *Service) CloseAll() {
	service.mu.Lock()
	defer service.mu.Unlock()
	for _, conn := range service.conns {
		conn.close()
	}
}

// Retire implements retireRuntimeCredential (HTTP-COMMAND-014): only when
// the new current generation has first authenticated and the slot holds a
// retiring generation; clears retiring and retires the old generation.
func (service *Service) Retire(ctx context.Context, slotName string, expectedRowVersion int64) (SlotView, error) {
	if !ValidSlot(slotName) {
		return SlotView{}, ErrNotFound
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return SlotView{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return SlotView{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var state string
	var rowVersion int64
	var currentID, retiringID sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT state,row_version,current_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, slotName).Scan(&state, &rowVersion, &currentID, &retiringID); err != nil {
		return SlotView{}, err
	}
	if rowVersion != expectedRowVersion {
		return SlotView{}, &RowVersionError{Current: rowVersion}
	}
	if state != string(StateRegistered) || !currentID.Valid || !retiringID.Valid {
		return SlotView{}, fmt.Errorf("%w: no retiring credential to retire", ErrActiveConflict)
	}
	var firstAuth sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT first_authenticated_at FROM runtime_credentials WHERE id=?`, currentID.Int64).Scan(&firstAuth); err != nil {
		return SlotView{}, err
	}
	if !firstAuth.Valid {
		return SlotView{}, fmt.Errorf("%w: new current generation has not first authenticated", ErrActiveConflict)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `UPDATE runtime_credentials SET retired_at=?,row_version=row_version+1 WHERE id=? AND retired_at IS NULL`, now, retiringID.Int64); err != nil {
		return SlotView{}, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE runtime_slots SET retiring_credential_id=NULL,row_version=row_version+1 WHERE slot=? AND row_version=?`, slotName, rowVersion)
	if err != nil {
		return SlotView{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return SlotView{}, &RowVersionError{Current: rowVersion}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return SlotView{}, err
	}
	committed = true
	return service.View(ctx, slotName)
}
