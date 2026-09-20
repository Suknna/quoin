// Package attempt owns the plinth agent attempt lifecycle on the Quoin
// side: the execution_attempts row state machine, dispatch binding against
// a live Plinth stream, fenced result commit, cancellation and the agent
// model-call/tool-call ledger (DATA-ATTEMPT-001..006, ARCH-TOOL-001..004).
//
// The package is the only product write path for these rows; the HTTP
// surface and the runtime control stream both call through here so commit
// order is decided by SQLite transactions, never by callers.
package attempt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// DispatchLease is the finite lease window every dispatched attempt
// carries and every heartbeat/reconcile renewal extends (RUNTIME-TASK-002/
// 007). One frozen release-internal constant owns the value for probes and
// agent attempts alike (RUNTIME-SCOPE-004): with the 10s heartbeat cadence
// it bounds the post-disconnect interruption latency at two minutes while
// tolerating a same-boot reconnect window of the same length.
const DispatchLease = 2 * time.Minute

// SweepInterval is the periodic lease-sweep cadence (RUNTIME-SCOPE-004:
// frozen release-internal constant, not a deployment input).
const SweepInterval = 5 * time.Second

// SetReleaseVersion feeds the quoin release string every dispatched
// attempt must freeze (DATA-ATTEMPT-001).
func SetReleaseVersion(version string) { releaseVersion = version }

var releaseVersion = "dev"

// RowVersionError reports a stale expected_row_version fence miss.
type RowVersionError struct {
	ID      int64
	Current int64
}

func (err *RowVersionError) Error() string {
	return fmt.Sprintf("attempt %d row version is %d", err.ID, err.Current)
}

// ErrLateResult reports a result proposal that lost the commit-order race
// against cancellation or another terminal transition (DATA-TX-005).
var ErrLateResult = errors.New("attempt is not running; late result rejected")

// Service is the attempt state-machine authority.
type Service struct {
	db  *sql.DB
	now func() time.Time
	// Catalogs is the boot-frozen per-generation tool catalog source
	// (ADR-0004), assembled once by the wiring layer from the plugin
	// registry and the resolved enablement; creation freezes its documents.
	Catalogs *Catalogs
	// SnapshotRebuilder rebuilds the canonical input bytes of one attempt
	// from its durable item references; the scope domain wires it (the
	// snapshot row stores only the digest).
	SnapshotRebuilder func(ctx context.Context, attemptID int64) ([]byte, error)
	// ToolResultGrants writes the attempt-scoped read grant for a sealed
	// tool_result artifact inside CompleteToolCall's transaction (the
	// frozen closure requires the tool call to be succeeded first). Nil
	// skips the grant (probes never produce artifacts).
	ToolResultGrants func(ctx context.Context, conn execution.Executor, attemptID, artifactID, toolCallID int64) error
	// ToolGrantResolver freezes connection bindings inside CompleteModelCall's
	// transaction. A deterministic domain-routing miss is returned as a
	// preflight result, not an infrastructure failure, so the model can ask a
	// human clarification without any credential ever leaving Quoin.
	ToolGrantResolver func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) (ToolResolution, error)
	// ToolGrantValidator re-checks the frozen binding before a pending
	// observation tool may begin executing (DATA-CONN-002). Nil skips the
	// check (tools without grants).
	ToolGrantValidator func(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, tool ToolDef) error
	// EvidenceWriter commits the deterministic Evidence of one succeeded
	// observation tool inside CompleteToolCall's transaction, while the
	// tool call is still running (ARCH-TOOL-003, DATA-EVIDENCE-001). Nil
	// skips evidence (non-observation tools).
	EvidenceWriter func(ctx context.Context, conn execution.Executor, attemptID, toolCallID, artifactID int64, payloadJSON []byte, toolName string) ([]int64, error)
	// runner owns the transactions of the standalone lifecycle stages and
	// persists their automatic audit rows (ADR-0006). The operation pointers
	// are the registered declarations; registration happens at construction.
	runner              *execution.Runner
	opModelCallBegin    *execution.Operation
	opModelCallComplete *execution.Operation
	opToolCallBegin     *execution.Operation
	opToolCallComplete  *execution.Operation
	opCancelFence       *execution.Operation
	opInterrupt         *execution.Operation
	opLeaseSweep        *execution.Operation
	opToolCallCancel    *execution.Operation
	opDispatchBind      *execution.Operation
	opDispatchAccept    *execution.Operation
	opResultCommit      *execution.Operation
	opCancelAck         *execution.Operation
}

