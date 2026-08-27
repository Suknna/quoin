package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/browser/exploration"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// explorationTraceEntry is intentionally action metadata only. In particular
// it has no locator, fill value, URL, observation, DOM text, screenshot bytes,
// cookie, or network payload.
type explorationTraceEntry struct {
	Action        string `json:"action"`
	Outcome       string `json:"outcome"`
	ErrorCode     string `json:"errorCode,omitempty"`
	PayloadSHA256 string `json:"payloadSha256,omitempty"`
	ArtifactID    int64  `json:"artifactId,omitempty"`
	At            string `json:"at"`
}

// explorationTraceSeal is the one immutable terminal trace body for an
// operation. Its complete bit reflects whether the close_session action reached
// its normal terminal boundary; cancellation/crash traces are incomplete.
type explorationTraceSeal struct {
	body       []byte
	complete   bool
	artifactID int64
	digest     []byte
}

func (channel *Channel) handleExplorationAction(envelope *runtimev1.ControlEnvelope, request *runtimev1.ExecuteBrowserExplorationAction) {
	if !channel.acceptExplorationAction(request) {
		return
	}
	channel.explorationMu.Lock()
	if channel.explorationResults == nil {
		channel.explorationResults = make(map[int64]*runtimev1.BrowserExplorationActionResult)
		channel.explorationRunning = make(map[int64]bool)
		channel.explorationTraces = make(map[int64][]explorationTraceEntry)
		channel.explorationTraceSeals = make(map[int64]explorationTraceSeal)
		channel.explorationChildren = make(map[int64]int64)
		channel.explorationCancels = make(map[int64]context.CancelFunc)
		channel.explorationDone = make(map[int64]chan struct{})
		channel.explorationCancelling = make(map[int64]bool)
		channel.explorationClaims = make(map[int64]*runtimev1.BrowserExplorationTerminalClaim)
		channel.explorationClaimAcks = make(map[int64]chan *runtimev1.BrowserExplorationTerminalClaimAck)
	}
	// A crash completion has no action ID on the wire. Remember the sole active
	// child for this operation so its incomplete trace upload is fenced to the
	// exact action that owns the currently running Chromium process.
	channel.explorationChildren[request.GetOperationId()] = request.GetChildAttemptId()
	if prior := channel.explorationResults[request.GetChildAttemptId()]; prior != nil {
		channel.explorationMu.Unlock()
		channel.sendExplorationResult(envelope, prior)
		return
	}
	if channel.explorationRunning[request.GetChildAttemptId()] {
		channel.explorationMu.Unlock()
		return
	}
	channel.explorationRunning[request.GetChildAttemptId()] = true
	actionCtx, cancel := context.WithCancel(context.Background())
	channel.explorationCancels[request.GetChildAttemptId()] = cancel
	channel.explorationDone[request.GetChildAttemptId()] = make(chan struct{})
	channel.explorationMu.Unlock()
	channel.operationMu.Lock()
	if channel.explorationActionCapabilities == nil {
		channel.explorationActionCapabilities = make(map[int64]int64)
	}
	channel.explorationActionCapabilities[request.GetOperationId()] = request.GetChildAttemptId()
	channel.operationMu.Unlock()
	go channel.executeExplorationAction(actionCtx, envelope, request)
}

func (channel *Channel) executeExplorationAction(ctx context.Context, envelope *runtimev1.ControlEnvelope, request *runtimev1.ExecuteBrowserExplorationAction) {
	defer func() {
		channel.explorationMu.Lock()
		done := channel.explorationDone[request.GetChildAttemptId()]
		delete(channel.explorationDone, request.GetChildAttemptId())
		channel.explorationMu.Unlock()
		channel.operationMu.Lock()
		if channel.explorationActionCapabilities[request.GetOperationId()] == request.GetChildAttemptId() {
			delete(channel.explorationActionCapabilities, request.GetOperationId())
		}
		channel.operationMu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	result := channel.runExplorationActionContext(ctx, request)
	channel.explorationMu.Lock()
	delete(channel.explorationRunning, request.GetChildAttemptId())
	delete(channel.explorationCancels, request.GetChildAttemptId())
	// Parent cancellation owns every normal ActionResult that has not yet been
	// accepted by Quoin, including a close that already committed its complete
	// trace artifact. The cancellation handler preserves that Artifact as history
	// and writes the separately required incomplete terminal trace.
	cancelling := channel.explorationCancelling[request.GetChildAttemptId()]
	channel.explorationMu.Unlock()
	if cancelling {
		// Parent cancellation is ordered before ActionResult acceptance. Even if a
		// close artifact committed meanwhile, this action must not publish its
		// normal result: the cancellation path retains that Artifact as history and
		// uploads a distinct, explicitly incomplete terminal trace.
		channel.discardUncommittedExplorationTraceSeal(request.GetOperationId())
		channel.releaseExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId())
		return
	}
	claimed := false
	if result.GetSessionTerminal() {
		// close_session claims before it seals its trace; other terminal action
		// failures claim here. Exactly one terminal path may write a trace.
		claimed = channel.holdsExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId()) || channel.claimExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId())
		if !claimed {
			return
		}
		defer channel.releaseExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId())
	}
	result = channel.ensureTerminalExplorationTrace(request, result)
	channel.explorationMu.Lock()
	cancelling = channel.explorationCancelling[request.GetChildAttemptId()]
	channel.explorationMu.Unlock()
	if cancelling {
		// Do not cache a normal result after cancellation, including where the
		// close artifact committed before the fence arrived. The cancellation
		// handler owns the replacement incomplete terminal result.
		return
	}
	channel.explorationMu.Lock()
	channel.explorationResults[request.GetChildAttemptId()] = result
	channel.explorationMu.Unlock()
	channel.sendExplorationResult(envelope, result)
}

