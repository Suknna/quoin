package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// completeRevisionProbe runs the only currently catalogued authentication
// probe. It never maps a technical fault to Unauthenticated: doing so would
// falsely revoke a usable profile.
func (channel *Channel) completeRevisionProbe(start *runtimev1.StartBrowserOperation) {
	channel.operationMu.Lock()
	if channel.completed[start.GetOperationId()] != nil || channel.completing[start.GetOperationId()] {
		channel.operationMu.Unlock()
		return
	}
	channel.completing[start.GetOperationId()] = true
	channel.operationMu.Unlock()
	defer func() {
		channel.operationMu.Lock()
		delete(channel.completing, start.GetOperationId())
		channel.operationMu.Unlock()
	}()

	var input struct {
		Probe struct {
			ID      string `json:"id"`
			Version uint64 `json:"version"`
			Params  struct {
				AuthenticatedURLPrefix string `json:"authenticatedUrlPrefix"`
			} `json:"params"`
			Catalog struct {
				Digest  string `json:"digest"`
				Version string `json:"version"`
			} `json:"catalog"`
		} `json:"probe"`
	}
	if json.Unmarshal(start.GetInput().GetCanonicalJson(), &input) != nil || input.Probe.ID != "authentication.url-prefix.v1" || input.Probe.Version != 1 || input.Probe.Params.AuthenticatedURLPrefix == "" {
		channel.sendProbeCompletion(start, nil, runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR)
		return
	}
	observed := timestamppb.Now()
	probe := &runtimev1.AuthenticationProbeObservation{
		Phase:                 runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_REVISION_CHANGE,
		JourneyId:             input.Probe.ID,
		JourneyVersion:        input.Probe.Version,
		JourneyCatalogDigest:  input.Probe.Catalog.Digest,
		JourneyCatalogVersion: input.Probe.Catalog.Version,
		ObservedAt:            observed,
	}
	authenticated, err := channel.browser.ProbeURLPrefix(context.Background(), start.GetOperationId(), input.Probe.Params.AuthenticatedURLPrefix)
	outcome := runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED
	terminal := runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_UNSPECIFIED
	if err != nil {
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "url_probe_unavailable"
		outcome, terminal = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE
	} else if !authenticated {
		probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED
		outcome, terminal = runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED
	} else {
		probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED
	}
	channel.sendProbeCompletion(start, probe, outcome, terminal)
}

// recordStartupFailure closes a Start that passed the no-side-effect admission
// checks but failed after Chromium, the recorder, or initial navigation began.
// The accepted StartAck is ordered before this goroutine's completion by the
// channel receive loop, and the completion is retained for same-boot replay.
func (channel *Channel) recordStartupFailure(start *runtimev1.StartBrowserOperation) {
	if start == nil {
		return
	}
	completion := &runtimev1.CompleteBrowserOperation{
		OperationId:    start.GetOperationId(),
		Outcome:        runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED,
		TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_RUNTIME_UNAVAILABLE,
		EndedAt:        timestamppb.Now(),
	}
	if start.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION {
		// Even when failure happened before the first Tool Call, seal and upload
		// the operation-level incomplete trace. The empty entry list is a genuine
		// continuous trace of that interval, not a fabricated action.
		trace := channel.traceForCrash(start.GetOperationId())
		digest := sha256.Sum256(trace)
		artifactID, err := channel.uploadExplorationTrace(context.Background(), start.GetOperationId(), 0, trace, digest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE)
		if err != nil {
			completion.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
		} else {
			completion.TraceArtifactId = artifactID
			completion.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
			completion.TraceDigest = digest[:]
		}
	}
	completion.ResultDigest = canonicalCompletionDigest(completion)
	channel.operationMu.Lock()
	if channel.completed == nil {
		channel.completed = make(map[int64]*runtimev1.CompleteBrowserOperation)
	}
	if channel.completed[start.GetOperationId()] != nil {
		channel.operationMu.Unlock()
		return
	}
	channel.completed[start.GetOperationId()] = completion
	// The startup-failure completing fence only blocks follow-up work until the
	// terminal completion is cached. Stop/replay now use completed as the durable
	// fence and must not inherit a permanently in-progress state.
	delete(channel.completing, start.GetOperationId())
	channel.operationMu.Unlock()
	channel.sendCompletion(completion)
}

