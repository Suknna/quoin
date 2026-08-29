package runtime

// Journey attempt execution (T23): Lintel accepts one dispatched
// inspection_collection/config_verification_run Attempt per Running journey
// Browser Operation, executes the frozen versioned Journey, uploads the
// mandatory trace for gap outcomes, and proposes the sealed
// browser_journey_result_v1 (RUNTIME-BROWSER-005/006, RUNTIME-TASK-012).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser/journey"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// journeyRun tracks one executing Journey child attempt for idempotent
// redispatch and cancellation fencing.
type journeyRun struct {
	cancel    context.CancelFunc
	done      chan struct{}
	operation int64
	sawResult bool
}

// journeyFrozenInput is the operation-side projection of the frozen
// inspection_collection_v1 canonical JSON the Journey executes.
type journeyFrozenInput struct {
	SchemaKind          string          `json:"schemaKind"`
	AttemptID           int64           `json:"attemptId"`
	OperationID         int64           `json:"operationId"`
	Identity            journeyIdentity `json:"identity"`
	Journey             journey.Binding `json:"journey"`
	AuthenticationProbe journey.Binding `json:"authenticationProbe"`
	PlanKey             string          `json:"planKey"`
	CheckKey            string          `json:"checkKey"`
}

type journeyIdentity struct {
	IdentityID          int64  `json:"identityId"`
	IdentityRevisionID  int64  `json:"identityRevisionId"`
	ProfileGenerationID int64  `json:"profileGenerationId"`
	ProfileGeneration   int64  `json:"profileGeneration"`
	StartURL            string `json:"startUrl"`
}

