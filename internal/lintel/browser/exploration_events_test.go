package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAwaitExplorationRecorderHealthRequiresAllTargetAcknowledgements(t *testing.T) {
	state := &explorationState{eventReady: true, eventPending: map[int]string{}}
	if err := awaitExplorationRecorderHealth(context.Background(), state); err != nil {
		t.Fatalf("healthy recorder: %v", err)
	}

	state.eventPending[7] = "Page.enable"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := awaitExplorationRecorderHealth(ctx, state); err == nil {
		t.Fatal("pending target setup was accepted as recorder health")
	}

	state.eventPending = map[int]string{}
	state.eventError = errors.New("Page.enable rejected")
	if err := awaitExplorationRecorderHealth(context.Background(), state); err == nil {
		t.Fatal("failed target setup was accepted as recorder health")
	}
}

func TestRecorderProjectsOnlyTopFrameRedirectWithBothEnds(t *testing.T) {
	state := &explorationState{knownTargets: map[string]bool{}, topFrames: map[string]string{"page-1": "top"}, eventOriginGeneration: map[string]uint64{"page-1": 0}, pendingRedirects: map[string][]browserEvent{}}
	state.eventTargets = map[string]string{"private": "page-1"}
	redirect, _ := browserEventFromRecorder(state, devtoolsMessage{SessionID: "page-1", Method: "Network.requestWillBeSent", Params: []byte(`{"frameId":"top","type":"Document","redirectResponse":{"url":"https://source.test/a?secret=1"},"request":{"url":"https://destination.test/b?token=2"}}`)})
	if redirect.Kind != "" || len(state.pendingRedirects["page-1\x00top"]) != 1 {
		t.Fatalf("early redirect was not held for its next document: event=%#v pending=%#v", redirect, state.pendingRedirects)
	}
	iframe, _ := browserEventFromRecorder(state, devtoolsMessage{SessionID: "page-1", Method: "Network.requestWillBeSent", Params: []byte(`{"frameId":"iframe","type":"Document","redirectResponse":{"url":"https://source.test/a"},"request":{"url":"https://destination.test/b"}}`)})
	if iframe.Kind != "" {
		t.Fatalf("iframe redirect projected as page event: %#v", iframe)
	}
}

func TestOpaqueExplorationRefDoesNotEncodeDOMIdentity(t *testing.T) {
	first, err := opaqueExplorationRef()
	if err != nil {
		t.Fatal(err)
	}
	second, err := opaqueExplorationRef()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) < 20 || strings.Contains(first, "page") || strings.Contains(first, "selector") || strings.Contains(first, ":") {
		t.Fatalf("reference is not opaque: %q", first)
	}
}

func TestRecorderRedirectUsesFrameTreeBeforeNavigation(t *testing.T) {
	state := &explorationState{eventReady: true, eventPending: map[int]string{}, eventTargets: map[string]string{"session": "page"}, topFrames: map[string]string{}, eventOriginGeneration: map[string]uint64{"page": 0}, pendingRedirects: map[string][]browserEvent{}}
	// The fixed Page.getFrameTree reply establishes frame identity only. It must
	// not release a redirect before the matching next-document navigation.
	state.eventPending[9] = "Page.getFrameTree"
	state.frameTreeRequests = map[int]string{9: "page"}
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{ID: 9, Result: json.RawMessage(`{"frameTree":{"frame":{"id":"top"}}}`)})
	event, download := browserEventFromRecorder(state, devtoolsMessage{Method: "Network.requestWillBeSent", SessionID: "page", Params: json.RawMessage(`{"frameId":"top","type":"Document","redirectResponse":{"url":"https://source.test/a?token=x"},"request":{"url":"https://dest.test/b?token=y"}}`)})
	if download || event.Kind != "" || len(state.pendingRedirects["page\x00top"]) != 1 {
		t.Fatalf("frame tree prematurely released redirect: event=%#v pending=%#v", event, state.pendingRedirects)
	}
}

func TestRecorderDropsIframeSameDocumentEvent(t *testing.T) {
	state := &explorationState{eventReady: true, eventPending: map[int]string{}, topFrames: map[string]string{"page": "top"}}
	event, download := browserEventFromRecorder(state, devtoolsMessage{Method: "Page.navigatedWithinDocument", SessionID: "page", Params: json.RawMessage(`{"frameId":"iframe","url":"https://example.test/#embedded"}`)})
	if download || event.Kind != "" {
		t.Fatalf("iframe same-document event leaked: %#v", event)
	}
}

