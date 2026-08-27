package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/browser/exploration"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func browserActionRequest(operationID, childID int64, body string) *runtimev1.ExecuteBrowserExplorationAction {
	digest := sha256.Sum256([]byte(body))
	return &runtimev1.ExecuteBrowserExplorationAction{OperationId: operationID, ChildAttemptId: childID, ParentAttemptId: 8, ToolCallId: 9, Input: &runtimev1.BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: []byte(body), ContentDigest: digest[:]}}
}

func TestExplorationActionRouteCachesCanonicalChildResult(t *testing.T) {
	var calls atomic.Int32
	results := make(chan *runtimev1.ControlEnvelope, 2)
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		started:     map[int64]*runtimev1.StartBrowserOperation{12: {OperationId: 12, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION}},
		stopBrowser: func(int64) error { return nil },
		executeBrowserAction: func(_ context.Context, operationID int64, action exploration.Action) browser.ExplorationResult {
			calls.Add(1)
			if operationID != 12 || action.Name != "read" {
				t.Fatalf("unexpected invocation op=%d action=%s", operationID, action.Name)
			}
			return browser.ExplorationResult{Success: true, Payload: map[string]any{"outcome": "success", "action": "read", "sessionId": "12", "observation": minimalObservation()}}
		},
	}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { results <- envelope; return nil }
	request := browserActionRequest(12, 15, `{"action":"read","sessionId":"12","locator":{"kind":"role","role":"button"}}`)
	envelope := &runtimev1.ControlEnvelope{MessageId: 3, BootId: "lintel", ConnectionEpoch: 4}
	channel.handleExplorationAction(envelope, request)
	first := receiveExplorationResult(t, results)
	if !first.GetBrowserExplorationActionResult().GetSuccess() || first.GetBrowserExplorationActionResult().GetPayload().GetSchemaKind() != "browser_tool_result_v1" {
		t.Fatalf("unexpected first result: %#v", first.GetBrowserExplorationActionResult())
	}
	if err := attempt.ValidateToolResultPayload(first.GetBrowserExplorationActionResult().GetPayload().GetSchemaKind(), first.GetBrowserExplorationActionResult().GetPayload().GetCanonicalJson()); err != nil {
		t.Fatalf("canonical browser result violates frozen schema: %v", err)
	}
	channel.handleExplorationAction(&runtimev1.ControlEnvelope{MessageId: 4, BootId: "lintel", ConnectionEpoch: 4}, request)
	second := receiveExplorationResult(t, results)
	if calls.Load() != 1 || string(first.GetBrowserExplorationActionResult().GetPayload().GetCanonicalJson()) != string(second.GetBrowserExplorationActionResult().GetPayload().GetCanonicalJson()) {
		t.Fatalf("child retry was not cached: calls=%d first=%#v second=%#v", calls.Load(), first.GetBrowserExplorationActionResult(), second.GetBrowserExplorationActionResult())
	}
}

func TestAcceptedSupersededTerminalAckClearsLintelResultCache(t *testing.T) {
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, SessionTerminal: true, ErrorCode: "BrowserCrashed", ErrorDetail: "late terminal replay"}
	channel := &Channel{
		explorationResults:  map[int64]*runtimev1.BrowserExplorationActionResult{15: result},
		explorationChildren: map[int64]int64{12: 15},
		explorationTraces:   map[int64][]explorationTraceEntry{12: {}},
	}
	// Quoin recovery has already made the action/child/tool/operation terminal.
	// Its accepted acknowledgement must bind the late result's exact digest, then
	// remove the Lintel retry cache without requiring that Quoin rewrote authority.
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{
		OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true, ResultDigest: explorationResultDigest(t, result),
	})
	if channel.explorationResults[15] != nil || channel.explorationChildren[12] != 0 {
		t.Fatalf("accepted recovery-superseded acknowledgement retained terminal cache: results=%#v children=%#v", channel.explorationResults, channel.explorationChildren)
	}
}

