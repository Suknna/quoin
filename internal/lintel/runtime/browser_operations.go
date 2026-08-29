package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (channel *Channel) reply(replyTo *runtimev1.ControlEnvelope) *runtimev1.ControlEnvelope {
	return &runtimev1.ControlEnvelope{CorrelationId: replyTo.GetMessageId(), ConnectionEpoch: channel.epoch, BootId: channel.bootID}
}

func (channel *Channel) inventoryResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.ProfileInventoryRequest) *runtimev1.ControlEnvelope {
	expectedProfiles := append([]*runtimev1.ExpectedBrowserProfile(nil), request.GetProfiles()...)
	// Inventory is a complete set, not a request-order projection. Sorting makes
	// the report deterministic across reconnects and lets Quoin compare it as a
	// stable complete inventory.
	sort.Slice(expectedProfiles, func(i, j int) bool {
		if expectedProfiles[i].GetIdentityId() != expectedProfiles[j].GetIdentityId() {
			return expectedProfiles[i].GetIdentityId() < expectedProfiles[j].GetIdentityId()
		}
		return expectedProfiles[i].GetProfileGenerationId() < expectedProfiles[j].GetProfileGenerationId()
	})
	observed := make([]*runtimev1.ObservedBrowserProfile, 0, len(expectedProfiles))
	for _, expected := range expectedProfiles {
		item := &runtimev1.ObservedBrowserProfile{IdentityId: expected.GetIdentityId(), ProfileGenerationId: expected.GetProfileGenerationId(), Status: runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_MISSING}
		manifest, digest, err := channel.profiles.Inspect(expected.GetIdentityId(), expected.GetGeneration())
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				item.Status = runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_MANIFEST_INVALID
			}
		} else {
			item.ObservedChromiumRevision, item.ObservedManifestDigest = manifest.ChromiumRevision, digest
			if manifest.ChromiumRevision != expected.GetChromiumRevision() {
				item.Status = runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_CHROMIUM_REVISION_MISMATCH
			} else if string(digest) != string(expected.GetProfileManifestDigest()) {
				item.Status = runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_MANIFEST_INVALID
			} else {
				item.Status = runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_COMPATIBLE
			}
		}
		observed = append(observed, item)
	}
	reply := channel.reply(envelope)
	reply.Msg = &runtimev1.ControlEnvelope_ProfileInventoryReport{ProfileInventoryReport: &runtimev1.ProfileInventoryReport{InventoryId: request.GetInventoryId(), Profiles: observed, Complete: true}}
	return reply
}