func TestPageNavigationReplaysEarlierTopFrameRedirectWithDestinationOrigin(t *testing.T) {
	state := &explorationState{
		eventReady: true, eventTargets: map[string]string{"session": "page"}, topFrames: map[string]string{},
		eventOrigins: map[string]string{}, eventOriginGeneration: map[string]uint64{"page": 0}, pendingRedirects: map[string][]browserEvent{},
	}
	// Redirect first, then frame tree, then Page.frameNavigated: the frame-tree
	// response must not release an origin=null event; the next navigation does.
	event, _ := browserEventFromRecorder(state, devtoolsMessage{Method: "Network.requestWillBeSent", SessionID: "page", Params: json.RawMessage(`{"frameId":"top","type":"Document","redirectResponse":{"url":"https://source.test/a"},"request":{"url":"https://destination.test/b"}}`)})
	if event.Kind != "" || len(state.pendingRedirects["page\x00top"]) != 1 {
		t.Fatalf("early redirect was not pending: event=%#v pending=%#v", event, state.pendingRedirects)
	}
	state.topFrames["page"] = "top" // equivalent identity-only frame-tree response
	navigation := devtoolsMessage{Method: "Page.frameNavigated", SessionID: "page", Params: json.RawMessage(`{"frame":{"id":"top","url":"https://destination.test/b"}}`)}
	if !recorderTopFrameNavigated(state, "page", navigation) {
		t.Fatal("top-frame navigation was not recognized")
	}
	invalidateRecorderOrigin(state, "page")
	state.eventOrigins["page"] = "https://destination.test"
	_, _ = browserEventFromRecorder(state, navigation)
	if len(state.events) != 1 || state.events[0].Kind != "redirect" || state.events[0].Origin != "https://destination.test" || state.events[0].DestinationURL != "https://destination.test/b" {
		t.Fatalf("navigation replay=%#v", state.events)
	}
	if len(state.pendingRedirects) != 0 {
		t.Fatalf("replayed redirect retained pending state: %#v", state.pendingRedirects)
	}
}

func TestRecorderTargetDetachCleansPrivateState(t *testing.T) {
	state := &explorationState{eventTargets: map[string]string{"private": "page"}, knownTargets: map[string]bool{"page": true}, topFrames: map[string]string{"page": "top"}, pendingRedirects: map[string][]browserEvent{"page\x00top": {{Kind: "redirect"}}}}
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{Method: "Target.detachedFromTarget", Params: json.RawMessage(`{"sessionId":"private","targetId":"page"}`)})
	if len(state.eventTargets) != 0 || len(state.knownTargets) != 0 || len(state.topFrames) != 0 || len(state.pendingRedirects) != 0 {
		t.Fatalf("detached target leaked recorder state: %#v", state)
	}
	if state.eventError == nil || !strings.Contains(state.eventError.Error(), "detached before recorder provenance") {
		t.Fatalf("detached target silently discarded unresolved redirect: %v", state.eventError)
	}
}

func TestRecorderCheckpointCapturesSequenceInAcknowledgementCriticalSection(t *testing.T) {
	waiter := make(chan recorderCheckpoint, 1)
	state := &explorationState{
		events:                 []browserEvent{{Sequence: 4, Kind: "navigation", PageID: "page"}, {Sequence: 7, Kind: "dialog", PageID: "page"}},
		eventSequence:          7,
		eventPending:           map[int]string{8: "Target.getTargets"},
		eventCheckpointWaiters: map[int]chan recorderCheckpoint{8: waiter},
	}
	// The response handler records the sequence while holding eventMu. An event
	// appended immediately after the acknowledgement belongs only to the next
	// observation; this deterministic interleave previously raced the later
	// sequence read in explorationRecorderBarrier.
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{ID: 8, Result: json.RawMessage(`{"targetInfos":[]}`)})
	checkpoint := <-waiter
	if checkpoint.err != nil || checkpoint.sequence != 7 {
		t.Fatalf("checkpoint=%+v, want successful seq=7", checkpoint)
	}
	state.eventMu.Lock()
	appendRecorderEvents(state, []browserEvent{{Kind: "popup", PageID: "page"}})
	state.eventMu.Unlock()
	captured, truncated := consumeRecorderEventsThroughCheckpoint(state, checkpoint.sequence)
	if truncated || len(captured) != 2 || captured[0].Sequence != 4 || captured[1].Sequence != 7 {
		t.Fatalf("checkpoint captured wrong event prefix: events=%#v truncated=%t", captured, truncated)
	}
	if state.checkpointSequence != 7 || len(state.events) != 1 || state.events[0].Sequence != 8 {
		t.Fatalf("later interleaved event was not retained for next observation: %#v", state)
	}
}

