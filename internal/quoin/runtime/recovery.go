package runtime

// Lintel recovery registration (T35, OPS-HELPER-005 / VERIFY-RECOVERY-002):
// the temporary same-Release recovery Runtime service enters LintelRecovery
// maintenance with deployment-helper ownership, freezes the helper-provided
// non-secret fence digests as immutable checklist items, and mints the
// recovery-revision-bound 60-second one-time registration token. The token
// exists only in this process's memory and the single stdout envelope the
// helper pipes into `lintel register` attached stdin; it is never persisted.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// LintelRecoveryFence freezes the helper-owned recovery inputs. The digests
// are computed by the helper from its own backend observations; the database
// only ever sees the digests, never report bodies or secrets.
type LintelRecoveryFence struct {
	Backend           string // compose | kubernetes
	Disposition       string // exclusively_reattached | retired
	DispositionDigest string
	FenceReportDigest string
}

// LintelRecoveryBegin is the outcome of BeginLintelRecoveryRegistration.
type LintelRecoveryBegin struct {
	MaintenanceRevision   int64
	ReplacementGeneration int64
	// RegistrationToken is the raw one-time token when NeedsRegistration is
	// true; empty on resume (a replacement credential already exists and only
	// its first Hello is pending).
	RegistrationToken string
	NeedsRegistration bool
}

var (
	// ErrLintelRecoveryFence reports invalid fence inputs (closed value set,
	// digest shape) — an input-side failure with no state effect.
	ErrLintelRecoveryFence = errors.New("lintel recovery fence inputs are invalid")
	// ErrLintelRecoveryState reports the slot or maintenance state cannot
	// start or resume recovery (different maintenance active, slot not
	// registered, a rotation is already pending).
	ErrLintelRecoveryState = errors.New("lintel recovery state does not allow recovery")
	// ErrLintelRecoveryFrozenFence reports an active LintelRecovery revision
	// was frozen with different digests; the same revision can never be
	// re-fenced with new evidence.
	ErrLintelRecoveryFrozenFence = errors.New("active lintel recovery revision was frozen with different digests")
)

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (fence LintelRecoveryFence) validate() error {
	if fence.Backend != "compose" && fence.Backend != "kubernetes" {
		return fmt.Errorf("%w: backend must be compose or kubernetes", ErrLintelRecoveryFence)
	}
	if fence.Disposition != "exclusively_reattached" && fence.Disposition != "retired" {
		return fmt.Errorf("%w: storage disposition must be exclusively_reattached or retired", ErrLintelRecoveryFence)
	}
	if !validDigest(fence.DispositionDigest) || !validDigest(fence.FenceReportDigest) {
		return fmt.Errorf("%w: disposition and fence digests must be lowercase sha256 hex", ErrLintelRecoveryFence)
	}
	return nil
}

const (
	fenceDispositionItem = "disposition:"
	fenceReportItem      = "fence-report:"
	// recoverySlotItemKey is the RuntimeSlot checklist entry that keeps the
	// maintenance exit blocked until the offline finalizer marks it Safe.
	recoverySlotItemKey = "lintel"
)

// recoveryBeginOutcome carries the committed business state of one recovery
// begin to the post-commit token minting.
type recoveryBeginOutcome struct {
	revision   int64
	generation int64
	// needsRegistration is false on resume: a replacement credential already
	// exists and only its first Hello is pending.
	needsRegistration bool
}