func TestExplorationActionExactSessionMismatchIsTerminal(t *testing.T) {
	channel := &Channel{}
	result := channel.runExplorationAction(browserActionRequest(12, 15, `{"action":"read","sessionId":"13","locator":{"kind":"role","role":"button"}}`))
	if result.GetSuccess() || !result.GetSessionTerminal() || result.GetErrorCode() != "ProtocolError" {
		t.Fatalf("result=%#v", result)
	}
}

func TestExplorationTraceExcludesFillValue(t *testing.T) {
	channel := &Channel{explorationTraces: make(map[int64][]explorationTraceEntry)}
	channel.appendTrace(12, traceEntry("fill", "success", "", map[string]any{"action": "fill", "value": "super-secret"}, 0))
	trace := string(channel.traceForClose(12, "close_session", map[string]any{"action": "close_session"}))
	if strings.Contains(trace, "super-secret") || strings.Contains(trace, `"value"`) || !strings.Contains(trace, "payloadSha256") {
		t.Fatalf("trace leaked fill input or lacks metadata digest: %s", trace)
	}
}

func TestCrashTraceReplacesUncommittedCompleteSeal(t *testing.T) {
	channel := &Channel{explorationTraces: map[int64][]explorationTraceEntry{12: {traceEntry("read", "success", "", nil, 0)}}}
	_ = channel.traceForClose(12, "close_session", map[string]any{"action": "close_session"})
	trace := string(channel.traceForCrash(12))
	if !strings.Contains(trace, `"incomplete":true`) {
		t.Fatalf("uncommitted complete seal was reused for crash trace: %s", trace)
	}
	if channel.explorationTraceSeals[12].complete {
		t.Fatal("crash trace retained complete integrity")
	}
}

func TestScreenshotUploadFailureIsArtifactCommitFailure(t *testing.T) {
	channel := &Channel{executeBrowserAction: func(context.Context, int64, exploration.Action) browser.ExplorationResult {
		return browser.ExplorationResult{Success: true, Screenshot: []byte("png"), Payload: map[string]any{"outcome": "success", "action": "screenshot", "sessionId": "12", "observation": minimalObservation()}}
	}}
	result := channel.runExplorationAction(browserActionRequest(12, 15, `{"action":"screenshot","sessionId":"12"}`))
	if result.GetSuccess() || !result.GetSessionTerminal() || result.GetErrorCode() != "ArtifactCommitFailed" {
		t.Fatalf("result=%#v", result)
	}
	entries := channel.explorationTraces[12]
	if len(entries) != 1 || entries[0].Action != "screenshot" || entries[0].Outcome != "error" || entries[0].ErrorCode != "ArtifactCommitFailed" {
		t.Fatalf("failed screenshot was not retained in trace: %#v", entries)
	}
}

func explorationResultDigest(t *testing.T, result *runtimev1.BrowserExplorationActionResult) []byte {
	t.Helper()
	body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return sum[:]
}

func receiveExplorationResult(t *testing.T, results <-chan *runtimev1.ControlEnvelope) *runtimev1.ControlEnvelope {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exploration result")
		return nil
	}
}

func minimalObservation() map[string]any {
	return map[string]any{"version": 1, "url": "https://example.test/", "origin": "https://example.test", "title": "Example", "pages": []any{map[string]any{"pageId": "page-12", "url": "https://example.test/", "origin": "https://example.test", "title": "Example", "current": true}}, "visibleText": "", "accessibilityText": "", "elements": []any{}, "events": []any{}, "originalSizeBytes": 0, "truncated": false}
}