// resendPendingExplorationResults restores every same-boot unknown-outcome
// action delivery after a control-stream reconnect. Quoin's Ack is the only
// authority that allows a result cache entry to be forgotten.
func (channel *Channel) resendPendingExplorationResults() {
	channel.explorationMu.Lock()
	results := make([]*runtimev1.BrowserExplorationActionResult, 0, len(channel.explorationResults))
	for _, result := range channel.explorationResults {
		results = append(results, result)
	}
	channel.explorationMu.Unlock()
	for _, result := range results {
		channel.sendExplorationResult(nil, result)
	}
}

func (channel *Channel) acknowledgeExplorationResult(ack *runtimev1.BrowserExplorationActionResultAck) {
	if ack == nil || !ack.GetAccepted() || ack.GetChildAttemptId() < 1 {
		return
	}
	var terminalOperationID int64
	channel.explorationMu.Lock()
	result := channel.explorationResults[ack.GetChildAttemptId()]
	if result != nil && result.GetOperationId() == ack.GetOperationId() && result.GetToolCallId() == ack.GetToolCallId() && equalExplorationResultDigest(result, ack.GetResultDigest()) {
		delete(channel.explorationResults, ack.GetChildAttemptId())
		delete(channel.explorationCancelling, ack.GetChildAttemptId())
		// A normal ActionResult acknowledgement only proves that one Tool Call was
		// committed. Its metadata is still part of the one continuous session trace
		// required by a later close/crash. Release it only after Quoin durably
		// accepts a terminal session result (or Stop confirms cleanup).
		if result.GetSessionTerminal() && channel.explorationChildren[result.GetOperationId()] == ack.GetChildAttemptId() {
			terminalOperationID = result.GetOperationId()
			delete(channel.explorationChildren, terminalOperationID)
			delete(channel.explorationTraces, terminalOperationID)
		}
	}
	channel.explorationMu.Unlock()
	if terminalOperationID != 0 {
		channel.operationMu.Lock()
		stopped := channel.stopAcks[terminalOperationID] != nil
		channel.operationMu.Unlock()
		if stopped {
			channel.forgetTerminalOperation(terminalOperationID)
		}
	}
}

// equalExplorationResultDigest prevents an acknowledgement for an earlier
// same-child result from deleting the exact replay that remains outstanding.
func equalExplorationResultDigest(result *runtimev1.BrowserExplorationActionResult, digest []byte) bool {
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(body)
	return string(sum[:]) == string(digest)
}

func (channel *Channel) sendExplorationResult(request *runtimev1.ControlEnvelope, result *runtimev1.BrowserExplorationActionResult) {
	if result == nil {
		return
	}
	var response *runtimev1.ControlEnvelope
	if request != nil {
		response = channel.reply(request)
	} else {
		response = &runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: uint64(result.GetChildAttemptId())}
	}
	response.Msg = &runtimev1.ControlEnvelope_BrowserExplorationActionResult{BrowserExplorationActionResult: result}
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send != nil {
		_ = send(response)
	}
}

// claimExplorationTerminalAtQuoin obtains the durable ordering point before an
// irreversible complete-trace upload. A claim is an unknown-outcome control
// message: it remains cached and is replayed on every same-boot reconnect until
// the matching typed Ack arrives.
func (channel *Channel) claimExplorationTerminalAtQuoin(ctx context.Context, request *runtimev1.ExecuteBrowserExplorationAction) bool {
	if request == nil {
		return false
	}
	claim := &runtimev1.BrowserExplorationTerminalClaim{OperationId: request.GetOperationId(), ChildAttemptId: request.GetChildAttemptId(), ParentAttemptId: request.GetParentAttemptId(), ToolCallId: request.GetToolCallId()}
	waiter := make(chan *runtimev1.BrowserExplorationTerminalClaimAck, 1)
	channel.explorationMu.Lock()
	if channel.explorationClaims == nil {
		channel.explorationClaims = make(map[int64]*runtimev1.BrowserExplorationTerminalClaim)
		channel.explorationClaimAcks = make(map[int64]chan *runtimev1.BrowserExplorationTerminalClaimAck)
	}
	channel.explorationClaims[claim.GetChildAttemptId()] = claim
	channel.explorationClaimAcks[claim.GetChildAttemptId()] = waiter
	channel.explorationMu.Unlock()
	defer func() {
		channel.explorationMu.Lock()
		delete(channel.explorationClaims, claim.GetChildAttemptId())
		delete(channel.explorationClaimAcks, claim.GetChildAttemptId())
		channel.explorationMu.Unlock()
	}()
	channel.sendExplorationTerminalClaim(claim)
	select {
	case ack := <-waiter:
		return ack != nil && ack.GetAccepted() && ack.GetOperationId() == claim.GetOperationId() && ack.GetToolCallId() == claim.GetToolCallId()
	case <-ctx.Done():
		return false
	}
}

