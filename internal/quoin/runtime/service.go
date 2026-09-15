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

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	SlotPlinth = "plinth"
	SlotLintel = "lintel"
	// registrationTokenTTL bounds the one-time reveal window (SEC-REVEAL-002).
	registrationTokenTTL = 60 * time.Second
)

// Stable identities of the slot authority's runner-executed commands (they
// double as the automatic audit actions) and their audit domain object types.
// Slots carry no surrogate integer id, so the slot-level audit domain
// reference uses the lifecycle generation the command prepares; credential
// commands reference the runtime_credentials row id.
const (
	opPrepareRegistration = "runtime_slot.prepare_registration"
	opRetireCredential    = "runtime_credential.retire"
	opRevealRegistration  = "runtime_slot.reveal_registration_token"
	// opRegisterRuntime is the runtime-initiated confirmation of a prepared
	// registration (register.go): the one-time token is the authority and the
	// runtime system principal is the executing actor.
	opRegisterRuntime = "runtime_slot.register"
	// opFirstAuthenticate is the runtime-initiated first-authentication
	// bookkeeping of an accepted Hello (register.go): the presented long-term
	// bearer is the authority and the runtime system principal is the
	// executing actor. It is a full audited machine write — a credential
	// lifecycle fact, deliberately not exempted as a heartbeat.
	opFirstAuthenticate = "runtime_credential.first_authenticate"
	// opBeginLintelRecovery is the deployment-helper entry into the
	// LintelRecovery maintenance revision (recovery.go); recovery stays
	// deployment-only with no management-surface enablement.
	opBeginLintelRecovery = "maintenance.lintel_recovery_begin"
	objectSlot            = "runtime_slot"
	objectCredential      = "runtime_credential"
	objectMaintenance     = "maintenance"
)

// Rejection codes recorded for the slot commands and the runtime-initiated
// registration paths.
const (
	codeRowVersionConflict = "row_version_conflict"
	codeActiveConflict     = "active_conflict"
	// codeRegisterPrecondition marks a deterministic registration conflict
	// after the one-time token was proven: the caller surfaces it as the
	// package's FAILED_PRECONDITION *RegisterError.
	codeRegisterPrecondition = "register_precondition"
	// codeRecoveryState and codeRecoveryFrozenFence map back onto the
	// recovery package's exported sentinel errors.
	codeRecoveryState       = "lintel_recovery_state"
	codeRecoveryFrozenFence = "lintel_recovery_frozen_fence"
)

// slotOperations holds the canonical registered declarations of this
// package's admin slot commands plus the runtime-initiated registration and
// the deployment-only recovery begin.
type slotOperations struct {
	prepare            *execution.Operation
	retire             *execution.Operation
	reveal             *execution.Operation
	register           *execution.Operation
	firstAuthenticate  *execution.Operation
	beginLintelRecover *execution.Operation
}

// registerSlotOperations declares this package's commands on the runner's
// registry; a duplicate name fails registration loudly.
func registerSlotOperations(runner *execution.Runner) (slotOperations, error) {
	ops := slotOperations{}
	var err error
	if ops.prepare, err = runner.Register(execution.Operation{
		Name:       opPrepareRegistration,
		Class:      execution.ClassWrite,
		ObjectType: objectSlot,
		Authorize:  authorizeSlotAdmin,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opPrepareRegistration, err)
	}
	if ops.retire, err = runner.Register(execution.Operation{
		Name:       opRetireCredential,
		Class:      execution.ClassWrite,
		ObjectType: objectCredential,
		Authorize:  authorizeSlotAdmin,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opRetireCredential, err)
	}
	if ops.reveal, err = runner.Register(execution.Operation{
		Name:       opRevealRegistration,
		Class:      execution.ClassWrite,
		ObjectType: objectSlot,
		Authorize:  authorizeSlotAdmin,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opRevealRegistration, err)
	}
	if ops.register, err = runner.Register(execution.Operation{
		Name:       opRegisterRuntime,
		Class:      execution.ClassWrite,
		ObjectType: objectSlot,
		Authorize:  authorizeRuntimeRegister,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opRegisterRuntime, err)
	}
	if ops.firstAuthenticate, err = runner.Register(execution.Operation{
		Name:       opFirstAuthenticate,
		Class:      execution.ClassWrite,
		ObjectType: objectCredential,
		Authorize:  authorizeRuntimeMachine,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opFirstAuthenticate, err)
	}
	if ops.beginLintelRecover, err = runner.Register(execution.Operation{
		Name:       opBeginLintelRecovery,
		Class:      execution.ClassWrite,
		ObjectType: objectMaintenance,
		Authorize:  authorizeRecoveryBegin,
	}); err != nil {
		return ops, fmt.Errorf("runtime: register %s: %w", opBeginLintelRecovery, err)
	}
	return ops, nil
}

