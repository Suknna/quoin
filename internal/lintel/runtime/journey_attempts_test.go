package runtime

// Journey dispatch adjudication tests (T23): the frozen input must bind the
// already started journey operation exactly; catalog/profile revision
// mismatches are rejected before any process side effect.

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
)

type recordedEnvelope struct {
	envelope *runtimev1.ControlEnvelope
	kind     string
}

type envelopeRecorder struct {
	mu    sync.Mutex
	items []recordedEnvelope
}

func (recorder *envelopeRecorder) record(envelope *runtimev1.ControlEnvelope) {
	kind := "other"
	switch envelope.Msg.(type) {
	case *runtimev1.ControlEnvelope_AttemptAccept:
		kind = "accept"
	case *runtimev1.ControlEnvelope_AttemptReject:
		kind = "reject"
	case *runtimev1.ControlEnvelope_ResultProposal:
		kind = "result"
	case *runtimev1.ControlEnvelope_CancelAck:
		kind = "cancel"
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.items = append(recorder.items, recordedEnvelope{envelope, kind})
}

func (recorder *envelopeRecorder) snapshot() []recordedEnvelope {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]recordedEnvelope(nil), recorder.items...)
}

func newJourneyTestChannel(t *testing.T) (*Channel, *envelopeRecorder) {
	t.Helper()
	directory := t.TempDir()
	var manager *browser.Manager
	if _, err := exec.LookPath("Xvfb"); err == nil {
		if built, buildErr := browser.NewManager(browser.Config{StateDirectory: directory, Capacity: 2}); buildErr == nil {
			manager = built
		}
	}
	channel := &Channel{
		Config: ChannelConfig{StateDirectory: directory, ChromiumRevision: "Chromium-test", Browser: manager},
		bootID: "boot-j", epoch: 3,
		browser:          manager,
		profiles:         profile.NewStore(directory),
		started:          map[int64]*runtimev1.StartBrowserOperation{},
		startAcks:        map[int64]*runtimev1.StartBrowserOperationAck{},
		startAckFences:   map[int64]chan struct{}{},
		stopAcks:         map[int64]*runtimev1.StopBrowserOperationAck{},
		completed:        map[int64]*runtimev1.CompleteBrowserOperation{},
		completing:       map[int64]bool{},
		journeyRuns:      map[int64]*journeyRun{},
		journeyProposals: map[int64]*runtimev1.ResultProposal{},
		journeyCancelled: map[int64]bool{},
	}
	recorder := &envelopeRecorder{}
	channel.controlMu.Lock()
	channel.controlSend = func(envelope *runtimev1.ControlEnvelope) error {
		recorder.record(envelope)
		return nil
	}
	channel.controlMu.Unlock()
	return channel, recorder
}

