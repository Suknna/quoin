package auth

// The two-step login and the unified start entry (docs/authentication-design.md
// §2-§4). A verified password never creates a session: it starts a persistent
// login flow, the second factor is chosen among admin-assigned contacts, and
// only the atomically consumed challenge creates the full session.
// StartAuthentication classifies the verified credentials into exactly one
// flow — pending admin initialization, operator initialization, or login —
// with a single password verification and one shared rate-limit budget.
// The password is verified in the caller's scope; the transaction's
// authorization callback re-checks the stored PHC hash without ever seeing
// the raw secret.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// findUserByNameOn reads a user by username through any row reader.
func findUserByNameOn(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, username string,
) (User, error) {
	row := reader.QueryRowContext(ctx, `SELECT id,username,display_name,role,enabled,auth_revision,initialized,row_version,password_change_required,password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=users.id) FROM users WHERE username=?`, username)
	return scanUserRowValues(row)
}

// scanUserRowValues shares the positional users projection (service.scanUser
// keeps its own stable order for the legacy queries).
func scanUserRowValues(row *sql.Row) (User, error) {
	var user User
	var enabled, initialized, required int
	var lastLogin sql.NullString
	if err := row.Scan(&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &initialized, &user.RowVersion, &required, &user.passwordPHC, &lastLogin); err != nil {
		return User{}, err
	}
	user.Locator = fmt.Sprint(user.ID)
	user.Enabled = enabled == 1
	user.Initialized = initialized == 1
	user.PasswordChangeRequired = required == 1
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.String
	}
	return user, nil
}

// classifyUser reads the non-secret routing facts for the start entries.
type userClassification struct {
	ID          int64
	Role        string
	Enabled     bool
	Initialized bool
	HasContact  bool
}

func (service *Service) classifyUser(ctx context.Context, username string) (userClassification, error) {
	var classification userClassification
	var contactCount int
	err := service.read().QueryRowContext(ctx, `SELECT u.id,u.role,u.enabled,u.initialized,(SELECT COUNT(*) FROM user_contacts c WHERE c.user_id=u.id) FROM users u WHERE u.username=?`, username).Scan(&classification.ID, &classification.Role, &classification.Enabled, &classification.Initialized, &contactCount)
	if err != nil {
		return userClassification{}, err
	}
	classification.HasContact = contactCount > 0
	return classification, nil
}

// StartAuthentication verifies the presented credential exactly once and
// starts the one flow the user may currently run:
//   - uninitialized built-in administrator -> admin_initialize directly (the
//     deployment access boundary is owned by the intranet environment),
//   - any other uninitialized user -> operator_initialize,
//   - initialized users -> login (second factor; requires a contact target).
func (service *Service) StartAuthentication(ctx context.Context, username, password, userAgent string) (Flow, time.Duration, error) {
	normalized := NormalizeUsername(username)
	if allowed, retryAfter := service.limiter.allow(normalized); !allowed {
		service.passwords.VerifyDummy(password)
		return Flow{}, retryAfter, ErrRateLimited
	}
	classification, err := service.classifyUser(ctx, normalized)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			service.passwords.VerifyDummy(password)
			service.limiter.failed(normalized)
			return Flow{}, 0, ErrUnauthenticated
		}
		return Flow{}, 0, err
	}
	class := startClassLogin
	op := service.ops.loginStart
	if !classification.Initialized {
		if classification.Role == "admin" {
			class, op = startClassAdmin, service.ops.adminInitStart
		} else {
			class, op = startClassOperator, service.ops.operatorInitStart
		}
	}
	flow, err := service.executeFlowStart(ctx, op, class, normalized, password, userAgent)
	return flow, 0, err
}