func TestExplorationResultReconnectResendsUntilDurableAck(t *testing.T) {
	results := make(chan *runtimev1.ControlEnvelope, 2)
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, ErrorCode: "Cancelled", ErrorDetail: "parent cancelled", SessionTerminal: true}
	channel := &Channel{bootID: "lintel", epoch: 4, explorationResults: map[int64]*runtimev1.BrowserExplorationActionResult{15: result}}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { results <- envelope; return nil }
	// Same-boot reconnect replays the exact unacknowledged cancellation result.
	channel.resendPendingExplorationResults()
	got := receiveExplorationResult(t, results).GetBrowserExplorationActionResult()
	if got.GetChildAttemptId() != 15 || got.GetErrorCode() != "Cancelled" {
		t.Fatalf("replay=%#v", got)
	}
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true, ResultDigest: explorationResultDigest(t, result)})
	channel.resendPendingExplorationResults()
	select {
	case envelope := <-results:
		t.Fatalf("acked cancellation replayed: %#v", envelope)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestExplorationResultAckRequiresExactReplayDigest(t *testing.T) {
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, ErrorCode: "Cancelled", ErrorDetail: "parent cancelled", SessionTerminal: true}
	channel := &Channel{explorationResults: map[int64]*runtimev1.BrowserExplorationActionResult{15: result}}
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true, ResultDigest: make([]byte, sha256.Size)})
	if channel.explorationResults[15] == nil {
		t.Fatal("mismatched acknowledgement digest discarded replay")
	}
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true, ResultDigest: explorationResultDigest(t, result)})
	if channel.explorationResults[15] != nil {
		t.Fatal("exact acknowledgement digest did not discard replay")
	}
}

func TestExplorationTerminalCASHasOneTraceOwner(t *testing.T) {
	channel := &Channel{completed: map[int64]*runtimev1.CompleteBrowserOperation{}, completing: map[int64]bool{}}
	if !channel.claimExplorationTerminal(12, 15) || !channel.holdsExplorationTerminal(12, 15) {
		t.Fatal("first terminal writer did not acquire trace ownership")
	}
	if channel.claimExplorationTerminal(12, 16) {
		t.Fatal("second terminal writer acquired the same operation trace")
	}
	channel.releaseExplorationTerminal(12, 16)
	if !channel.holdsExplorationTerminal(12, 15) {
		t.Fatal("non-owner released trace ownership")
	}
	channel.releaseExplorationTerminal(12, 15)
	if channel.holdsExplorationTerminal(12, 15) {
		t.Fatal("trace ownership remained after terminal release")
	}
}

func TestIdleExplorationCancellationReturnsOperationCompletion(t *testing.T) {
	frames := make(chan *runtimev1.ControlEnvelope, 1)
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		started: map[int64]*runtimev1.StartBrowserOperation{12: {OperationId: 12, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION}},
		stopBrowser: func(operationID int64) error {
			if operationID != 12 {
				t.Fatalf("stopped operation=%d", operationID)
			}
			return nil
		},
	}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { frames <- envelope; return nil }
	channel.handleExplorationCancellation(&runtimev1.ControlEnvelope{MessageId: 3, BootId: "lintel", ConnectionEpoch: 4}, &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ParentAttemptId: 8})
	select {
	case completion := <-frames:
		result := completion.GetCompleteBrowserOperation()
		if result == nil || result.GetOperationId() != 12 || result.GetTerminalReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_ARTIFACT_COMMIT_FAILED || result.GetOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED {
			t.Fatalf("idle cancel completion=%#v", result)
		}
		if len(result.GetResultDigest()) != sha256.Size {
			t.Fatalf("idle cancel lacks replay digest: %#v", result)
		}
		// A duplicated Cancel after the first completion must replay that exact
		// completion rather than silently relying on a later reconnect.
		channel.handleExplorationCancellation(&runtimev1.ControlEnvelope{MessageId: 4, BootId: "lintel", ConnectionEpoch: 4}, &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ParentAttemptId: 8})
		replayed := receiveExplorationCompletion(t, frames).GetCompleteBrowserOperation()
		if replayed == nil || string(replayed.GetResultDigest()) != string(result.GetResultDigest()) {
			t.Fatalf("idle cancellation replay=%#v", replayed)
		}
	case <-time.After(time.Second):
		t.Fatal("idle cancellation did not send operation completion")
	}
}

func receiveExplorationCompletion(t *testing.T, frames <-chan *runtimev1.ControlEnvelope) *runtimev1.ControlEnvelope {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exploration completion")
		return nil
	}
}

