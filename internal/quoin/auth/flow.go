package auth

// Persistent authentication flows (docs/authentication-design.md §2-§4).
// A flow is a short-lived server-side record bound to one user, one purpose
// and the user's current auth_revision. Flow bearers are separate credentials
// from session bearers: they live in auth_flows, are never accepted by
// Authenticate (sessions), and every mutation rechecks the binding inside an
// open transaction so concurrent security changes invalidate in-flight flows.

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

// FlowType is the strict flow classification persisted in auth_flows.flow_type.
type FlowType string

const (
	FlowAdminInitialize    FlowType = "admin_initialize"
	FlowOperatorInitialize FlowType = "operator_initialize"
	FlowLogin              FlowType = "login"
	// FlowContactChange is the administrator's self-service factor change: a
	// password-verified flow that verifies the replacement target before the
	// old one stops being usable.
	FlowContactChange FlowType = "contact_change"
)

// initializeFlowTypes are the flows that require the user to still be
// uninitialized; login flows require an initialized user. Recovery does not
// mint its own flow: `quoin admin recover` only sets a temporary password, and
// the next sign-in classifies into the ordinary admin_initialize flow.
var initializeFlowTypes = map[FlowType]bool{FlowAdminInitialize: true, FlowOperatorInitialize: true}

const (
	// initializeFlowTTL bounds admin/operator initialization flows.
	initializeFlowTTL = 30 * time.Minute
	// loginFlowTTL bounds the second-factor wait after a verified password.
	loginFlowTTL = 15 * time.Minute
	// activityRenewalInterval throttles session activity renewal so background
	// polling cannot renew idle expiry on every request.
	activityRenewalInterval = 30 * time.Minute
	// bootstrapDefaultPassword is accepted only while the built-in admin is
	// uninitialized; it never satisfies the formal password policy.
	bootstrapDefaultPassword = "admin"
)

// Sentinel errors with their intended HTTP classes (app mapping):
//   - 401: ErrUnauthenticated (wrong password/bearer/session)
//   - 422: ErrOtpInvalid, ErrFlowInvalid, ErrFlowExpired, ErrInitializationIncomplete,
//     ErrInitializationRequired, ErrNoContact, ErrValidation, ErrPasswordPolicy
//   - 429: ErrRateLimited / ErrChallengeRateLimited (both carry a retryAfter)
//   - 503: ErrFlowDeliveryNotConfigured and raw Sender errors (infrastructure;
//     delivery failure must never degrade to single-factor login)
var (
	// ErrFlowInvalid marks an unknown, consumed, revoked or rebinding-broken flow.
	ErrFlowInvalid = errors.New("authentication flow is invalid or no longer active")
	// ErrFlowExpired marks a flow past its expires_at; the caller restarts.
	ErrFlowExpired = errors.New("authentication flow has expired")
	// ErrOtpInvalid marks a wrong/expired/unsent verification code (422, not infra).
	ErrOtpInvalid = errors.New("verification code is invalid")
	// ErrInitializationIncomplete marks a completion attempt whose flow has not
	// finished the required steps (password set and/or verified contact).
	ErrInitializationIncomplete = errors.New("initialization steps are not complete")
	// ErrInitializationRequired marks verified credentials that may not log in
	// because the user has not completed initialization.
	ErrInitializationRequired = errors.New("user initialization is required before login")
	// ErrNoContact marks a login for a user without any deliverable contact.
	ErrNoContact = errors.New("user has no contact target for verification")
	// ErrChallengeRateLimited marks the per-flow resend cooldown (429).
	ErrChallengeRateLimited = errors.New("please wait before requesting another verification code")
)

// MaskedContact is the only contact shape ever exposed to clients.
type MaskedContact struct {
	Locator      string `json:"id"`
	Channel      string `json:"channel"`
	MaskedTarget string `json:"maskedTarget"`
	Verified     bool   `json:"verified"`
}