func TestBoundedObservationTextPreservesUTF8Boundary(t *testing.T) {
	value := "a界"
	if got := bounded(value, 2); got != "a" {
		t.Fatalf("bounded UTF-8 text=%q, want valid prefix", got)
	}
	if !strings.Contains(fixedDocumentObservationScript, "TextEncoder") || !strings.Contains(fixedCandidateObjectsScript, "total:") {
		t.Fatal("observation scripts do not report UTF-8 original bytes and candidate total")
	}
}

func TestRecorderRetentionTruncationBelongsToFirstAffectedCheckpoint(t *testing.T) {
	state := &explorationState{}
	for index := 0; index < 502; index++ {
		appendRecorderEvents(state, []browserEvent{{Kind: "navigation", PageID: "page"}})
	}
	if len(state.droppedIntervals) != 1 || state.droppedIntervals[0] != (explorationEventInterval{first: 1, last: 2}) || len(state.events) != 500 {
		t.Fatalf("retention intervals=%#v events=%d", state.droppedIntervals, len(state.events))
	}
	_, truncated := consumeRecorderEventsThroughCheckpoint(state, 502)
	if !truncated {
		t.Fatal("checkpoint containing evicted sequences must be truncated")
	}
	appendRecorderEvents(state, []browserEvent{{Kind: "dialog", PageID: "page"}})
	captured, truncated := consumeRecorderEventsThroughCheckpoint(state, 503)
	if truncated || len(captured) != 1 || captured[0].Sequence != 503 {
		t.Fatalf("historical loss tainted later checkpoint: events=%#v truncated=%t", captured, truncated)
	}
}

func TestCheckSetAndSelectHelpersTreatDesiredStateAsIdempotent(t *testing.T) {
	if !sameStringSet(stringSet([]string{"a", "b"}), map[string]bool{"a": true, "b": true}) {
		t.Fatal("equal select sets did not compare equal")
	}
	if sameStringSet(stringSet([]string{"a"}), map[string]bool{"a": true, "b": true}) {
		t.Fatal("different select sets compared equal")
	}
	if got := indexOfExplorationOption([]string{"one", "two"}, "two"); got != 1 {
		t.Fatalf("option index=%d", got)
	}
}

func TestRecorderLossBetweenCheckpointAndConsumptionTaintsThatCheckpointOnly(t *testing.T) {
	state := &explorationState{
		checkpointSequence: 10,
		droppedIntervals:   []explorationEventInterval{{first: 11, last: 11}},
		events:             []browserEvent{{Sequence: 12, Kind: "navigation", PageID: "page"}},
	}
	// An event can be evicted after the recorder barrier has issued its command
	// but before the projection consumes the acknowledged prefix. Its sequence is
	// still inside this checkpoint and must not be hidden by later retention.
	_, truncated := consumeRecorderEventsThroughCheckpoint(state, 12)
	if !truncated {
		t.Fatal("loss inside the active checkpoint was not reported")
	}
	state.events = append(state.events, browserEvent{Sequence: 13, Kind: "dialog", PageID: "page"})
	_, truncated = consumeRecorderEventsThroughCheckpoint(state, 13)
	if truncated {
		t.Fatal("already-reported loss tainted the next checkpoint")
	}
}

func TestDelayedOriginReplyCannotOverwriteNewerTopFrameGeneration(t *testing.T) {
	state := &explorationState{
		eventPending:          map[int]string{3: "Runtime.evaluate"},
		originRequests:        map[int]originRequest{3: {targetID: "page", generation: 1}},
		eventOriginGeneration: map[string]uint64{"page": 2},
		eventOrigins:          map[string]string{},
	}
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{ID: 3, Result: json.RawMessage(`{"result":{"type":"string","value":"https://old.example"}}`)})
	if _, found := state.eventOrigins["page"]; found {
		t.Fatal("delayed older-document origin reply overwrote current generation")
	}
}