func (channel *Channel) sendExplorationTerminalClaim(claim *runtimev1.BrowserExplorationTerminalClaim) {
	if claim == nil {
		return
	}
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send != nil {
		_ = send(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: uint64(claim.GetChildAttemptId()), Msg: &runtimev1.ControlEnvelope_BrowserExplorationTerminalClaim{BrowserExplorationTerminalClaim: claim}})
	}
}

func (channel *Channel) resendPendingExplorationClaims() {
	channel.explorationMu.Lock()
	claims := make([]*runtimev1.BrowserExplorationTerminalClaim, 0, len(channel.explorationClaims))
	for _, claim := range channel.explorationClaims {
		claims = append(claims, proto.Clone(claim).(*runtimev1.BrowserExplorationTerminalClaim))
	}
	channel.explorationMu.Unlock()
	for _, claim := range claims {
		channel.sendExplorationTerminalClaim(claim)
	}
}

func (channel *Channel) acknowledgeExplorationTerminalClaim(ack *runtimev1.BrowserExplorationTerminalClaimAck) {
	if ack == nil || ack.GetChildAttemptId() < 1 {
		return
	}
	channel.explorationMu.Lock()
	claim := channel.explorationClaims[ack.GetChildAttemptId()]
	waiter := channel.explorationClaimAcks[ack.GetChildAttemptId()]
	channel.explorationMu.Unlock()
	if claim == nil || claim.GetOperationId() != ack.GetOperationId() || claim.GetToolCallId() != ack.GetToolCallId() {
		return
	}
	if waiter != nil {
		select {
		case waiter <- ack:
		default:
		}
	}
}

// handleIdleExplorationCancellation is the operation-level counterpart to an
// action cancellation. There is deliberately no fabricated child Attempt: the
// operation-owned (attempt_id=0) trace proves the session that was stopped.
func (channel *Channel) handleIdleExplorationCancellation(envelope *runtimev1.ControlEnvelope, request *runtimev1.CancelBrowserExplorationAction) {
	channel.operationMu.Lock()
	if completion := channel.completed[request.GetOperationId()]; completion != nil {
		channel.operationMu.Unlock()
		// Duplicate idle-cancel frames are at-least-once requests. Replay the
		// exact cached completion immediately; waiting only for reconnect would
		// leave Quoin's original cancellation fence unnecessarily ambiguous.
		channel.sendCompletion(completion)
		return
	}
	if channel.completing[request.GetOperationId()] {
		channel.operationMu.Unlock()
		return
	}
	start := channel.started[request.GetOperationId()]
	if start == nil || start.GetKind() != runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION {
		channel.operationMu.Unlock()
		return
	}
	if channel.completing == nil {
		channel.completing = make(map[int64]bool)
	}
	channel.completing[request.GetOperationId()] = true
	channel.operationMu.Unlock()

	trace := channel.traceForCrash(request.GetOperationId())
	digest := sha256.Sum256(trace)
	completion := &runtimev1.CompleteBrowserOperation{
		OperationId: request.GetOperationId(), Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED,
		TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED,
		EndedAt:        timestamppb.Now(),
	}
	// Operation-level cancellation has no child attempt, but its continuous
	// trace still requires the same staged-before-upload lifecycle as every
	// other terminal trace. StopAck may only claim staging cleanup after this.
	artifactID, err := channel.uploadExplorationTrace(context.Background(), request.GetOperationId(), 0, trace, digest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE)
	if err != nil {
		completion.Outcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
		completion.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
	} else {
		completion.TraceArtifactId = artifactID
		completion.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
		completion.TraceDigest = digest[:]
	}
	_ = channel.stopBrowserOperation(request.GetOperationId())
	completion.ResultDigest = canonicalCompletionDigest(completion)
	channel.operationMu.Lock()
	delete(channel.completing, request.GetOperationId())
	if channel.completed == nil {
		channel.completed = make(map[int64]*runtimev1.CompleteBrowserOperation)
	}
	channel.completed[request.GetOperationId()] = completion
	channel.operationMu.Unlock()
	channel.sendCompletion(completion)
}

func (channel *Channel) runExplorationAction(request *runtimev1.ExecuteBrowserExplorationAction) *runtimev1.BrowserExplorationActionResult {
	return channel.runExplorationActionContext(context.Background(), request)
}