func TestExplorationCancellationWinsAgainstLateNormalActionResult(t *testing.T) {
	results := make(chan *runtimev1.ControlEnvelope, 3)
	started := make(chan struct{})
	release := make(chan struct{})
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		started:     map[int64]*runtimev1.StartBrowserOperation{12: {OperationId: 12, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION}},
		stopBrowser: func(int64) error { return nil },
		executeBrowserAction: func(ctx context.Context, _ int64, _ exploration.Action) browser.ExplorationResult {
			close(started)
			<-ctx.Done()
			<-release
			return browser.ExplorationResult{Success: true, Payload: map[string]any{"outcome": "success", "action": "read", "sessionId": "12", "observation": minimalObservation()}}
		},
	}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { results <- envelope; return nil }
	request := browserActionRequest(12, 15, `{"action":"read","sessionId":"12","locator":{"kind":"role","role":"button"}}`)
	envelope := &runtimev1.ControlEnvelope{MessageId: 3, BootId: "lintel", ConnectionEpoch: 4}
	channel.handleExplorationAction(envelope, request)
	<-started
	cancelReturned := make(chan struct{})
	go func() {
		channel.handleExplorationCancellation(envelope, &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9})
		close(cancelReturned)
	}()
	// The cancellation trace cannot be sealed while the cancelled action still
	// owns the executor. Releasing the action establishes the quiescence boundary.
	select {
	case <-cancelReturned:
		t.Fatal("cancellation returned before active action quiesced")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-cancelReturned:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not finish after action quiesced")
	}
	var terminal *runtimev1.BrowserExplorationActionResult
	deadline := time.After(time.Second)
	for terminal == nil {
		select {
		case frame := <-results:
			if result := frame.GetBrowserExplorationActionResult(); result != nil {
				terminal = result
			}
		case <-deadline:
			t.Fatal("timed out waiting for cancellation result")
		}
	}
	if terminal.GetSuccess() || terminal.GetErrorCode() == "" || !terminal.GetSessionTerminal() {
		t.Fatalf("unexpected cancellation result: %#v", terminal)
	}
	select {
	case frame := <-results:
		if result := frame.GetBrowserExplorationActionResult(); result != nil && result.GetSuccess() {
			t.Fatalf("late normal action result won cancellation CAS: %#v", result)
		}
	case <-time.After(40 * time.Millisecond):
	}
}

func TestCancelledCloseReleasesItsTerminalClaim(t *testing.T) {
	channel := &Channel{
		explorationCancelling: map[int64]bool{15: true},
		executeBrowserAction: func(context.Context, int64, exploration.Action) browser.ExplorationResult {
			return browser.ExplorationResult{Success: true, Payload: map[string]any{"outcome": "success", "action": "read", "sessionId": "12", "observation": minimalObservation()}}
		},
	}
	if !channel.claimExplorationTerminal(12, 15) {
		t.Fatal("setup terminal claim")
	}
	channel.executeExplorationAction(context.Background(), &runtimev1.ControlEnvelope{}, browserActionRequest(12, 15, `{"action":"read","sessionId":"12","locator":{"kind":"role","role":"button"}}`))
	if channel.holdsExplorationTerminal(12, 15) {
		t.Fatal("cancelled action leaked close terminal claim")
	}
}

func TestNormalActionAckPreservesContinuousTraceUntilTerminalResult(t *testing.T) {
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Success: true}
	channel := &Channel{
		explorationResults:  map[int64]*runtimev1.BrowserExplorationActionResult{15: result},
		explorationChildren: map[int64]int64{12: 15},
		explorationTraces:   map[int64][]explorationTraceEntry{12: {{Action: "read"}}},
	}
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true, ResultDigest: explorationResultDigest(t, result)})
	if len(channel.explorationResults) != 0 || len(channel.explorationTraces[12]) != 1 || channel.explorationChildren[12] != 15 {
		t.Fatalf("normal action acknowledgement discarded session trace: %#v", channel)
	}
	terminal := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 16, ToolCallId: 10, SessionTerminal: true}
	channel.explorationResults[16] = terminal
	channel.explorationChildren[12] = 16
	channel.acknowledgeExplorationResult(&runtimev1.BrowserExplorationActionResultAck{OperationId: 12, ChildAttemptId: 16, ToolCallId: 10, Accepted: true, ResultDigest: explorationResultDigest(t, terminal)})
	if len(channel.explorationTraces) != 0 || len(channel.explorationChildren) != 0 {
		t.Fatalf("terminal action acknowledgement retained session trace: %#v", channel)
	}
}