// handleJourneyDispatch validates the frozen input against the already
// accepted Start binding and runs the Journey. Replays of the same attempt
// re-answer the cached acceptance instead of executing twice.
func (channel *Channel) handleJourneyDispatch(envelope *runtimev1.ControlEnvelope, dispatch *runtimev1.DispatchAttempt) {
	reject := func(reason runtimev1.AttemptRejectReason, detail string) {
		sharedops.LogEvent("lintel", "warn", "journey.dispatch_rejected", "attempt="+fmt.Sprint(dispatch.GetAttemptId())+" reason="+detail)
		_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_AttemptReject{AttemptReject: &runtimev1.AttemptReject{AttemptId: dispatch.GetAttemptId(), Reason: reason}}})
	}
	if dispatch.GetAttemptType() != runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION ||
		(dispatch.GetScopeType() != runtimev1.ScopeType_SCOPE_TYPE_CONFIG_VERIFICATION_RUN && dispatch.GetScopeType() != runtimev1.ScopeType_SCOPE_TYPE_RUN_CHECK) ||
		dispatch.GetCheckKey() == "" || dispatch.GetInput() == nil {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey dispatch envelope is incomplete")
		return
	}
	input := dispatch.GetInput()
	if input.GetSchemaKind() != "inspection_collection_v1" || len(input.GetCanonicalJson()) == 0 || len(input.GetContentDigest()) != sha256.Size {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey input envelope is invalid")
		return
	}
	digest := sha256.Sum256(input.GetCanonicalJson())
	if string(digest[:]) != string(input.GetContentDigest()) {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey input digest mismatch")
		return
	}
	var frozen journeyFrozenInput
	if err := json.Unmarshal(input.GetCanonicalJson(), &frozen); err != nil || frozen.SchemaKind != "inspection_collection_v1" || frozen.AttemptID != dispatch.GetAttemptId() {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey input identity is invalid")
		return
	}
	channel.operationMu.Lock()
	start := channel.started[frozen.OperationID]
	busy := channel.completing[frozen.OperationID] || channel.completed[frozen.OperationID] != nil || channel.stopAcks[frozen.OperationID] != nil
	channel.operationMu.Unlock()
	if start == nil || busy || start.GetKind() != runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY ||
		frozen.Journey.Catalog.Digest == "" || frozen.Journey.Catalog.Version == "" || frozen.Identity.StartURL == "" || frozen.AuthenticationProbe.ID == "" {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey operation binding is not running")
		return
	}
	if start.GetIdentityId() != frozen.Identity.IdentityID || start.GetIdentityRevisionId() != frozen.Identity.IdentityRevisionID || start.GetProfileGenerationId() != frozen.Identity.ProfileGenerationID {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey identity binding does not match the started operation")
		return
	}
	// StartBrowserOperation and DispatchAttempt must carry exactly the same
	// sealed execution input. Checking the outer identity fields is insufficient:
	// it would permit a changed Journey parameter, Catalog reference, or
	// authentication-probe binding to run under an otherwise identical profile.
	startInput := start.GetInput()
	if startInput == nil || startInput.GetSchemaKind() != input.GetSchemaKind() ||
		!bytes.Equal(startInput.GetCanonicalJson(), input.GetCanonicalJson()) ||
		!bytes.Equal(startInput.GetContentDigest(), input.GetContentDigest()) {
		reject(runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED, "journey dispatch does not match the sealed start binding")
		return
	}
	runContext, cancel := context.WithCancel(context.Background())
	run := &journeyRun{cancel: cancel, done: make(chan struct{}), operation: frozen.OperationID}
	channel.operationMu.Lock()
	if channel.journeyRuns[frozen.AttemptID] != nil {
		channel.operationMu.Unlock()
		cancel()
		_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: frozen.AttemptID}}})
		// Result delivery is at-least-once until ResultAck. This also covers the
		// short interval after runJourney sealed its proposal but before its
		// deferred journeyRuns cleanup executes.
		channel.resendJourneyResult(frozen.AttemptID)
		return
	}
	if proposal := channel.journeyProposals[frozen.AttemptID]; proposal != nil {
		channel.operationMu.Unlock()
		cancel()
		_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: frozen.AttemptID}}})
		channel.sendJourneyResult(proposal)
		return
	}
	if channel.journeyCancelled[frozen.AttemptID] {
		completion := channel.journeyCancelDone[frozen.AttemptID]
		channel.operationMu.Unlock()
		cancel()
		if completion == nil {
			// A prior physical Stop failed and deliberately released its leader.
			// Do not falsely acknowledge it; the next CancelAttempt retries Stop.
			return
		}
		go func() {
			<-completion
			channel.sendJourneyCancelAck(envelope.GetCorrelationId(), frozen.AttemptID)
		}()
		return
	}
	channel.journeyRuns[frozen.AttemptID] = run
	channel.operationMu.Unlock()
	_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: envelope.GetCorrelationId(), Msg: &runtimev1.ControlEnvelope_AttemptAccept{AttemptAccept: &runtimev1.AttemptAccept{AttemptId: frozen.AttemptID}}})
	go channel.runJourney(runContext, run, frozen)
}