// Flow is the client-visible projection of an auth_flows row. The bearer and
// correlation id are transport-only (json:"-"): the bearer is returned once by
// the Start* calls and never serialized; CorrelationID is an internal-read
// property for the admission resolver to keep audit correlation continuous
// and must never reach the wire.
type Flow struct {
	Bearer        string   `json:"-"`
	CorrelationID string   `json:"-"`
	Type          FlowType `json:"type"`
	User          User     `json:"user"`
	// Contacts always serializes as an array (never omitted): the frontend
	// pane router reads contacts.length, and omitempty would drop the empty
	// list of a just-password-set flow and crash the client.
	Contacts []MaskedContact `json:"contacts"`
	// Candidate is the staged factor-change target while a contact_change
	// flow is pending; it lives on the flow, never in user_contacts, until
	// the verified atomic completion.
	Candidate   *MaskedContact `json:"candidate,omitempty"`
	PasswordSet bool           `json:"passwordSet"`
	// FactorVerified is the server-authoritative per-flow completion marker:
	// true only after THIS flow's own challenge was verified. A retained
	// verified contact (recovery password mode) keeps contacts[].verified
	// true yet never flips this marker — every flow requires a fresh OTP.
	// Pending login flows always read false: login completes atomically into
	// a session, so a readable login flow is always pre-OTP.
	FactorVerified bool   `json:"factorVerified"`
	ExpiresAt      string `json:"expiresAt"`
}

// flowRow is the server-side auth_flows record.
type flowRow struct {
	ID                int64
	Type              FlowType
	UserID            int64
	CorrelationID     string
	Revision          int64
	PasswordSet       bool
	VerifiedContactID sql.NullInt64
	CandidateChannel  sql.NullString
	CandidateTarget   sql.NullString
	ClientLabel       string
	Status            string
	CreatedAt         string
	ExpiresAt         string
	FailedAttempts    int
}

// randomBearer returns a fresh 32-byte credential with its digest. Random
// source failure rejects issuance (no predictable fallback).
func randomBearer() (bearer string, digest []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate bearer: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

const flowColumns = `f.id,f.flow_type,f.user_id,f.correlation_id,f.auth_revision_at_issue,f.password_set,f.verified_contact_id,f.candidate_channel,f.candidate_target,f.client_label,f.status,f.created_at,f.expires_at,f.failed_attempts`

func scanFlow(row *sql.Row) (flowRow, User, error) {
	var flow flowRow
	var user User
	var passwordSet, required, enabled, initialized int
	var lastLogin sql.NullString
	err := row.Scan(&flow.ID, (*string)(&flow.Type), &flow.UserID, &flow.CorrelationID, &flow.Revision, &passwordSet, &flow.VerifiedContactID, &flow.CandidateChannel, &flow.CandidateTarget, &flow.ClientLabel, &flow.Status, &flow.CreatedAt, &flow.ExpiresAt, &flow.FailedAttempts,
		&user.ID, &user.Username, &user.DisplayName, &user.Role, &enabled, &user.AuthRevision, &initialized, &required, &user.RowVersion, &user.passwordPHC, &lastLogin)
	if err != nil {
		return flowRow{}, User{}, err
	}
	flow.PasswordSet = passwordSet == 1
	user.Locator = fmt.Sprint(user.ID)
	user.Enabled = enabled == 1
	user.Initialized = initialized == 1
	user.PasswordChangeRequired = required == 1
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.String
	}
	return flow, user, nil
}

// flowSelect joins the flow with its user projection for binding checks.
const flowSelect = `SELECT ` + flowColumns + `,u.id,u.username,u.display_name,u.role,u.enabled,u.auth_revision,u.initialized,u.password_change_required,u.row_version,u.password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=u.id) FROM auth_flows f JOIN users u ON u.id=f.user_id WHERE f.flow_token_digest=?`

// resolveFlow validates a bearer against one allowed flow type on an open
// transaction (or a plain reader for read-only paths). Every binding rule is
// enforced here: pending status, expiry, enabled user, revision equality and
// the initialization-state invariant of the flow type.
func resolveFlow(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, bearer string, allowed ...FlowType,
) (flowRow, User, error) {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	sum := sha256.Sum256(raw)
	return resolveFlowDigest(ctx, reader, sum[:], allowed...)
}

