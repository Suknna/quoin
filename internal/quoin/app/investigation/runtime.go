package appinvestigation

// Runtime slice (T13): DispatchAttempt envelopes for investigation
// attempts on the live Plinth stream, ResultProposal adjudication into the
// investigation aggregate, and the ModelTokenDelta fan-out into the
// transient stream feeds (RUNTIME-AGENT-004, RUNTIME-TASK-001..008). The
// slice takes its transport primitives from the app package, so the
// runtime service stays the single stream authority.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/investigation"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PlinthView is the transport projection one dispatch needs.
type PlinthView struct {
	Connected       bool
	BootID          string
	ConnectionEpoch uint64
}

// RuntimeSlice adapts the investigation aggregate to the control stream.
type RuntimeSlice struct {
	Service *investigation.Service
	DB      *sql.DB
	// SlotView resolves the live Plinth projection.
	SlotView func(ctx context.Context) (PlinthView, error)
	// SendEnvelope stamps message ids and delivers one envelope.
	SendEnvelope func(envelope *runtimev1.ControlEnvelope) error
	// TerminationReason maps the wire termination enum onto the frozen SQL
	// name (injected from the app package's single mapper).
	TerminationReason func(reason runtimev1.TerminationReason) string
	// AfterCommit lets the Runtime owner close external resources owned by a
	// naturally terminal parent attempt. It is deliberately post-commit: the
	// attempt ledger remains the sole authority for the parent result.
	AfterCommit func(context.Context, int64)
}

// Dispatch binds one Queued investigation attempt to the live Plinth
// stream and sends the DispatchAttempt frame (RUNTIME-TASK-001/002).
func (slice *RuntimeSlice) Dispatch(ctx context.Context, attemptID int64) error {
	sharedops.LogEvent("quoin", "info", "investigation.dispatch_enter", "attempt="+int64String(attemptID))
	view, err := slice.SlotView(ctx)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.dispatch_slot", "attempt="+int64String(attemptID)+" "+err.Error())
		return err
	}
	if !view.Connected || view.ConnectionEpoch == 0 {
		sharedops.LogEvent("quoin", "error", "investigation.dispatch_slot", "attempt="+int64String(attemptID)+" connected="+boolString(view.Connected)+" epoch="+int64String(int64(view.ConnectionEpoch)))
		return errors.New("plinth is not connected")
	}
	if err := slice.Service.Attempts().BindToStream(ctx, attemptID, view.BootID, view.ConnectionEpoch, attempt.DispatchLease); err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.dispatch_bind", "attempt="+int64String(attemptID)+" "+err.Error())
		return err
	}
	input, err := slice.Service.Attempts().DispatchInputFor(ctx, attemptID)
	if err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.dispatch_input", "attempt="+int64String(attemptID)+" "+err.Error())
		return err
	}
	var scopeID int64
	if err := slice.DB.QueryRowContext(ctx, `SELECT scope_id FROM execution_attempts WHERE id=?`, attemptID).Scan(&scopeID); err != nil {
		return err
	}
	var artifactRefs []*runtimev1.ArtifactRef
	for _, ref := range input.ArtifactRefs {
		artifactRefs = append(artifactRefs, &runtimev1.ArtifactRef{
			ArtifactId: ref.ArtifactID, Role: ref.Role, MediaType: ref.MediaType,
			SizeBytes: uint64(ref.SizeBytes), Sha256: ref.SHA256, BodyExpired: ref.BodyExpired,
		})
	}
	var grants []*runtimev1.ConnectionGrant
	for _, grant := range input.Grants {
		grants = append(grants, &runtimev1.ConnectionGrant{
			GrantId: grant.GrantID, ConnectionRevisionId: grant.ConnectionRevisionID,
			CredentialGenerationId: grant.CredentialGenerationID, Purpose: grant.Purpose,
			ConnectionProbeResultId: grant.ConnectionProbeResultID,
		})
	}
	if err := slice.SendEnvelope(&runtimev1.ControlEnvelope{
		ConnectionEpoch: view.ConnectionEpoch,
		CorrelationId:   uint64(attemptID),
		BootId:          view.BootID,
		Msg: &runtimev1.ControlEnvelope_DispatchAttempt{
			DispatchAttempt: &runtimev1.DispatchAttempt{
				AttemptId:     attemptID,
				AttemptType:   runtimev1.AttemptType_ATTEMPT_TYPE_INVESTIGATION,
				ScopeType:     runtimev1.ScopeType_SCOPE_TYPE_INVESTIGATION,
				ScopeId:       scopeID,
				LeaseDeadline: timestamppb.New(time.Now().UTC().Add(attempt.DispatchLease)),
				Input: &runtimev1.AttemptInputSnapshot{
					SchemaKind:       input.SchemaKind,
					CanonicalJson:    input.CanonicalJSON,
					ContentDigest:    input.ContentDigest,
					ArtifactRefs:     artifactRefs,
					ConnectionGrants: grants,
					AgentVersion:     input.AgentVersion,
				},
			},
		},
	}); err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.dispatch_send", "attempt="+int64String(attemptID)+" "+err.Error())
		return err
	}
	sharedops.LogEvent("quoin", "info", "investigation.dispatch_sent", "attempt="+int64String(attemptID))
	return nil
}