// handleJourneyCancel stops the executing Journey child. The mandatory
// incomplete trace is still proposed (Quoin's cancellation fence may lawfully
// reject it as late; the physical Stop fence releases the operation).
func (channel *Channel) handleJourneyCancel(envelope *runtimev1.ControlEnvelope, request *runtimev1.CancelAttempt) {
	channel.operationMu.Lock()
	channel.journeyCancelled[request.GetAttemptId()] = true
	run := channel.journeyRuns[request.GetAttemptId()]
	operationID := channel.journeyOperations[request.GetAttemptId()]
	if operationID == 0 && run != nil {
		operationID = run.operation
	}
	if channel.journeyCancelDone == nil {
		channel.journeyCancelDone = make(map[int64]chan struct{})
	}
	completion := channel.journeyCancelDone[request.GetAttemptId()]
	leader := completion == nil
	if leader {
		completion = make(chan struct{})
		channel.journeyCancelDone[request.GetAttemptId()] = completion
	}
	channel.operationMu.Unlock()
	if run != nil {
		run.cancel()
	}
	// All duplicate cancellation frames share one completion fence. A Journey
	// may have a started Chromium operation before its DispatchAttempt arrives;
	// the operation binding published by StartBrowserOperation makes that window
	// physically cancellable rather than falsely acknowledging an ownerless tab.
	go func(correlationID uint64, attemptID, operationID int64, run *journeyRun, completion chan struct{}, leader bool) {
		if leader {
			if run != nil {
				<-run.done
			}
			if operationID != 0 {
				if err := channel.stopBrowserOperation(operationID); err != nil {
					sharedops.LogEvent("lintel", "error", "journey.cancel_stop_failed", fmt.Sprintf("attempt=%d operation=%d error=%s", attemptID, operationID, err))
					// Do not acknowledge a failed physical stop. Release leadership so a
					// replayed CancelAttempt can retry instead of wedging this boot.
					channel.operationMu.Lock()
					if channel.journeyCancelDone[attemptID] == completion {
						delete(channel.journeyCancelDone, attemptID)
					}
					channel.operationMu.Unlock()
					return
				}
			}
			close(completion)
		}
		<-completion
		channel.sendJourneyCancelAck(correlationID, attemptID)
	}(envelope.GetCorrelationId(), request.GetAttemptId(), operationID, run, completion, leader)
}

func (channel *Channel) sendJourneyCancelAck(correlationID uint64, attemptID int64) {
	_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: correlationID, Msg: &runtimev1.ControlEnvelope_CancelAck{CancelAck: &runtimev1.CancelAck{AttemptId: attemptID}}})
}

// runJourney executes the frozen program and proposes the sealed result. The
// proposal stays retained until its typed ResultAck so a same-boot reconnect
// replays the exact bytes.
func (channel *Channel) runJourney(ctx context.Context, run *journeyRun, frozen journeyFrozenInput) {
	defer func() {
		channel.operationMu.Lock()
		delete(channel.journeyRuns, frozen.AttemptID)
		channel.operationMu.Unlock()
		close(run.done)
	}()
	trace := &journey.Trace{}
	trace.Append("dispatched", map[string]any{"attemptId": frozen.AttemptID, "operationId": frozen.OperationID, "planKey": frozen.PlanKey, "checkKey": frozen.CheckKey})
	runner := &journey.Runner{
		Deps: journey.Deps{
			PageEndpoint: func(ctx context.Context) (string, error) {
				return channel.browser.JourneyDevToolsBase(ctx, run.operation)
			},
			ProbeAuthenticated: func(ctx context.Context, prefix string) (bool, error) {
				return channel.browser.ProbeURLPrefix(ctx, run.operation, prefix)
			},
		},
		Trace: trace, StartURL: frozen.Identity.StartURL,
		Journey: frozen.Journey, Probe: frozen.AuthenticationProbe,
		AttemptID: frozen.AttemptID, OperationID: frozen.OperationID,
	}
	outcome := runner.Run(ctx)
	if ctx.Err() != nil {
		outcome = journey.Outcome{GapCode: "cancelled", TerminalReason: "cancelled", ErrorDetail: "journey execution was cancelled", TraceIntegrity: "incomplete"}
	}
	payload, err := channel.journeyResultPayload(frozen, outcome, trace)
	if err != nil {
		sharedops.LogEvent("lintel", "error", "journey.result_compose_failed", err.Error())
		// The deferred cleanup below still closes run.done so cancellation
		// fences are not blocked; the Quoin lease sweeper converges the
		// unrepresentable attempt as a technical gap.
		return
	}
	proposal := &runtimev1.ResultProposal{
		AttemptId: frozen.AttemptID, BootId: channel.bootID, ConnectionEpoch: channel.epoch,
		Outcome: runtimev1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
		Payload: &runtimev1.ResultPayload{SchemaKind: "browser_journey_result_v1", CanonicalJson: payload.canonical, ContentDigest: payload.digest[:]},
	}
	proposal.Payload.ArtifactIds = payload.artifactIDs
	channel.operationMu.Lock()
	channel.journeyProposals[frozen.AttemptID] = proposal
	if run := channel.journeyRuns[frozen.AttemptID]; run != nil {
		run.sawResult = true
	}
	channel.operationMu.Unlock()
	channel.sendJourneyResult(proposal)
}