// authorizeSlotAdmin re-verifies inside the runner transaction that the
// context carries a live administrator session proof (auth.
// VerifyExecutionSession): a session revoked or drifted after admission, or a
// non-admin principal, fails closed with a clean rollback and no durable
// trace.
func authorizeSlotAdmin(ctx context.Context, tx *execution.Tx) error {
	return auth.VerifyExecutionSession(ctx, tx, "admin")
}

// authorizeRuntimeRegister re-verifies inside the runner transaction that the
// execution context is exactly the runtime deployment scope this package
// itself establishes after the one-time token proof: the system principal
// acting through the internal Runtime service entry. A user or service
// context — in particular any metadata wired at a future gRPC boundary —
// can never drive the registration, and there is no session fallback that
// could fake it.
func authorizeRuntimeRegister(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("runtime: registration executes as the runtime system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind != execution.SourceInternal {
		return fmt.Errorf("runtime: registration requires the %q source, got %q", execution.SourceInternal, meta.Source.Kind)
	}
	return nil
}

// authorizeRuntimeMachine re-verifies inside the runner transaction that the
// execution context is exactly the runtime machine scope this package itself
// establishes for the handshake's first-authentication bookkeeping: the
// system principal acting through the internal Runtime service entry. A user
// or service context — in particular any metadata wired at a future gRPC
// boundary — can never drive it, and there is no session fallback that could
// fake it.
func authorizeRuntimeMachine(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("runtime: handshake bookkeeping executes as the runtime system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind != execution.SourceInternal {
		return fmt.Errorf("runtime: handshake bookkeeping requires the %q source, got %q", execution.SourceInternal, meta.Source.Kind)
	}
	return nil
}

// authorizeRecoveryBegin re-verifies inside the runner transaction that the
// execution context is exactly the deployment-helper scope the recovery
// entry establishes: the system principal through the offline CLI source.
// The Lintel recovery stays deployment-only — an HTTP/session scope can
// never begin it, and the retired Lintel slot gains no general management
// enablement.
func authorizeRecoveryBegin(ctx context.Context, _ *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return fmt.Errorf("runtime: lintel recovery begins only as the deployment helper system principal, got %s/%d", meta.Actor.Kind, meta.Actor.ID)
	}
	if meta.Source.Kind != execution.SourceCLI {
		return fmt.Errorf("runtime: lintel recovery requires the %q source, got %q", execution.SourceCLI, meta.Source.Kind)
	}
	return nil
}

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
	ReleaseVersion       string    `json:"releaseVersion,omitempty"`
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
	// correlation and initiator preserve the issuing operation's business
	// scope (ADR-0006): the management preparation's correlation and
	// originating initiator, or the Lintel recovery begin's deployment-helper
	// scope. Register re-roots onto them after the one-time proof so the
	// confirmation's audit row joins the issuing operation's lifecycle; the
	// raw token value itself never travels in any Context.
	correlation string
	initiator   execution.Principal
	// recoveryRevision is non-zero only for the helper-owned Lintel recovery
	// token (T35): it binds the one-time token to the exact LintelRecovery
	// maintenance revision and routes Register into the recovery rotation.
	recoveryRevision int64
}

// connection is the transient control-stream projection (RUNTIME-CTRL-001).
// closing carries the stop signal for the handler loop: cancelling the
// stream context makes gRPC terminate the RPC on both ends.
type connection struct {
	bootID               string
	epoch                uint64
	releaseVersion       string // Informational peer build provenance, never admission.
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
	// reader is the narrowed read-only query surface for the pure reads in
	// this package (View, ValidateBearer, Adjudicate's handshake reads). The
	// zero value fails closed — queries error until SetReader installs the
	// OpenReadOnly pool; the writable database is never a read fallback.
	reader execution.Reader
	// runner executes the admin slot commands, the runtime-initiated
	// registration and the handshake's first-authentication bookkeeping with
	// automatic audit.
	runner *execution.Runner
	ops    slotOperations
	now    func() time.Time

	mu      sync.Mutex
	tokens  map[string]*registrationToken // handle -> token
	pending map[string]*registrationToken // slot+"\x00"+generation -> token awaiting Register
	conns   map[string]*connection        // slot -> active stream
	// bootEpochs remembers the highest accepted epoch per (slot, boot) so
	// stale reconnects are rejected with EPOCH_STALE (RUNTIME-CTRL-004).
	bootEpochs map[string]uint64
}