// ReleaseVersion returns the quoin release string dispatched attempts
// freeze (DATA-ATTEMPT-001).
func ReleaseVersion() string { return releaseVersion }

// NewService builds the attempt service on the product database.
func NewService(db *sql.DB) *Service {
	now := func() time.Time { return time.Now().UTC() }
	service := &Service{db: db, now: now, Catalogs: DefaultCatalogs()}
	service.runner = execution.NewRunnerWithClock(db, execution.NewRegistry(), audit.NewWriterWithClock(now), now)
	service.registerOperations()
	return service
}

func (service *Service) SetReader(reader audit.Reader) error {
	return service.runner.SetReader(reader)
}

func (service *Service) Reader() audit.Reader {
	return service.runner.Reader()
}

// DefaultCatalogs is the unwired-wiring fallback: the process default
// plugin registry (plugins.Default, ADR-0011 blank-import assembly) under
// its default enablement, assembled through the ONE BuildCatalogs path.
// The application wiring replaces it with a registry-built set resolved
// from the deployment configuration; tests and minimal hosts get whatever
// the process registered (hosts wanting the builtin mainline blank-import
// internal/plugins/builtin).
func DefaultCatalogs() *Catalogs {
	registry := plugins.Default()
	enabled, err := registry.ResolveEnabled(nil)
	if err != nil {
		panic("default plugin enablement must always resolve: " + err.Error())
	}
	catalogs, err := BuildCatalogs(registry, enabled)
	if err != nil {
		panic("default catalogs must always build: " + err.Error())
	}
	return catalogs
}

// nowText formats the service clock for SQLite timestamps.
func (service *Service) nowText() string {
	return service.now().Format(time.RFC3339Nano)
}

// View is the read projection of one attempt row.
type View struct {
	ID                int64
	AttemptType       string
	ScopeType         string
	ScopeID           int64
	State             string
	RowVersion        int64
	RuntimeSlot       *string
	BootID            *string
	ConnectionEpoch   *int64
	LeaseUntil        *string
	StartedAt         *string
	EndedAt           *string
	TerminationReason *string
	CreatedAt         string
}

// Get returns one attempt row.
func (service *Service) Get(ctx context.Context, attemptID int64) (View, error) {
	var view View
	var slot, boot sql.NullString
	var epoch sql.NullInt64
	var started, ended, reason sql.NullString
	err := service.Reader().QueryRowContext(ctx, `
		SELECT id, attempt_type, scope_type, scope_id, state, row_version, runtime_slot,
		       boot_id, connection_epoch, started_at, ended_at, termination_reason, created_at
		FROM execution_attempts WHERE id=?`, attemptID).
		Scan(&view.ID, &view.AttemptType, &view.ScopeType, &view.ScopeID, &view.State, &view.RowVersion,
			&slot, &boot, &epoch, &started, &ended, &reason, &view.CreatedAt)
	if err != nil {
		return View{}, err
	}
	if slot.Valid {
		view.RuntimeSlot = &slot.String
	}
	if boot.Valid {
		view.BootID = &boot.String
	}
	if epoch.Valid {
		view.ConnectionEpoch = &epoch.Int64
	}
	if started.Valid {
		view.StartedAt = &started.String
	}
	if ended.Valid {
		view.EndedAt = &ended.String
	}
	if reason.Valid {
		view.TerminationReason = &reason.String
	}
	return view, nil
}