func (channel *Channel) startResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.StartBrowserOperation) *runtimev1.ControlEnvelope {
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	if channel.startAcks == nil {
		channel.startAcks = make(map[int64]*runtimev1.StartBrowserOperationAck)
	}
	if channel.started == nil {
		channel.started = make(map[int64]*runtimev1.StartBrowserOperation)
	}
	if channel.startAckFences == nil {
		channel.startAckFences = make(map[int64]chan struct{})
	}
	if channel.journeyOperations == nil {
		channel.journeyOperations = make(map[int64]int64)
	}
	// Every Start response is an unknown-outcome reply. Replay the exact cached
	// response before evaluating current operation state, including the cached
	// stale-stream rejection below.
	if previous := channel.startAcks[request.GetOperationId()]; previous != nil {
		reply := channel.reply(envelope)
		reply.Msg = &runtimev1.ControlEnvelope_StartBrowserOperationAck{StartBrowserOperationAck: previous}
		return reply
	}
	// Stop can legitimately arrive before an in-flight Start reaches Lintel.
	// The idempotent Stop acknowledgement is a terminal tombstone: never allow
	// a later Start with the same operation ID to recreate Chromium.
	if channel.stopAcks[request.GetOperationId()] != nil {
		ack := &runtimev1.StartBrowserOperationAck{OperationId: request.GetOperationId(), RejectReason: runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM, Detail: "operation was already stopped"}
		return channel.startAckReply(envelope, ack)
	}
	ack := &runtimev1.StartBrowserOperationAck{OperationId: request.GetOperationId()}
	if err := validateStartInput(request); err != nil {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "frozen browser input validation failed"
		return channel.startAckReply(envelope, ack)
	}
	if request.GetJourneyCatalogDigest() != catalog.Digest() || request.GetJourneyCatalogVersion() != catalog.Version {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "Journey Catalog binding does not match Lintel"
		return channel.startAckReply(envelope, ack)
	}
	var input struct {
		AttemptID int64 `json:"attemptId"`
		Identity  struct {
			StartURL          string `json:"startUrl"`
			ProfileGeneration uint64 `json:"profileGeneration"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(request.GetInput().GetCanonicalJson(), &input); err != nil || input.Identity.StartURL == "" {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "invalid frozen browser input"
		return channel.startAckReply(envelope, ack)
	}
	if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY && input.AttemptID < 1 {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "journey start is missing attempt binding"
		return channel.startAckReply(envelope, ack)
	}
	if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY && channel.journeyCancelled[input.AttemptID] {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM, "journey attempt was already cancelled"
		return channel.startAckReply(envelope, ack)
	}
	// Publish the operation binding before starting Chromium. The manager can
	// report a child-process crash synchronously with Start; without this
	// barrier browserCrashed sees no operation and loses the only completion
	// tombstone before the StartAck is cached.
	channel.started[request.GetOperationId()] = request
	if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY {
		channel.journeyOperations[input.AttemptID] = request.GetOperationId()
	}
	// A post-admission process loss may reach the crash callback before the
	// receive loop can send this StartAck. Its terminal completion waits on this
	// gate so Quoin always observes the accepted Start boundary first.
	channel.startAckFences[request.GetOperationId()] = make(chan struct{})
	// Any rejection after publishing this provisional binding must remove it.
	// Otherwise a failed profile/catalog validation looks like an active operation
	// to crash/cancel routing and can strand its identity indefinitely.
	started := false
	defer func() {
		if !started && channel.started[request.GetOperationId()] == request {
			delete(channel.started, request.GetOperationId())
			delete(channel.startAckFences, request.GetOperationId())
			if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY && channel.journeyOperations[input.AttemptID] == request.GetOperationId() {
				delete(channel.journeyOperations, input.AttemptID)
			}
		}
	}()
	var err error
	switch request.GetKind() {
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN:
		_, err = channel.browser.Start(context.Background(), request.GetOperationId(), input.Identity.StartURL)
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE, runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION, runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY:
		if request.GetProfileGenerationId() < 1 || input.Identity.ProfileGeneration == 0 {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "frozen profile generation is missing"
			return channel.startAckReply(envelope, ack)
		}
		manifest, _, inspectErr := channel.profiles.Inspect(request.GetIdentityId(), input.Identity.ProfileGeneration)
		// The profile is immutable and Chromium revision-bound before a non-login
		// operation may attach it.
		if inspectErr != nil || manifest.ChromiumRevision != channel.Config.ChromiumRevision {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "published profile is unavailable or incompatible"
			return channel.startAckReply(envelope, ack)
		}
		path, pathErr := channel.profiles.GenerationPath(request.GetIdentityId(), input.Identity.ProfileGeneration)
		if pathErr != nil {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "published profile path is invalid"
			return channel.startAckReply(envelope, ack)
		}
		if request.GetKind() == runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION {
			_, err = channel.browser.StartExplorationWithProfile(context.Background(), request.GetOperationId(), input.Identity.StartURL, path)
		} else {
			_, err = channel.browser.StartWithProfile(context.Background(), request.GetOperationId(), input.Identity.StartURL, path)
		}
	default:
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "browser operation kind is not implemented"
		return channel.startAckReply(envelope, ack)
	}
	if err != nil {
		sharedops.LogEvent("lintel", "warn", "browser.start_failed", err.Error())
		// Capacity is checked before Manager reserves an operation or launches a
		// process. It is the one retryable physical non-start.
		if errors.Is(err, browser.ErrBusy) {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY, "browser capacity exhausted"
			return channel.startAckReply(envelope, ack)
		}
		// Chromium/recorder/initial-navigation startup is already a physical
		// side-effect boundary. It must never be reported as a rejected Start:
		// Quoin would close the child without a typed terminal trace and a later
		// Stop could not fence the process attempt. Accept the durable operation,
		// then emit the normal replayable terminal completion and Stop tombstone.
		ack.Accepted, ack.StartedAt = true, timestamppb.Now()
		started = true
		channel.startAcks[request.GetOperationId()] = ack
		// Physical startup has crossed the operation boundary but did not yield a
		// usable Chromium. Own the terminal fence before StartAck is exposed: no
		// action, probe, tunnel, or publish follow-up may race the post-Ack
		// completion work item.
		if channel.completing == nil {
			channel.completing = make(map[int64]bool)
		}
		channel.completing[request.GetOperationId()] = true
		if channel.startupFailures == nil {
			channel.startupFailures = make(map[int64]*runtimev1.StartBrowserOperation)
		}
		// Do not upload or emit Completion from this function. RunConnect writes
		// the accepted StartAck first, then consumes this one-shot work item on the
		// same installed stream. Until Completion/Stop are acknowledged, started
		// retains the operation in Hello/Heartbeat reconciliation.
		channel.startupFailures[request.GetOperationId()] = request
		return channel.startAckReply(envelope, ack)
	}
	ack.Accepted, ack.StartedAt = true, timestamppb.Now()
	started = true
	channel.startAcks[request.GetOperationId()] = ack
	return channel.startAckReply(envelope, ack)
}

// takeStartupFailure consumes the post-StartAck work item exactly once. It is
// called only after the current control stream has successfully sent StartAck.
func (channel *Channel) takeStartupFailure(operationID int64) *runtimev1.StartBrowserOperation {
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	start := channel.startupFailures[operationID]
	delete(channel.startupFailures, operationID)
	return start
}

func browserStartRejectReason(err error) runtimev1.BrowserOperationStartRejectReason {
	if errors.Is(err, browser.ErrBusy) {
		return runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY
	}
	if errors.Is(err, browser.ErrDownloadBlocked) {
		return runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_DOWNLOAD_BLOCKED
	}
	return runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INTERNAL
}

// recordStopTombstoneLocked retains every same-boot terminal fence. Eviction
// would make a delayed Start observable again and could recreate Chromium after
// Quoin had already durably stopped the operation. Channel lifetime (one boot)
// is the only valid tombstone boundary.
func (channel *Channel) recordStopTombstoneLocked(operationID int64, ack *runtimev1.StopBrowserOperationAck) {
	channel.stopAcks[operationID] = ack
}

func (channel *Channel) startAckReply(envelope *runtimev1.ControlEnvelope, ack *runtimev1.StartBrowserOperationAck) *runtimev1.ControlEnvelope {
	// Start acknowledgements, except NO_CAPACITY, are unknown-outcome replies.
	// NO_CAPACITY proves no Chromium process was created and Quoin deliberately
	// retains FIFO position in WaitingForCapacity; caching it here would make a
	// later capacity retry on this same boot replay rejection forever.
	if ack != nil && ack.GetOperationId() > 0 && ack.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY && channel.startAcks[ack.GetOperationId()] == nil {
		channel.startAcks[ack.GetOperationId()] = ack
	}
	reply := channel.reply(envelope)
	reply.Msg = &runtimev1.ControlEnvelope_StartBrowserOperationAck{StartBrowserOperationAck: ack}
	return reply
}

func (channel *Channel) publishResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.PublishBrowserProfile) *runtimev1.ControlEnvelope {
	// Publishing owns the operation-state transition until DetachProfile has
	// resolved any concurrent child-process exit. Crash completion and Stop use
	// this same mutex, so they cannot observe or mutate these maps halfway
	// through profile adoption.
	channel.operationMu.Lock()
	defer channel.operationMu.Unlock()
	result := &runtimev1.PublishBrowserProfileResult{OperationId: request.GetOperationId(), Generation: request.GetNewGeneration(), CommandId: request.GetCommandId()}
	reply := func() *runtimev1.ControlEnvelope {
		out := channel.reply(envelope)
		out.Msg = &runtimev1.ControlEnvelope_PublishBrowserProfileResult{PublishBrowserProfileResult: result}
		return out
	}
	if previous := channel.published[request.GetOperationId()]; previous != nil {
		if previous.GetCommandId() == request.GetCommandId() && previous.GetGeneration() == request.GetNewGeneration() {
			result = previous
			return reply()
		}
		result.RejectReason, result.Detail = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR, "a different generation was already published for this operation"
		return reply()
	}
	start := channel.started[request.GetOperationId()]
	if start == nil || start.GetIdentityId() != request.GetIdentityId() || start.GetIdentityRevisionId() != request.GetIdentityRevisionId() || request.GetNewGeneration() == 0 {
		result.RejectReason, result.Detail = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR, "operation binding mismatch"
		return reply()
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
	if err := json.Unmarshal(start.GetInput().GetCanonicalJson(), &input); err != nil || input.AuthenticationProbe.ID != "authentication.url-prefix.v1" || input.AuthenticationProbe.Version != 1 || input.AuthenticationProbe.Catalog.Digest != catalog.Digest() || input.AuthenticationProbe.Catalog.Version != catalog.Version || input.AuthenticationProbe.Params.AuthenticatedURLPrefix == "" {
		result.RejectReason, result.Detail = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_PROTOCOL_ERROR, "unsupported frozen authentication probe"
		return reply()
	}
	observedAt := timestamppb.Now()
	authenticated, err := channel.browser.ProbeURLPrefix(context.Background(), request.GetOperationId(), input.AuthenticationProbe.Params.AuthenticatedURLPrefix)
	probe := &runtimev1.AuthenticationProbeObservation{Phase: runtimev1.AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_PUBLISH, JourneyId: input.AuthenticationProbe.ID, JourneyVersion: uint64(input.AuthenticationProbe.Version), JourneyCatalogDigest: input.AuthenticationProbe.Catalog.Digest, JourneyCatalogVersion: input.AuthenticationProbe.Catalog.Version, ObservedAt: observedAt}
	if err != nil {
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "url_probe_unavailable"
		result.ProbeResult, result.RejectReason, result.Detail = probe, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE, "authentication probe unavailable"
		return reply()
	}
	if !authenticated {
		// This is an ordinary unauthenticated observation, not a terminal
		// operation failure: the operator remains in the running login session.
		probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED
		result.ProbeResult, result.RejectReason, result.Detail = probe, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_REQUIRED, "authentication probe did not observe the authenticated URL"
		return reply()
	}
	// Quiesce the only RFB relay before terminating Chromium. Otherwise an active
	// x0vncserver client can defer its process exit while DetachProfile waits,
	// leaving the publish reply permanently pending and allowing a live relay to
	// write into a profile generation being adopted.
	channel.tunnelMu.Lock()
	cancel := channel.tunnelCancels[request.GetOperationId()]
	done := channel.tunnelDones[request.GetOperationId()]
	if cancel != nil {
		cancel()
	}
	channel.tunnelMu.Unlock()
	if done != nil {
		<-done
	}
	profilePath, err := channel.browser.DetachProfile(request.GetOperationId())
	if err != nil {
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "browser_detach_failed"
		result.ProbeResult, result.RejectReason, result.Detail = probe, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE, "cannot safely publish browser profile"
		return reply()
	}
	digest, err := channel.profiles.Install(profilePath, profile.Manifest{IdentityID: request.GetIdentityId(), Generation: request.GetNewGeneration(), IdentityRevision: request.GetIdentityRevisionId(), ChromiumRevision: channel.Config.ChromiumRevision})
	if err != nil {
		// The browser has stopped and its temporary profile was detached. Do not
		// claim authentication or a published generation when durable adoption fails.
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "profile_publication_failed"
		result.ProbeResult, result.RejectReason, result.Detail = probe, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE, "profile publication failed"
		return reply()
	}
	probe.Result = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED
	result.Accepted, result.ChromiumRevision, result.ProfileManifestDigest, result.ProbeResult = true, channel.Config.ChromiumRevision, digest, probe
	channel.published[request.GetOperationId()] = result
	// Retain the lifecycle binding until the terminal operation is durably
	// acknowledged and Stop cleanup has completed. Detached Chromium is not a
	// license for Hello/Heartbeat to report the operation absent in this window.
	return reply()
}

func (channel *Channel) stopResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.StopBrowserOperation) *runtimev1.ControlEnvelope {
	channel.operationMu.Lock()
	if previous := channel.stopAcks[request.GetOperationId()]; previous != nil && previous.GetCleanupOutcome() == runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED {
		channel.operationMu.Unlock()
		reply := channel.reply(envelope)
		reply.Msg = &runtimev1.ControlEnvelope_StopBrowserOperationAck{StopBrowserOperationAck: previous}
		return reply
	}
	// A failed cleanup acknowledgement is not a terminal cleanup tombstone.
	// Retrying Stop must retry the idempotent local cleanup (especially trace
	// staging removal) until it can honestly report success. The operation ID is
	// still fenced by stopAcks, so a delayed Start cannot recreate Chromium.
	// Keep the lifecycle binding through physical Stop and cleanup. Hello and
	// Heartbeat must reconcile it until the terminal outcome is durably fenced;
	// forgetTerminalOperation removes it only after the required acknowledgements.
	channel.operationMu.Unlock()
	channel.tunnelMu.Lock()
	cancel := channel.tunnelCancels[request.GetOperationId()]
	done := channel.tunnelDones[request.GetOperationId()]
	if cancel != nil {
		cancel()
	}
	channel.tunnelMu.Unlock()
	if done != nil {
		<-done
	}
	err := channel.browser.Stop(request.GetOperationId())
	traceErr := channel.deleteTraceStaging(request.GetOperationId())
	outcome := runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED
	failure := runtimev1.BrowserCleanupFailureCode_BROWSER_CLEANUP_FAILURE_CODE_UNSPECIFIED
	if err != nil || traceErr != nil {
		outcome, failure = runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_FAILED, runtimev1.BrowserCleanupFailureCode_BROWSER_CLEANUP_FAILURE_CODE_INTERNAL
	}
	// traceStagingDeleted is true only after removal of a locally staged trace
	// (or when this operation never staged one); it is not inferred from the
	// unrelated Chromium Stop result.
	traceDeleted := traceErr == nil
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%t:%t", request.GetOperationId(), err == nil, traceDeleted)))
	ack := &runtimev1.StopBrowserOperationAck{OperationId: request.GetOperationId(), StoppedAt: timestamppb.Now(), CleanupOutcome: outcome, ProcessStopped: err == nil, TunnelClosed: err == nil, TraceStagingDeleted: traceDeleted, TemporaryProfileDeleted: err == nil, CleanupStateHash: sum[:], FailureCode: failure}
	channel.operationMu.Lock()
	channel.recordStopTombstoneLocked(request.GetOperationId(), ack)
	completionPending := channel.completed[request.GetOperationId()] != nil
	channel.operationMu.Unlock()
	// Once Stop is durable on this boot, all non-tombstone state can be released
	// only if no terminal completion/action result still awaits Quoin's Ack.
	// The Stop Ack itself remains to fence delayed Start frames.
	if !completionPending && !channel.hasPendingExplorationResult(request.GetOperationId()) {
		channel.forgetTerminalOperation(request.GetOperationId())
	}
	reply := channel.reply(envelope)
	// Keep the Stop acknowledgement as a same-boot tombstone. A delayed Start
	// must be rejected and a duplicate Stop must replay these exact facts; only a
	// new Lintel boot discards this process-local control state.
	reply.Msg = &runtimev1.ControlEnvelope_StopBrowserOperationAck{StopBrowserOperationAck: ack}
	return reply
}
