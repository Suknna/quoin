package auth

// Flow steps for initialization and second factor (docs/authentication-design.md
// §2-§4). Every mutating step runs through the execution runner: one
// runner-owned IMMEDIATE transaction, authorization re-checked inside it, and
// the audit row committed atomically — no private audit calls. Flow results
// carry transient secrets (bearers, codes) that must never enter the command
// ledger, so steps use execution.Execute, not execution.Run.
//
// Secrets never travel through context: the step call carries only row ids,
// digests and the captured password PHC. Raw credentials (password,
// verification code) live in the caller's scope or the per-call
// business closures only; the fixed Authorize callbacks re-check binding
// facts by id and digest.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// startClass is the internal classification of one credential-verified start.
type startClass string

const (
	startClassAdmin         startClass = "admin_initialize"
	startClassOperator      startClass = "operator_initialize"
	startClassLogin         startClass = "login"
	startClassContactChange startClass = "contact_change"
)

// flowDigest derives the context-safe form of a flow bearer.
func flowDigest(bearer string) []byte {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return nil
	}
	digest := sha256.Sum256(raw)
	return digest[:]
}

// stepCall carries one call's NON-SECRET facts into the registered Authorize
// callback through a private context key and receives the authoritatively
// resolved flow back.
type stepCall struct {
	flowID      int64
	tokenDigest []byte // SHA-256 of the flow bearer; the raw credential stays out of context

	// Credential-start facts: the password was verified outside the
	// transaction against passwordPHC; Authorize rejects when the stored PHC
	// moved since (credential rotated mid-flight).
	userID       int64
	username     string
	passwordPHC  string
	credentialID int64
	userAgent    string

	contactID int64
	// password carries the current-password proof of a factor-change start;
	// it is read only inside that start's business closure and never leaves
	// the request scope.
	password string

	flow flowRow
	user User
}

type stepCallKey struct{}

func withStepCall(ctx context.Context, call *stepCall) context.Context {
	return context.WithValue(ctx, stepCallKey{}, call)
}

func stepCallFromContext(ctx context.Context) *stepCall {
	call, _ := ctx.Value(stepCallKey{}).(*stepCall)
	return call
}

// flowCallContext restores the persisted flow correlation for a child step
// (deliberate re-rooting per the execution contract): audit rows for every
// step of one flow share the correlation created at flow start.
func flowCallContext(ctx context.Context, flow flowRow, userID int64) (context.Context, error) {
	if flow.CorrelationID == "" {
		return nil, fmt.Errorf("auth flow %d has no persisted correlation", flow.ID)
	}
	return execution.ReplaceMetadata(ctx, execution.Metadata{
		CorrelationID: flow.CorrelationID,
		Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: userID},
		Source:        execution.Source{Kind: execution.SourceInternal},
	})
}

// preresolveFlow hashes the raw bearer and resolves the pending flow outside
// the transaction — only to restore correlation metadata and fail fast. The
// authoritative check happens inside the runner transaction.
func (service *Service) preresolveFlow(ctx context.Context, bearer string, allowed ...FlowType) (flowRow, User, error) {
	digest := flowDigest(bearer)
	if digest == nil {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	return resolveFlowDigest(ctx, service.read(), digest, allowed...)
}

// authorizeFlowStep resolves the flow by id and token digest inside the
// transaction and stores the authoritative row for the business stage.
func authorizeFlowStep(allowed ...FlowType) func(context.Context, *execution.Tx) error {
	return func(ctx context.Context, tx *execution.Tx) error {
		call := stepCallFromContext(ctx)
		if call == nil {
			return errors.New("auth: flow step inputs are missing from the request context")
		}
		flow, user, err := resolveFlowDigest(ctx, tx, call.tokenDigest, allowed...)
		if err != nil {
			return err
		}
		call.flow, call.user = flow, user
		return nil
	}
}

// authorizeCredentialStart re-checks the outside-verified credential inside
// the transaction without touching any raw secret: the stored password PHC
// must still equal the hash the password was verified against, the user must
// still be enabled and match the class state.
func authorizeCredentialStart(class startClass) func(context.Context, *execution.Tx) error {
	return func(ctx context.Context, tx *execution.Tx) error {
		call := stepCallFromContext(ctx)
		if call == nil {
			return errors.New("auth: flow start inputs are missing from the request context")
		}
		user, err := findUserByID(ctx, tx, call.userID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrUnauthenticated
			}
			return err
		}
		if user.Username != call.username || subtle.ConstantTimeCompare([]byte(user.passwordPHC), []byte(call.passwordPHC)) != 1 {
			// The credential changed since the outside verification: the
			// caller must restart with the current credential.
			return ErrUnauthenticated
		}
		if !user.Enabled {
			return ErrUnauthenticated
		}
		switch class {
		case startClassAdmin:
			if user.Role != "admin" {
				return ErrInitializeRejected
			}
			if user.Initialized {
				return ErrInitializationRequired
			}
		case startClassOperator:
			if user.Role != "operator" {
				return ErrInitializeRejected
			}
			if user.Initialized {
				return ErrInitializationRequired
			}
		case startClassLogin:
			if !user.Initialized {
				return ErrInitializationRequired
			}
		case startClassContactChange:
			// The self-service factor change is the administrator's safe
			// channel replacement; operators are rotated by the admin.
			if user.Role != "admin" || !user.Initialized {
				return ErrInitializeRejected
			}
		}
		call.user = user
		return nil
	}
}