type composedJourneyResult struct {
	canonical   []byte
	digest      [sha256.Size]byte
	artifactIDs []int64
}

// journeyResultPayload uploads the mandatory trace (gap outcomes with an
// operation) and composes the canonical browser_journey_result_v1 bytes.
func (channel *Channel) journeyResultPayload(frozen journeyFrozenInput, outcome journey.Outcome, trace *journey.Trace) (composedJourneyResult, error) {
	result := map[string]any{
		"schemaKind":      "browser_journey_result_v1",
		"attemptId":       frozen.AttemptID,
		"operationId":     frozen.OperationID,
		"probeResults":    probesAsMaps(outcome.Probes),
		"traceArtifactId": nil,
		"traceIntegrity":  nil,
		"gapCode":         nil,
		"originalGapCode": nil,
		"terminalReason":  nil,
		"errorDetail":     nil,
	}
	var artifactIDs []int64
	needsTrace := !outcome.Success && outcome.GapCode != "identity_busy" &&
		outcome.TerminalReason != "artifact_commit_failed" && outcome.TerminalReason != "new_boot" && outcome.TerminalReason != "runtime_unavailable"
	if needsTrace {
		integrity := outcome.TraceIntegrity
		if integrity == "" {
			integrity = "incomplete"
		}
		body := trace.Bytes(integrity)
		sum := sha256.Sum256(body)
		artifactID, uploadErr := channel.UploadBrowserArtifact(context.Background(), BrowserArtifactUpload{
			OperationID: frozen.OperationID, Kind: runtimev1.ArtifactKind_ARTIFACT_KIND_TRACE,
			Body: body, SHA256: sum[:], MediaType: "application/x-ndjson", Sensitive: true,
			TraceIntegrity: traceIntegrityOf(integrity),
		})
		if uploadErr != nil {
			sharedops.LogEvent("lintel", "error", "journey.trace_upload_failed", fmt.Sprintf("operation=%d error=%s", frozen.OperationID, uploadErr.Error()))
			if outcome.GapCode == "journey_failed" {
				// The step failure class must survive the trace commit failure
				// (DATA-BROWSER-006): the outer gap becomes artifact_commit_failed
				// and the original classification is retained.
				outcome = journey.Outcome{
					GapCode: "artifact_commit_failed", TerminalReason: "artifact_commit_failed", OriginalGap: "journey_failed",
					ErrorDetail: fmt.Sprintf("journey step failed (%s) and the mandatory trace upload also failed: %s", boundedDetail(outcome.ErrorDetail), boundedDetail(uploadErr.Error())),
					Probes:      outcome.Probes, TraceIntegrity: "incomplete",
				}
			} else {
				// The frozen contract cannot express a probe/cancellation gap
				// without its committed trace. Sending an unrepresentable result
				// would loop schema rejections forever; the honest convergence is
				// Quoin's lease sweeper closing the attempt as a technical gap.
				return composedJourneyResult{}, fmt.Errorf("journey %d gap %s cannot be represented without its mandatory trace", frozen.AttemptID, outcome.GapCode)
			}
		} else {
			result["traceArtifactId"] = artifactID
			result["traceIntegrity"] = integrity
			artifactIDs = append(artifactIDs, artifactID)
		}
	}
	if outcome.Success {
		result["outcome"] = "success"
		result["evidence"] = []map[string]any{{
			"kind": "structured", "primary": true,
			"observedAt": time.Now().UTC().Format(time.RFC3339Nano),
			"content":    outcome.Output, "artifactId": nil,
		}}
	} else {
		result["outcome"] = "gap"
		result["evidence"] = []map[string]any{}
		result["gapCode"] = outcome.GapCode
		result["terminalReason"] = outcome.TerminalReason
		result["errorDetail"] = boundedDetail(outcome.ErrorDetail)
		if outcome.OriginalGap != "" {
			result["originalGapCode"] = outcome.OriginalGap
		}
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return composedJourneyResult{}, err
	}
	return composedJourneyResult{canonical: canonical, digest: sha256.Sum256(canonical), artifactIDs: artifactIDs}, nil
}