func (channel *Channel) runExplorationActionContext(ctx context.Context, request *runtimev1.ExecuteBrowserExplorationAction) *runtimev1.BrowserExplorationActionResult {
	base := &runtimev1.BrowserExplorationActionResult{OperationId: request.GetOperationId(), ChildAttemptId: request.GetChildAttemptId(), ParentAttemptId: request.GetParentAttemptId(), ToolCallId: request.GetToolCallId()}
	if request.GetInput().GetSchemaKind() != "browser_tool_v1" {
		return explorationTerminalFailure(base, "ProtocolError", "browser action schema kind is invalid")
	}
	action, err := exploration.Parse(request.GetInput().GetCanonicalJson(), request.GetInput().GetContentDigest())
	if err != nil {
		return explorationTerminalFailure(base, "ProtocolError", "frozen browser action validation failed")
	}
	if action.Name != "open" && action.SessionID != strconv.FormatInt(request.GetOperationId(), 10) {
		return explorationTerminalFailure(base, "ProtocolError", "session does not belong to this browser operation")
	}
	outcome := channel.executeBrowserExplorationAction(ctx, request.GetOperationId(), action)
	if !outcome.Success {
		channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "error", outcome.ErrorCode, outcome.Payload, 0))
		return explorationFailureFromManager(base, action, outcome)
	}
	if action.Name == "open" {
		if admission, code, detail := channel.admissionProbe(ctx, request.GetOperationId()); admission != nil {
			if admission.GetResult() != runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED {
				channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "error", code, outcome.Payload, 0))
				return &runtimev1.BrowserExplorationActionResult{OperationId: base.OperationId, ChildAttemptId: base.ChildAttemptId, ParentAttemptId: base.ParentAttemptId, ToolCallId: base.ToolCallId, ErrorCode: code, ErrorDetail: detail, SessionTerminal: true, AdmissionProbe: admission}
			}
			base.AdmissionProbe = admission
		} else {
			channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "error", code, outcome.Payload, 0))
			return explorationTerminalFailure(base, code, detail)
		}
	}
	if outcome.Payload == nil {
		return explorationTerminalFailure(base, "ProtocolError", "browser action returned no result payload")
	}
	if action.Name == "open" {
		outcome.Payload["sessionId"] = strconv.FormatInt(request.GetOperationId(), 10)
	}
	if action.Name == "screenshot" {
		artifactID, uploadErr := channel.uploadScreenshot(request, outcome.Screenshot)
		if uploadErr != nil {
			// The failed screenshot attempt is still an action boundary of the
			// continuous trace. Without this entry a later mandatory terminal
			// trace falsely implies that the requested capture never happened.
			channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "error", "ArtifactCommitFailed", outcome.Payload, 0))
			return explorationTerminalFailure(base, "ArtifactCommitFailed", "screenshot artifact could not be committed")
		}
		outcome.Payload["screenshotArtifactId"] = artifactID
		channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "success", "", outcome.Payload, artifactID))
	} else if action.Name != "close_session" {
		// traceForClose appends close_session exactly once, after the terminal
		// payload is fixed. Recording it here as well would create two logical
		// close actions in one continuous trace.
		channel.appendTrace(request.GetOperationId(), traceEntry(action.Name, "success", "", outcome.Payload, 0))
	}
	payload, err := canonicalBrowserPayload(outcome.Payload)
	if err != nil {
		return explorationTerminalFailure(base, "ProtocolError", "browser result cannot be encoded")
	}
	base.Success, base.Payload = true, payload
	if action.Name != "close_session" {
		return base
	}
	// The completion probe must run while the operation-private Chromium still
	// exists. It is then sealed with the trace before Chromium is stopped.
	// Claiming first establishes the single trace writer against cancellation and
	// crash completion. executeExplorationAction releases the claim after it has
	// cached the exact terminal result for replay.
	if !channel.claimExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId()) {
		return explorationTerminalFailure(base, "Cancelled", "another terminal browser outcome already owns this operation")
	}
	// Quoin serializes this durable pre-upload claim with parent cancellation.
	// A complete trace cannot be uploaded until the claim has been accepted.
	if !channel.claimExplorationTerminalAtQuoin(ctx, request) {
		channel.releaseExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId())
		return explorationTerminalFailure(base, "Cancelled", "parent cancellation won terminal trace arbitration")
	}
	// A complete seal is legal only after both the completion probe and the
	// physical Stop boundary succeeded. In particular, do not upload a complete
	// artifact merely because the probe succeeded: Chromium can still crash while
	// Stop tears down the process. The terminal failure path below is then sealed
	// by ensureTerminalExplorationTrace as the one required incomplete trace.
	completion, completionCode, completionDetail := channel.completionProbe(ctx, request.GetOperationId())
	base.CompletionProbe = completion
	stopErr := channel.stopBrowserOperation(request.GetOperationId())
	if completionCode != "" {
		return explorationTerminalFailure(base, completionCode, completionDetail)
	}
	if stopErr != nil || channel.browserCrashObserved(request.GetOperationId()) {
		return explorationTerminalFailure(base, "BrowserCrashed", "browser operation could not be stopped before complete trace commit")
	}
	trace := channel.traceForClose(request.GetOperationId(), action.Name, outcome.Payload)
	traceDigest := sha256.Sum256(trace)
	traceArtifactID, uploadErr := channel.uploadExplorationTrace(context.Background(), request.GetOperationId(), request.GetChildAttemptId(), trace, traceDigest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE)
	if uploadErr != nil {
		return explorationTerminalFailure(base, "ArtifactCommitFailed", "continuous browser trace could not be committed")
	}
	channel.markExplorationTraceCommitted(request.GetOperationId(), traceArtifactID, traceDigest[:])
	base.SessionTerminal = true
	base.TraceArtifactId = traceArtifactID
	base.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE
	base.TraceDigest = traceDigest[:]
	base.TerminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED
	return base
}

