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

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// RegisterError distinguishes the canonical gRPC statuses (RUNTIME-REG-003).
type RegisterError struct {
	Status string // "INVALID_ARGUMENT" | "UNAUTHENTICATED" | "FAILED_PRECONDITION"
	Detail string
}

func (e *RegisterError) Error() string { return fmt.Sprintf("%s: %s", e.Status, e.Detail) }

// Register consumes a one-time token inside the registration window
// (RUNTIME-REG-002): one runner-owned IMMEDIATE transaction creates the
// confirmed credential (generation bound to the token), sets current and
// moves the slot to registered, and the automatic audit event commits in the
// same transaction. contractFingerprint must be the canonical complete Proto
// authority digest.
//
// The one-time token is the registration authority: proof of possession —
// the single in-memory consumption by digest — both authenticates the
// runtime and identifies the exact prepared generation. The execution scope
// is never taken from the caller: the gRPC entry carries no execution
// metadata, and a raw token value must not travel in a Context. Instead the
// consumed token carries the issuing operation's correlation and initiator —
// the management preparation (admin session) or the Lintel recovery begin
// (deployment helper) — and Register re-roots onto that scope with the
// runtime system principal as the executing actor, so the audit row records
// the original initiator and the current executor separately (ADR-0006).
// Deterministic conflicts after the proof are recorded as rejected facts and
// surface as this package's typed *RegisterError.
//
// Infra failures (connection, audit write, uncertain commit) leave no
// durable trace and no credential: the token is re-armed within its original
// reveal window so the runtime's registration retry can complete. A
// deterministic rejection permanently consumes the token that produced the
// recorded fact; an unknown, expired or already consumed token stays the
// unaudited bounded UNAUTHENTICATED path (docs/audit-design.md §4).
func (service *Service) Register(ctx context.Context, slotName, oneTimeToken string, generation int64, bootID, contractFingerprint, currentFingerprint string) (longTermToken string, generationOut int64, err error) {
	if !ValidSlot(slotName) || generation == 0 || bootID == "" {
		return "", 0, &RegisterError{Status: "INVALID_ARGUMENT", Detail: "slot, generation and boot id are required"}
	}
	raw, err := base64.RawURLEncoding.DecodeString(oneTimeToken)
	if err != nil || len(raw) != 32 {
		return "", 0, &RegisterError{Status: "INVALID_ARGUMENT", Detail: "token must be 32 bytes base64url"}
	}
	if !validContractFingerprint(contractFingerprint) || contractFingerprint != currentFingerprint {
		return "", 0, &RegisterError{Status: "FAILED_PRECONDITION", Detail: "Proto contract fingerprint mismatch"}
	}
	// Single-consume under the service lock: the winner consumes the token
	// atomically; every other racer sees UNAUTHENTICATED (already consumed).
	digest := sha256.Sum256(raw)
	token, ok := service.consumeToken(digest, slotName, generation)
	if !ok {
		return "", 0, &RegisterError{Status: "UNAUTHENTICATED", Detail: "registration token unknown, expired or already consumed"}
	}
	// Re-root onto the issuing operation's scope (deliberate re-rooting: the
	// caller's context carries at most untrusted transport metadata). The
	// stored values were validated when the metadata was attached, so a
	// failure here is a programming error — treated like any infra failure.
	scope, err := execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: token.correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     token.initiator,
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		service.rearmToken(token)
		return "", 0, err
	}
	outcome, err := execution.Execute(scope, service.runner, service.ops.register, func(tx *execution.Tx) (registerOutcome, error) {
		if token.recoveryRevision != 0 {
			// Helper-owned Lintel recovery (T35): rotate the current credential
			// through the frozen pending→confirmed→current/retiring protocol
			// instead of the unregistered/revoked registration window.
			return service.registerLintelRecoveryRotation(scope, tx, token)
		}
		return service.registerSlotWindow(scope, tx, token)
	}, func(outcome registerOutcome) int64 { return outcome.generation })
	if err != nil {
		var rejection *execution.Rejection
		if errors.As(err, &rejection) {
			return "", 0, registerRejectionError(rejection)
		}
		// Infra failure: nothing durable happened and no credential was
		// minted, so the token returns to the pending window for the retry.
		service.rearmToken(token)
		return "", 0, err
	}
	return outcome.longTerm, outcome.generation, nil
}