func journeyOperationInput(operationID int64) map[string]any {
	return map[string]any{
		"schemaKind": "inspection_collection_v1", "attemptId": 77, "operationId": operationID,
		"identity":            map[string]any{"identityId": 5, "identityRevisionId": 6, "profileGenerationId": 7, "profileGeneration": 1, "startUrl": "http://fixture.internal/login"},
		"journey":             map[string]any{"id": "page.status-marker.v1", "version": 2, "params": map[string]any{"path": "/status"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}},
		"authenticationProbe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "http://fixture.internal/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}},
		"planKey":             "browser-plan", "checkKey": "status-page",
	}
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func browserOperationInput(canonical []byte) *runtimev1.BrowserOperationInput {
	digest := sha256.Sum256(canonical)
	return &runtimev1.BrowserOperationInput{SchemaKind: "inspection_collection_v1", CanonicalJson: canonical, ContentDigest: digest[:]}
}

func journeyDispatchEnvelope(t *testing.T, canonical []byte) (*runtimev1.ControlEnvelope, *runtimev1.DispatchAttempt) {
	t.Helper()
	digest := sha256.Sum256(canonical)
	dispatch := &runtimev1.DispatchAttempt{
		AttemptId: 77, AttemptType: runtimev1.AttemptType_ATTEMPT_TYPE_INSPECTION_COLLECTION,
		ScopeType: runtimev1.ScopeType_SCOPE_TYPE_CONFIG_VERIFICATION_RUN, ScopeId: 9,
		PlanKey: "browser-plan", CheckKey: "status-page",
		Input: &runtimev1.AttemptInputSnapshot{SchemaKind: "inspection_collection_v1", CanonicalJson: canonical, ContentDigest: digest[:]},
	}
	return &runtimev1.ControlEnvelope{MessageId: 11, ConnectionEpoch: 3, BootId: "boot-j"}, dispatch
}

func TestJourneyDispatchRejectsUnknownOperationBinding(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	channel.handleJourneyDispatch(envelope, dispatch)
	sent := recorder.snapshot()
	if len(sent) != 1 || sent[0].kind != "reject" {
		t.Fatalf("an input binding an unstarted operation must be rejected: %#v", sent)
	}
	if reason := sent[0].envelope.GetAttemptReject().GetReason(); reason != runtimev1.AttemptRejectReason_ATTEMPT_REJECT_REASON_INPUT_UNSUPPORTED {
		t.Fatalf("reject reason must be INPUT_UNSUPPORTED: %v", reason)
	}
}

func TestJourneyDispatchRejectsDigestMismatch(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	channel.started[42] = &runtimev1.StartBrowserOperation{OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY}
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	dispatch.Input.ContentDigest = make([]byte, sha256.Size)
	channel.handleJourneyDispatch(envelope, dispatch)
	sent := recorder.snapshot()
	if len(sent) != 1 || sent[0].kind != "reject" {
		t.Fatalf("a tampered digest must be rejected before execution: %#v", sent)
	}
}

func TestJourneyDispatchRejectsForeignOperationKind(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	channel.started[42] = &runtimev1.StartBrowserOperation{OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_EXPLORATION}
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	channel.handleJourneyDispatch(envelope, dispatch)
	sent := recorder.snapshot()
	if len(sent) != 1 || sent[0].kind != "reject" {
		t.Fatalf("a non-journey operation must not execute journeys: %#v", sent)
	}
}

func TestJourneyDispatchRejectsChangedFrozenBinding(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	original := mustMarshalJSON(t, journeyOperationInput(42))
	channel.started[42] = &runtimev1.StartBrowserOperation{
		OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		Input: browserOperationInput(original),
	}
	changed := journeyOperationInput(42)
	changed["journey"] = map[string]any{"id": "page.status-marker.v1", "version": 2, "params": map[string]any{"path": "/other"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}
	envelope, dispatch := journeyDispatchEnvelope(t, mustMarshalJSON(t, changed))
	channel.handleJourneyDispatch(envelope, dispatch)
	if sent := recorder.snapshot(); len(sent) != 1 || sent[0].kind != "reject" {
		t.Fatalf("a dispatch that changes frozen Journey params must be rejected: %#v", sent)
	}
}

func TestJourneyRedispatchRejectsChangedFrozenBinding(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	original := mustMarshalJSON(t, journeyOperationInput(42))
	channel.started[42] = &runtimev1.StartBrowserOperation{
		OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		Input: browserOperationInput(original),
	}
	channel.journeyRuns[77] = &journeyRun{done: make(chan struct{}), operation: 42}
	changed := journeyOperationInput(42)
	changed["authenticationProbe"] = map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "http://other.internal/"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}
	envelope, dispatch := journeyDispatchEnvelope(t, mustMarshalJSON(t, changed))
	channel.handleJourneyDispatch(envelope, dispatch)
	if sent := recorder.snapshot(); len(sent) != 1 || sent[0].kind != "reject" {
		t.Fatalf("a redispatch with changed frozen input must be rejected: %#v", sent)
	}
}

func TestCompletedJourneyRedispatchReplaysPendingProposal(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	channel.started[42] = &runtimev1.StartBrowserOperation{
		OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		Input: browserOperationInput(canonical),
	}
	proposal := &runtimev1.ResultProposal{AttemptId: 77, Payload: &runtimev1.ResultPayload{ContentDigest: []byte("sealed-result")}}
	channel.journeyProposals[77] = proposal
	// Model runJourney's narrow post-proposal/pre-defer window: the retained
	// Result exists while the running-map entry has not yet been removed.
	completed := &journeyRun{sawResult: true}
	channel.journeyRuns[77] = completed
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	channel.handleJourneyDispatch(envelope, dispatch)
	sent := recorder.snapshot()
	if len(sent) != 2 || sent[0].kind != "accept" || sent[1].kind != "result" || sent[1].envelope.GetResultProposal() != proposal {
		t.Fatalf("completed unacknowledged journey must only accept and replay its sealed result: %#v", sent)
	}
	if run := channel.journeyRuns[77]; run != completed {
		t.Fatal("completed unacknowledged journey must not replace its running entry")
	}
}

func TestJourneyCancelBeforeDispatchPreventsWorkerStart(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	channel.started[42] = &runtimev1.StartBrowserOperation{
		OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version,
		Input: browserOperationInput(canonical),
	}
	if channel.journeyOperations == nil {
		channel.journeyOperations = make(map[int64]int64)
	}
	channel.journeyOperations[77] = 42
	stopped := make(chan struct{})
	channel.stopBrowser = func(operationID int64) error {
		if operationID != 42 {
			t.Fatalf("stopped wrong pre-dispatch operation: %d", operationID)
		}
		close(stopped)
		return nil
	}
	cancelEnvelope := &runtimev1.ControlEnvelope{CorrelationId: 19}
	channel.handleJourneyCancel(cancelEnvelope, &runtimev1.CancelAttempt{AttemptId: 77})
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	channel.handleJourneyDispatch(envelope, dispatch)
	channel.operationMu.Lock()
	run := channel.journeyRuns[77]
	cancelled := channel.journeyCancelled[77]
	channel.operationMu.Unlock()
	if !cancelled || run != nil {
		t.Fatalf("delayed dispatch started cancelled journey: cancelled=%v run=%v", cancelled, run)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancel-before-dispatch did not stop the started browser operation")
	}
	if got := countKind(recorder.snapshot(), "cancel"); got < 1 {
		t.Fatalf("cancel-before-dispatch must acknowledge the stopped attempt, got %#v", recorder.snapshot())
	}
}

func TestJourneyCancelAcknowledgesOnlyAfterExecutionReturns(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	cancelled := make(chan struct{})
	done := make(chan struct{})
	stopped := make(chan struct{})
	channel.stopBrowser = func(operationID int64) error {
		if operationID != 42 {
			t.Fatalf("stopped wrong operation: %d", operationID)
		}
		close(stopped)
		return nil
	}
	channel.journeyRuns[88] = &journeyRun{cancel: func() { close(cancelled) }, done: done, operation: 42}
	channel.handleJourneyCancel(&runtimev1.ControlEnvelope{CorrelationId: 20}, &runtimev1.CancelAttempt{AttemptId: 88})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel was not delivered to the running journey")
	}
	if got := countKind(recorder.snapshot(), "cancel"); got != 0 {
		t.Fatalf("CancelAck arrived before journey execution returned: %#v", recorder.snapshot())
	}
	close(done)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("CancelAck fence did not stop the browser operation")
	}
	deadline := time.Now().Add(time.Second)
	for countKind(recorder.snapshot(), "cancel") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := countKind(recorder.snapshot(), "cancel"); got != 1 {
		t.Fatalf("CancelAck was not emitted after journey execution returned: %#v", recorder.snapshot())
	}
}