// StartContactChange starts the administrator's self-service factor change.
// It requires the fully authenticated session (its validity is re-checked
// inside the transaction) plus the current password proof — a stolen
// password alone can never take over the second factor. The replacement
// target is staged on the flow and only becomes live at the verified
// completion.
func (service *Service) StartContactChange(ctx context.Context, session Session, currentPassword string) (Flow, time.Duration, error) {
	normalized := NormalizeUsername(session.User.Username)
	if allowed, retryAfter := service.limiter.allow(normalized); !allowed {
		service.passwords.VerifyDummy(currentPassword)
		return Flow{}, retryAfter, ErrRateLimited
	}
	runCtx, err := service.sessionContext(ctx, session)
	if err != nil {
		return Flow{}, 0, err
	}
	call := &stepCall{
		userID: session.User.ID, username: normalized, passwordPHC: session.User.passwordPHC,
		password: currentPassword,
	}
	result, err := execution.Execute(withStepCall(withAdminCall(runCtx, session), call), service.runner, service.ops.contactChangeStart, func(tx *execution.Tx) (startResult, error) {
		user, err := findUserByID(runCtx, tx, session.User.ID)
		if err != nil {
			return startResult{}, err
		}
		normalizedCandidate, normalizeErr := NormalizePassword(currentPassword)
		if normalizeErr != nil || !VerifyPassword(normalizedCandidate, user.passwordPHC) || !user.Enabled {
			return startResult{}, ErrUnauthenticated
		}
		if user.Role != "admin" || !user.Initialized {
			return startResult{}, ErrInitializeRejected
		}
		contacts, err := listMaskedContacts(runCtx, tx, user.ID)
		if err != nil {
			return startResult{}, err
		}
		if len(contacts) == 0 {
			return startResult{}, ErrNoContact
		}
		flow, bearer, flowErr := createFlow(runCtx, tx, user.ID, FlowContactChange, user.AuthRevision, "Browser")
		if flowErr != nil {
			return startResult{}, flowErr
		}
		// A single active factor change per administrator.
		if _, err := tx.ExecContext(runCtx, `UPDATE auth_flows SET status='revoked' WHERE user_id=? AND id<>? AND flow_type='contact_change' AND status='pending'`, user.ID, flow.ID); err != nil {
			return startResult{}, err
		}
		return startResult{Flow: flow, Bearer: bearer, User: user}, nil
	}, func(result startResult) int64 { return result.Flow.UserID })
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			service.limiter.failed(normalized)
		}
		return Flow{}, 0, mapAdminRejection(err)
	}
	service.limiter.succeeded(normalized)
	contacts, err := listMaskedContacts(ctx, service.read(), result.Flow.UserID)
	if err != nil {
		return Flow{}, 0, err
	}
	return Flow{
		Bearer: result.Bearer, CorrelationID: result.Flow.CorrelationID, Type: result.Flow.Type,
		User: result.User, Contacts: contacts, PasswordSet: result.Flow.PasswordSet, FactorVerified: result.Flow.VerifiedContactID.Valid, ExpiresAt: result.Flow.ExpiresAt,
	}, 0, nil
}

// StartLogin starts the second-factor login flow for an initialized user.
// Verified credentials of an uninitialized user report
// ErrInitializationRequired so callers can fall back to StartAuthentication.
func (service *Service) StartLogin(ctx context.Context, username, password, userAgent string) (Flow, time.Duration, error) {
	return service.startOfClass(ctx, startClassLogin, username, password, userAgent)
}

// StartAdminInitialization starts the administrator initialization flow: the
// pending built-in administrator signs in with the public default credential.
func (service *Service) StartAdminInitialization(ctx context.Context, username, password string) (Flow, time.Duration, error) {
	return service.startOfClass(ctx, startClassAdmin, username, password, "")
}

// StartOperatorInitialization starts the operator initialization flow with
// the temporary credential issued at account creation.
func (service *Service) StartOperatorInitialization(ctx context.Context, username, tempPassword string) (Flow, time.Duration, error) {
	return service.startOfClass(ctx, startClassOperator, username, tempPassword, "")
}

func (service *Service) startOfClass(ctx context.Context, class startClass, username, password, userAgent string) (Flow, time.Duration, error) {
	normalized := NormalizeUsername(username)
	if allowed, retryAfter := service.limiter.allow(normalized); !allowed {
		service.passwords.VerifyDummy(password)
		return Flow{}, retryAfter, ErrRateLimited
	}
	if _, err := service.classifyUser(ctx, normalized); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Flow{}, 0, err
	}
	op := service.ops.loginStart
	switch class {
	case startClassAdmin:
		op = service.ops.adminInitStart
	case startClassOperator:
		op = service.ops.operatorInitStart
	case startClassContactChange:
		op = service.ops.contactChangeStart
	}
	flow, err := service.executeFlowStart(ctx, op, class, normalized, password, userAgent)
	return flow, 0, err
}