// BindToStream moves one Queued attempt to Assigned against the live Plinth
// binding (RUNTIME-TASK-001/002). The row-version increment and the WHERE
// fence happen in one UPDATE statement (DATA-ATTEMPT-006).
func (service *Service) BindToStream(ctx context.Context, attemptID int64, bootID string, epoch uint64, lease time.Duration, peerReleaseVersion ...string) error {
	return service.BindToSlot(ctx, attemptID, "plinth", bootID, epoch, lease, peerReleaseVersion...)
}

// BindToSlot is the slot-parameterized dispatch binding: config verification
// browser children bind to lintel (CFG-VERIFYRUN-002), every other caller
// keeps the Plinth supervisor binding.
func (service *Service) BindToSlot(ctx context.Context, attemptID int64, slot, bootID string, epoch uint64, lease time.Duration, peerReleaseVersion ...string) error {
	version := releaseVersion
	if len(peerReleaseVersion) > 0 && peerReleaseVersion[0] != "" {
		version = peerReleaseVersion[0]
	}
	return service.executeDispatch(ctx, service.opDispatchBind, attemptID,
		func(tx *execution.Tx) error {
			result, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Assigned', runtime_slot=?, boot_id=?, connection_epoch=?,
		    lease_until=?, runtime_release_version=?, row_version=row_version+1
		WHERE id=? AND state='Queued'`, slot, bootID, epoch, service.now().Add(lease).Format(time.RFC3339Nano), version, attemptID)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return fmt.Errorf("attempt %d is not Queued; dispatch binding refused", attemptID)
			}
			return nil
		})
}

// executeDispatch runs one fenced dispatch/lifecycle UPDATE through the
// shared execution runner under the attempt's persisted-correlation machine
// scope; the runner records the automatic audit row atomically with it.
func (service *Service) executeDispatch(ctx context.Context, op *execution.Operation, attemptID int64, stage func(tx *execution.Tx) error) error {
	authority, err := service.lifecycleAuthority(ctx, attemptID)
	if err != nil {
		return err
	}
	_, err = execution.Execute(authority, service.runner, op,
		func(tx *execution.Tx) (struct{}, error) {
			return struct{}{}, stage(tx)
		},
		func(struct{}) int64 { return attemptID })
	return err
}

// Accept moves an Assigned attempt to Running and records accepted_at
// (RUNTIME-TASK-004). The fence matches the dispatch boot: the frozen
// schema makes the binding epoch immutable once set, so an Assigned
// attempt that was idempotently re-dispatched after a same-boot reconnect
// (RUNTIME-TASK-005) still accepts on the newer epoch — the inbound
// envelope fence already proved the frame arrived on the current stream.
// A different boot can never accept (new-boot attempts interrupt first).
func (service *Service) Accept(ctx context.Context, attemptID int64, bootID string, epoch uint64) error {
	_ = epoch // transport context only; see the fence note above
	return service.executeDispatch(ctx, service.opDispatchAccept, attemptID,
		func(tx *execution.Tx) error {
			result, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Running', accepted_at=?, started_at=?, row_version=row_version+1
		WHERE id=? AND state='Assigned' AND boot_id=?`,
				service.nowText(), service.nowText(), attemptID, bootID)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return fmt.Errorf("attempt %d acceptance refused (not Assigned or binding mismatch)", attemptID)
			}
			return nil
		})
}

// CommitResult seals one running attempt as Succeeded or Failed. The fence
// re-checks state='Running' and the boot/epoch binding inside the UPDATE so
// a cancellation committed earlier wins by SQLite commit order
// (DATA-TX-005, RUNTIME-CANCEL-002).
func (service *Service) CommitResult(ctx context.Context, attemptID int64, bootID string, epoch uint64, succeeded bool, terminationReason string) error {
	return service.executeDispatch(ctx, service.opResultCommit, attemptID,
		func(tx *execution.Tx) error {
			return service.commitResultOn(ctx, tx, attemptID, bootID, epoch, succeeded, terminationReason)
		})
}