func TestJourneyDispatchAcceptsAndConvergesProbeUnavailable(t *testing.T) {
	channel, recorder := newJourneyTestChannel(t)
	if channel.browser == nil {
		t.Skip("browser manager needs the Xvfb/Chromium stack; the real convergence path is covered by the ticket acceptance stack")
	}
	channel.started[42] = &runtimev1.StartBrowserOperation{
		OperationId: 42, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version,
		Input: browserOperationInput(mustMarshalJSON(t, journeyOperationInput(42))),
	}
	canonical := mustMarshalJSON(t, journeyOperationInput(42))
	envelope, dispatch := journeyDispatchEnvelope(t, canonical)
	channel.handleJourneyDispatch(envelope, dispatch)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if results := countKind(recorder.snapshot(), "result"); results > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var proposal *runtimev1.ResultProposal
	for _, item := range recorder.snapshot() {
		if item.kind == "result" {
			proposal = item.envelope.GetResultProposal()
		}
	}
	if proposal == nil {
		t.Fatalf("the journey must converge to a typed result even without a browser: %#v", recorder.snapshot())
	}
	if proposal.GetPayload().GetSchemaKind() != "browser_journey_result_v1" {
		t.Fatalf("result schema kind wrong: %s", proposal.GetPayload().GetSchemaKind())
	}
	var body map[string]any
	if err := json.Unmarshal(proposal.GetPayload().GetCanonicalJson(), &body); err != nil {
		t.Fatal(err)
	}
	if body["gapCode"] != "authentication_probe_unavailable" {
		t.Fatalf("a browserless operation must converge as probe-unavailable, never unauthenticated: %s", body["gapCode"])
	}
}