// executeFlowStart verifies the credential in the caller's scope (single
// verification), then runs the flow creation through the runner whose
// authorization callback re-checks the stored PHC hash and the class state
// inside the transaction. The flow row and its audit event commit atomically.
func (service *Service) executeFlowStart(ctx context.Context, op *execution.Operation, class startClass, normalizedUsername, password, userAgent string) (Flow, error) {
	user, err := findUserByNameOn(ctx, service.read(), normalizedUsername)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			service.passwords.VerifyDummy(password)
			service.limiter.failed(normalizedUsername)
			return Flow{}, ErrUnauthenticated
		}
		return Flow{}, err
	}
	candidate, normalizeErr := NormalizePassword(password)
	if normalizeErr != nil && password == bootstrapDefaultPassword {
		// The public default password only ever verifies against the pending
		// bootstrap administrator; it never satisfies the formal policy.
		candidate = password
	}
	if !VerifyPassword(candidate, user.passwordPHC) || !user.Enabled {
		service.limiter.failed(normalizedUsername)
		return Flow{}, ErrUnauthenticated
	}
	credentialID := int64(0)
	if class == startClassAdmin {
		if user.Role != "admin" || user.Initialized {
			return Flow{}, ErrInitializeRejected
		}
	}
	if class == startClassOperator && (user.Role != "operator" || user.Initialized) {
		return Flow{}, ErrInitializeRejected
	}
	if class == startClassLogin && !user.Initialized {
		return Flow{}, ErrInitializationRequired
	}
	if class == startClassContactChange && (user.Role != "admin" || !user.Initialized) {
		return Flow{}, ErrInitializeRejected
	}
	if class == startClassContactChange {
		contacts, contactErr := listMaskedContacts(ctx, service.read(), user.ID)
		if contactErr != nil {
			return Flow{}, contactErr
		}
		if len(contacts) == 0 {
			return Flow{}, ErrNoContact
		}
	}
	// Durable backstop: a user who already burned two fully failed flows
	// (5 wrong codes each) inside the window cannot keep minting flows even
	// across process restarts; the in-memory limiter sits in front of it.
	if class != startClassAdmin {
		failedFlows, err := service.otpFailedFlows(ctx, service.read(), user.ID)
		if err != nil {
			return Flow{}, err
		}
		if failedFlows >= otpFailedFlowsPerUserWindow {
			return Flow{}, ErrChallengeRateLimited
		}
	}
	if class == startClassLogin {
		// No deliverable target exists: fail before creating any flow. This
		// is a deployment configuration gap, not an authentication fallback.
		contacts, contactErr := listMaskedContacts(ctx, service.read(), user.ID)
		if contactErr != nil {
			return Flow{}, contactErr
		}
		if len(contacts) == 0 {
			return Flow{}, ErrNoContact
		}
	}
	call := &stepCall{
		userID: user.ID, username: user.Username, passwordPHC: user.passwordPHC,
		credentialID: credentialID, userAgent: userAgent,
	}
	meta, exists := execution.FromContext(ctx)
	if !exists {
		correlation, corrErr := execution.NewCorrelationID()
		if corrErr != nil {
			return Flow{}, corrErr
		}
		meta = execution.Metadata{
			CorrelationID: correlation,
			Actor:         execution.Principal{Kind: execution.PrincipalUser, ID: user.ID},
			Source:        execution.Source{Kind: execution.SourceInternal},
		}
		attached, attachErr := execution.WithMetadata(ctx, meta)
		if attachErr != nil {
			return Flow{}, attachErr
		}
		ctx = attached
	}
	startCtx := ctx
	result, err := execution.Execute(withStepCall(startCtx, call), service.runner, op, func(tx *execution.Tx) (startResult, error) {
		flow, bearer, flowErr := createFlow(startCtx, tx, call.user.ID, FlowType(class), call.user.AuthRevision, clientLabel(call.userAgent))
		if flowErr != nil {
			return startResult{}, flowErr
		}
		if class != startClassLogin {
			// Only one active initialization flow per user: a new start
			// revokes the pending ones, so superseded bearers die immediately
			// and concurrent initializations have at most one live winner.
			if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET status='revoked' WHERE user_id=? AND id<>? AND flow_type IN ('admin_initialize','operator_initialize','contact_change') AND status='pending'`, call.user.ID, flow.ID); err != nil {
				return startResult{}, err
			}
		}
		return startResult{Flow: flow, Bearer: bearer, User: call.user}, nil
	}, func(result startResult) int64 { return result.Flow.UserID })
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			// A credential rotated mid-flight consumes the shared per-username
			// failure budget.
			service.limiter.failed(normalizedUsername)
		}
		return Flow{}, err
	}
	service.limiter.succeeded(normalizedUsername)
	contacts, err := listMaskedContacts(ctx, service.read(), result.Flow.UserID)
	if err != nil {
		return Flow{}, err
	}
	return Flow{
		Bearer: result.Bearer, CorrelationID: result.Flow.CorrelationID, Type: result.Flow.Type,
		User: result.User, Contacts: contacts, PasswordSet: result.Flow.PasswordSet, FactorVerified: result.Flow.VerifiedContactID.Valid, ExpiresAt: result.Flow.ExpiresAt,
	}, nil
}

// otpUserKey is the in-memory limiter key of one user's verification
// failures (cross-flow; restarts reset it, the durable failed-flow gate
// above does not).
func otpUserKey(userID int64) string { return strconv.FormatInt(userID, 10) }

type loginCompleteResult struct {
	Bearer  string
	UserID  int64
	Contact int64
}

// CompleteLogin consumes the second-factor challenge atomically and issues
// the full session. The flow bearer alone can never resolve to a session and
// the session resolver never reads auth_flows. The presented code is captured
// by this per-call closure; the runner records a rejected audit event when it
// is wrong.
func (service *Service) CompleteLogin(ctx context.Context, bearer, code string) (LoginResult, error) {
	key, _, configured := service.delivery()
	if !configured {
		return LoginResult{}, ErrFlowDeliveryNotConfigured
	}
	flow, user, err := service.preresolveFlow(ctx, bearer, FlowLogin)
	if err != nil {
		return LoginResult{}, err
	}
	if allowed, retryAfter := service.otpFailures.allow(otpUserKey(user.ID)); !allowed {
		return LoginResult{}, fmt.Errorf("%w: retry after %s", ErrChallengeRateLimited, retryAfter)
	}
	runCtx, err := flowCallContext(ctx, flow, user.ID)
	if err != nil {
		return LoginResult{}, err
	}
	call := &stepCall{flowID: flow.ID, tokenDigest: flowDigest(bearer)}
	result, err := execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.completeLogin, func(tx *execution.Tx) (loginCompleteResult, error) {
		verify, err := service.evaluateFlowChallenge(ctx, tx, call, key, PurposeSecondFactor, code)
		if err != nil {
			return loginCompleteResult{}, err
		}
		now := service.timestamp()
		// Proving receipt of the code at the assigned target also verifies a
		// contact the administrator rotated after initialization.
		if _, err := tx.ExecContext(ctx, `UPDATE user_contacts SET verified_at=? WHERE id=? AND verified_at IS NULL`, now, verify.ContactID); err != nil {
			return loginCompleteResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_flows SET verified_contact_id=?,status='completed',completed_at=? WHERE id=?`, verify.ContactID, now, call.flow.ID); err != nil {
			return loginCompleteResult{}, err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return loginCompleteResult{}, fmt.Errorf("create session: %w", err)
		}
		digest := sha256.Sum256(raw)
		nowTime := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?,?)`,
			call.user.ID, digest[:], call.flow.Revision, call.flow.ClientLabel, now, now, nowTime.Add(12*time.Hour).Format(time.RFC3339Nano), nowTime.Add(7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
			return loginCompleteResult{}, fmt.Errorf("persist session: %w", err)
		}
		return loginCompleteResult{Bearer: base64.RawURLEncoding.EncodeToString(raw), UserID: call.user.ID, Contact: verify.ContactID}, nil
	}, func(result loginCompleteResult) int64 { return call.flow.ID })
	if err != nil {
		if mapped := mapChallengeFailure(err); errors.Is(mapped, ErrOtpInvalid) {
			service.otpFailures.failed(otpUserKey(user.ID))
			return LoginResult{}, mapped
		}
		return LoginResult{}, mapChallengeFailure(err)
	}
	service.otpFailures.succeeded(otpUserKey(user.ID))
	sessionUser, err := findUserByID(ctx, service.read(), result.UserID)
	if err != nil {
		return LoginResult{}, err
	}
	now := service.timestamp()
	sessionUser.LastLoginAt = &now
	return LoginResult{Bearer: result.Bearer, User: sessionUser}, nil
}