// commitResultOn is the fenced terminal UPDATE shared by CommitResult and
// the transaction-composable CommitResultOn.
func (service *Service) commitResultOn(ctx context.Context, db execution.Executor, attemptID int64, bootID string, epoch uint64, succeeded bool, terminationReason string) error {
	if succeeded {
		terminationReason = ""
	}
	if !succeeded && terminationReason == "" {
		terminationReason = "worker_protocol_error"
	}
	state := "Failed"
	if succeeded {
		state = "Succeeded"
	}
	var nullableTermination any
	if terminationReason != "" {
		nullableTermination = terminationReason
	}
	result, err := db.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state=?, ended_at=?, termination_reason=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
		state, service.nowText(), nullableTermination, attemptID, bootID, epoch)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrLateResult
	}
	return nil
}

// CommitResultOn is CommitResult's transaction-composable form. Scope
// services use it when their typed result, Evidence and parent lifecycle must
// commit atomically with the fenced Attempt terminal transition.
func (service *Service) CommitResultOn(ctx context.Context, db execution.Executor, attemptID int64, bootID string, epoch uint64, succeeded bool, terminationReason string) error {
	if succeeded {
		terminationReason = ""
	}
	if !succeeded && terminationReason == "" {
		terminationReason = "worker_protocol_error"
	}
	state := "Failed"
	if succeeded {
		state = "Succeeded"
	}
	var nullableTermination any
	if terminationReason != "" {
		nullableTermination = terminationReason
	}
	result, err := db.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state=?, ended_at=?, termination_reason=?, row_version=row_version+1
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`,
		state, service.nowText(), nullableTermination, attemptID, bootID, epoch)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrLateResult
	}
	return nil
}

// CancelFence commits the idempotent cancellation fence (DATA-ATTEMPT-003):
// Queued closes as Cancelled directly; Assigned/Running close to Cancelling
// because an Assigned DispatchAttempt may already be in flight (the runtime's
// CancelAck finishes it). Terminal attempts return their
// state unchanged so the caller can answer "already completed" instead of a
// conflict (HTTP-COMMAND-005).
//
// The standalone stage runs through the shared execution runner (ADR-0006):
// the runner owns the transaction and records the automatic audit fact on it,
// attributed to the system runtime authority on the attempt's persisted
// association. A terminal or already-Cancelling no-op changed nothing and
// records nothing: the in-transaction state check returns execution.ErrNoTransition,
// which the runner commits without an audit row; the caller answers from a
// fresh read.
func (service *Service) CancelFence(ctx context.Context, attemptID int64) (state string, err error) {
	authority, err := service.lifecycleAuthority(ctx, attemptID)
	if err != nil {
		return "", err
	}
	if _, err := execution.Execute(authority, service.runner, service.opCancelFence,
		func(tx *execution.Tx) (struct{}, error) {
			var before string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&before); err != nil {
				return struct{}{}, err
			}
			switch before {
			case "Succeeded", "Failed", "Cancelled", "Interrupted", "Cancelling":
				// Terminal no-op, or the attempt is already Cancelling: the
				// fence's UPDATE is fenced to Assigned/Running and Queued, so
				// it changed nothing and records nothing.
				return struct{}{}, fmt.Errorf("%w: attempt %d is %s", execution.ErrNoTransition, attemptID, before)
			}
			_, err := service.CancelFenceOn(authority, tx, attemptID)
			return struct{}{}, err
		},
		func(struct{}) int64 { return attemptID }); err != nil {
		if missed, state := service.noOpState(ctx, attemptID, err); missed {
			return state, nil
		}
		return "", err
	}
	return service.currentState(ctx, attemptID)
}

// noOpState resolves the ErrNoTransition contract of the lifecycle stages:
// when err wraps execution.ErrNoTransition the attempt's authoritative state
// is re-read and returned as the stage's answer. The first return value
// reports whether err was a missed transition; the second is the re-read
// state ("" when it was not).
func (service *Service) noOpState(ctx context.Context, attemptID int64, err error) (bool, string) {
	if !errors.Is(err, execution.ErrNoTransition) {
		return false, ""
	}
	state, readErr := service.currentState(ctx, attemptID)
	if readErr != nil {
		return true, ""
	}
	return true, state
}

// currentState reads the authoritative attempt state.
func (service *Service) currentState(ctx context.Context, attemptID int64) (string, error) {
	var state string
	err := service.Reader().QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state)
	return state, err
}

// CancelFenceOn is the conn-scoped variant of CancelFence: it runs the
// same state machine on the caller's transaction (scope services compose
// it with their own domain updates; SQLite single-writer forbids a nested
// BEGIN).
func (service *Service) CancelFenceOn(ctx context.Context, db execution.Executor, attemptID int64) (string, error) {
	var state string
	if err := db.QueryRowContext(ctx, `SELECT state FROM execution_attempts WHERE id=?`, attemptID).Scan(&state); err != nil {
		return "", err
	}
	switch state {
	case "Succeeded", "Failed", "Cancelled", "Interrupted":
		return state, nil
	case "Queued":
		result, err := db.ExecContext(ctx, `
			UPDATE execution_attempts
			SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
			WHERE id=? AND state=?`, service.nowText(), attemptID, state)
		if err != nil {
			return "", err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return "", fmt.Errorf("attempt %d cancellation fence lost the race", attemptID)
		}
		return "Cancelled", nil
	case "Assigned", "Running", "Cancelling":
		// Assigned is a dispatch-commit state, not proof that the runtime has
		// not started. A frame can be in flight (or Accepted can be delayed),
		// so it must receive the same durable cancellation and replay treatment
		// as Running rather than being locally declared stopped.
		// A terminal claim only authorizes the upload attempt. It is not a parent
		// terminal fact: cancellation remains authoritative until the ActionResult
		// commits in this same SQLite serialization domain. This prevents a staged
		// complete artifact from making a prior parent cancellation inexpressible.
		if _, err := db.ExecContext(ctx, `
			UPDATE execution_attempts SET state='Cancelling', row_version=row_version+1
			WHERE id=? AND state IN ('Assigned','Running')`, attemptID); err != nil {
			return "", err
		}
		// Whether the UPDATE won or the attempt was already Cancelling,
		// the state is the same (the fence is idempotent).
		return "Cancelling", nil
	default:
		return "", fmt.Errorf("attempt %d has unknown state %q", attemptID, state)
	}
}

// CancelAck finishes Cancelling -> Cancelled once the runtime confirmed the
// attempt stopped (RUNTIME-CANCEL-003).
func (service *Service) CancelAck(ctx context.Context, attemptID int64) error {
	return service.executeDispatch(ctx, service.opCancelAck, attemptID,
		func(tx *execution.Tx) error {
			result, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts
		SET state='Cancelled', ended_at=?, termination_reason='cancelled', row_version=row_version+1
		WHERE id=? AND state='Cancelling'`, service.nowText(), attemptID)
			if err != nil {
				return err
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return fmt.Errorf("attempt %d is not Cancelling", attemptID)
			}
			return nil
		})
}

