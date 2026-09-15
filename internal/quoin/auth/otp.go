package auth

// One-time verification codes (docs/authentication-design.md §4). Codes are
// low entropy, so only a keyed hash may be stored: HMAC-SHA256 under the
// configured OTP key, bound to the flow, user, contact, target version,
// auth revision and purpose — never a bare digest of the code. Codes are
// short-lived, single-consumption, and resend supersedes the previous active
// code while failure counters accumulate across resends on the flow.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

const (
	// otpCodeDigits is the numeric code length (frontend InputOTP shape).
	otpCodeDigits = 6
	// otpTTL bounds every challenge.
	otpTTL = 5 * time.Minute
	// otpMaxAttempts caps failed verifications accumulated across resends.
	otpMaxAttempts = 5
	// otpResendCooldown is the minimum spacing between two sends on one flow.
	otpResendCooldown = 30 * time.Second
	// otpMaxPerFlow caps total sends per flow lifetime (cost bounding).
	otpMaxPerFlow = 12
	// otpMaxPerUserWindow caps sends per user across all flows (SMS cost).
	otpMaxPerUserWindow = 20
	// otpUserSendWindow bounds the per-user send cap window.
	otpUserSendWindow = 15 * time.Minute
	// otpFailedFlowsPerUserWindow caps fully failed flows (5 wrong codes
	// each) a user may burn per window even across process restarts; the
	// in-memory per-user verify limiter sits in front of it.
	otpFailedFlowsPerUserWindow = 2
	// otpUserFailureWindow bounds the durable failed-flow window.
	otpUserFailureWindow = 30 * time.Minute
)

// Challenge purposes (auth_challenges.purpose CHECK).
const (
	PurposeSecondFactor        = "second_factor"
	PurposeContactVerification = "contact_verification"
)

// Delivery outcomes (auth_challenges.delivery_status CHECK). Sender errors are
// recorded as unknown: a timeout must never be recorded as a definite failure.
const (
	DeliveryPending  = "pending"
	DeliveryAccepted = "accepted"
	DeliveryFailed   = "failed"
	DeliveryUnknown  = "unknown"
)

type challengeRow struct {
	ID             int64
	FlowID         int64
	UserID         int64
	Purpose        string
	ContactID      int64
	ContactVersion int64
	Revision       int64
	DeliveryID     string
	DeliveryStatus string
	CreatedAt      string
	ExpiresAt      string
	ConsumedAt     sql.NullString
}

const challengeColumns = `id,flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,delivery_status,created_at,expires_at,consumed_at`

func scanChallenge(row *sql.Row) (challengeRow, []byte, error) {
	var challenge challengeRow
	var digest []byte
	if err := row.Scan(&challenge.ID, &challenge.FlowID, &challenge.UserID, &challenge.Purpose, &challenge.ContactID, &challenge.ContactVersion, &challenge.Revision, &digest, &challenge.DeliveryID, &challenge.DeliveryStatus, &challenge.CreatedAt, &challenge.ExpiresAt, &challenge.ConsumedAt); err != nil {
		return challengeRow{}, nil, err
	}
	return challenge, digest, nil
}

// generateOTP returns a uniformly distributed numeric code. Rejection sampling
// avoids the modulo bias of a naive reduction.
func generateOTP() (string, error) {
	ceiling := (^uint64(0) / 1_000_000) * 1_000_000
	for {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate verification code: %w", err)
		}
		value := binary.BigEndian.Uint64(raw)
		if value >= ceiling {
			continue
		}
		return fmt.Sprintf("%0*d", otpCodeDigits, value%1_000_000), nil
	}
}

// challengeDigest computes the keyed, identity-bound code digest. Verification
// recomputes it from the stored row so a swapped or replayed digest alone is
// useless without the key and the bound identity.
func challengeDigest(key []byte, flow flowRow, contactID, contactVersion int64, purpose, code string) []byte {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "quoin-otp-v1\x00flow=%d\x00user=%d\x00contact=%d\x00contact_version=%d\x00revision=%d\x00purpose=%s\x00code=%s",
		flow.ID, flow.UserID, contactID, contactVersion, flow.Revision, purpose, code)
	return mac.Sum(nil)
}

// candidateChallengeDigest binds the keyed digest to the STAGED candidate on
// the flow (channel+target live on auth_flows for factor changes; the
// challenge's contact anchor only holds the FK to the existing row).
func candidateChallengeDigest(key []byte, flow flowRow, anchorContactID, anchorVersion int64, purpose, code string) []byte {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "quoin-otp-candidate-v1\x00flow=%d\x00user=%d\x00anchor=%d\x00anchor_version=%d\x00channel=%s\x00target=%s\x00revision=%d\x00purpose=%s\x00code=%s",
		flow.ID, flow.UserID, anchorContactID, anchorVersion, flow.CandidateChannel.String, flow.CandidateTarget.String, flow.Revision, purpose, code)
	return mac.Sum(nil)
}

