package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
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

// browserCrashed converts unexpected process loss into the same durable,
// acknowledgement-replayed terminal message as every other Runtime result.
func (channel *Channel) browserCrashed(operationID int64) {
	channel.operationMu.Lock()
	if channel.completed[operationID] != nil || channel.completing[operationID] || channel.started[operationID] == nil {
		channel.operationMu.Unlock()
		return
	}
	channel.completing[operationID] = true
	channel.operationMu.Unlock()
	payload, _ := json.Marshal(struct {
		OperationID       int64
		Outcome, Terminal string
	}{operationID, "failed", "browser_crashed"})
	digest := sha256.Sum256(payload)
	completion := &runtimev1.CompleteBrowserOperation{OperationId: operationID, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, EndedAt: timestamppb.Now(), ResultDigest: digest[:]}
	channel.operationMu.Lock()
	delete(channel.completing, operationID)
	channel.completed[operationID] = completion
	channel.operationMu.Unlock()
	channel.sendCompletion(completion)
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
	}
}

func (channel *Channel) sendCompletion(completion *runtimev1.CompleteBrowserOperation) {
	channel.controlMu.Lock()
	send := channel.controlSend
	channel.controlMu.Unlock()
	if send != nil {
		_ = send(&runtimev1.ControlEnvelope{MessageId: channel.nextMessageID(), CorrelationId: uint64(completion.GetOperationId()), ConnectionEpoch: channel.epoch, BootId: channel.bootID, Msg: &runtimev1.ControlEnvelope_CompleteBrowserOperation{CompleteBrowserOperation: completion}})
	}
}

func (channel *Channel) sendProbeCompletion(start *runtimev1.StartBrowserOperation, probe *runtimev1.AuthenticationProbeObservation, outcome runtimev1.BrowserOperationOutcome, terminal runtimev1.BrowserOperationTerminalReason) {
	// The actual process is stopped before Quoin admits the terminal fact. Quoin
	// still sends the normal Stop fence and requires its typed acknowledgement
	// before releasing the identity/slot ledger.
	_ = channel.browser.Stop(start.GetOperationId())
	payload, _ := json.Marshal(struct {
		OperationID int64  `json:"operationId"`
		Outcome     string `json:"outcome"`
		Terminal    string `json:"terminalReason,omitempty"`
		Probe       any    `json:"probe,omitempty"`
	}{start.GetOperationId(), outcome.String(), terminal.String(), probe})
	digest := sha256.Sum256(payload)
	completion := &runtimev1.CompleteBrowserOperation{OperationId: start.GetOperationId(), Outcome: outcome, TerminalReason: terminal, ProbeResults: []*runtimev1.AuthenticationProbeObservation{probe}, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_UNSPECIFIED, EndedAt: timestamppb.New(time.Now()), ResultDigest: digest[:]}
	channel.operationMu.Lock()
	channel.completed[start.GetOperationId()] = completion
	channel.operationMu.Unlock()
	channel.sendCompletion(completion)
}