// registerOutcome carries the committed business state of one Register
// execution; the long-term token stays in memory only (non-ledger execution).
type registerOutcome struct {
	longTerm   string
	generation int64
}

// registerSlotWindow performs the unregistered/revoked registration-window
// transition inside the runner transaction: insert the confirmed credential
// for the token's generation, then move the slot to registered with the
// current pointer in one guarded UPDATE.
func (service *Service) registerSlotWindow(ctx context.Context, tx *execution.Tx, token *registrationToken) (registerOutcome, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM runtime_slots WHERE slot=?`, token.slot).Scan(&state); err != nil {
		return registerOutcome{}, err
	}
	// Registration window: unregistered (first) or revoked (replacement).
	if state != string(StateUnregistered) && state != string(StateRevoked) {
		return registerOutcome{}, preconditionRejection("slot is not in a registration window (ALREADY_REGISTERED)")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM runtime_credentials WHERE slot=? AND generation=?`, token.slot, token.generation).Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return registerOutcome{}, err
	}
	if exists == 1 {
		// Replacement retry for the same generation: refresh digest identity
		// is not allowed — a consumed token can never mint twice, so this
		// branch is unreachable in practice; fail closed.
		return registerOutcome{}, preconditionRejection("generation already exists")
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	longRaw := make([]byte, 32)
	if _, err := rand.Read(longRaw); err != nil {
		return registerOutcome{}, err
	}
	longDigest := sha256.Sum256(longRaw)
	insert, err := tx.ExecContext(ctx, `INSERT INTO runtime_credentials(slot,generation,token_digest,confirmed_at,row_version,created_at) VALUES(?,?,?,?,1,?)`,
		token.slot, token.generation, longDigest[:], now, now)
	if err != nil {
		return registerOutcome{}, err
	}
	credentialID, err := insert.LastInsertId()
	if err != nil {
		return registerOutcome{}, err
	}
	// registered slot + current pointer in one UPDATE; schema triggers
	// verify the credential is confirmed, unretired and same-slot.
	result, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET state='registered',current_credential_id=?,row_version=row_version+1 WHERE slot=? AND state IN ('unregistered','revoked')`, credentialID, token.slot)
	if err != nil {
		return registerOutcome{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return registerOutcome{}, preconditionRejection("slot is not in a registration window (ALREADY_REGISTERED)")
	}
	return registerOutcome{longTerm: base64.RawURLEncoding.EncodeToString(longRaw), generation: token.generation}, nil
}

// preconditionRejection builds the deterministic registration rejection the
// runner records as a rejected fact; registerRejectionError maps it back to
// the stable typed error.
func preconditionRejection(detail string) *execution.Rejection {
	return &execution.Rejection{Code: codeRegisterPrecondition, Detail: detail}
}

// registerRejectionError rebuilds the package's stable *RegisterError from a
// recorded runner rejection, so the gRPC status mapping and existing callers
// keep their exact error surface. Any other rejection passes through.
func registerRejectionError(rejection *execution.Rejection) error {
	if rejection.Code == codeRegisterPrecondition {
		return &RegisterError{Status: "FAILED_PRECONDITION", Detail: rejection.Detail}
	}
	return rejection
}

// rearmToken returns an infra-failed registration token to the pending map
// within its original reveal window (RUNTIME-REG-002): the runner left no
// durable trace, so the token has not done its one job and the runtime's
// retry must still be able to complete. An expired token stays consumed, and
// a newer token minted for the same slot and generation while the failed
// execution was in flight keeps the window — the stale token is never
// resurrected over it.
func (service *Service) rearmToken(token *registrationToken) {
	service.mu.Lock()
	defer service.mu.Unlock()
	key := token.slot + "\x00" + itoa64(token.generation)
	if !token.consumed || !service.now().Before(token.expiresAt) {
		return
	}
	if _, live := service.pending[key]; live {
		return
	}
	token.consumed = false
	service.pending[key] = token
}

// registerLintelRecoveryRotation executes the schema-owned two-phase
// rotation for the recovery registration: insert the confirmed replacement,
// set pending, then atomically promote it to current while the predecessor
// becomes retiring (DATA-RUNTIME-001b). The caller owns the runner
// transaction; the slot stays registered the whole time — revocation is
// never used, so the old credential survives until the offline finalizer
// retires it after the replacement's first authenticated Hello.
func (service *Service) registerLintelRecoveryRotation(ctx context.Context, tx *execution.Tx, token *registrationToken) (registerOutcome, error) {
	if token.slot != SlotLintel {
		return registerOutcome{}, preconditionRejection("recovery registration is lintel-only")
	}
	var active int
	var reason string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return registerOutcome{}, err
	}
	if active != 1 || reason != "LintelRecovery" || revision != token.recoveryRevision {
		return registerOutcome{}, preconditionRejection("lintel recovery maintenance revision changed")
	}
	var state string
	var currentID sql.NullInt64
	var pendingID, retiringID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT state,current_credential_id,pending_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, SlotLintel).Scan(&state, &currentID, &pendingID, &retiringID); err != nil {
		return registerOutcome{}, err
	}
	if state != string(StateRegistered) || !currentID.Valid || pendingID.Valid || retiringID.Valid {
		return registerOutcome{}, preconditionRejection("lintel slot is not in the recovery rotation window")
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM runtime_credentials WHERE slot=? AND generation=?`, SlotLintel, token.generation).Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return registerOutcome{}, err
	}
	if exists == 1 {
		return registerOutcome{}, preconditionRejection("generation already exists")
	}
	now := service.now().UTC().Format(time.RFC3339Nano)
	longRaw := make([]byte, 32)
	if _, err := rand.Read(longRaw); err != nil {
		return registerOutcome{}, err
	}
	longDigest := sha256.Sum256(longRaw)
	insert, err := tx.ExecContext(ctx, `INSERT INTO runtime_credentials(slot,generation,token_digest,row_version,created_at) VALUES(?,?,?,1,?)`,
		SlotLintel, token.generation, longDigest[:], now)
	if err != nil {
		return registerOutcome{}, err
	}
	replacementID, err := insert.LastInsertId()
	if err != nil {
		return registerOutcome{}, err
	}
	// The frozen two-phase sequence (DATA-RUNTIME-001b): pending pointer,
	// then the runtime-persisted confirmation, then the atomic promotion.
	pending, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET pending_credential_id=?,row_version=row_version+1 WHERE slot=? AND state='registered' AND current_credential_id=? AND pending_credential_id IS NULL AND retiring_credential_id IS NULL`,
		replacementID, SlotLintel, currentID.Int64)
	if err != nil {
		return registerOutcome{}, err
	}
	if rows, _ := pending.RowsAffected(); rows != 1 {
		return registerOutcome{}, preconditionRejection("lintel slot is not in the recovery rotation window")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_credentials SET confirmed_at=?,row_version=row_version+1 WHERE id=? AND confirmed_at IS NULL`, now, replacementID); err != nil {
		return registerOutcome{}, err
	}
	promote, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET current_credential_id=?,pending_credential_id=NULL,retiring_credential_id=?,row_version=row_version+1 WHERE slot=? AND state='registered' AND current_credential_id=? AND pending_credential_id=? AND retiring_credential_id IS NULL`,
		replacementID, currentID.Int64, SlotLintel, currentID.Int64, replacementID)
	if err != nil {
		return registerOutcome{}, err
	}
	if rows, _ := promote.RowsAffected(); rows != 1 {
		return registerOutcome{}, preconditionRejection("lintel slot is not in the recovery rotation window")
	}
	return registerOutcome{longTerm: base64.RawURLEncoding.EncodeToString(longRaw), generation: token.generation}, nil
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

// validContractFingerprint accepts only the canonical, complete Proto
// authority fingerprint. Empty and malformed peer values are rejected.
func validContractFingerprint(value string) bool {
	return contract.ValidProtoAuthorityFingerprint(value)
}

// HelloDecision is the adjudication result of a Connect handshake.
type HelloDecision struct {
	Accepted                 bool
	Reason                   string // empty when accepted; else TOKEN_INVALID | SLOT_REVOKED | CONTRACT_MISMATCH | EPOCH_STALE | CATALOG_MISMATCH
	LastConnectionEpoch      uint64
	ProfileReconcileRequired bool
	// generation advanced to first_authenticated_at by this handshake.
	MarkedFirstAuthenticated bool
}

// Adjudicate authenticates a Connect bearer and validates the Hello fields
// (RUNTIME-AUTH-003/004, RUNTIME-CTRL-002..004/010). catalogDigest is only
// required for lintel (empty expectation for plinth). The handshake reads run
// on the narrowed read-only reader and the first-authentication bookkeeping
// executes as an audited runner operation.
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
	err = service.reader.QueryRowContext(ctx, `SELECT s.state, s.current_credential_id FROM runtime_slots s JOIN runtime_credentials c ON c.id=s.current_credential_id WHERE s.slot=? AND c.token_digest=?`, slotName, digest[:]).Scan(&state, &currentID)
	if err != nil || state != string(StateRegistered) {
		return false
	}
	return true
}

func (service *Service) Adjudicate(ctx context.Context, bearer string, slotName, bootID string, epoch uint64, contractFingerprint, currentFingerprint, expectedCatalogDigest, journeyCatalogDigest string) (HelloDecision, error) {
	decision := HelloDecision{}
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		decision.Reason = "TOKEN_INVALID"
		return decision, nil
	}
	digest := sha256.Sum256(raw)
	reader := service.reader
	// Slot state first: a revoked slot rejects every bearer with
	// SLOT_REVOKED (RUNTIME-AUTH-003), even one whose credential was
	// auto-retired and no longer sits under any slot pointer.
	var state string
	if err := reader.QueryRowContext(ctx, `SELECT state FROM runtime_slots WHERE slot=?`, slotName).Scan(&state); err != nil {
		return decision, err
	}
	if state != string(StateRegistered) {
		decision.Reason = "SLOT_REVOKED"
		return decision, nil
	}
	var role string
	var confirmedAt, retiredAt, firstAuth sql.NullString
	err = reader.QueryRowContext(ctx, `
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
	if !validContractFingerprint(contractFingerprint) || contractFingerprint != currentFingerprint {
		decision.Reason = "CONTRACT_MISMATCH"
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
	// The durable bookkeeping is an audited runner operation — a credential
	// lifecycle write, never an exempt heartbeat. An already-marked
	// generation (early precheck here, re-checked inside the transaction)
	// records no duplicate audit fact.
	if role == "current" && !firstAuth.Valid {
		marked, err := service.markFirstAuthenticated(ctx, slotName, digest)
		if err != nil {
			return decision, err
		}
		decision.MarkedFirstAuthenticated = marked
	}
	if slotName == SlotLintel {
		decision.ProfileReconcileRequired = true
	}
	return decision, nil
}