// insertCandidateChallenge persists the factor-change challenge: the anchor
// row satisfies the contact FK while the delivery goes to the staged
// candidate target. The row stays delivery-pending; the outcome is recorded
// outside the transaction like every other send.
func insertCandidateChallenge(ctx context.Context, writer flowWriter, key []byte, flow flowRow, anchor contactRow, secret string) (string, string, error) {
	deliveryID, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	if _, err := writer.ExecContext(ctx, `INSERT INTO auth_challenges(flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,delivery_status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		flow.ID, flow.UserID, PurposeContactVerification, anchor.ID, anchor.Version, flow.Revision, candidateChallengeDigest(key, flow, anchor.ID, anchor.Version, PurposeContactVerification, secret), deliveryID, DeliveryPending, now.Format(time.RFC3339Nano), now.Add(otpTTL).Format(time.RFC3339Nano)); err != nil {
		return "", "", fmt.Errorf("persist candidate challenge: %w", err)
	}
	return secret, deliveryID, nil
}

// insertChallenge persists a pending challenge inside the caller's
// transaction. The delivery outcome is recorded later, outside the
// transaction that created the row (the Sender is never called inside a
// database transaction).
func insertChallenge(ctx context.Context, writer flowWriter, key []byte, flow flowRow, contact contactRow, purpose, secret string) (string, string, error) {
	deliveryID, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	if _, err := writer.ExecContext(ctx, `INSERT INTO auth_challenges(flow_id,user_id,purpose,contact_id,contact_version,auth_revision_at_issue,code_digest,delivery_id,delivery_status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		flow.ID, flow.UserID, purpose, contact.ID, contact.Version, flow.Revision, challengeDigest(key, flow, contact.ID, contact.Version, purpose, secret), deliveryID, DeliveryPending, now.Format(time.RFC3339Nano), now.Add(otpTTL).Format(time.RFC3339Nano)); err != nil {
		return "", "", fmt.Errorf("persist challenge: %w", err)
	}
	return secret, deliveryID, nil
}

// latestActiveChallenge reads the newest unconsumed challenge of a flow and
// purpose; sql.ErrNoRows means no active code exists.
func latestActiveChallenge(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, flowID int64, purpose string,
) (challengeRow, []byte, error) {
	return scanChallenge(reader.QueryRowContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE flow_id=? AND purpose=? AND consumed_at IS NULL ORDER BY id DESC LIMIT 1`, flowID, purpose))
}

// challengeUsable re-checks every binding of a stored challenge at verification
// time: single consumption, expiry, accepted delivery, unchanged contact
// target version and unchanged user auth revision. Any mismatch is an
// ErrOtpInvalid fact — never an infrastructure fallback.
func challengeUsable(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, challenge challengeRow, user User,
) (contactRow, error) {
	if challenge.ConsumedAt.Valid {
		return contactRow{}, ErrOtpInvalid
	}
	if expires, err := time.Parse(time.RFC3339Nano, challenge.ExpiresAt); err != nil || !time.Now().UTC().Before(expires) {
		return contactRow{}, ErrOtpInvalid
	}
	if challenge.DeliveryStatus != DeliveryAccepted {
		return contactRow{}, ErrOtpInvalid
	}
	contact, err := findContactByID(ctx, reader, challenge.ContactID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contactRow{}, ErrOtpInvalid
		}
		return contactRow{}, err
	}
	if !contact.Enabled || contact.Version != challenge.ContactVersion || contact.UserID != challenge.UserID {
		return contactRow{}, ErrOtpInvalid
	}
	if challenge.Revision != user.AuthRevision {
		return contactRow{}, ErrOtpInvalid
	}
	return contact, nil
}

// consumeChallenge marks the challenge consumed exactly once.
func consumeChallenge(ctx context.Context, writer flowWriter, challengeID int64) error {
	result, err := writer.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE id=? AND consumed_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), challengeID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrOtpInvalid
	}
	return nil
}

// otpUserSends counts the challenges issued for one user inside the send
// window; the count is durable, so restarts cannot bypass the SMS cost cap.
func (service *Service) otpUserSends(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64,
) (int, error) {
	windowStart := time.Now().UTC().Add(-otpUserSendWindow).Format(time.RFC3339Nano)
	var sends int
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges c JOIN auth_flows f ON f.id=c.flow_id WHERE f.user_id=? AND c.created_at>=?`, userID, windowStart).Scan(&sends)
	return sends, err
}

// otpFailedFlows counts the flows the user burned by exhausting the attempt
// cap inside the durable failure window.
func (service *Service) otpFailedFlows(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64,
) (int, error) {
	windowStart := time.Now().UTC().Add(-otpUserFailureWindow).Format(time.RFC3339Nano)
	var failed int
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_flows WHERE user_id=? AND status='failed' AND created_at>=?`, userID, windowStart).Scan(&failed)
	return failed, err
}

// deliveryOutcomeCall carries the delivery fact into the fixed Authorize
// callback; ids and the outcome enum only — the verification code never
// travels through context.
type deliveryOutcomeCall struct {
	challengeID int64
	status      string
	flowID      int64
	userID      int64
}

type deliveryOutcomeKey struct{}

func withDeliveryOutcome(ctx context.Context, call *deliveryOutcomeCall) context.Context {
	return context.WithValue(ctx, deliveryOutcomeKey{}, call)
}

// authorizeDeliveryResult re-checks inside the transaction that the caller is
// the delivery bookkeeping task (system actor, task source) acting on behalf
// of exactly the user the challenge belongs to, and that the challenge still
// awaits its delivery outcome.
func authorizeDeliveryResult(ctx context.Context, tx *execution.Tx) error {
	meta, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if meta.Actor.Kind != execution.PrincipalSystem || meta.Actor.ID != 0 {
		return errors.New("auth: delivery outcomes are recorded by the system actor")
	}
	if meta.Source.Kind != execution.SourceTask {
		return errors.New("auth: delivery outcomes are recorded from the task source")
	}
	if meta.Initiator.Kind != execution.PrincipalUser || meta.Initiator.ID < 1 {
		return errors.New("auth: delivery outcomes require a user initiator")
	}
	call, _ := ctx.Value(deliveryOutcomeKey{}).(*deliveryOutcomeCall)
	if call == nil {
		return errors.New("auth: delivery outcome inputs are missing from the request context")
	}
	var flowID, challengeUser int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT flow_id,user_id,delivery_status FROM auth_challenges WHERE id=?`, call.challengeID).Scan(&flowID, &challengeUser, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("auth: delivery outcome recorded for a missing challenge")
		}
		return err
	}
	if challengeUser != meta.Initiator.ID {
		return errors.New("auth: delivery outcome initiator does not match the challenge user")
	}
	if status != DeliveryPending {
		return errors.New("auth: challenge delivery outcome was already recorded")
	}
	call.flowID, call.userID = flowID, challengeUser
	return nil
}