// DispatchQueued binds and dispatches every Queued investigation attempt
// (created while the slot was disconnected).
func (slice *RuntimeSlice) DispatchQueued(ctx context.Context) {
	view, err := slice.SlotView(ctx)
	if err != nil || !view.Connected || view.ConnectionEpoch == 0 {
		return
	}
	ids, err := slice.Service.Attempts().QueuedAgentAttempts(ctx, "investigation")
	if err != nil {
		sharedops.LogEvent("quoin", "error", "investigation.queue_scan", err.Error())
		return
	}
	for _, id := range ids {
		if err := slice.Dispatch(ctx, id); err != nil {
			sharedops.LogEvent("quoin", "error", "investigation.queue_dispatch", err.Error())
		}
	}
}

// HandleResultProposal adjudicates one investigation result: the sealed
// assistant message and the attempt terminal state commit in one
// transaction (DATA-INVEST-001/003); late results keep their audit-only
// verdict (DATA-ATTEMPT-004).
func (slice *RuntimeSlice) HandleResultProposal(ctx context.Context, envelope *runtimev1.ControlEnvelope, proposal *runtimev1.ResultProposal) {
	ack := &runtimev1.ControlEnvelope{
		ConnectionEpoch: envelope.GetConnectionEpoch(),
		CorrelationId:   envelope.GetCorrelationId(),
		BootId:          envelope.GetBootId(),
		Msg:             &runtimev1.ControlEnvelope_ResultAck{ResultAck: &runtimev1.ResultAck{AttemptId: proposal.GetAttemptId()}},
	}
	reject := func(reason string) {
		ack.GetResultAck().Accepted = false
		ack.GetResultAck().Detail = reason
		_ = slice.SendEnvelope(ack)
		sharedops.LogEvent("quoin", "error", "investigation.result_rejected", "attempt="+strconv.FormatInt(proposal.GetAttemptId(), 10)+" reason="+reason)
	}
	payload := proposal.GetPayload()
	if payload == nil || payload.GetSchemaKind() == "" || len(payload.GetCanonicalJson()) == 0 {
		reject("result payload incomplete")
		return
	}
	succeeded := proposal.GetOutcome() == runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED
	termination := ""
	if !succeeded {
		if slice.TerminationReason == nil {
			reject("termination reason mapper not wired")
			return
		}
		termination = slice.TerminationReason(proposal.GetTerminationReason())
		if termination == "" {
			reject("failed outcome requires a termination reason")
			return
		}
	}
	err := slice.Service.CommitResult(ctx, investigation.Result{
		AttemptID: proposal.GetAttemptId(), BootID: proposal.GetBootId(),
		Epoch: proposal.GetConnectionEpoch(), Succeeded: succeeded, Termination: termination,
		SchemaKind: payload.GetSchemaKind(), Canonical: payload.GetCanonicalJson(),
		Digest: payload.GetContentDigest(), EvidenceIDs: payload.GetEvidenceIds(), ArtifactIDs: payload.GetArtifactIds(),
	})
	if err != nil {
		if errors.Is(err, investigation.ErrLateResult) || errors.Is(err, attempt.ErrLateResult) {
			reject("late result rejected")
			return
		}
		reject(err.Error())
		return
	}
	// A natural result that owns an Exploration is durably staged, not yet
	// terminal. Withhold ResultAck so Plinth keeps the same proposal available
	// until Runtime has closed the browser's trace/Stop obligation.
	if slice.Service.HasPendingTerminal(ctx, proposal.GetAttemptId()) {
		if slice.AfterCommit != nil {
			go slice.AfterCommit(context.Background(), proposal.GetAttemptId())
		}
		sharedops.LogEvent("quoin", "info", "investigation.result_pending_browser_cleanup", "attempt="+strconv.FormatInt(proposal.GetAttemptId(), 10))
		return
	}
	ack.GetResultAck().Accepted = true
	_ = slice.SendEnvelope(ack)
	if slice.AfterCommit != nil {
		go slice.AfterCommit(context.Background(), proposal.GetAttemptId())
	}
	sharedops.LogEvent("quoin", "info", "investigation.result_committed", "attempt="+strconv.FormatInt(proposal.GetAttemptId(), 10)+" succeeded="+strconv.FormatBool(succeeded))
}

// HandleDelta validates and fans one visible model delta into the
// attempt's transient feed. Unknown/model-mismatched/non-monotonic and
// post-terminal deltas are dropped by the feed (RUNTIME-AGENT-004).
func (slice *RuntimeSlice) HandleDelta(attemptID, modelCallID int64, deltaSeq uint64, text string) {
	slice.Service.DeliverDelta(attemptID, modelCallID, deltaSeq, text)
}

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