func TestForgetTerminalOperationReleasesAllBoundedCaches(t *testing.T) {
	channel := &Channel{
		started: map[int64]*runtimev1.StartBrowserOperation{12: {}}, startAcks: map[int64]*runtimev1.StartBrowserOperationAck{12: {}},
		completed: map[int64]*runtimev1.CompleteBrowserOperation{12: {}}, completing: map[int64]bool{12: true},
		stopAcks: map[int64]*runtimev1.StopBrowserOperationAck{12: {}}, published: map[int64]*runtimev1.PublishBrowserProfileResult{12: {}},
		explorationChildren: map[int64]int64{12: 15}, explorationTraces: map[int64][]explorationTraceEntry{12: {{Action: "read"}}},
	}
	channel.forgetTerminalOperation(12)
	if len(channel.started) != 0 || len(channel.startAcks) != 0 || len(channel.completed) != 0 || len(channel.completing) != 0 || len(channel.published) != 0 || len(channel.explorationChildren) != 0 || len(channel.explorationTraces) != 0 {
		t.Fatalf("terminal operation cache leaked: %#v", channel)
	}
	if channel.stopAcks[12] == nil {
		t.Fatal("same-boot Stop tombstone was discarded")
	}
}

func TestExplorationCancellationReplacesUnacknowledgedCachedNormalResult(t *testing.T) {
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		stopBrowser: func(int64) error { return nil },
		explorationResults: map[int64]*runtimev1.BrowserExplorationActionResult{15: {
			OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9,
			Success: true, Payload: &runtimev1.ResultPayload{SchemaKind: "browser_tool_result_v1"},
		}},
	}
	channel.handleExplorationCancellation(&runtimev1.ControlEnvelope{MessageId: 3, BootId: "lintel", ConnectionEpoch: 4}, &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9})
	got := channel.explorationResults[15]
	if got == nil || got.GetSuccess() || !got.GetSessionTerminal() || got.GetErrorCode() == "" {
		t.Fatalf("unacknowledged normal result survived cancellation: %#v", got)
	}
}

func TestActiveRunningExplorationChildDoesNotReusePriorActionCapability(t *testing.T) {
	channel := &Channel{
		explorationChildren: map[int64]int64{12: 15},
		explorationRunning:  map[int64]bool{15: true},
	}
	if got := channel.activeRunningExplorationChild(12); got != 15 {
		t.Fatalf("active child=%d want=15", got)
	}
	channel.explorationRunning[15] = false
	if got := channel.activeRunningExplorationChild(12); got != 0 {
		t.Fatalf("stale child capability reused: %d", got)
	}
}

func TestCloseTraceIsImmutableAcrossRetry(t *testing.T) {
	channel := &Channel{explorationTraces: map[int64][]explorationTraceEntry{12: {{Action: "read", At: "2026-01-01T00:00:00Z"}}}}
	first := channel.traceForClose(12, "close_session", map[string]any{"action": "close_session"})
	second := channel.traceForClose(12, "close_session", map[string]any{"action": "close_session", "unexpected": "ignored"})
	if string(first) != string(second) {
		t.Fatalf("close trace retry changed immutable bytes:\nfirst=%s\nsecond=%s", first, second)
	}
	if got := channel.traceIntegrityFor(12); got != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
		t.Fatalf("close trace integrity=%s", got)
	}
}