// recordDeliveryOutcome records the Sender result for a pending challenge
// through the execution runner (action auth.challenge.delivery_result): the
// flow's persisted correlation is restored so the delivery fact lands in the
// same audit trail as the flow that caused it, with the flow's user as actor
// and the task source — this step runs after the user's request has returned.
// Sender errors map to unknown — a timeout must never be recorded as a
// definite failure. The recording is detached from the caller's context with
// a bounded timeout: a canceled request must still store the outcome.
func (service *Service) recordDeliveryOutcome(ctx context.Context, challengeID int64, sendErr error) error {
	// Detached, bounded scope from the very first query: a canceled or lost
	// request context must not stop the bookkeeping.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	status := DeliveryAccepted
	if sendErr != nil {
		status = DeliveryUnknown
	}
	var flowID, userID int64
	var correlation string
	err := service.read().QueryRowContext(recordCtx, `SELECT c.flow_id,c.user_id,f.correlation_id FROM auth_challenges c JOIN auth_flows f ON f.id=c.flow_id WHERE c.id=?`, challengeID).Scan(&flowID, &userID, &correlation)
	if err != nil {
		return fmt.Errorf("resolve delivery outcome flow: %w", err)
	}
	if correlation == "" {
		return fmt.Errorf("auth flow %d has no persisted correlation", flowID)
	}
	// The task bookkeeping acts as the SYSTEM on behalf of the flow's user:
	// audit attribution states the executor and the initiator separately.
	meta := execution.Metadata{
		CorrelationID: correlation,
		Actor:         execution.Principal{Kind: execution.PrincipalSystem},
		Initiator:     execution.Principal{Kind: execution.PrincipalUser, ID: userID},
		Source:        execution.Source{Kind: execution.SourceTask},
	}
	runCtx, err := execution.ReplaceMetadata(recordCtx, meta)
	if err != nil {
		return err
	}
	call := &deliveryOutcomeCall{challengeID: challengeID, status: status, flowID: flowID, userID: userID}
	_, err = execution.Execute(withDeliveryOutcome(runCtx, call), service.runner, service.ops.challengeDelivery, func(tx *execution.Tx) (bool, error) {
		update, err := tx.ExecContext(recordCtx, `UPDATE auth_challenges SET delivery_status=? WHERE id=? AND delivery_status=?`, status, challengeID, DeliveryPending)
		if err != nil {
			return false, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			// A concurrent recorder won the transition; the fact is stored.
			return false, nil
		}
		if status == DeliveryUnknown {
			// An uncertain send is genuinely unavailable, not a definite
			// failure: it commits the unknown state and audits with the
			// unknown outcome class.
			return false, &execution.RecordedFailure{Code: "delivery_unknown", Detail: "sender outcome unknown", ObjectID: challengeID, Outcome: audit.OutcomeUnknown}
		}
		return true, nil
	}, func(bool) int64 { return challengeID })
	var failure *execution.RecordedFailure
	if errors.As(err, &failure) && failure.Code == "delivery_unknown" {
		// The intended bookkeeping outcome; the caller's send error is the
		// one surfaced to the user.
		return nil
	}
	return err
}