// ensureTerminalExplorationTrace makes every terminal action result auditable before
// it can leave Lintel. A failed upload is explicitly the only trace-less terminal.
func (channel *Channel) ensureTerminalExplorationTrace(request *runtimev1.ExecuteBrowserExplorationAction, result *runtimev1.BrowserExplorationActionResult) *runtimev1.BrowserExplorationActionResult {
	if request == nil || result == nil || !result.GetSessionTerminal() || result.GetTraceArtifactId() != 0 {
		return result
	}
	trace := channel.traceForCrash(request.GetOperationId())
	digest := sha256.Sum256(trace)
	artifactID, err := channel.uploadExplorationTrace(context.Background(), request.GetOperationId(), request.GetChildAttemptId(), trace, digest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE)
	if err == nil {
		channel.markExplorationTraceCommitted(request.GetOperationId(), artifactID, digest[:])
	}
	if err != nil {
		result.Success, result.Payload = false, nil
		result.ErrorCode, result.ErrorDetail = "ArtifactCommitFailed", "continuous browser trace could not be committed"
		result.TerminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
		result.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
		return result
	}
	result.TraceArtifactId, result.TraceIntegrity, result.TraceDigest = artifactID, channel.traceIntegrityFor(request.GetOperationId()), digest[:]
	if result.GetTerminalOutcome() == runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_UNSPECIFIED {
		result.TerminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
	}
	if result.GetTerminalReason() == runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_UNSPECIFIED {
		result.TerminalReason = terminalReasonForExplorationError(result.GetErrorCode())
	}
	return result
}

func (channel *Channel) executeBrowserExplorationAction(ctx context.Context, operationID int64, action exploration.Action) browser.ExplorationResult {
	if channel.executeBrowserAction != nil {
		return channel.executeBrowserAction(ctx, operationID, action)
	}
	return channel.browser.ExecuteExplorationAction(ctx, operationID, action)
}

func (channel *Channel) stopBrowserOperation(operationID int64) error {
	if channel.stopBrowser != nil {
		return channel.stopBrowser(operationID)
	}
	return channel.browser.Stop(operationID)
}

func (channel *Channel) admissionProbe(parent context.Context, operationID int64) (*runtimev1.AuthenticationProbeObservation, string, string) {
	// Probe I/O is part of an action's terminal boundary. Never detach it onto
	// Background: a stalled DevTools connection must observe cancellation and a
	// bounded deadline just like the action that requested it.
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	channel.operationMu.Lock()
	start := channel.started[operationID]
	channel.operationMu.Unlock()
	if start == nil {
		return nil, "ProtocolError", "exploration operation start is unknown"
	}
	var input struct {
		AuthenticationProbe struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
			Params  struct {
				AuthenticatedURLPrefix string `json:"authenticatedUrlPrefix"`
			} `json:"params"`
			Catalog struct {
				Digest  string `json:"digest"`
				Version string `json:"version"`
			} `json:"catalog"`
		} `json:"authenticationProbe"`
	}
	if json.Unmarshal(start.GetInput().GetCanonicalJson(), &input) != nil || input.AuthenticationProbe.ID != "authentication.url-prefix.v1" || input.AuthenticationProbe.Version != 1 || input.AuthenticationProbe.Params.AuthenticatedURLPrefix == "" || input.AuthenticationProbe.Catalog.Digest != catalog.Digest() || input.AuthenticationProbe.Catalog.Version != catalog.Version {
		return nil, "ProtocolError", "frozen authentication probe is invalid"
	}
	probe := &runtimev1.AuthenticationProbeObservation{Phase: runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_ADMISSION, JourneyId: input.AuthenticationProbe.ID, JourneyVersion: uint64(input.AuthenticationProbe.Version), JourneyCatalogDigest: input.AuthenticationProbe.Catalog.Digest, JourneyCatalogVersion: input.AuthenticationProbe.Catalog.Version, ObservedAt: timestamppb.Now()}
	authenticated, err := channel.browser.ProbeURLPrefix(ctx, operationID, input.AuthenticationProbe.Params.AuthenticatedURLPrefix)
	if err != nil {
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "url_probe_unavailable"
		return probe, "AuthenticationProbeUnavailable", "authentication probe could not observe the browser"
	}
	if !authenticated {
		probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED
		return probe, "AuthenticationRequired", "browser profile is no longer authenticated"
	}
	probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED
	return probe, "", ""
}

// completionProbe repeats the frozen URL-prefix admission probe immediately
// before an Exploration is closed. An explicit unauthenticated observation is
// a domain failure; a technical probe failure is separately classified.
func (channel *Channel) completionProbe(ctx context.Context, operationID int64) (*runtimev1.AuthenticationProbeObservation, string, string) {
	probe, code, detail := channel.admissionProbe(ctx, operationID)
	if probe != nil {
		probe.Phase = runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_COMPLETION
	}
	return probe, code, detail
}