func TestTopFrameNavigationInvalidatesOriginGeneration(t *testing.T) {
	state := &explorationState{
		topFrames:    map[string]string{"page": "top"},
		eventOrigins: map[string]string{"page": "https://old.example"},
	}
	message := devtoolsMessage{Method: "Page.navigatedWithinDocument", Params: json.RawMessage(`{"frameId":"top"}`)}
	if !recorderTopFrameNavigated(state, "page", message) {
		t.Fatal("top-frame same-document navigation was not recognized")
	}
	// The live recorder deletes this cached origin and issues its fixed
	// location.origin query before attributing subsequent events.
	delete(state.eventOrigins, "page")
	if _, found := state.eventOrigins["page"]; found {
		t.Fatal("previous document origin survived top-frame navigation")
	}
	iframe := devtoolsMessage{Method: "Page.navigatedWithinDocument", Params: json.RawMessage(`{"frameId":"iframe"}`)}
	if recorderTopFrameNavigated(state, "page", iframe) {
		t.Fatal("iframe navigation invalidated the top-frame origin")
	}
}

func TestRecorderStartupTargetInventorySuppressesQueuedBaselineCreateOnly(t *testing.T) {
	state := &explorationState{
		eventPending:                 map[int]string{3: "Target.getTargets"},
		eventBaselineTargetRequestID: 3,
		knownTargets:                 map[string]bool{},
	}
	// Target.targetCreated may be queued behind the startup fence acknowledgement.
	// The fixed snapshot still declares it pre-existing, so enabling the recorder
	// afterwards must not fabricate a popup. A newly unknown target after that
	// fence remains an actual popup.
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{ID: 3, Result: json.RawMessage(`{"targetInfos":[{"targetId":"baseline-page","type":"page"}]}`)})
	if state.eventBaselineTargetRequestID != 0 || !state.knownTargets["baseline-page"] || len(state.eventPending) != 0 {
		t.Fatalf("startup target inventory was not committed: %#v", state)
	}
	state.eventReady = true
	baseline, _ := browserEventFromRecorder(state, devtoolsMessage{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetInfo":{"targetId":"baseline-page","type":"page","url":"https://baseline.example/"}}`)})
	if baseline.Kind != "" {
		t.Fatalf("queued baseline target fabricated popup: %#v", baseline)
	}
	popup, _ := browserEventFromRecorder(state, devtoolsMessage{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetInfo":{"targetId":"post-fence-page","type":"page","url":"https://popup.example/"}}`)})
	if popup.Kind != "popup" || popup.PageID != "post-fence-page" {
		t.Fatalf("unknown post-fence target was not projected as popup: %#v", popup)
	}
}

func TestRecorderUsesChromiumOriginInsteadOfURLDerivedOrigin(t *testing.T) {
	state := &explorationState{
		eventReady:   true,
		knownTargets: map[string]bool{},
		eventOrigins: map[string]string{"page": "null"},
	}
	handleExplorationRecorderMessage(nil, state, devtoolsMessage{
		Method: "Target.targetCreated",
		Params: json.RawMessage(`{"targetInfo":{"targetId":"page","type":"page","url":"https://looks-normal.example/"}}`),
	})
	if len(state.events) != 1 || state.events[0].Origin != "null" {
		t.Fatalf("event origin=%#v, want Chromium origin null", state.events)
	}
}

func TestRapidTopFrameNavigationFaultsRatherThanSilentlyDroppingDeferredOriginEvents(t *testing.T) {
	state := &explorationState{
		eventTargets:          map[string]string{"session": "page"},
		topFrames:             map[string]string{"page": "top"},
		eventOrigins:          map[string]string{"page": "https://old.example"},
		eventOriginGeneration: map[string]uint64{"page": 3},
		deferredOriginEvents: map[string]map[uint64][]browserEvent{
			"page": {3: {{Kind: "navigation", PageID: "page"}}},
		},
	}
	invalidateRecorderOrigin(state, "page")
	if state.eventOriginGeneration["page"] != 4 {
		t.Fatalf("origin generation=%d, want 4", state.eventOriginGeneration["page"])
	}
	if state.eventError == nil || !strings.Contains(state.eventError.Error(), "origin resolution was superseded") {
		t.Fatalf("rapid navigation silently discarded deferred origin events: %v", state.eventError)
	}
	if _, found := state.deferredOriginEvents["page"]; found {
		t.Fatal("faulted stale-document deferred events retained unbounded state")
	}
}