func TestStartRejectsJourneyCatalogDigestMismatch(t *testing.T) {
	channel, _ := newJourneyTestChannel(t)
	input := journeyOperationInput(50)
	request := &runtimev1.StartBrowserOperation{
		OperationId: 50, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		JourneyCatalogDigest: "0000000000000000000000000000000000000000000000000000000000000000", JourneyCatalogVersion: catalog.Version,
		Input: &runtimev1.BrowserOperationInput{SchemaKind: "inspection_collection_v1", CanonicalJson: mustMarshalJSON(t, input), ContentDigest: digestOf(mustMarshalJSON(t, input))},
	}
	ack := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 3, ConnectionEpoch: 3, BootId: "boot-j"}, request)
	response := ack.GetStartBrowserOperationAck()
	if response.GetAccepted() {
		t.Fatalf("a catalog digest mismatch must reject the Start")
	}
	if response.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED {
		t.Fatalf("reject reason must be INPUT_UNSUPPORTED: %v", response.GetRejectReason())
	}
}

func TestStartRejectsJourneyProfileRevisionMismatch(t *testing.T) {
	channel, _ := newJourneyTestChannel(t)
	// Install a published generation whose manifest pins a different Chromium
	// revision than this Lintel build.
	source := t.TempDir()
	if err := osWriteFile(filepath.Join(source, "Preferences"), "{}"); err != nil {
		t.Fatal(err)
	}
	store := profile.NewStore(channel.Config.StateDirectory)
	if _, err := store.Install(source, profile.Manifest{IdentityID: 5, Generation: 1, IdentityRevision: 6, ChromiumRevision: "Chromium-OTHER"}); err != nil {
		t.Fatal(err)
	}
	input := journeyOperationInput(51)
	canonical := mustMarshalJSON(t, input)
	request := &runtimev1.StartBrowserOperation{
		OperationId: 51, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_JOURNEY,
		IdentityId: 5, IdentityRevisionId: 6, ProfileGenerationId: 7,
		JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version,
		Input: &runtimev1.BrowserOperationInput{SchemaKind: "inspection_collection_v1", CanonicalJson: canonical, ContentDigest: digestOf(canonical)},
	}
	ack := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 4, ConnectionEpoch: 3, BootId: "boot-j"}, request)
	response := ack.GetStartBrowserOperationAck()
	if response.GetAccepted() {
		t.Fatalf("a profile Chromium revision mismatch must reject the Start")
	}
	if response.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_PROFILE_UNAVAILABLE {
		t.Fatalf("reject reason must be PROFILE_UNAVAILABLE: %v", response.GetRejectReason())
	}
}

func osWriteFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func countKind(items []recordedEnvelope, kind string) int {
	total := 0
	for _, item := range items {
		if item.kind == kind {
			total++
		}
	}
	return total
}

func digestOf(canonical []byte) []byte {
	sum := sha256.Sum256(canonical)
	return sum[:]
}