type startResult struct {
	Flow   flowRow
	Bearer string
	User   User
}

// mapRejection translates deterministic runner rejections back into the
// package sentinels so the app keeps one stable error surface.
func mapRejection(err error) error {
	var rejection *execution.Rejection
	if errors.As(err, &rejection) {
		switch rejection.Code {
		case "validation_failed":
			return fmt.Errorf("%w: %s", ErrValidation, rejection.Detail)
		case "password_policy":
			return fmt.Errorf("%w: %s", ErrPasswordPolicy, rejection.Detail)
		case "not_found":
			return ErrNotFound
		}
	}
	return err
}

func rejection(code, detail string, objectID int64) error {
	return &execution.Rejection{Code: code, Detail: detail, ObjectID: objectID}
}

// SetFlowPassword persists a new formal password for a pending
// initialization flow: users row, flow password_set flag and the flow's
// revision binding advance in ONE transaction, so a refresh or restart can
// never roll the completed step back to the temporary credential. The new
// password is captured by this per-call business closure only.
func (service *Service) SetFlowPassword(ctx context.Context, bearer, newPassword string) error {
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowAdminInitialize, FlowOperatorInitialize)
	if err != nil {
		return err
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	_, err = execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.setFlowPassword, func(tx *execution.Tx) (int64, error) {
		normalized, policyErr := ValidateNewPassword(newPassword, call.user.Username, call.user.DisplayName)
		if policyErr != nil {
			return 0, rejection("password_policy", policyErr.Error(), call.user.ID)
		}
		phc, err := HashPassword(normalized)
		if err != nil {
			return 0, err
		}
		now := service.timestamp()
		update, err := tx.ExecContext(ctx, `UPDATE users SET password_phc=?,password_change_required=0,password_change_required_at=NULL,auth_revision=auth_revision+1,row_version=row_version+1,updated_at=? WHERE id=? AND auth_revision=?`, phc, now, call.user.ID, call.flow.Revision)
		if err != nil {
			return 0, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return 0, ErrFlowInvalid
		}
		advanced, err := tx.ExecContext(ctx, `UPDATE auth_flows SET password_set=1,auth_revision_at_issue=auth_revision_at_issue+1 WHERE id=? AND auth_revision_at_issue=? AND status='pending'`, call.flow.ID, call.flow.Revision)
		if err != nil {
			return 0, err
		}
		if rows, _ := advanced.RowsAffected(); rows != 1 {
			return 0, ErrFlowInvalid
		}
		// A password change revokes every session of the user (none should
		// exist mid-initialization; enforced defensively).
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, call.user.ID); err != nil {
			return 0, err
		}
		return call.user.ID, nil
	}, func(int64) int64 { return call.user.ID })
	return mapRejection(err)
}