func terminalReasonForExplorationError(code string) runtimev1.BrowserOperationTerminalReason {
	switch code {
	case "AuthenticationRequired":
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED
	case "AuthenticationProbeUnavailable":
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE
	case "ArtifactCommitFailed":
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
	case "BrowserCrashed":
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED
	case "RuntimeUnavailable":
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_RUNTIME_UNAVAILABLE
	default:
		return runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR
	}
}

func explorationFailureFromManager(base *runtimev1.BrowserExplorationActionResult, action exploration.Action, outcome browser.ExplorationResult) *runtimev1.BrowserExplorationActionResult {
	result := proto.Clone(base).(*runtimev1.BrowserExplorationActionResult)
	result.ErrorCode, result.ErrorDetail, result.SessionTerminal = outcome.ErrorCode, outcome.ErrorDetail, outcome.SessionTerminal
	if !outcome.SessionTerminal && outcome.Payload != nil {
		payload, err := canonicalBrowserPayload(outcome.Payload)
		if err == nil {
			result.Payload = payload
		}
	}
	return result
}

func explorationTerminalFailure(base *runtimev1.BrowserExplorationActionResult, code, detail string) *runtimev1.BrowserExplorationActionResult {
	result := proto.Clone(base).(*runtimev1.BrowserExplorationActionResult)
	// A terminal failure cannot retain a successful tool payload. In particular a
	// close may have constructed its success projection before trace upload or Stop
	// fails; leaving either field behind makes conflicting child/tool outcomes
	// representable.
	result.Success, result.Payload = false, nil
	result.ErrorCode, result.ErrorDetail, result.SessionTerminal = code, detail, true
	result.TerminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
	result.TerminalReason = terminalReasonForExplorationError(code)
	return result
}

func canonicalBrowserPayload(value map[string]any) (*runtimev1.ResultPayload, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	return &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1", CanonicalJson: body, ContentDigest: digest[:]}, nil
}

func (channel *Channel) uploadScreenshot(request *runtimev1.ExecuteBrowserExplorationAction, body []byte) (int64, error) {
	if len(body) == 0 {
		return 0, fmt.Errorf("empty screenshot")
	}
	digest := sha256.Sum256(body)
	return channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{OperationID: request.GetOperationId(), ChildAttemptID: request.GetChildAttemptId(), Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_SCREENSHOT, Body: body, SHA256: digest[:], MediaType: "image/png", Sensitive: true})
}

func traceEntry(action, outcome, code string, payload map[string]any, artifactID int64) explorationTraceEntry {
	entry := explorationTraceEntry{Action: action, Outcome: outcome, ErrorCode: code, ArtifactID: artifactID, At: time.Now().UTC().Format(time.RFC3339Nano)}
	if payload != nil {
		body, _ := json.Marshal(payload)
		digest := sha256.Sum256(body)
		entry.PayloadSHA256 = hex.EncodeToString(digest[:])
	}
	return entry
}

func (channel *Channel) appendTrace(operationID int64, entry explorationTraceEntry) {
	channel.explorationMu.Lock()
	if channel.explorationTraces == nil {
		channel.explorationTraces = make(map[int64][]explorationTraceEntry)
	}
	channel.explorationTraces[operationID] = append(channel.explorationTraces[operationID], entry)
	channel.explorationMu.Unlock()
	// Staging is an operation-local cache, never a second authority. Still,
	// appending every safe metadata entry makes a crash between actions visible
	// to cleanup rather than replacing the whole session only at close time.
	if body, err := json.Marshal(entry); err == nil {
		_ = channel.appendExplorationTraceEntry(operationID, body)
	}
}

func (channel *Channel) traceForClose(operationID int64, action string, payload map[string]any) []byte {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	if seal, ok := channel.explorationTraceSeals[operationID]; ok {
		return append([]byte(nil), seal.body...)
	}
	entries := append([]explorationTraceEntry(nil), channel.explorationTraces[operationID]...)
	entries = append(entries, traceEntry(action, "success", "", payload, 0))
	body, _ := json.Marshal(struct {
		Version    int                     `json:"version"`
		Incomplete bool                    `json:"incomplete,omitempty"`
		Entries    []explorationTraceEntry `json:"entries"`
	}{Version: 1, Incomplete: action != "close_session", Entries: entries})
	if channel.explorationTraceSeals == nil {
		channel.explorationTraceSeals = make(map[int64]explorationTraceSeal)
	}
	channel.explorationTraceSeals[operationID] = explorationTraceSeal{body: append([]byte(nil), body...), complete: action == "close_session"}
	return body
}

// traceForCrash seals only the accumulated non-secret action metadata. It does
// not synthesize a successful close entry: the resulting Artifact is explicitly
// incomplete because Chromium exited before the action/session could finish.
// traceForCancellation always builds a new incomplete body. It intentionally
// does not replace a committed complete seal: that artifact is immutable
// historical evidence, whereas cancellation must attach an independently
// committed incomplete trace to the terminal Operation.
func (channel *Channel) traceForCancellation(operationID int64) []byte {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	entries := append([]explorationTraceEntry(nil), channel.explorationTraces[operationID]...)
	entries = append(entries, traceEntry("cancelled", "session_closed", "Cancelled", nil, 0))
	body, _ := json.Marshal(struct {
		Version    int                     `json:"version"`
		Incomplete bool                    `json:"incomplete"`
		Entries    []explorationTraceEntry `json:"entries"`
	}{Version: 1, Incomplete: true, Entries: entries})
	return body
}