// ActiveAttempt returns the id of the one active attempt for a scope, or 0
// when none exists (ux_execution_attempt_active_scope guarantees at most
// one; DATA-ATTEMPT-002).
func (service *Service) ActiveAttempt(ctx context.Context, scopeType string, scopeID int64) (int64, error) {
	var id int64
	err := service.Reader().QueryRowContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE scope_type=? AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`,
		scopeType, scopeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// QueuedAgentAttempts lists plinth agent attempts of one type still
// waiting for a live stream (created while the slot was disconnected).
func (service *Service) QueuedAgentAttempts(ctx context.Context, attemptType string) ([]int64, error) {
	rows, err := service.Reader().QueryContext(ctx, `
		SELECT id FROM execution_attempts
		WHERE attempt_type=? AND state='Queued' ORDER BY id`, attemptType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ArtifactRef is the dispatch-facing read-only artifact reference.
type ArtifactRef struct {
	ArtifactID  int64
	Role        string
	MediaType   string
	SizeBytes   int64
	SHA256      []byte
	BodyExpired bool
}

// Grant is the dispatch-facing attempt connection grant.
type Grant struct {
	GrantID                 int64
	ConnectionRevisionID    int64
	CredentialGenerationID  int64
	Purpose                 string
	ConnectionProbeResultID int64
}

// ToolGrant is the non-secret connection binding frozen inside the Tool
// Call persistence transaction (ARCH-INPUT-003); it travels in the
// CompleteModelCallAck authorization so the supervisor can fetch the
// credential for exactly one tool execution.
type ToolGrant struct {
	GrantID                int64
	ConnectionRevisionID   int64
	CredentialGenerationID int64
	Purpose                string
}

// ToolResolution is the authorization result for one persisted Tool Call.
// PreflightCode is a closed, model-visible routing outcome and is mutually
// exclusive with Grants; the supervisor closes it without external I/O.
type ToolResolution struct {
	Grants          []ToolGrant
	PreflightCode   string
	PreflightDetail string
}

// DispatchInput is everything DispatchAttempt.input carries (RUNTIME-TASK-011).
type DispatchInput struct {
	SchemaKind    string
	CanonicalJSON []byte
	ContentDigest []byte
	ArtifactRefs  []ArtifactRef
	Grants        []Grant
	AgentVersion  string
}

// DispatchInputFor rebuilds the frozen dispatch input for one Assigned
// attempt (RUNTIME-TASK-011): the canonical snapshot bytes are rebuilt
// deterministically by the scope's rebuilder (the snapshot row stores only
// the digest — 正文仍由各领域对象拥有), then verified against the frozen
// digest before dispatch.
func (service *Service) DispatchInputFor(ctx context.Context, attemptID int64) (DispatchInput, error) {
	var input DispatchInput
	var contentDigest string
	var agentVersion sql.NullString
	err := service.Reader().QueryRowContext(ctx, `
		SELECT s.schema_kind, s.content_digest, a.agent_version
		FROM attempt_input_snapshots s
		JOIN execution_attempts a ON a.id=s.attempt_id
		WHERE s.attempt_id=?`, attemptID).Scan(&input.SchemaKind, &contentDigest, &agentVersion)
	if err != nil {
		return DispatchInput{}, err
	}
	// Supervisor-only collection attempts (Config Verification / Resource
	// Refresh) carry no agent; NULL agent_version is their normal shape.
	if agentVersion.Valid {
		input.AgentVersion = agentVersion.String
	}
	if service.SnapshotRebuilder == nil {
		return DispatchInput{}, fmt.Errorf("attempt %d has no snapshot rebuilder wired", attemptID)
	}
	canonical, err := service.SnapshotRebuilder(ctx, attemptID)
	if err != nil {
		return DispatchInput{}, err
	}
	// The rebuilt canonical JSON must still match the frozen snapshot
	// (input immutability, DATA-ATTEMPT-003).
	if rebuilt := sha256Hex(canonical); rebuilt != contentDigest {
		return DispatchInput{}, fmt.Errorf("attempt %d input snapshot digest mismatch (rebuilt %s, frozen %s)", attemptID, rebuilt, contentDigest)
	}
	input.CanonicalJSON = canonical
	input.ContentDigest, err = hexDecode(contentDigest)
	if err != nil {
		return DispatchInput{}, err
	}
	// Input artifact refs are the attempt_artifact_grants rows frozen from
	// the input snapshot (DATA-ARTIFACT-006 grants read access for the
	// attempt; the logical metadata stays on the artifacts row).
	artifactRows, err := service.Reader().QueryContext(ctx, `
		SELECT a.id, a.media_type, b.size_bytes, b.sha256, a.body_expired
		FROM attempt_artifact_grants g
		JOIN artifacts a ON a.id=g.artifact_id
		JOIN artifact_blobs b ON b.id=a.blob_id
		WHERE g.attempt_id=? AND g.source_kind='input_snapshot'
		ORDER BY g.source_id`, attemptID)
	if err != nil {
		return DispatchInput{}, err
	}
	defer artifactRows.Close()
	for artifactRows.Next() {
		var ref ArtifactRef
		if err := artifactRows.Scan(&ref.ArtifactID, &ref.MediaType, &ref.SizeBytes, &ref.SHA256, &ref.BodyExpired); err != nil {
			return DispatchInput{}, err
		}
		ref.Role = "source"
		input.ArtifactRefs = append(input.ArtifactRefs, ref)
	}
	if err := artifactRows.Err(); err != nil {
		return DispatchInput{}, err
	}
	grantRows, err := service.Reader().QueryContext(ctx, `
		SELECT id, connection_revision_id, credential_generation_id, purpose,
		       COALESCE(qualified_probe_result_id, 0)
		FROM attempt_connection_grants WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return DispatchInput{}, err
	}
	defer grantRows.Close()
	for grantRows.Next() {
		var grant Grant
		if err := grantRows.Scan(&grant.GrantID, &grant.ConnectionRevisionID, &grant.CredentialGenerationID, &grant.Purpose, &grant.ConnectionProbeResultID); err != nil {
			return DispatchInput{}, err
		}
		input.Grants = append(input.Grants, grant)
	}
	return input, grantRows.Err()
}