// RegisterFlowContact assigns the administrator's own contact target during
// the admin initialization flow. Operators cannot call this: their targets are
// admin-assigned through CreateUser/SetUserContacts only.
func (service *Service) RegisterFlowContact(ctx context.Context, bearer, channel, target string) (MaskedContact, error) {
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowAdminInitialize, FlowContactChange)
	if err != nil {
		return MaskedContact{}, err
	}
	if err := validateContactInput(channel, target); err != nil {
		return MaskedContact{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return MaskedContact{}, err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	if flow.Type == FlowContactChange {
		// Factor change stages the replacement on the flow row: the live
		// user_contacts target keeps its verification until the atomic
		// completion, so a failed delivery can never lock the administrator
		// out. Re-staging a different target supersedes the outstanding
		// challenge.
		_, err = execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.stageFlowContact, func(tx *execution.Tx) (bool, error) {
			if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET candidate_channel=?,candidate_target=? WHERE id=? AND status='pending'`, channel, target, call.flow.ID); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE flow_id=? AND consumed_at IS NULL`, service.timestamp(), call.flow.ID); err != nil {
				return false, err
			}
			return true, nil
		}, func(bool) int64 { return call.flow.ID })
		if err != nil {
			return MaskedContact{}, mapRejection(err)
		}
		return MaskedContact{Channel: channel, MaskedTarget: maskTarget(channel, target), Verified: false}, nil
	}
	contact, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.registerFlowContact, func(tx *execution.Tx) (contactRow, error) {
		stored, _, err := upsertContact(ctx, tx, call.user.ID, ContactInput{Channel: channel, Target: target}, service.timestamp())
		return stored, err
	}, func(contact contactRow) int64 { return contact.ID }) // audit target: the actual user_contacts row
	if err != nil {
		return MaskedContact{}, mapRejection(err)
	}
	return MaskedContact{Locator: strconv.FormatInt(contact.ID, 10), Channel: contact.Channel, MaskedTarget: maskTarget(contact.Channel, contact.Target), Verified: contact.VerifiedAt.Valid}, nil
}

type challengeResult struct {
	Issued      bool
	RetryAfter  time.Duration
	Code        string
	DeliveryID  string
	ChallengeID int64
	Channel     string
	Recipient   string
	Purpose     string
	Masked      MaskedContact
}