// markFirstAuthenticated records the first successful authentication of the
// current credential generation through the runner: one audited IMMEDIATE
// transaction re-verifies inside the transaction that the digest names the
// slot's confirmed, unretired CURRENT generation on a registered slot with
// first_authenticated_at still NULL, then flips it (compare-and-set). The
// execution scope is deliberately re-rooted onto the trusted runtime machine
// identity — system principal, internal source, fresh correlation — so the
// presented bearer is the sole authority: no transport metadata and no
// borrower (user or service) scope can shape or drive the audited write, and
// the raw token value never enters the scope. A lost race (another accepted
// Hello already marked the generation, or a replacement revoked it between
// the handshake read and this transaction) surfaces as
// execution.ErrNoTransition: the runner records no fact at all and this
// handshake simply did not do the marking.
func (service *Service) markFirstAuthenticated(ctx context.Context, slotName string, digest [32]byte) (bool, error) {
	correlationID, err := execution.NewCorrelationID()
	if err != nil {
		return false, err
	}
	scope, err := execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: correlationID,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		return false, err
	}
	if _, err := execution.Execute(scope, service.runner, service.ops.firstAuthenticate,
		func(tx *execution.Tx) (int64, error) {
			var credentialID int64
			err := tx.QueryRowContext(ctx, `
				SELECT c.id
				FROM runtime_credentials c
				JOIN runtime_slots s ON s.slot = c.slot
				WHERE c.slot=? AND c.token_digest=? AND c.id = s.current_credential_id
				  AND s.state='registered'
				  AND c.confirmed_at IS NOT NULL AND c.retired_at IS NULL
				  AND c.first_authenticated_at IS NULL`, slotName, digest[:]).Scan(&credentialID)
			if errors.Is(err, sql.ErrNoRows) {
				return 0, execution.ErrNoTransition
			}
			if err != nil {
				return 0, err
			}
			result, err := tx.ExecContext(ctx, `UPDATE runtime_credentials SET first_authenticated_at=?,row_version=row_version+1 WHERE id=? AND first_authenticated_at IS NULL`, service.now().UTC().Format(time.RFC3339Nano), credentialID)
			if err != nil {
				return 0, err
			}
			if rows, _ := result.RowsAffected(); rows != 1 {
				return 0, execution.ErrNoTransition
			}
			return credentialID, nil
		},
		func(credentialID int64) int64 { return credentialID }); err != nil {
		if errors.Is(err, execution.ErrNoTransition) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// AttachStream records an accepted control stream as the slot's single active
// connection. Adjudicate and attachment are intentionally separate because two
// admitted Hellos can reach this boundary out of order. A stale attachment
// returns nil and, crucially, leaves the current owner open (RUNTIME-CTRL-001/004).
func (service *Service) AttachStream(slotName, bootID string, epoch uint64) <-chan struct{} {
	closing, _ := service.attachStream(slotName, bootID, epoch, "", nil)
	return closing
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

// Retire implements retireRuntimeCredential (HTTP-COMMAND-014) through the
// execution runner: only when the new current generation has first
// authenticated and the slot holds a retiring generation; clears retiring and
// retires the old generation. The automatic audit event commits in the same
// runner transaction; deterministic conflicts are recorded as rejected facts
// and surface as the package's typed errors.
func (service *Service) Retire(ctx context.Context, slotName string, expectedRowVersion int64) (SlotView, error) {
	if !ValidSlot(slotName) {
		return SlotView{}, ErrNotFound
	}
	type retireOutcome struct {
		view      SlotView
		retiredID int64
	}
	outcome, err := execution.Execute(ctx, service.runner, service.ops.retire,
		func(tx *execution.Tx) (retireOutcome, error) {
			var state string
			var rowVersion int64
			var currentID, retiringID sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT state,row_version,current_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, slotName).Scan(&state, &rowVersion, &currentID, &retiringID); err != nil {
				return retireOutcome{}, err
			}
			if rowVersion != expectedRowVersion {
				return retireOutcome{}, rejectionRowVersion(rowVersion)
			}
			if state != string(StateRegistered) || !currentID.Valid || !retiringID.Valid {
				return retireOutcome{}, rejectionActive("no retiring credential to retire")
			}
			var firstAuth sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT first_authenticated_at FROM runtime_credentials WHERE id=?`, currentID.Int64).Scan(&firstAuth); err != nil {
				return retireOutcome{}, err
			}
			if !firstAuth.Valid {
				return retireOutcome{}, rejectionActive("new current generation has not first authenticated")
			}
			now := service.now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, `UPDATE runtime_credentials SET retired_at=?,row_version=row_version+1 WHERE id=? AND retired_at IS NULL`, now, retiringID.Int64); err != nil {
				return retireOutcome{}, err
			}
			result, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET retiring_credential_id=NULL,row_version=row_version+1 WHERE slot=? AND row_version=?`, slotName, rowVersion)
			if err != nil {
				return retireOutcome{}, err
			}
			if rows, _ := result.RowsAffected(); rows != 1 {
				return retireOutcome{}, rejectionRowVersion(rowVersion)
			}
			view, err := service.slotView(ctx, tx, slotName)
			if err != nil {
				return retireOutcome{}, err
			}
			return retireOutcome{view: view, retiredID: retiringID.Int64}, nil
		},
		func(outcome retireOutcome) int64 { return outcome.retiredID })
	if err != nil {
		return SlotView{}, translateSlotRejection(err)
	}
	return outcome.view, nil
}