// LookupChatContract returns the frozen chat contract of the attempt's
// chat_model grant: model id, context budget and max output tokens from the
// qualified probe result child row (ARCH-AGENT-003).
func (service *Service) LookupChatContract(ctx context.Context, attemptID int64) (modelID string, contextBudget, maxOutput int64, err error) {
	return service.lookupChatContractOn(ctx, service.Reader(), attemptID)
}

// lookupChatContractOn runs the contract lookup against one queryer (the
// pool, or the caller's own transaction connection).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (service *Service) lookupChatContractOn(ctx context.Context, queryer rowQuerier, attemptID int64) (modelID string, contextBudget, maxOutput int64, err error) {
	err = queryer.QueryRowContext(ctx, `
		SELECT p.chat_model_id, p.context_budget_tokens, p.max_output_tokens
		FROM attempt_connection_grants g
		JOIN model_provider_connection_probe_results p ON p.probe_result_id=g.qualified_probe_result_id
		WHERE g.attempt_id=? AND g.purpose='chat_model'`, attemptID).
		Scan(&modelID, &contextBudget, &maxOutput)
	return modelID, contextBudget, maxOutput, err
}

// InputSnapshotDigest returns the hex snapshot digest for dispatch fencing.
func (service *Service) InputSnapshotDigest(ctx context.Context, attemptID int64) (string, error) {
	var digest string
	err := service.Reader().QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&digest)
	return digest, err
}

// jsonValid reports whether the byte slice is valid JSON of the given type.
func jsonValid(body []byte, kind string) bool {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return false
	}
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	default:
		return true
	}
}

// sha256Hex returns the lowercase hex SHA-256 of the input.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// hexDecode decodes a 32-byte hex digest into raw bytes.
func hexDecode(value string) ([]byte, error) {
	body, err := hex.DecodeString(value)
	if err != nil || len(body) != 32 {
		return nil, fmt.Errorf("invalid hex digest")
	}
	return body, nil
}