// SendFlowChallenge issues the flow's active OTP: it supersedes any previous
// active code, persists the keyed digest with a pending delivery, commits,
// and only then calls the Sender outside the transaction. The delivery
// outcome is recorded afterwards; a Sender failure never creates a session
// and never downgrades the flow.
func (service *Service) SendFlowChallenge(ctx context.Context, bearer, contactLocator string) (MaskedContact, time.Duration, error) {
	// Factor-change sends target the staged candidate on the flow; the
	// locator carries the candidate channel for UI symmetry and is not a
	// contact row id.
	contactID, parseErr := strconv.ParseInt(contactLocator, 10, 64)
	if parseErr != nil && contactLocator != "email" && contactLocator != "sms" {
		return MaskedContact{}, 0, fmt.Errorf("%w: contact locator is not valid", ErrValidation)
	}
	key, sender, configured := service.delivery()
	if !configured {
		return MaskedContact{}, 0, ErrFlowDeliveryNotConfigured
	}
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowAdminInitialize, FlowOperatorInitialize, FlowLogin, FlowContactChange)
	if err != nil {
		return MaskedContact{}, 0, err
	}
	if flow.Type == FlowContactChange {
		return service.sendCandidateChallenge(ctx, bearer, flow, user)
	}
	if parseErr != nil {
		return MaskedContact{}, 0, fmt.Errorf("%w: contact locator is not valid", ErrValidation)
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return MaskedContact{}, 0, err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer), contactID: contactID}
	result, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.issueChallenge, func(tx *execution.Tx) (challengeResult, error) {
		contact, err := findContactByID(ctx, tx, call.contactID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return challengeResult{}, rejection("validation_failed", "收码目标不存在", call.flow.ID)
			}
			return challengeResult{}, err
		}
		if contact.UserID != call.flow.UserID || !contact.Enabled {
			return challengeResult{}, rejection("validation_failed", "收码目标不存在", call.flow.ID)
		}
		purpose := PurposeContactVerification
		if call.flow.Type == FlowLogin {
			purpose = PurposeSecondFactor
		}
		var lastCreated sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT MAX(created_at) FROM auth_challenges WHERE flow_id=?`, call.flow.ID).Scan(&lastCreated); err != nil {
			return challengeResult{}, err
		}
		if lastCreated.Valid {
			if last, parseErr := time.Parse(time.RFC3339Nano, lastCreated.String); parseErr == nil {
				if elapsed := time.Since(last); elapsed < otpResendCooldown {
					return challengeResult{RetryAfter: otpResendCooldown - elapsed}, nil
				}
			}
		}
		var sends int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges WHERE flow_id=?`, call.flow.ID).Scan(&sends); err != nil {
			return challengeResult{}, err
		}
		if sends >= otpMaxPerFlow {
			return challengeResult{}, ErrFlowInvalid
		}
		// Durable per-user send budget across all flows (SMS cost bounding).
		userSends, err := service.otpUserSends(ctx, tx, call.flow.UserID)
		if err != nil {
			return challengeResult{}, err
		}
		if userSends >= otpMaxPerUserWindow {
			return challengeResult{}, ErrChallengeRateLimited
		}
		now := service.timestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE flow_id=? AND purpose=? AND consumed_at IS NULL`, now, call.flow.ID, purpose); err != nil {
			return challengeResult{}, err
		}
		secret, err := service.factorFor(contact.Channel).Secret(ctx)
		if err != nil {
			return challengeResult{}, err
		}
		code, deliveryID, err := insertChallenge(ctx, tx, key, call.flow, contact, purpose, secret)
		if err != nil {
			return challengeResult{}, err
		}
		var challengeID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_challenges WHERE delivery_id=?`, deliveryID).Scan(&challengeID); err != nil {
			return challengeResult{}, err
		}
		return challengeResult{
			Issued: true, Code: code, DeliveryID: deliveryID, ChallengeID: challengeID,
			Channel: contact.Channel, Recipient: contact.Target, Purpose: purpose,
			Masked: MaskedContact{Locator: strconv.FormatInt(contact.ID, 10), Channel: contact.Channel, MaskedTarget: maskTarget(contact.Channel, contact.Target), Verified: contact.VerifiedAt.Valid},
		}, nil
	}, func(result challengeResult) int64 { return call.flow.ID }) // audit target: the flow, success and rejection alike
	if err != nil {
		return MaskedContact{}, 0, mapRejection(err)
	}
	if !result.Issued {
		return result.Masked, result.RetryAfter, ErrChallengeRateLimited
	}
	template := "contact_verification"
	if result.Purpose == PurposeSecondFactor {
		template = "login_verification"
	}
	sendErr := sender.Send(ctx, Message{
		DeliveryID: result.DeliveryID, Channel: result.Channel, Recipient: result.Recipient,
		Template:  template,
		Variables: map[string]string{"code": result.Code, "expires_in_seconds": "300"},
	})
	if recordErr := service.recordDeliveryOutcome(ctx, result.ChallengeID, sendErr); recordErr != nil && sendErr == nil {
		return result.Masked, 0, recordErr
	}
	if sendErr != nil {
		return result.Masked, 0, sendErr
	}
	return result.Masked, 0, nil
}