// BeginLintelRecoveryRegistration enters (or resumes) the helper-owned
// LintelRecovery maintenance revision, freezes the fence digests as immutable
// checklist items, and — only when no replacement credential exists yet —
// mints the recovery one-time token in memory.
//
// The transition runs through the shared runner: one runner-owned IMMEDIATE
// transaction holds the deployment-scope re-check, the maintenance enter,
// the fence freeze and the automatic audit event; deterministic state
// conflicts are recorded as rejected facts and map back onto the package's
// exported sentinel errors. The token is created only after the runner
// committed and carries the begin's correlation and deployment-helper scope,
// so the later Register confirmation joins the same recovery lifecycle
// (ADR-0006); the raw token value never travels in a Context.
//
// Recovery stays deployment-only: the entry establishes (or strictly
// re-verifies) the offline deployment-helper scope, and the retired Lintel
// slot gains no general management enablement.
func (service *Service) BeginLintelRecoveryRegistration(ctx context.Context, fence LintelRecoveryFence) (LintelRecoveryBegin, error) {
	if err := fence.validate(); err != nil {
		return LintelRecoveryBegin{}, err
	}
	scope, err := deploymentRecoveryScope(ctx)
	if err != nil {
		return LintelRecoveryBegin{}, err
	}
	meta, _ := execution.FromContext(scope)
	begin, err := execution.Execute(scope, service.runner, service.ops.beginLintelRecover, func(tx *execution.Tx) (recoveryBeginOutcome, error) {
		return service.beginLintelRecoveryOn(scope, tx, fence)
	}, func(outcome recoveryBeginOutcome) int64 { return outcome.revision })
	if err != nil {
		return LintelRecoveryBegin{}, translateRecoveryRejection(err)
	}
	result := LintelRecoveryBegin{MaintenanceRevision: begin.revision, ReplacementGeneration: begin.generation}
	if !begin.needsRegistration {
		return result, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return LintelRecoveryBegin{}, err
	}
	token := &registrationToken{
		slot: SlotLintel, generation: begin.generation,
		raw:              base64.RawURLEncoding.EncodeToString(raw),
		digest:           sha256.Sum256(raw),
		expiresAt:        service.now().Add(registrationTokenTTL),
		recoveryRevision: begin.revision,
		correlation:      meta.CorrelationID,
		initiator:        meta.Initiator,
	}
	service.mu.Lock()
	service.pending[SlotLintel+"\x00"+itoa64(begin.generation)] = token
	service.mu.Unlock()
	result.RegistrationToken = token.raw
	result.NeedsRegistration = true
	return result, nil
}

// deploymentRecoveryScope establishes the execution scope of the
// deployment-only recovery entry (docs/audit-design.md §4: CLI and
// maintenance operations establish an explicit system principal and source at
// the entry and use the shared execution path). The offline helper sessions
// run against the stopped server with direct database ownership and no
// session identity, so this package is the trusted entry: a context without
// metadata receives a fresh correlation under the system principal with the
// CLI source. A context that already carries metadata is accepted only as
// the same deployment scope — a user session can never begin the recovery.
func deploymentRecoveryScope(ctx context.Context) (context.Context, error) {
	if meta, ok := execution.FromContext(ctx); ok {
		if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 || meta.Source.Kind != execution.SourceCLI {
			return nil, fmt.Errorf("%w: lintel recovery begins only from the deployment helper entry", ErrLintelRecoveryState)
		}
		return ctx, nil
	}
	correlation, err := execution.NewCorrelationID()
	if err != nil {
		return nil, err
	}
	return execution.WithMetadata(ctx, execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Source:        execution.Source{Kind: execution.SourceCLI},
	})
}