// browserCrashed converts unexpected process loss into the same durable,
// acknowledgement-replayed terminal message as every other Runtime result.
func (channel *Channel) browserCrashed(operationID int64) {
	// Claim the operation terminal CAS before waiting for a running action. If
	// the action were allowed to finish first it could seal a normal close and
	// make the process-loss observation disappear. The claim is deliberately
	// taken before cancellation/quiescence; browserCrashes then makes any late
	// action classify itself as the crash winner.
	channel.operationMu.Lock()
	if channel.browserCrashes == nil {
		channel.browserCrashes = make(map[int64]bool)
	}
	channel.browserCrashes[operationID] = true
	start := channel.started[operationID]
	if channel.completed[operationID] != nil || channel.completing[operationID] || start == nil {
		channel.operationMu.Unlock()
		return
	}
	// Capture the exact in-flight child capability under the same operation
	// terminal CAS that makes this crash authoritative. The action retains this
	// binding until its goroutine has published or yielded; reading the older
	// explorationRunning map after this lock could otherwise observe a cleared
	// flag and upload an operation-owned trace that Quoin rejects for the child.
	childAttemptID := channel.explorationActionCapabilities[operationID]
	channel.completing[operationID] = true
	channel.operationMu.Unlock()
	if childAttemptID == 0 {
		// Channels constructed by older unit seams do not have the operation-level
		// capability map. Production registers it before starting the goroutine;
		// this fallback preserves the same behaviour for those isolated seams.
		childAttemptID = channel.activeRunningExplorationChild(operationID)
	}

	// Fence a concurrently executing typed action before sealing the crash trace.
	// The manager's crash callback can run while that goroutine still owns the
	// recorder and trace append path; letting both proceed would produce a trace
	// whose terminal boundary races a late action metadata write.
	channel.explorationMu.Lock()
	cancel := channel.explorationCancels[childAttemptID]
	done := channel.explorationDone[childAttemptID]
	channel.explorationMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	completion := &runtimev1.CompleteBrowserOperation{OperationId: operationID, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, EndedAt: timestamppb.Now()}
	// Exploration's operation itself is the crash-trace authority: process loss
	// can occur before its first action is durable or between two child attempts.
	// The operation-owned trace uses attempt_id=0 rather than inventing a stale
	// child fence. Other browser operation kinds retain their existing completion.
	if start.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION {
		trace := channel.traceForCrash(operationID)
		traceDigest := sha256.Sum256(trace)
		// A crash during a running action must retain that child's capability.
		// Using operation ownership in that interleaving fails the child fence and
		// loses the only required incomplete trace; idle crashes correctly use 0.
		artifactID, err := channel.uploadExplorationTrace(context.Background(), operationID, childAttemptID, trace, traceDigest[:], runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE)
		if err != nil {
			completion.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED
		} else {
			completion.TraceArtifactId = artifactID
			completion.TraceIntegrity = runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
			completion.TraceDigest = traceDigest[:]
		}
	}
	completion.ResultDigest = canonicalCompletionDigest(completion)
	channel.operationMu.Lock()
	delete(channel.completing, operationID)
	channel.completed[operationID] = completion
	channel.operationMu.Unlock()
	channel.awaitStartAckFence(operationID)
	channel.sendCompletion(completion)
}

func (channel *Channel) browserCrashObserved(operationID int64) bool {
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	return channel.browserCrashes[operationID]
}

func (channel *Channel) resendPendingCompletions() {
	channel.operationMu.Lock()
	pending := make([]*runtimev1.CompleteBrowserOperation, 0, len(channel.completed))
	for _, completion := range channel.completed {
		pending = append(pending, completion)
	}
	channel.operationMu.Unlock()
	for _, completion := range pending {
		channel.sendCompletion(completion)
	}
}

func (channel *Channel) acknowledgeCompletion(ack *runtimev1.CompleteBrowserOperationAck) {
	if ack == nil || !ack.GetAccepted() {
		return
	}
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	completion := channel.completed[ack.GetOperationId()]
	if completion != nil && string(completion.GetResultDigest()) == string(ack.GetResultDigest()) {
		delete(channel.completed, ack.GetOperationId())
		if channel.stopAcks[ack.GetOperationId()] != nil {
			go channel.forgetTerminalOperation(ack.GetOperationId())
		}
	}
}

func (channel *Channel) sendCompletion(completion *runtimev1.CompleteBrowserOperation) {
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send != nil {
		_ = send(&runtimev1.ControlEnvelope{CorrelationId: uint64(completion.GetOperationId()), ConnectionEpoch: channel.epoch, BootId: channel.bootID, Msg: &runtimev1.ControlEnvelope_CompleteBrowserOperation{CompleteBrowserOperation: completion}})
	}
}

// canonicalCompletionDigest binds every replay-relevant completion field. It
// deliberately clears the self-referential digest before deterministic protobuf
// encoding, matching BrowserExplorationActionResult acknowledgement semantics.
func canonicalCompletionDigest(completion *runtimev1.CompleteBrowserOperation) []byte {
	if completion == nil {
		return nil
	}
	copy := proto.Clone(completion).(*runtimev1.CompleteBrowserOperation)
	copy.ResultDigest = nil
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(body)
	return sum[:]
}

func (channel *Channel) sendProbeCompletion(start *runtimev1.StartBrowserOperation, probe *runtimev1.AuthenticationProbeObservation, outcome runtimev1.BrowserOperationOutcome, terminal runtimev1.BrowserOperationTerminalReason) {
	// The actual process is stopped before Quoin admits the terminal fact. Quoin
	// still sends the normal Stop fence and requires its typed acknowledgement
	// before releasing the identity/slot ledger.
	_ = channel.browser.Stop(start.GetOperationId())
	completion := &runtimev1.CompleteBrowserOperation{OperationId: start.GetOperationId(), Outcome: outcome, TerminalReason: terminal, ProbeResults: []*runtimev1.AuthenticationProbeObservation{probe}, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED, EndedAt: timestamppb.New(time.Now())}
	completion.ResultDigest = canonicalCompletionDigest(completion)
	channel.operationMu.Lock()
	channel.completed[start.GetOperationId()] = completion
	channel.operationMu.Unlock()
	channel.sendCompletion(completion)
}