func (channel *Channel) traceForCrash(operationID int64) []byte {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	if seal, ok := channel.explorationTraceSeals[operationID]; ok {
		// A complete seal is immutable only after its Artifact commit. A bounded
		// upload failure must be represented by the required incomplete trace,
		// rather than a complete trace paired with ArtifactCommitFailed.
		if !seal.complete || seal.artifactID != 0 {
			return append([]byte(nil), seal.body...)
		}
	}
	entries := append([]explorationTraceEntry(nil), channel.explorationTraces[operationID]...)
	body, _ := json.Marshal(struct {
		Version    int                     `json:"version"`
		Incomplete bool                    `json:"incomplete"`
		Entries    []explorationTraceEntry `json:"entries"`
	}{Version: 1, Incomplete: true, Entries: entries})
	if channel.explorationTraceSeals == nil {
		channel.explorationTraceSeals = make(map[int64]explorationTraceSeal)
	}
	channel.explorationTraceSeals[operationID] = explorationTraceSeal{body: append([]byte(nil), body...), complete: false}
	return body
}

// discardUncommittedExplorationTraceSeal drops only a local snapshot which
// never reached Artifact commit. A committed seal is immutable operation history
// even if its action result lost the race with parent cancellation.
func (channel *Channel) discardUncommittedExplorationTraceSeal(operationID int64) {
	channel.explorationMu.Lock()
	if seal, ok := channel.explorationTraceSeals[operationID]; ok && seal.artifactID == 0 {
		delete(channel.explorationTraceSeals, operationID)
	}
	channel.explorationMu.Unlock()
}

func (channel *Channel) markExplorationTraceCommitted(operationID, artifactID int64, digest []byte) {
	if artifactID < 1 || len(digest) != sha256.Size {
		return
	}
	channel.explorationMu.Lock()
	if seal, ok := channel.explorationTraceSeals[operationID]; ok {
		seal.artifactID = artifactID
		seal.digest = append([]byte(nil), digest...)
		channel.explorationTraceSeals[operationID] = seal
	}
	channel.explorationMu.Unlock()
}

func (channel *Channel) committedExplorationTrace(operationID int64) (explorationTraceSeal, bool) {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	seal, ok := channel.explorationTraceSeals[operationID]
	if !ok || seal.artifactID < 1 || len(seal.digest) != sha256.Size {
		return explorationTraceSeal{}, false
	}
	seal.body, seal.digest = append([]byte(nil), seal.body...), append([]byte(nil), seal.digest...)
	return seal, true
}

func (channel *Channel) traceIntegrityFor(operationID int64) runtimev1.BrowserTraceIntegrity {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	if channel.explorationTraceSeals[operationID].complete {
		return runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE
	}
	return runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
}

// claimExplorationTerminal is the operation-level terminal CAS shared by
// close, cancellation, idle cancellation and crash completion. It protects the
// continuous trace from competing terminal writers.
func (channel *Channel) claimExplorationTerminal(operationID, childID int64) bool {
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	if channel.completed[operationID] != nil || channel.completing[operationID] {
		return false
	}
	if channel.completing == nil {
		channel.completing = make(map[int64]bool)
	}
	if channel.explorationTerminalChildren == nil {
		channel.explorationTerminalChildren = make(map[int64]int64)
	}
	channel.completing[operationID] = true
	channel.explorationTerminalChildren[operationID] = childID
	return true
}

func (channel *Channel) holdsExplorationTerminal(operationID, childID int64) bool {
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	return channel.completing[operationID] && channel.explorationTerminalChildren[operationID] == childID
}

func (channel *Channel) releaseExplorationTerminal(operationID, childID int64) {
	channel.operationMu.Lock()
	if channel.explorationTerminalChildren[operationID] == childID {
		delete(channel.completing, operationID)
		delete(channel.explorationTerminalChildren, operationID)
	}
	channel.operationMu.Unlock()
}

func (channel *Channel) activeExplorationChild(operationID int64) int64 {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	return channel.explorationChildren[operationID]
}

// activeRunningExplorationChild returns a child capability only while that
// exact action goroutine is still running. A prior acknowledged action cannot
// authorize a later idle crash trace.
func (channel *Channel) activeRunningExplorationChild(operationID int64) int64 {
	channel.explorationMu.Lock()
	defer channel.explorationMu.Unlock()
	childID := channel.explorationChildren[operationID]
	if childID != 0 && channel.explorationRunning[childID] {
		return childID
	}
	return 0
}