func TestCancellationSupersedesUnacknowledgedCloseTraceSeal(t *testing.T) {
	channel := &Channel{explorationTraces: map[int64][]explorationTraceEntry{12: {{Action: "close_session", At: "2026-01-01T00:00:00Z"}}}}
	_ = channel.traceForClose(12, "close_session", map[string]any{"action": "close_session"})
	if got := channel.traceIntegrityFor(12); got != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
		t.Fatalf("pre-cancellation trace integrity=%s", got)
	}
	channel.discardUncommittedExplorationTraceSeal(12)
	body := channel.traceForClose(12, "cancelled", nil)
	if got := channel.traceIntegrityFor(12); got != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_INCOMPLETE || !bytes.Contains(body, []byte(`"incomplete":true`)) {
		t.Fatalf("cancellation reused complete close trace integrity=%s body=%s", got, body)
	}
}

func TestCancellationPreservesCommittedCloseTraceSeal(t *testing.T) {
	channel := &Channel{explorationTraces: map[int64][]explorationTraceEntry{12: {{Action: "close_session", At: "2026-01-01T00:00:00Z"}}}}
	body := channel.traceForClose(12, "close_session", map[string]any{"action": "close_session"})
	digest := sha256.Sum256(body)
	channel.markExplorationTraceCommitted(12, 91, digest[:])
	channel.discardUncommittedExplorationTraceSeal(12)
	seal, ok := channel.committedExplorationTrace(12)
	if !ok || seal.artifactID != 91 || !seal.complete || string(seal.digest) != string(digest[:]) {
		t.Fatalf("committed close trace was not retained: %#v ok=%t", seal, ok)
	}
	if got := channel.traceIntegrityFor(12); got != runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
		t.Fatalf("committed close trace integrity=%s", got)
	}
}

func TestTraceStagingIsConcreteAndRemovedOnlyByCleanup(t *testing.T) {
	channel := &Channel{Config: ChannelConfig{StateDirectory: t.TempDir()}, traceStaging: make(map[int64]string)}
	if err := channel.stageExplorationTrace(12, []byte(`{"entries":[]}`)); err != nil {
		t.Fatal(err)
	}
	path := channel.traceStaging[12]
	if path == "" {
		t.Fatal("trace staging path was not recorded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staged trace was not written: %v", err)
	}
	if err := channel.deleteTraceStaging(12); err != nil {
		t.Fatal(err)
	}
	if channel.traceStaging[12] != "" {
		t.Fatal("trace staging path remained after cleanup")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staged trace remained on disk: %v", err)
	}
}

func TestCleanupExplorationTraceStagingRemovesOnlyPriorBootParts(t *testing.T) {
	directory := t.TempDir()
	staging := directory + "/browser-trace-staging"
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	part := staging + "/12.json.part"
	legacy := staging + "/13.json"
	keep := staging + "/keep.txt"
	if err := os.WriteFile(part, []byte("sensitive trace metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("old sensitive trace metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupExplorationTraceStaging(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("prior boot trace staging remains: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy prior boot trace staging remains: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-staging file was removed: %v", err)
	}
}

func TestCancellationWaitsForOwnTerminalSnapshotThenWins(t *testing.T) {
	frames := make(chan *runtimev1.ControlEnvelope, 1)
	done := make(chan struct{})
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		completed: map[int64]*runtimev1.CompleteBrowserOperation{}, completing: map[int64]bool{},
		explorationTerminalChildren: map[int64]int64{},
		explorationCancelling:       map[int64]bool{}, explorationDone: map[int64]chan struct{}{15: done},
		explorationResults: map[int64]*runtimev1.BrowserExplorationActionResult{},
		stopBrowser:        func(int64) error { return nil },
	}
	if !channel.claimExplorationTerminal(12, 15) {
		t.Fatal("setup terminal claim failed")
	}
	channel.controlSend = func(frame *runtimev1.ControlEnvelope) error {
		if frame.GetBrowserExplorationActionResult() != nil {
			frames <- frame
		}
		return nil
	}
	request := &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9}
	returned := make(chan struct{})
	go func() {
		channel.handleExplorationCancellation(&runtimev1.ControlEnvelope{}, request)
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("cancellation escaped while its action still owned terminal snapshot")
	case <-time.After(20 * time.Millisecond):
	}
	channel.releaseExplorationTerminal(12, 15)
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not take ownership after action released terminal snapshot")
	}
	got := receiveExplorationResult(t, frames).GetBrowserExplorationActionResult()
	if got.GetSuccess() || got.GetErrorCode() == "" || !got.GetSessionTerminal() {
		t.Fatalf("cancellation did not replace terminal snapshot: %#v", got)
	}
}

