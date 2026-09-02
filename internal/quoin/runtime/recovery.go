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
)

// LintelRecoveryFence freezes the helper-owned recovery inputs. The digests
// are computed by the helper from its own backend observations; the database
// only ever sees the digests, never report bodies or secrets.
type LintelRecoveryFence struct {
	Backend           string // compose | helm
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
	if fence.Backend != "compose" && fence.Backend != "helm" {
		return fmt.Errorf("%w: backend must be compose or helm", ErrLintelRecoveryFence)
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

// BeginLintelRecoveryRegistration enters (or resumes) the helper-owned
// LintelRecovery maintenance revision, freezes the fence digests as immutable
// checklist items, and — only when no replacement credential exists yet —
// mints the recovery one-time token in memory.
func (service *Service) BeginLintelRecoveryRegistration(ctx context.Context, fence LintelRecoveryFence) (LintelRecoveryBegin, error) {
	if err := fence.validate(); err != nil {
		return LintelRecoveryBegin{}, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return LintelRecoveryBegin{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return LintelRecoveryBegin{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var active int
	var reason string
	var revision int64
	if err := conn.QueryRowContext(ctx, `SELECT active,COALESCE(reason,''),row_version FROM maintenance_state WHERE id=1`).Scan(&active, &reason, &revision); err != nil {
		return LintelRecoveryBegin{}, err
	}
	if active == 1 && reason != "LintelRecovery" {
		return LintelRecoveryBegin{}, fmt.Errorf("%w: maintenance %s is active", ErrLintelRecoveryState, reason)
	}
	if active == 0 {
		// The maintenance enter trigger binds LintelRecovery to the
		// deployment_helper actor and requires the previous exit's fields to
		// be cleared in the same transition; the row_version CAS fences a
		// racing begin.
		now := service.now().UTC().Format(time.RFC3339Nano)
		result, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET active=1,reason='LintelRecovery',entered_at=?,entered_by_type='deployment_helper',entered_by_id=0,exited_at=NULL,exited_by_type=NULL,exited_by_id=NULL,row_version=row_version+1 WHERE id=1 AND active=0 AND row_version=?`, now, revision)
		if err != nil {
			return LintelRecoveryBegin{}, err
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return LintelRecoveryBegin{}, fmt.Errorf("%w: concurrent maintenance transition", ErrLintelRecoveryState)
		}
		revision++
	}
	if err := freezeRecoveryFence(ctx, conn, revision, fence); err != nil {
		return LintelRecoveryBegin{}, err
	}

	var state string
	var currentID sql.NullInt64
	var pendingID, retiringID sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT state,current_credential_id,pending_credential_id,retiring_credential_id FROM runtime_slots WHERE slot=?`, SlotLintel).Scan(&state, &currentID, &pendingID, &retiringID); err != nil {
		return LintelRecoveryBegin{}, err
	}
	if state != string(StateRegistered) || !currentID.Valid {
		return LintelRecoveryBegin{}, fmt.Errorf("%w: lintel slot must stay registered with a current credential", ErrLintelRecoveryState)
	}
	begin := LintelRecoveryBegin{MaintenanceRevision: revision}
	if retiringID.Valid {
		// Resume: a replacement is already current and only its first Hello
		// (or the finalizer) is pending. Never mint a second token.
		if pendingID.Valid {
			return LintelRecoveryBegin{}, fmt.Errorf("%w: unexpected pending credential during lintel recovery", ErrLintelRecoveryState)
		}
		if err := conn.QueryRowContext(ctx, `SELECT generation FROM runtime_credentials WHERE id=?`, currentID.Int64).Scan(&begin.ReplacementGeneration); err != nil {
			return LintelRecoveryBegin{}, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return LintelRecoveryBegin{}, err
		}
		committed = true
		return begin, nil
	}
	if pendingID.Valid {
		return LintelRecoveryBegin{}, fmt.Errorf("%w: a rotation is already pending", ErrLintelRecoveryState)
	}
	var next int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM runtime_credentials WHERE slot=?`, SlotLintel).Scan(&next); err != nil {
		return LintelRecoveryBegin{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return LintelRecoveryBegin{}, err
	}
	committed = true

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return LintelRecoveryBegin{}, err
	}
	token := &registrationToken{
		slot: SlotLintel, generation: next,
		raw:              base64.RawURLEncoding.EncodeToString(raw),
		digest:           sha256.Sum256(raw),
		expiresAt:        service.now().Add(registrationTokenTTL),
		recoveryRevision: revision,
	}
	service.mu.Lock()
	service.pending[SlotLintel+"\x00"+itoa64(next)] = token
	service.mu.Unlock()
	begin.ReplacementGeneration = next
	begin.RegistrationToken = token.raw
	begin.NeedsRegistration = true
	return begin, nil
}

// freezeRecoveryFence makes the two digest bindings immutable for the
// revision: re-freezing the same revision with different digests conflicts,
// and the offline finalizer later requires exactly these object keys.
func freezeRecoveryFence(ctx context.Context, conn *sql.Conn, revision int64, fence LintelRecoveryFence) error {
	expected := map[string]string{
		fenceDispositionItem + fence.DispositionDigest: "storage_disposition_frozen",
		fenceReportItem + fence.FenceReportDigest:      "workload_fence_frozen",
	}
	rows, err := conn.QueryContext(ctx, `SELECT object_key FROM maintenance_items WHERE maintenance_revision=? AND kind='LintelRecoveryFence'`, revision)
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
			return fmt.Errorf("%w: revision %d already frozen with %q", ErrLintelRecoveryFrozenFence, revision, key)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for key, detail := range expected {
		if _, ok := frozen[key]; ok {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at) VALUES(?,?,?,?,?,?)`,
			revision, "LintelRecoveryFence", key, "Blocking", detail, now); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_items(maintenance_revision,kind,object_key,safe_state,detail_code,updated_at)
		SELECT ?, 'RuntimeSlot', ?, 'Blocking', 'replacement_registration_required', ?
		WHERE NOT EXISTS (SELECT 1 FROM maintenance_items WHERE maintenance_revision=? AND kind='RuntimeSlot' AND object_key=?)`,
		revision, recoverySlotItemKey, now, revision, recoverySlotItemKey); err != nil {
		return err
	}
	return nil
}
