package runtimev1

import (
	"crypto/sha256"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestBrowserExplorationActionEnvelopeRoundTrip(t *testing.T) {
	canonical := []byte(`{"action":"read","sessionId":"session-1"}`)
	digest := sha256.Sum256(canonical)
	want := &ControlEnvelope{
		MessageId:       7,
		ConnectionEpoch: 3,
		CorrelationId:   99,
		BootId:          "lintel-boot",
		Msg: &ControlEnvelope_ExecuteBrowserExplorationAction{ExecuteBrowserExplorationAction: &ExecuteBrowserExplorationAction{
			OperationId: 41, ChildAttemptId: 42, ParentAttemptId: 11, ToolCallId: 17,
			Input: &BrowserSubExecutionInput{SchemaKind: "browser_tool_v1", CanonicalJson: canonical, ContentDigest: digest[:]},
		}},
	}
	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := &ControlEnvelope{}
	if err := proto.Unmarshal(encoded, got); err != nil {
		t.Fatal(err)
	}
	action := got.GetExecuteBrowserExplorationAction()
	if action == nil || action.GetOperationId() != 41 || action.GetChildAttemptId() != 42 || action.GetToolCallId() != 17 {
		t.Fatalf("round-trip action = %#v", action)
	}
	if string(action.GetInput().GetCanonicalJson()) != string(canonical) || string(action.GetInput().GetContentDigest()) != string(digest[:]) {
		t.Fatalf("input binding changed: %#v", action.GetInput())
	}
}

func TestBrowserExplorationCompletionProbeRoundTrip(t *testing.T) {
	observed := &AuthenticationProbeObservation{
		Phase:                 AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_COMPLETION,
		JourneyId:             "authentication.url-prefix.v1",
		JourneyVersion:        1,
		JourneyCatalogDigest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		JourneyCatalogVersion: "v1",
		Result:                AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED,
		ObservedAt:            nil,
	}
	want := &BrowserExplorationActionResult{OperationId: 1, ChildAttemptId: 2, ToolCallId: 3, CompletionProbe: observed}
	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := &BrowserExplorationActionResult{}
	if err := proto.Unmarshal(encoded, got); err != nil {
		t.Fatal(err)
	}
	if got.GetCompletionProbe() == nil || got.GetCompletionProbe().GetPhase() != AuthenticationProbePhase_AUTHENTICATION_PROBE_PHASE_COMPLETION || got.GetCompletionProbe().GetResult() != AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED {
		t.Fatalf("completion probe was not retained: %#v", got.GetCompletionProbe())
	}
}