func TestTraceStagingAppendsActionMetadataBeforeTerminalSeal(t *testing.T) {
	channel := &Channel{Config: ChannelConfig{StateDirectory: t.TempDir()}, traceStaging: make(map[int64]string)}
	channel.appendTrace(12, explorationTraceEntry{Action: "read", At: "2026-01-01T00:00:00Z"})
	channel.appendTrace(12, explorationTraceEntry{Action: "click", At: "2026-01-01T00:00:01Z"})
	path := channel.traceStaging[12]
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"action":"read"`)) || !bytes.Contains(body, []byte(`"action":"click"`)) {
		t.Fatalf("staging did not retain continuous action entries: %s", body)
	}
}

func TestCompletionDigestBindsCanonicalCompletion(t *testing.T) {
	completion := &runtimev1.CompleteBrowserOperation{OperationId: 12, Outcome: runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED, TerminalReason: runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED, EndedAt: timestamppb.Now()}
	completion.ResultDigest = canonicalCompletionDigest(completion)
	first := append([]byte(nil), completion.ResultDigest...)
	completion.TerminalReason = runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_CANCELLED
	if bytes.Equal(first, canonicalCompletionDigest(completion)) {
		t.Fatal("completion digest did not bind terminal reason")
	}
}

func TestLateCancellationReplacesCommittedCompleteCloseWithIncompleteTerminal(t *testing.T) {
	frames := make(chan *runtimev1.ControlEnvelope, 3)
	result := &runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9, Success: true, SessionTerminal: true, TraceArtifactId: 77, TraceIntegrity: runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE, TraceDigest: make([]byte, sha256.Size)}
	channel := &Channel{
		bootID: "lintel", epoch: 4,
		stopBrowser:           func(int64) error { return nil },
		explorationResults:    map[int64]*runtimev1.BrowserExplorationActionResult{15: result},
		explorationTraceSeals: map[int64]explorationTraceSeal{12: {body: []byte(`{"version":1}`), complete: true, artifactID: 77, digest: make([]byte, sha256.Size)}},
	}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { frames <- envelope; return nil }
	envelope := &runtimev1.ControlEnvelope{MessageId: 3, BootId: "lintel", ConnectionEpoch: 4}
	channel.handleExplorationCancellation(envelope, &runtimev1.CancelBrowserExplorationAction{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9})
	var terminal *runtimev1.BrowserExplorationActionResult
	for len(frames) > 0 {
		if got := (<-frames).GetBrowserExplorationActionResult(); got != nil {
			terminal = got
		}
	}
	if terminal == nil || terminal.GetSuccess() || terminal.GetTraceIntegrity() == runtimev1.BrowserTraceIntegrity_BROWSER_TRACE_INTEGRITY_COMPLETE {
		t.Fatalf("cancellation retained complete close result: %#v", terminal)
	}
	if channel.explorationResults[15] == result {
		t.Fatal("late cancellation retained committed normal close result")
	}
	if seal, ok := channel.committedExplorationTrace(12); !ok || !seal.complete || seal.artifactID != 77 {
		t.Fatalf("historical complete seal was changed: %#v committed=%t", seal, ok)
	}
}

func TestPendingTerminalClaimReconnectReplaysExactIdentity(t *testing.T) {
	frames := make(chan *runtimev1.ControlEnvelope, 2)
	claim := &runtimev1.BrowserExplorationTerminalClaim{OperationId: 12, ChildAttemptId: 15, ParentAttemptId: 8, ToolCallId: 9}
	channel := &Channel{
		bootID: "lintel", epoch: 5,
		explorationClaims:    map[int64]*runtimev1.BrowserExplorationTerminalClaim{15: claim},
		explorationClaimAcks: map[int64]chan *runtimev1.BrowserExplorationTerminalClaimAck{15: make(chan *runtimev1.BrowserExplorationTerminalClaimAck, 1)},
	}
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error { frames <- envelope; return nil }
	channel.resendPendingExplorationClaims()
	got := receiveExplorationResult(t, frames).GetBrowserExplorationTerminalClaim()
	if got == nil || got.GetOperationId() != claim.GetOperationId() || got.GetChildAttemptId() != claim.GetChildAttemptId() || got.GetParentAttemptId() != claim.GetParentAttemptId() || got.GetToolCallId() != claim.GetToolCallId() {
		t.Fatalf("claim replay identity=%#v", got)
	}
	// An Ack for another operation must not release the bound waiter.
	channel.acknowledgeExplorationTerminalClaim(&runtimev1.BrowserExplorationTerminalClaimAck{OperationId: 13, ChildAttemptId: 15, ToolCallId: 9, Accepted: true})
	select {
	case <-channel.explorationClaimAcks[15]:
		t.Fatal("mismatched claim Ack released waiter")
	default:
	}
	channel.acknowledgeExplorationTerminalClaim(&runtimev1.BrowserExplorationTerminalClaimAck{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Accepted: true})
	select {
	case ack := <-channel.explorationClaimAcks[15]:
		if !ack.GetAccepted() {
			t.Fatalf("claim ack=%#v", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("matching claim Ack was not delivered")
	}
}

func TestTerminalFailureClearsConstructedSuccessPayload(t *testing.T) {
	payload, err := canonicalBrowserPayload(map[string]any{"outcome": "success", "action": "close_session", "sessionId": "12", "observation": minimalObservation()})
	if err != nil {
		t.Fatal(err)
	}
	failed := explorationTerminalFailure(&runtimev1.BrowserExplorationActionResult{OperationId: 12, ChildAttemptId: 15, ToolCallId: 9, Success: true, Payload: payload}, "BrowserCrashed", "stop failed")
	if failed.GetSuccess() || failed.GetPayload() != nil || !failed.GetSessionTerminal() || failed.GetTerminalOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED || failed.GetTerminalReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED {
		t.Fatalf("terminal failure retained success state: %#v", failed)
	}
}

func TestFailedTraceStagingRemainsRegisteredAndCannotClaimCleanup(t *testing.T) {
	stateDirectory := t.TempDir()
	blocked := stateDirectory + "/not-a-directory"
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	channel := &Channel{Config: ChannelConfig{StateDirectory: blocked}, traceStaging: map[int64]string{}}
	if err := channel.stageExplorationTrace(12, []byte(`{"entries":[]}`)); err == nil {
		t.Fatal("staging through a file path unexpectedly succeeded")
	}
	if channel.traceStaging[12] == "" {
		t.Fatal("failed staging did not retain its cleanup obligation")
	}
	if err := channel.deleteTraceStaging(12); err == nil {
		t.Fatal("missing registered staging file was falsely accepted as cleaned")
	}
}

func TestCrashUsesRegisteredActionCapabilityAfterRunningFlagChanges(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	channel := &Channel{
		started:   map[int64]*runtimev1.StartBrowserOperation{12: {OperationId: 12, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION}},
		completed: map[int64]*runtimev1.CompleteBrowserOperation{}, completing: map[int64]bool{},
		explorationActionCapabilities: map[int64]int64{12: 15},
		explorationChildren:           map[int64]int64{12: 15}, explorationRunning: map[int64]bool{15: false},
		explorationCancels: map[int64]context.CancelFunc{15: func() { cancelled <- struct{}{} }},
		explorationDone:    map[int64]chan struct{}{15: done},
	}
	returned := make(chan struct{})
	go func() { channel.browserCrashed(12); close(returned) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("crash did not use atomically registered child capability")
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("crash did not finish after child quiesced")
	}
}