// beginLintelRecoveryOn performs the maintenance enter, the fence freeze and
// the replacement-window determination inside the runner transaction; the
// helper-provided digests are the only fence content the database ever sees.
func (service *Service) beginLintelRecoveryOn(ctx context.Context, tx *execution.Tx, fence LintelRecoveryFence) (recoveryBeginOutcome, error) {
	outcome := recoveryBeginOutcome{}
	var active int
	var reason string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return outcome, err
	}
	if active == 1 && reason != "LintelRecovery" {
		return outcome, recoveryStateRejection(fmt.Sprintf("maintenance %s is active", reason))
	}
	if active == 0 {
		// The maintenance enter trigger binds LintelRecovery to the
		// deployment_helper actor and requires the previous exit's fields to
		// be cleared in the same transition; the row_version CAS fences a
		// racing begin.
		now := service.now().UTC().Format(time.RFC3339Nano)
		result, err := tx.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason='LintelRecovery',entered_at=?,entered_by_type='deployment_helper',entered_by_id=0,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, now, revision)
		if err != nil {
			return outcome, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return outcome, recoveryStateRejection("concurrent maintenance transition")
		}
		revision++
	}
	if err := freezeRecoveryFence(ctx, tx, revision, fence); err != nil {
		return outcome, err
	}

	var state string
	var currentID sql.NullInt64
	var pendingID, retiringID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT state,current_credential_id,pending_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, SlotLintel).Scan(&state, &currentID, &pendingID, &retiringID); err != nil {
		return outcome, err
	}
	if state != string(StateRegistered) || !currentID.Valid {
		return outcome, recoveryStateRejection("lintel slot must stay registered with a current credential")
	}
	outcome.revision = revision
	if retiringID.Valid {
		// Resume: a replacement is already current and only its first Hello
		// (or the finalizer) is pending. Never mint a second token.
		if pendingID.Valid {
			return outcome, recoveryStateRejection("unexpected pending credential during lintel recovery")
		}
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM runtime_credentials WHERE id=?`, currentID.Int64).Scan(&outcome.generation); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
	if pendingID.Valid {
		return outcome, recoveryStateRejection("a rotation is already pending")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM runtime_credentials WHERE slot=?`, SlotLintel).Scan(&outcome.generation); err != nil {
		return outcome, err
	}
	outcome.needsRegistration = true
	return outcome, nil
}

// recoveryStateRejection and the frozen-fence rejection build the
// deterministic recovery rejections the runner records as rejected facts;
// translateRecoveryRejection maps them back onto the exported sentinels.
func recoveryStateRejection(detail string) *execution.Rejection {
	return &execution.Rejection{Code: codeRecoveryState, Detail: detail}
}

func recoveryFrozenFenceRejection(detail string) *execution.Rejection {
	return &execution.Rejection{Code: codeRecoveryFrozenFence, Detail: detail}
}

// translateRecoveryRejection rebuilds the package's sentinel-wrapped errors
// from recorded runner rejections, so existing callers keep their exact
// error surface. Any non-rejection error passes through verbatim.
func translateRecoveryRejection(err error) error {
	var rejection *execution.Rejection
	if !errors.As(err, &rejection) {
		return err
	}
	switch rejection.Code {
	case codeRecoveryState:
		return fmt.Errorf("%w: %s", ErrLintelRecoveryState, rejection.Detail)
	case codeRecoveryFrozenFence:
		return fmt.Errorf("%w: %s", ErrLintelRecoveryFrozenFence, rejection.Detail)
	default:
		return err
	}
}

// freezeRecoveryFence makes the two digest bindings immutable for the
// revision: re-freezing the same revision with different digests conflicts,
// and the offline finalizer later requires exactly these object keys. The
// executor parameter composes with both a raw connection and the runner's
// guarded transaction handle.
func freezeRecoveryFence(ctx context.Context, executor execution.Executor, revision int64, fence LintelRecoveryFence) error {
	expected := map[string]string{
		fenceDispositionItem + fence.DispositionDigest: "storage_disposition_frozen",
		fenceReportItem + fence.FenceReportDigest:      "workload_fence_frozen",
	}
	rows, err := executor.QueryContext(ctx, `SELECT object_key FROM maintenance_items WHERE maintenance_revision=? AND kind='LintelRecoveryFence'`, revision)
	if err != nil {
		return err
	}
	frozen := map[string]struct{}{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		frozen[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key := range frozen {
		if _, ok := expected[key]; !ok {
			return recoveryFrozenFenceRejection(fmt.Sprintf("revision %d already frozen with %q", revision, key))
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, detail := range expected {
		if _, ok := frozen[key]; ok {
			continue
		}
		if _, err := executor.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?)`,
			revision, "LintelRecoveryFence", key, "Blocking", detail, now); err != nil {
			return err
		}
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at)
		SELECT ?, 'RuntimeSlot', ?, 'Blocking', 'replacement_registration_required', ?
		WHERE NOT EXISTS (SELECT 1 FROM maintenance_items WHERE maintenance_revision=? AND kind='RuntimeSlot' AND object_key=?)`,
		revision, recoverySlotItemKey, now, revision, recoverySlotItemKey); err != nil {
		return err
	}
	return nil
}