// NewService is the pre-composition constructor: the service owns a private
// operation registry and starts WITHOUT a read source, so every pure read
// fails closed until SetReader installs the real read-only pool. The writable
// database is never a read fallback. The composed application switches to
// NewServiceWithReader with the real read-only database and the shared runner.
func NewService(db *sql.DB) *Service {
	runner := execution.NewRunner(db, execution.NewRegistry(), nil)
	service, err := compose(runner)
	if err != nil {
		panic("runtime: compose default service: " + err.Error())
	}
	return service
}

// compose registers this package's declared operations on the runner and
// builds the service with no read source wired (fail-closed reads).
func compose(runner *execution.Runner) (*Service, error) {
	ops, err := registerSlotOperations(runner)
	if err != nil {
		return nil, err
	}
	return &Service{
		runner: runner, ops: ops, now: time.Now,
		tokens:     map[string]*registrationToken{},
		pending:    map[string]*registrationToken{},
		conns:      map[string]*connection{},
		bootEpochs: map[string]uint64{},
	}, nil
}

// NewServiceWithReader assembles the composed service: db stays the
// runner-owned write authority (this service never touches it directly),
// reader is the pure-read query surface, and runner is the command executor
// whose registry receives this package's declared operations. The reader must
// be the execution.OpenReadOnly factory product: the existing runner
// validates it, so an arbitrary pool that cannot prove it never writes is
// rejected at assembly time.
func NewServiceWithReader(db *sql.DB, reader audit.Reader, runner *execution.Runner) (*Service, error) {
	if db == nil {
		return nil, errors.New("runtime: database is required")
	}
	if reader == nil {
		return nil, errors.New("runtime: read capability is required")
	}
	if runner == nil {
		return nil, errors.New("runtime: command runner is required")
	}
	if err := runner.SetReader(reader); err != nil {
		return nil, err
	}
	service, err := compose(runner)
	if err != nil {
		return nil, err
	}
	service.reader = reader.(execution.Reader)
	return service, nil
}

// SetReader installs the read-only source used by the pure read paths (View,
// ValidateBearer, Adjudicate's handshake reads). The existing runner validates
// it: only the execution.OpenReadOnly factory product is accepted and an
// unavailable pool fails the wiring. Until one is installed every pure read
// FAILS CLOSED — the writable database is never a read fallback. Call it once
// during composition; concurrent reconfiguration is unsupported.
func (service *Service) SetReader(reader audit.Reader) error {
	if err := service.runner.SetReader(reader); err != nil {
		return err
	}
	service.reader = reader.(execution.Reader)
	return nil
}

func ValidSlot(slot string) bool { return slot == SlotPlinth || slot == SlotLintel }

// rowQuerier is the single-row read surface shared by *sql.Conn, the narrowed
// reader and the runner's guarded *execution.Tx, so the slot projection
// composes unchanged inside a runner transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// View projects one slot for HTTP.
func (service *Service) View(ctx context.Context, slot string) (SlotView, error) {
	if !ValidSlot(slot) {
		return SlotView{}, ErrNotFound
	}
	return service.slotView(ctx, service.reader, slot)
}