func (channel *Channel) sendJourneyResult(proposal *runtimev1.ResultProposal) {
	_ = channel.sendControl(&runtimev1.ControlEnvelope{ConnectionEpoch: channel.epoch, BootId: channel.bootID, CorrelationId: uint64(proposal.GetAttemptId()), Msg: &runtimev1.ControlEnvelope_ResultProposal{ResultProposal: proposal}})
}

// sendControl writes one envelope on the current installed control stream.
func (channel *Channel) sendControl(envelope *runtimev1.ControlEnvelope) error {
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send == nil {
		return errors.New("lintel control stream is not installed")
	}
	return send(envelope)
}

// resendJourneyResult replays a still-unacknowledged Journey result after a
// same-boot reconnect.
func (channel *Channel) resendJourneyResult(attemptID int64) {
	channel.operationMu.Lock()
	proposal := channel.journeyProposals[attemptID]
	channel.operationMu.Unlock()
	if proposal != nil {
		channel.sendJourneyResult(proposal)
	}
}

const maxJourneyResultReplayBatch = 32

// resendPendingJourneyResults snapshots the reconnect set once, sorts it once,
// then yields fixed-size control-stream batches. Re-scanning and sorting the
// full retained map for every page would turn a large replay into O(n²) work.
func (channel *Channel) resendPendingJourneyResults() {
	channel.operationMu.Lock()
	attemptIDs := make([]int64, 0, len(channel.journeyProposals))
	for attemptID := range channel.journeyProposals {
		attemptIDs = append(attemptIDs, attemptID)
	}
	channel.operationMu.Unlock()
	sort.Slice(attemptIDs, func(left, right int) bool { return attemptIDs[left] < attemptIDs[right] })
	channel.resendJourneyResultBatch(attemptIDs, 0)
}

func (channel *Channel) resendJourneyResultBatch(attemptIDs []int64, offset int) {
	end := min(offset+maxJourneyResultReplayBatch, len(attemptIDs))
	for _, attemptID := range attemptIDs[offset:end] {
		channel.operationMu.Lock()
		proposal := channel.journeyProposals[attemptID]
		channel.operationMu.Unlock()
		if proposal != nil {
			channel.sendJourneyResult(proposal)
		}
	}
	if end < len(attemptIDs) {
		time.AfterFunc(10*time.Millisecond, func() { channel.resendJourneyResultBatch(attemptIDs, end) })
	}
}

// acknowledgeJourneyResult releases the replay retention once Quoin has
// adjudicated (accepted or rejected) the Journey result.
func (channel *Channel) acknowledgeJourneyResult(ack *runtimev1.ResultAck) {
	if ack == nil || ack.GetAttemptId() < 1 {
		return
	}
	channel.operationMu.Lock()
	delete(channel.journeyProposals, ack.GetAttemptId())
	channel.operationMu.Unlock()
}

func probesAsMaps(probes []journey.ProbeObservation) []map[string]any {
	out := make([]map[string]any, 0, len(probes))
	for _, probe := range probes {
		encoded, _ := json.Marshal(probe)
		var decoded map[string]any
		if json.Unmarshal(encoded, &decoded) == nil {
			out = append(out, decoded)
		}
	}
	return out
}

func traceIntegrityOf(value string) runtimev1.BrowserTraceIntegrity {
	if value == "complete" {
		return runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE
	}
	return runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE
}

func boundedDetail(value string) string {
	if len(value) > 2000 {
		return value[:2000]
	}
	return value
}