// sendCandidateChallenge issues the factor-change challenge: the recipient is
// the staged candidate on the flow, the challenge anchors to any existing
// verified contact of the administrator (identity proof binding, not the
// recipient), and user_contacts stays untouched until atomic completion.
func (service *Service) sendCandidateChallenge(ctx context.Context, bearer string, flow flowRow, user User) (MaskedContact, time.Duration, error) {
	key, sender, configured := service.delivery()
	if !configured {
		return MaskedContact{}, 0, ErrFlowDeliveryNotConfigured
	}
	if !flow.CandidateTarget.Valid {
		return MaskedContact{}, 0, ErrNoContact
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return MaskedContact{}, 0, err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	result, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.issueChallenge, func(tx *execution.Tx) (challengeResult, error) {
		// Anchor: any existing enabled+verified contact proves the identity
		// binding; the candidate channel may be brand new (email vs sms).
		anchor, err := scanContact(tx.QueryRowContext(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE user_id=? AND enabled=1 AND verified_at IS NOT NULL ORDER BY (channel=?) DESC, id LIMIT 1`, call.flow.UserID, flow.CandidateChannel.String))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return challengeResult{}, rejection("validation_failed", "管理员缺少已验证的联系方式作为锚点", call.flow.ID)
			}
			return challengeResult{}, err
		}
		var lastCreated sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT MAX(created_at) FROM auth_challenges WHERE flow_id=?`, call.flow.ID).Scan(&lastCreated); err != nil {
			return challengeResult{}, err
		}
		if lastCreated.Valid {
			if last, parseErr := time.Parse(time.RFC3339Nano, lastCreated.String); parseErr == nil {
				if elapsed := time.Since(last); elapsed < otpResendCooldown {
					return challengeResult{RetryAfter: otpResendCooldown - elapsed}, nil
				}
			}
		}
		var sends int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_challenges WHERE flow_id=?`, call.flow.ID).Scan(&sends); err != nil {
			return challengeResult{}, err
		}
		if sends >= otpMaxPerFlow {
			return challengeResult{}, ErrFlowInvalid
		}
		userSends, err := service.otpUserSends(ctx, tx, call.flow.UserID)
		if err != nil {
			return challengeResult{}, err
		}
		if userSends >= otpMaxPerUserWindow {
			return challengeResult{}, ErrChallengeRateLimited
		}
		now := service.timestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE flow_id=? AND consumed_at IS NULL`, now, call.flow.ID); err != nil {
			return challengeResult{}, err
		}
		secret, err := service.factorFor(flow.CandidateChannel.String).Secret(ctx)
		if err != nil {
			return challengeResult{}, err
		}
		code, deliveryID, err := insertCandidateChallenge(ctx, tx, key, call.flow, anchor, secret)
		if err != nil {
			return challengeResult{}, err
		}
		var challengeID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_challenges WHERE delivery_id=?`, deliveryID).Scan(&challengeID); err != nil {
			return challengeResult{}, err
		}
		return challengeResult{
			Issued: true, Code: code, DeliveryID: deliveryID, ChallengeID: challengeID,
			Channel: flow.CandidateChannel.String, Recipient: flow.CandidateTarget.String, Purpose: PurposeContactVerification,
			Masked: MaskedContact{Locator: strconv.FormatInt(anchor.ID, 10), Channel: flow.CandidateChannel.String, MaskedTarget: maskTarget(flow.CandidateChannel.String, flow.CandidateTarget.String), Verified: false},
		}, nil
	}, func(result challengeResult) int64 { return call.flow.ID }) // audit target: the flow, success and rejection alike
	if err != nil {
		return MaskedContact{}, 0, mapRejection(err)
	}
	if !result.Issued {
		return result.Masked, result.RetryAfter, ErrChallengeRateLimited
	}
	sendErr := sender.Send(ctx, Message{
		DeliveryID: result.DeliveryID, Channel: result.Channel, Recipient: result.Recipient,
		Template:  "contact_verification",
		Variables: map[string]string{"code": result.Code, "expires_in_seconds": "300"},
	})
	if recordErr := service.recordDeliveryOutcome(ctx, result.ChallengeID, sendErr); recordErr != nil && sendErr == nil {
		return result.Masked, 0, recordErr
	}
	if sendErr != nil {
		return result.Masked, 0, sendErr
	}
	return result.Masked, 0, nil
}

type verifyResult struct {
	Valid     bool
	ContactID int64
}

// rejectionOTPInvalid is the stable Rejection code of a failed verification.
const rejectionOTPInvalid = "otp_invalid"

// evaluateFlowChallenge is the authoritative in-transaction evaluation: it
// consumes the challenge exactly once on success. A failed verification
// increments the accumulated attempt count in the SAME transaction (failure
// counting can never be lost to a crash between two transactions) and returns
// a RecordedFailure so the runner commits that state and audits a FAILURE —
// the audit never pretends a verification succeeded. The presented code
// arrives through the per-call closure, never through context.
func (service *Service) evaluateFlowChallenge(ctx context.Context, tx *execution.Tx, call *stepCall, key []byte, purpose, code string) (verifyResult, error) {
	invalid := func() (verifyResult, error) {
		attempts := call.flow.FailedAttempts + 1
		if attempts >= otpMaxAttempts {
			if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='failed',failed_attempts=? WHERE id=? AND status='pending'`, attempts, call.flow.ID); err != nil {
				return verifyResult{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE flow_id=? AND consumed_at IS NULL`, service.timestamp(), call.flow.ID); err != nil {
				return verifyResult{}, err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET failed_attempts=? WHERE id=? AND status='pending'`, attempts, call.flow.ID); err != nil {
			return verifyResult{}, err
		}
		return verifyResult{}, &execution.RecordedFailure{Code: rejectionOTPInvalid, Detail: "验证码无效", ObjectID: call.flow.ID}
	}
	challenge, digest, err := latestActiveChallenge(ctx, tx, call.flow.ID, purpose)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return invalid()
		}
		return verifyResult{}, err
	}
	contact, usableErr := challengeUsable(ctx, tx, challenge, call.user)
	if usableErr != nil && !errors.Is(usableErr, ErrOtpInvalid) {
		return verifyResult{}, usableErr
	}
	// Factor-change challenges bind the STAGED candidate on the flow; every
	// other purpose binds the live contact identity.
	var computed []byte
	if call.flow.Type == FlowContactChange {
		if !call.flow.CandidateTarget.Valid {
			return invalid()
		}
		computed = candidateChallengeDigest(key, call.flow, challenge.ContactID, challenge.ContactVersion, purpose, code)
	} else {
		computed = challengeDigest(key, call.flow, challenge.ContactID, challenge.ContactVersion, purpose, code)
	}
	if usableErr != nil || !hmac.Equal(digest, computed) {
		return invalid()
	}
	if err := consumeChallenge(ctx, tx, challenge.ID); err != nil {
		return verifyResult{}, err
	}
	if call.flow.Type == FlowContactChange {
		// Success only sets the flow marker: user_contacts stays untouched
		// until the authenticated atomic completion swaps the target.
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET password_set=1 WHERE id=? AND status='pending'`, call.flow.ID); err != nil {
			return verifyResult{}, err
		}
		return verifyResult{Valid: true, ContactID: contact.ID}, nil
	}
	return verifyResult{Valid: true, ContactID: contact.ID}, nil
}

// mapChallengeFailure maps the recorded verification failure to the stable
// sentinel; the attempt count and the failure audit already committed inside
// the verification transaction.
func mapChallengeFailure(err error) error {
	var failure *execution.RecordedFailure
	if errors.As(err, &failure) && failure.Code == rejectionOTPInvalid {
		return ErrOtpInvalid
	}
	return mapRejection(err)
}

// VerifyFlowChallenge verifies the contact-verification code of an
// initialization flow: it marks the target verified server-side
// (auth_flows.verified_contact_id) and creates no session.
func (service *Service) VerifyFlowChallenge(ctx context.Context, bearer, code string) error {
	_, _, _, err := service.verifyInitChallenge(ctx, bearer, code)
	return err
}

// verifyInitChallenge shares the verification with the completion callers.
func (service *Service) verifyInitChallenge(ctx context.Context, bearer, code string) (flowRow, User, int64, error) {
	key, _, configured := service.delivery()
	if !configured {
		return flowRow{}, User{}, 0, ErrFlowDeliveryNotConfigured
	}
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowAdminInitialize, FlowOperatorInitialize, FlowContactChange)
	if err != nil {
		return flowRow{}, User{}, 0, err
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return flowRow{}, User{}, 0, err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	result, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.verifyChallenge, func(tx *execution.Tx) (verifyResult, error) {
		verify, err := service.evaluateFlowChallenge(ctx, tx, call, key, PurposeContactVerification, code)
		if err != nil {
			return verifyResult{}, err
		}
		// Successful contact verification persists its markers in the same
		// transaction: the verified target and the flow reference that the
		// completion step requires.
		now := service.timestamp()
		if _, err := tx.ExecContext(ctx, `UPDATE user_contacts SET verified_at=? WHERE id=? AND verified_at IS NULL`, now, verify.ContactID); err != nil {
			return verifyResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET verified_contact_id=? WHERE id=?`, verify.ContactID, call.flow.ID); err != nil {
			return verifyResult{}, err
		}
		return verify, nil
	}, func(result verifyResult) int64 { return call.flow.ID }) // audit target: the flow (the recorded failure uses the same id)
	if err != nil {
		return flowRow{}, User{}, 0, mapChallengeFailure(err)
	}
	return flow, user, result.ContactID, nil
}