// slotView renders the persistent projection plus the transient connection
// state from any single-row reader — including the runner's guarded
// transaction handle, so a command returns its own committed-to-be state.
func (service *Service) slotView(ctx context.Context, reader rowQuerier, slot string) (SlotView, error) {
	row := reader.QueryRowContext(ctx, `
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
		view.ReleaseVersion = conn.releaseVersion
		epoch := conn.epoch
		view.ConnectionEpoch = &epoch
		seen := conn.updated.UTC().Format(time.RFC3339Nano)
		view.LastSeenAt = &seen
	}
	return view, nil
}

// prepareOutcome carries the committed business state of one prepare command
// to the post-commit token minting.
type prepareOutcome struct {
	view        SlotView
	generation  int64
	replayed    bool
	replacement bool
}

// PrepareRegistration implements prepareRuntimeRegistration
// (HTTP-COMMAND-012) through the execution runner: one runner-owned
// IMMEDIATE transaction holding the administrator session re-check, the
// slot state change and the automatic audit event. First registration keeps
// unregistered and prepares generation 1; replacement confirms the
// DATA-RUNTIME-001b fence (no active execution attempts bound to the slot, no
// un-stopped lintel browser operations), then revokes the slot; the schema
// trigger retires its credentials. The one-time token is created only after
// the runner commits and lives in memory for 60 seconds bound to the creating
// admin session. Deterministic conflicts (stale row version, active work) are
// recorded as rejected facts and surface as the package's typed errors.
// Returns (view, handle, available).
func (service *Service) PrepareRegistration(ctx context.Context, slot string, expectedRowVersion int64, sessionDigest [32]byte) (SlotView, string, bool, error) {
	if !ValidSlot(slot) {
		return SlotView{}, "", false, ErrNotFound
	}
	outcome, err := execution.Execute(ctx, service.runner, service.ops.prepare,
		func(tx *execution.Tx) (prepareOutcome, error) {
			// Same-session replay while the original handle is still valid
			// returns the same handle without re-preparing (SEC-REVEAL-003);
			// the repeated command is still one audited execution.
			if _, generation, ok := service.LookupToken(sessionDigest, slot); ok {
				view, viewErr := service.slotView(ctx, tx, slot)
				if viewErr != nil {
					return prepareOutcome{}, viewErr
				}
				return prepareOutcome{view: view, generation: generation, replayed: true}, nil
			}
			var state string
			var rowVersion int64
			if err := tx.QueryRowContext(ctx, `SELECT state,row_version FROM runtime_slots WHERE slot=?`, slot).Scan(&state, &rowVersion); err != nil {
				return prepareOutcome{}, err
			}
			if rowVersion != expectedRowVersion {
				return prepareOutcome{}, rejectionRowVersion(rowVersion)
			}
			outcome := prepareOutcome{}
			if state != string(StateUnregistered) {
				// Replacement: DATA-RUNTIME-001b fence first — conflicts must
				// not revoke anything (HTTP-RUNTIME-002).
				var activeAttempts int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE runtime_slot=? AND state IN ('Queued','Assigned','Running','Cancelling')`, slot).Scan(&activeAttempts); err != nil {
					return prepareOutcome{}, err
				}
				if activeAttempts > 0 {
					return prepareOutcome{}, rejectionActive("slot has active execution attempts")
				}
				if slot == SlotLintel {
					var activeOps int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_operations WHERE stop_confirmed_at IS NULL`).Scan(&activeOps); err != nil {
						return prepareOutcome{}, err
					}
					if activeOps > 0 {
						return prepareOutcome{}, rejectionActive("lintel has active browser operations")
					}
				}
				// Single UPDATE: revoke + clear pointers; the AFTER trigger
				// retires every unretired credential of the slot.
				result, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET state='revoked',current_credential_id=NULL,pending_credential_id=NULL,retiring_credential_id=NULL,row_version=row_version+1 WHERE slot=? AND row_version=?`, slot, rowVersion)
				if err != nil {
					return prepareOutcome{}, err
				}
				if rows, _ := result.RowsAffected(); rows != 1 {
					return prepareOutcome{}, rejectionRowVersion(rowVersion)
				}
				outcome.replacement = true
			} else {
				result, err := tx.ExecContext(ctx, `UPDATE runtime_slots SET row_version=row_version+1 WHERE slot=? AND row_version=? AND state='unregistered'`, slot, rowVersion)
				if err != nil {
					return prepareOutcome{}, err
				}
				if rows, _ := result.RowsAffected(); rows != 1 {
					return prepareOutcome{}, rejectionRowVersion(rowVersion)
				}
			}
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM runtime_credentials WHERE slot=?`, slot).Scan(&outcome.generation); err != nil {
				return prepareOutcome{}, err
			}
			view, err := service.slotView(ctx, tx, slot)
			if err != nil {
				return prepareOutcome{}, err
			}
			outcome.view = view
			return outcome, nil
		},
		func(outcome prepareOutcome) int64 { return outcome.generation })
	if err != nil {
		return SlotView{}, "", false, translateSlotRejection(err)
	}
	if outcome.replayed {
		// The stored handle is still the one live capability of this session.
		handle, _, _ := service.LookupToken(sessionDigest, slot)
		return outcome.view, handle, true, nil
	}
	// Replacement revoked the slot: close the old control stream so the
	// replaced runtime loses its epoch immediately (RUNTIME-REVOKE-001).
	if outcome.replacement {
		service.CloseSlot(slot)
	}
	// Token creation after the runner committed; the raw value exists only in
	// memory. The token carries the preparation's correlation and original
	// initiator so the later Register confirmation joins this operation's
	// lifecycle (ADR-0006) — never the raw token value itself.
	meta, err := execution.Require(ctx)
	if err != nil {
		return SlotView{}, "", false, err
	}
	handle, err := service.mintRegistrationToken(slot, outcome.generation, sessionDigest, meta)
	if err != nil {
		return SlotView{}, "", false, err
	}
	return outcome.view, handle, true, nil
}

// mintRegistrationToken stores the one-time registration token for slot and
// generation bound to the creating admin session, carries the preparation's
// execution scope for the later Register confirmation, and returns its reveal
// handle (RUNTIME-REG-001).
func (service *Service) mintRegistrationToken(slot string, generation int64, sessionDigest [32]byte, meta execution.Metadata) (string, error) {
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", err
	}
	handleBytes := make([]byte, 24)
	if _, err := rand.Read(handleBytes); err != nil {
		return "", err
	}
	handle := base64.RawURLEncoding.EncodeToString(handleBytes)
	token := &registrationToken{
		slot: slot, generation: generation,
		raw:    base64.RawURLEncoding.EncodeToString(rawBytes),
		digest: sha256.Sum256(rawBytes), session: sessionDigest,
		expiresAt:   service.now().Add(registrationTokenTTL),
		correlation: meta.CorrelationID,
		initiator:   meta.Initiator,
	}
	service.mu.Lock()
	service.tokens[handle] = token
	service.pending[slot+"\x00"+itoa64(generation)] = token
	service.mu.Unlock()
	return handle, nil
}

// rejectionRowVersion and rejectionActive build the deterministic slot
// command rejections; the row version rides in Detail so the typed error can
// be rebuilt for the caller.
func rejectionRowVersion(current int64) *execution.Rejection {
	return &execution.Rejection{Code: codeRowVersionConflict, Detail: strconv.FormatInt(current, 10)}
}

func rejectionActive(detail string) *execution.Rejection {
	return &execution.Rejection{Code: codeActiveConflict, Detail: detail}
}

// translateSlotRejection rebuilds the package's stable typed errors from a
// recorded runner rejection, so existing callers keep their exact error
// surface (the HTTP problem mapping keys on these types). Any non-rejection
// error passes through verbatim.
func translateSlotRejection(err error) error {
	var rejection *execution.Rejection
	if !errors.As(err, &rejection) {
		return err
	}
	switch rejection.Code {
	case codeRowVersionConflict:
		current, parseErr := strconv.ParseInt(rejection.Detail, 10, 64)
		if parseErr != nil {
			return err
		}
		return &RowVersionError{Current: current}
	case codeActiveConflict:
		return fmt.Errorf("%w: %s", ErrActiveConflict, rejection.Detail)
	default:
		return err
	}
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

// RecordRevealAudit persists the authorized-access audit fact for a
// registration-token reveal BEFORE the caller releases the raw token
// (SEC-REVEAL-002/004): the automatic audit row commits inside the runner
// transaction, so an audit failure withholds the secret. It is an access
// fact, not a replayable command — the one-time reveal handle carries the
// idempotency — so it runs as a non-ledger execution. The record carries the
// acting administrator from the execution metadata and the slot-local
// generation as the domain reference; never the handle or the raw token.
func (service *Service) RecordRevealAudit(ctx context.Context, slot string, generation int64) error {
	if !ValidSlot(slot) {
		return ErrNotFound
	}
	_, err := execution.Execute(ctx, service.runner, service.ops.reveal,
		func(tx *execution.Tx) (bool, error) {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM runtime_slots WHERE slot=?`, slot).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return false, &execution.Rejection{Code: "not_found", Detail: "runtime slot 不存在"}
				}
				return false, err
			}
			return true, nil
		},
		func(bool) int64 { return generation })
	return err
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