// resolveFlowDigest validates a flow by its token digest. Every binding rule
// is enforced here: pending status, expiry, enabled user, revision equality
// and the initialization-state invariant of the flow type.
func resolveFlowDigest(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, digest []byte, allowed ...FlowType,
) (flowRow, User, error) {
	if len(digest) != 32 {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	flow, user, err := scanFlow(reader.QueryRowContext(ctx, flowSelect, digest))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return flowRow{}, User{}, ErrFlowInvalid
		}
		return flowRow{}, User{}, err
	}
	allowedType := false
	for _, kind := range allowed {
		if flow.Type == kind {
			allowedType = true
			break
		}
	}
	if !allowedType || flow.Status != "pending" {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	if expires, parseErr := time.Parse(time.RFC3339Nano, flow.ExpiresAt); parseErr != nil || !serviceClock().Before(expires) {
		return flowRow{}, User{}, ErrFlowExpired
	}
	if !user.Enabled || flow.Revision != user.AuthRevision {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	if initializeFlowTypes[flow.Type] && user.Initialized {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	if flow.Type == FlowLogin && !user.Initialized {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	if flow.Type == FlowContactChange && !user.Initialized {
		return flowRow{}, User{}, ErrFlowInvalid
	}
	return flow, user, nil
}

type flowWriter = execution.Executor

// createFlow inserts a pending flow row and returns its record plus bearer.
// Must run inside the caller's transaction together with the user re-check.
func createFlow(ctx context.Context, writer flowWriter, userID int64, flowType FlowType, revision int64, clientLabel string) (flowRow, string, error) {
	bearer, digest, err := randomBearer()
	if err != nil {
		return flowRow{}, "", err
	}
	ttl := initializeFlowTTL
	if flowType == FlowLogin {
		ttl = loginFlowTTL
	}
	if clientLabel == "" {
		clientLabel = "Browser"
	}
	// The flow persists the correlation of the start operation that created
	// it: the start audit event and every child step share one identity.
	meta, err := execution.Require(ctx)
	if err != nil {
		return flowRow{}, "", err
	}
	now := serviceClock().UTC()
	// Login flows follow an already-formal credential by construction: any
	// temporary credential (bootstrap default, admin reset, CLI recovery) is
	// classified into an initialization flow instead. Starting them with
	// password_set=1 keeps the two-step login on the challenge pane; the
	// password step belongs exclusively to initialization-style flows.
	passwordSet := 0
	if flowType == FlowLogin {
		passwordSet = 1
	}
	result, err := writer.ExecContext(ctx, `INSERT INTO auth_flows(flow_type,user_id,flow_token_digest,correlation_id,auth_revision_at_issue,password_set,client_label,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)`,
		string(flowType), userID, digest, meta.CorrelationID, revision, passwordSet, clientLabel, now.Format(time.RFC3339Nano), now.Add(ttl).Format(time.RFC3339Nano))
	if err != nil {
		return flowRow{}, "", fmt.Errorf("persist auth flow: %w", err)
	}
	flowID, err := result.LastInsertId()
	if err != nil {
		return flowRow{}, "", err
	}
	row := writer.QueryRowContext(ctx, `SELECT `+flowColumns+`,u.id,u.username,u.display_name,u.role,u.enabled,u.auth_revision,u.initialized,u.password_change_required,u.row_version,u.password_phc,(SELECT MAX(created_at) FROM sessions WHERE user_id=u.id) FROM auth_flows f JOIN users u ON u.id=f.user_id WHERE f.id=?`, flowID)
	flow, _, err := scanFlow(row)
	if err != nil {
		return flowRow{}, "", err
	}
	return flow, bearer, nil
}

// ReadFlow returns the current projection of a pending flow (refresh-safe
// resume). It never includes the bearer.
func (service *Service) ReadFlow(ctx context.Context, bearer string) (Flow, error) {
	flow, user, err := resolveFlow(ctx, service.read(), bearer, FlowAdminInitialize, FlowOperatorInitialize, FlowLogin, FlowContactChange)
	if err != nil {
		return Flow{}, err
	}
	contacts, err := listMaskedContacts(ctx, service.read(), user.ID)
	if err != nil {
		return Flow{}, err
	}
	projection := Flow{Bearer: "", CorrelationID: flow.CorrelationID, Type: flow.Type, User: user, Contacts: contacts, PasswordSet: flow.PasswordSet, FactorVerified: flow.VerifiedContactID.Valid, ExpiresAt: flow.ExpiresAt}
	if flow.CandidateTarget.Valid {
		projection.Candidate = &MaskedContact{Channel: flow.CandidateChannel.String, MaskedTarget: maskTarget(flow.CandidateChannel.String, flow.CandidateTarget.String), Verified: flow.PasswordSet}
	}
	return projection, nil
}

// sessionActivity carries the touch-relevant facts of one live session row.
type sessionActivity struct {
	sessionID         int64
	userID            int64
	revoked           string
	lastActiveAt      string
	idleExpiresAt     string
	absoluteExpiresAt string
}

const sessionActivitySelect = `SELECT s.id,u.id,COALESCE(s.revoked_at,''),s.last_active_at,s.idle_expires_at,s.absolute_expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.session_token_digest=? AND s.auth_revision_at_issue=u.auth_revision AND u.enabled=1`

// readSessionActivity reads the touch-relevant session facts through any row
// reader. A missing, revoked, revision-stale, disabled or expired session is
// ErrUnauthenticated; a live session returns its facts.
func readSessionActivity(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, digest []byte,
) (sessionActivity, error) {
	var state sessionActivity
	err := reader.QueryRowContext(ctx, sessionActivitySelect, digest).Scan(&state.sessionID, &state.userID, &state.revoked, &state.lastActiveAt, &state.idleExpiresAt, &state.absoluteExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionActivity{}, ErrUnauthenticated
	}
	if err != nil {
		return sessionActivity{}, err
	}
	now := serviceClock()
	absolute, err := time.Parse(time.RFC3339Nano, state.absoluteExpiresAt)
	if err != nil || !now.Before(absolute) {
		return sessionActivity{}, ErrUnauthenticated
	}
	idle, err := time.Parse(time.RFC3339Nano, state.idleExpiresAt)
	if err != nil || !now.Before(idle) {
		return sessionActivity{}, ErrUnauthenticated
	}
	if _, err := time.Parse(time.RFC3339Nano, state.lastActiveAt); err != nil {
		return sessionActivity{}, ErrUnauthenticated
	}
	if state.revoked != "" {
		return sessionActivity{}, ErrUnauthenticated
	}
	return state, nil
}

// due reports whether the renewal throttle window has elapsed.
func (state sessionActivity) due() bool {
	last, err := time.Parse(time.RFC3339Nano, state.lastActiveAt)
	return err == nil && serviceClock().Sub(last) >= activityRenewalInterval
}

// TouchSessionActivity renews a session's activity window under a throttle:
// renewal happens only when the last update is older than
// activityRenewalInterval, and idle expiry never passes the absolute cap.
// The application decides which endpoints count as real activity; SSE
// heartbeats and background polling must not call this.
//
// Audit constraints (ADR-0006): the frequent no-op touches are pure reads —
// the throttle pre-check keeps them off the write path and therefore
// unaudited; only an actual renewal executes as the audited
// auth.session.activity operation, bounding the audit volume to one row per
// session and renewal interval. The runner transaction re-checks every
// session fact so the pre-check is an optimization, never the authority.
func (service *Service) TouchSessionActivity(ctx context.Context, bearer string) error {
	raw, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil || len(raw) != 32 {
		return ErrUnauthenticated
	}
	digest := sha256.Sum256(raw)
	state, err := readSessionActivity(ctx, service.read(), digest[:])
	if err != nil {
		return err
	}
	if !state.due() {
		return nil
	}
	runCtx, err := withExecutionMetadataIfAbsent(ctx, execution.Metadata{
		Actor:  execution.Principal{Kind: execution.PrincipalUser, ID: state.userID},
		Source: execution.Source{Kind: execution.SourceInternal},
	})
	if err != nil {
		return err
	}
	call := &stepCall{userID: state.userID, tokenDigest: digest[:]}
	_, err = execution.Execute(withStepCall(runCtx, call), service.runner, service.ops.touchSessionActivity, func(tx *execution.Tx) (bool, error) {
		// The authoritative re-read decides again: a concurrent touch that
		// renewed first simply loses the race and stays a no-op.
		current, err := readSessionActivity(ctx, tx, call.tokenDigest)
		if err != nil {
			return false, err
		}
		if !current.due() {
			return false, nil
		}
		now := serviceClock()
		renewedIdle := now.Add(12 * time.Hour)
		if absolute, parseErr := time.Parse(time.RFC3339Nano, current.absoluteExpiresAt); parseErr == nil && renewedIdle.After(absolute) {
			renewedIdle = absolute
		}
		update, err := tx.ExecContext(ctx, `UPDATE sessions SET last_active_at=?,idle_expires_at=? WHERE id=? AND revoked_at IS NULL`, now.Format(time.RFC3339Nano), renewedIdle.Format(time.RFC3339Nano), current.sessionID)
		if err != nil {
			return false, err
		}
		if rows, _ := update.RowsAffected(); rows != 1 {
			return false, ErrUnauthenticated
		}
		return true, nil
	}, func(bool) int64 { return state.sessionID }) // audit target: the touched sessions row itself
	return err
}

// serviceClock is a tiny indirection so resolveFlow stays package-level.
var clockNow = func() time.Time { return time.Now() }

func serviceClock() time.Time { return clockNow().UTC().Round(0) }