// handleExplorationCancellation is the typed parent-cancellation fence. It is
// idempotent by child attempt: a completed action is replayed unchanged, while
// a running action is made terminal by stopping its operation and returning the
// same durable cancellation result until Quoin acknowledges it.
func (channel *Channel) handleExplorationCancellation(envelope *runtimev1.ControlEnvelope, request *runtimev1.CancelBrowserExplorationAction) {
	if request == nil || request.GetOperationId() < 1 || request.GetParentAttemptId() < 1 {
		return
	}
	// A zero child/tool pair is the operation-level cancellation used while an
	// Exploration is idle (after StartAck or between actions). It has no action
	// result to terminalize: CompleteBrowserOperation is its durable reply.
	if request.GetChildAttemptId() == 0 || request.GetToolCallId() == 0 {
		if request.GetChildAttemptId() != 0 || request.GetToolCallId() != 0 {
			return
		}
		channel.handleIdleExplorationCancellation(envelope, request)
		return
	}
	ack := &runtimev1.CancelBrowserExplorationActionAck{OperationId: request.GetOperationId(), ChildAttemptId: request.GetChildAttemptId(), ToolCallId: request.GetToolCallId(), Accepted: true}
	reply := channel.reply(envelope)
	reply.Msg = &runtimev1.ControlEnvelope_CancelBrowserExplorationActionAck{CancelBrowserExplorationActionAck: ack}
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send != nil {
		_ = send(reply)
	}
	channel.explorationMu.Lock()
	// A cancellation fence wins over an unacknowledged cached normal result.
	// Quoin may have cancelled the parent after that result was sent but before
	// it was durably accepted; replaying it would resurrect a cancelled action.
	// The cancellation result below replaces the cache and remains replayable
	// until Quoin acknowledges that exact terminal outcome.
	if channel.explorationCancelling == nil {
		channel.explorationCancelling = make(map[int64]bool)
	}
	if channel.explorationCancelling[request.GetChildAttemptId()] {
		channel.explorationMu.Unlock()
		return
	}
	channel.explorationCancelling[request.GetChildAttemptId()] = true
	cancel := channel.explorationCancels[request.GetChildAttemptId()]
	done := channel.explorationDone[request.GetChildAttemptId()]
	channel.explorationMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Do not seal the cancellation trace or stop Chromium while the action still
	// owns its executor/recorder state. In particular, cancellation can race a
	// close_session action after it has observed a page but before it appends the
	// final trace metadata. The action goroutine always observes the cancelling
	// fence and closes done without publishing its normal result.
	if done != nil {
		<-done
	}
	if !channel.claimExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId()) {
		// This child can temporarily hold the terminal CAS while sealing a normal
		// result. Cancellation has commit priority until that action observes the
		// marker and releases its snapshot; then it owns the replacement result.
		if !channel.holdsExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId()) {
			channel.explorationMu.Lock()
			delete(channel.explorationCancelling, request.GetChildAttemptId())
			channel.explorationMu.Unlock()
			return
		}
		if !channel.claimExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId()) {
			channel.explorationMu.Lock()
			delete(channel.explorationCancelling, request.GetChildAttemptId())
			channel.explorationMu.Unlock()
			return
		}
	}
	defer channel.releaseExplorationTerminal(request.GetOperationId(), request.GetChildAttemptId())
	// Cancellation is a distinct terminal fact. A complete close Artifact which
	// committed before the ActionResult remains immutable history, but cannot be
	// attached to this cancellation or relabelled incomplete. Upload a separate
	// incomplete trace under the still-Cancelling child fence.
	traceID := int64(0)
	traceIntegrity := runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
	trace := channel.traceForCancellation(request.GetOperationId())
	sum := sha256.Sum256(trace)
	digest := sum[:]
	traceID, uploadErr := channel.uploadExplorationTrace(context.Background(), request.GetOperationId(), request.GetChildAttemptId(), trace, digest, runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE)
	code, detail, reason := "Cancelled", "parent attempt cancelled", runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED
	if uploadErr != nil {
		code, detail, reason = "ArtifactCommitFailed", "continuous browser trace could not be committed", runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
	}
	_ = channel.stopBrowserOperation(request.GetOperationId())
	terminalOutcome := runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_CANCELLED
	if uploadErr != nil {
		terminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
	}
	if channel.browserCrashObserved(request.GetOperationId()) {
		code, detail, reason = "BrowserCrashed", "browser operation crashed while cancellation was in progress", runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED
		terminalOutcome = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED
	}
	result := &runtimev1.BrowserExplorationActionResult{OperationId: request.GetOperationId(), ChildAttemptId: request.GetChildAttemptId(), ParentAttemptId: request.GetParentAttemptId(), ToolCallId: request.GetToolCallId(), ErrorCode: code, ErrorDetail: detail, SessionTerminal: true, TerminalOutcome: terminalOutcome, TerminalReason: reason}
	if uploadErr == nil {
		result.TraceArtifactId, result.TraceIntegrity, result.TraceDigest = traceID, traceIntegrity, digest
	}
	channel.explorationMu.Lock()
	if channel.explorationResults == nil {
		channel.explorationResults = map[int64]*runtimev1.BrowserExplorationActionResult{}
	}
	channel.explorationResults[request.GetChildAttemptId()] = result
	channel.explorationMu.Unlock()
	channel.sendExplorationResult(envelope, result)
}