// CompleteAdminInitialization atomically finishes the administrator setup:
// it requires the password step and the verified contact, flips the user to
// initialized, completes the flow and revokes sibling flows — permanently
// closing the default-password entry.
func (service *Service) CompleteAdminInitialization(ctx context.Context, bearer string) error {
	return service.completeInitialization(ctx, bearer, service.ops.completeAdminInit)
}

// CompleteOperatorInitialization finishes the operator setup with the same
// prerequisites.
func (service *Service) CompleteOperatorInitialization(ctx context.Context, bearer string) error {
	return service.completeInitialization(ctx, bearer, service.ops.completeOperatorInit)
}

// CompleteContactChange atomically applies the verified factor change: the
// acting admin session and the current password are re-proven inside the
// transaction, the staged candidate replaces the live target (version bump,
// verified now), and EVERY session of the administrator — including the
// acting one — plus sibling flows and outstanding challenges are revoked.
// The caller must log in again afterwards.
func (service *Service) CompleteContactChange(ctx context.Context, session Session, bearer, currentPassword string) error {
	runCtx, err := service.sessionContext(ctx, session)
	if err != nil {
		return err
	}
	_, err = execution.Execute(withStepCall(withAdminCall(runCtx, session), &stepCall{tokenDigest: flowDigest(bearer), password: currentPassword}), service.runner, service.ops.contactChangeDone, func(tx *execution.Tx) (int64, error) {
		flow, user, err := resolveFlowDigest(ctx, tx, flowDigest(bearer), FlowContactChange)
		if err != nil {
			return 0, err
		}
		if !flow.CandidateTarget.Valid || !flow.PasswordSet {
			return 0, rejection("initialization_incomplete", "联系方式更换步骤未完成", user.ID)
		}
		current, err := findUserByID(ctx, tx, user.ID)
		if err != nil {
			return 0, err
		}
		normalized, normalizeErr := NormalizePassword(currentPassword)
		if normalizeErr != nil || !VerifyPassword(normalized, current.passwordPHC) || !current.Enabled || current.AuthRevision != flow.Revision {
			return 0, ErrUnauthenticated
		}
		now := service.timestamp()
		// Atomic target swap: insert the channel when absent, otherwise
		// replace it; the candidate arrives proven, so verified_at is now.
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_contacts(user_id,channel,target,enabled,version,verified_at,created_at,updated_at)
			VALUES(?,?,?,1,1,?,?,?)
			ON CONFLICT(user_id,channel) DO UPDATE SET target=excluded.target,enabled=1,version=user_contacts.version+1,verified_at=excluded.verified_at,updated_at=excluded.updated_at`,
			user.ID, flow.CandidateChannel.String, flow.CandidateTarget.String, now, now, now); err != nil {
			return 0, err
		}
		// The factor change revokes every session (design §5): the caller
		// returns to the login page and completes the two-step login again.
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, user.ID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='revoked' WHERE user_id=? AND id<>? AND status='pending'`, user.ID, flow.ID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=? WHERE user_id=? AND consumed_at IS NULL`, now, user.ID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='completed',completed_at=? WHERE id=?`, now, flow.ID); err != nil {
			return 0, err
		}
		return user.ID, nil
	}, func(int64) int64 { return session.User.ID }) // audit target: the administrator (the rejection uses the same user id)
	return mapAdminRejection(err)
}

