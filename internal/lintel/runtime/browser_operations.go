package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (channel *Channel) reply(replyTo *runtimev1.ControlEnvelope) *runtimev1.ControlEnvelope {
	return &runtimev1.ControlEnvelope{MessageId: channel.nextMessageID(), CorrelationId: replyTo.GetMessageId(), ConnectionEpoch: channel.epoch, BootId: channel.bootID}
}

func (channel *Channel) inventoryResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.ProfileInventoryRequest) *runtimev1.ControlEnvelope {
	observed := make([]*runtimev1.ObservedBrowserProfile, 0, len(request.GetProfiles()))
	for _, expected := range request.GetProfiles() {
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
	// Stop can legitimately arrive before an in-flight Start reaches Lintel.
	// The idempotent Stop acknowledgement is a terminal tombstone: never allow
	// a later Start with the same operation ID to recreate Chromium.
	if channel.stopAcks[request.GetOperationId()] != nil {
		ack := &runtimev1.StartBrowserOperationAck{OperationId: request.GetOperationId(), RejectReason: runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM, Detail: "operation was already stopped"}
		return channel.startAckReply(envelope, ack)
	}
	if previous := channel.startAcks[request.GetOperationId()]; previous != nil {
		reply := channel.reply(envelope)
		reply.Msg = &runtimev1.ControlEnvelope_StartBrowserOperationAck{StartBrowserOperationAck: previous}
		return reply
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
		Identity struct {
			StartURL          string `json:"startUrl"`
			ProfileGeneration uint64 `json:"profileGeneration"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(request.GetInput().GetCanonicalJson(), &input); err != nil || input.Identity.StartURL == "" {
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "invalid frozen browser input"
		return channel.startAckReply(envelope, ack)
	}
	var err error
	switch request.GetKind() {
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN:
		_, err = channel.browser.Start(context.Background(), request.GetOperationId(), input.Identity.StartURL)
	case runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_AUTHENTICATION_PROBE:
		if request.GetProfileGenerationId() < 1 || input.Identity.ProfileGeneration == 0 {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "frozen profile generation is missing"
			return channel.startAckReply(envelope, ack)
		}
		manifest, _, inspectErr := channel.profiles.Inspect(request.GetIdentityId(), input.Identity.ProfileGeneration)
		// A revision-change probe deliberately runs the new immutable probe
		// configuration against the current generation, whose manifest remains
		// bound to the revision that published it. Identity/path integrity and
		// Chromium compatibility are still verified by Inspect plus this check.
		if inspectErr != nil || manifest.ChromiumRevision != channel.Config.ChromiumRevision {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "published profile is unavailable or incompatible"
			return channel.startAckReply(envelope, ack)
		}
		path, pathErr := channel.profiles.GenerationPath(request.GetIdentityId(), input.Identity.ProfileGeneration)
		if pathErr != nil {
			ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE, "published profile path is invalid"
			return channel.startAckReply(envelope, ack)
		}
		_, err = channel.browser.StartWithProfile(context.Background(), request.GetOperationId(), input.Identity.StartURL, path)
	default:
		ack.RejectReason, ack.Detail = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED, "browser operation kind is not implemented"
		return channel.startAckReply(envelope, ack)
	}
	if err != nil {
		if errors.Is(err, browser.ErrBusy) {
			ack.RejectReason = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY
		} else {
			ack.RejectReason = runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INTERNAL
		}
		ack.Detail = "browser start failed"
		return channel.startAckReply(envelope, ack)
	}
	channel.started[request.GetOperationId()] = request
	ack.Accepted, ack.StartedAt = true, timestamppb.Now()
	channel.startAcks[request.GetOperationId()] = ack
	return channel.startAckReply(envelope, ack)
}

func (channel *Channel) startAckReply(envelope *runtimev1.ControlEnvelope, ack *runtimev1.StartBrowserOperationAck) *runtimev1.ControlEnvelope {
	reply := channel.reply(envelope)
	reply.Msg = &runtimev1.ControlEnvelope_StartBrowserOperationAck{StartBrowserOperationAck: ack}
	return reply
}

func (channel *Channel) publishResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.PublishBrowserProfile) *runtimev1.ControlEnvelope {
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
	profilePath, err := channel.browser.DetachProfile(request.GetOperationId())
	if err != nil {
		probe.Result, probe.ReasonCode = runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE, "browser_detach_failed"
		result.ProbeResult, result.RejectReason, result.Detail = probe, runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE, "cannot safely publish browser profile"
		return reply()
	}
	channel.tunnelMu.Lock()
	if cancel := channel.tunnelCancels[request.GetOperationId()]; cancel != nil {
		cancel()
	}
	channel.tunnelMu.Unlock()
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
	delete(channel.started, request.GetOperationId())
	return reply()
}

func (channel *Channel) stopResponse(envelope *runtimev1.ControlEnvelope, request *runtimev1.StopBrowserOperation) *runtimev1.ControlEnvelope {
	channel.operationMu.Lock()
	if previous := channel.stopAcks[request.GetOperationId()]; previous != nil {
		channel.operationMu.Unlock()
		reply := channel.reply(envelope)
		reply.Msg = &runtimev1.ControlEnvelope_StopBrowserOperationAck{StopBrowserOperationAck: previous}
		return reply
	}
	channel.operationMu.Unlock()
	channel.tunnelMu.Lock()
	if cancel := channel.tunnelCancels[request.GetOperationId()]; cancel != nil {
		cancel()
	}
	channel.tunnelMu.Unlock()
	err := channel.browser.Stop(request.GetOperationId())
	channel.operationMu.Lock()
	delete(channel.started, request.GetOperationId())
	channel.operationMu.Unlock()
	outcome := runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED
	failure := runtimev1.BrowserCleanupFailureCode_BROWSER_CLEANUP_FAILURE_CODE_UNSPECIFIED
	if err != nil {
		outcome, failure = runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_FAILED, runtimev1.BrowserCleanupFailureCode_BROWSER_CLEANUP_FAILURE_CODE_INTERNAL
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%t", request.GetOperationId(), err == nil)))
	ack := &runtimev1.StopBrowserOperationAck{OperationId: request.GetOperationId(), StoppedAt: timestamppb.Now(), CleanupOutcome: outcome, ProcessStopped: err == nil, TunnelClosed: err == nil, TraceStagingDeleted: err == nil, TemporaryProfileDeleted: err == nil, CleanupStateHash: sum[:], FailureCode: failure}
	channel.operationMu.Lock()
	channel.stopAcks[request.GetOperationId()] = ack
	channel.operationMu.Unlock()
	reply := channel.reply(envelope)
	reply.Msg = &runtimev1.ControlEnvelope_StopBrowserOperationAck{StopBrowserOperationAck: ack}
	return reply
}