// completeInitialization is the shared atomic finish for initialization-style
// flows (admin, operator and the recovery agent's completion op): it requires
// the password step and the verified contact, flips the user to initialized,
// completes the flow and revokes sibling flows.
func (service *Service) completeInitialization(ctx context.Context, bearer string, op *execution.Operation) error {
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowAdminInitialize, FlowOperatorInitialize)
	if err != nil {
		return err
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	_, err = execution.Execute(withStepCall(runCtx, call), service.runner, op, func(tx *execution.Tx) (int64, error) {
		if !call.flow.PasswordSet || !call.flow.VerifiedContactID.Valid {
			return 0, rejection("initialization_incomplete", "初始化步骤未完成", call.user.ID)
		}
		now := service.timestamp()
		update, err := tx.ExecContext(ctx, `UPDATE users SET initialized=1,row_version=row_version+1,updated_at=? WHERE id=? AND initialized=0`, now, call.user.ID)
		if err != nil {
			return 0, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return 0, ErrFlowInvalid
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='completed',completed_at=? WHERE id=?`, now, call.flow.ID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='revoked' WHERE user_id=? AND id<>? AND status='pending'`, call.user.ID, call.flow.ID); err != nil {
			return 0, err
		}
		return call.user.ID, nil
	}, func(int64) int64 { return call.user.ID })
	return mapRejection(err)
}
