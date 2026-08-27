package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

const maxBrowserEvents = 500

// dialCDP performs every private DevTools WebSocket handshake under an
// operation-bounded context. x/net/websocket.Dial has no cancellation path;
// Config.DialContext prevents a dead Chromium listener from retaining a browser
// slot or an action mutex indefinitely.
func dialCDP(ctx context.Context, endpoint string) (*websocket.Conn, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	config, err := websocket.NewConfig(endpoint, "http://127.0.0.1")
	if err != nil {
		return nil, err
	}
	return config.DialContext(ctx)
}

// closeCDPOnContext makes a post-handshake CDP read/write obey the same action
// deadline as the dial. websocket JSON operations do not take a Context; closing
// the private loopback socket is the only cancellation mechanism they expose.
// The returned function is required to stop the watcher when normal completion
// closes the socket first.
func closeCDPOnContext(ctx context.Context, connection *websocket.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// startExplorationEventRecorder establishes the single private Browser-level
// CDP subscription for an operation. It is deliberately not exposed outside the
// browser package: the only output is the bounded browserEvent projection.
func startExplorationEventRecorder(ctx context.Context, op *operation) error {
	endpoint, err := browserDevToolsURL(ctx, op.profile)
	if err != nil {
		return err
	}
	connection, err := dialCDP(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("dial Chromium browser event endpoint: %w", err)
	}
	state := op.exploration
	if state == nil {
		_ = connection.Close()
		return errors.New("browser operation event state is unavailable")
	}
	state.eventMu.Lock()
	if state.eventConnection != nil {
		state.eventMu.Unlock()
		_ = connection.Close()
		return nil
	}
	state.eventConnection, state.eventDone = connection, make(chan struct{})
	done := state.eventDone
	if state.eventTargets == nil {
		state.eventTargets = make(map[string]string)
	}
	state.eventPending = make(map[int]string)
	state.eventCheckpointWaiters = make(map[int]chan recorderCheckpoint)
	state.eventOrigins = make(map[string]string)
	state.originRequests = make(map[int]originRequest)
	state.eventOriginGeneration = make(map[string]uint64)
	state.deferredOriginEvents = make(map[string]map[uint64][]browserEvent)
	state.knownTargets = make(map[string]bool)
	state.topFrames = make(map[string]string)
	state.frameTreeRequests = make(map[int]string)
	state.pendingRedirects = make(map[string][]browserEvent)
	state.eventNextID = 0
	state.eventBaselineTargetRequestID = 0
	state.eventSequence, state.checkpointSequence = 0, 0
	state.droppedIntervals = nil
	state.eventError = nil
	state.eventReady = false
	state.eventMu.Unlock()
	// Enable target discovery and flattened auto-attach before navigation, then
	// take a command-ordered target inventory fence. These are commands, not
	// fire-and-forget messages: an unsupported command means the recorder cannot
	// make the trace claim and operation startup must fail. In particular, target
	// discovery can queue Target.targetCreated notifications before the recorder
	// loop starts; the Target.getTargets reply identifies which of those pages
	// predate the operation so they cannot be fabricated as popups.
	for _, command := range []devtoolsMessage{
		{Method: "Target.setDiscoverTargets", Params: []byte(`{"discover":true}`)},
		{Method: "Target.setAutoAttach", Params: []byte(`{"autoAttach":true,"waitForDebuggerOnStart":false,"flatten":true}`)},
		{Method: "Target.getTargets", Params: []byte(`{}`)},
	} {
		if err := sendRecorderCommand(connection, state, command); err != nil {
			discardUnstartedExplorationRecorder(connection, state)
			return err
		}
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		discardUnstartedExplorationRecorder(connection, state)
		return fmt.Errorf("set Chromium event recorder setup deadline: %w", err)
	}
	setupErr := awaitRecorderSetup(connection, state)
	_ = connection.SetDeadline(time.Time{})
	if setupErr != nil {
		discardUnstartedExplorationRecorder(connection, state)
		return setupErr
	}
	state.eventMu.Lock()
	state.eventReady = true
	state.eventMu.Unlock()
	go recordExplorationEvents(connection, state, done)
	return nil
}

func browserDevToolsURL(ctx context.Context, profile string) (string, error) {
	// Chromium must publish its local endpoint shortly after launch. A bounded
	// watcher prevents a broken startup from retaining a browser slot forever
	// when callers use a non-expiring operation context.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path := filepath.Join(profile, "DevToolsActivePort")
	for {
		body, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) < 2 || lines[0] == "" || lines[1] == "" {
				return "", errors.New("Chromium browser DevTools endpoint is malformed")
			}
			port, parseErr := strconv.ParseUint(lines[0], 10, 16)
			if parseErr != nil || port == 0 || !strings.HasPrefix(lines[1], "/devtools/browser/") {
				return "", errors.New("Chromium browser DevTools endpoint is malformed")
			}
			return (&url.URL{Scheme: "ws", Host: net.JoinHostPort("127.0.0.1", lines[0]), Path: lines[1]}).String(), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read Chromium browser DevTools endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("Chromium browser event endpoint did not become ready: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func discardUnstartedExplorationRecorder(connection *websocket.Conn, state *explorationState) {
	_ = connection.Close()
	state.eventMu.Lock()
	state.eventConnection, state.eventDone = nil, nil
	state.eventMu.Unlock()
}

func stopExplorationEventRecorder(state *explorationState) {
	if state == nil {
		return
	}
	state.eventMu.Lock()
	connection, done := state.eventConnection, state.eventDone
	state.eventConnection, state.eventDone = nil, nil
	state.eventMu.Unlock()
	if connection == nil {
		return
	}
	_ = connection.Close()
	if done != nil {
		<-done
	}
}

func sendRecorderCommand(connection *websocket.Conn, state *explorationState, command devtoolsMessage) error {
	state.eventMu.Lock()
	state.eventNextID++
	command.ID = state.eventNextID
	state.eventPending[command.ID] = command.Method
	// The sole Target.getTargets sent before readiness is the startup inventory
	// fence. Later Target.getTargets requests are action checkpoints and must not
	// reclassify action-caused pages as pre-existing.
	if command.Method == "Target.getTargets" && !state.eventReady {
		state.eventBaselineTargetRequestID = command.ID
	}
	if command.Method == "Runtime.evaluate" {
		if state.originRequests == nil {
			state.originRequests = make(map[int]originRequest)
		}
		targetID := state.eventTargets[command.SessionID]
		state.originRequests[command.ID] = originRequest{targetID: targetID, generation: state.eventOriginGeneration[targetID]}
	}
	if command.Method == "Page.getFrameTree" {
		if state.frameTreeRequests == nil {
			state.frameTreeRequests = make(map[int]string)
		}
		state.frameTreeRequests[command.ID] = state.eventTargets[command.SessionID]
	}
	state.eventMu.Unlock()
	if err := websocket.JSON.Send(connection, command); err != nil {
		state.eventMu.Lock()
		delete(state.eventPending, command.ID)
		if state.eventBaselineTargetRequestID == command.ID {
			state.eventBaselineTargetRequestID = 0
		}
		state.eventMu.Unlock()
		return fmt.Errorf("send Chromium event recorder command %s: %w", command.Method, err)
	}
	return nil
}

func awaitRecorderSetup(connection *websocket.Conn, state *explorationState) error {
	for {
		state.eventMu.Lock()
		pending := len(state.eventPending)
		recorderErr := state.eventError
		state.eventMu.Unlock()
		if recorderErr != nil {
			return recorderErr
		}
		if pending == 0 {
			return nil
		}
		var message devtoolsMessage
		if err := websocket.JSON.Receive(connection, &message); err != nil {
			return fmt.Errorf("await Chromium event recorder acknowledgement: %w", err)
		}
		handleExplorationRecorderMessage(connection, state, message)
	}
}

func recordExplorationEvents(connection *websocket.Conn, state *explorationState, done chan struct{}) {
	// Keep the channel captured at recorder startup. Stop clears state.eventDone
	// before closing the socket, so reading that mutable field here could close a
	// nil/different channel and leave Stop waiting forever.
	defer close(done)
	defer connection.Close()
	for {
		var message devtoolsMessage
		if err := websocket.JSON.Receive(connection, &message); err != nil {
			// Stop clears eventConnection before closing it. Any other receive
			// failure is an operation-lifetime recorder fault and must make later
			// typed actions terminal rather than silently dropping trace events.
			state.eventMu.Lock()
			live := state.eventConnection == connection
			state.eventMu.Unlock()
			if live {
				setRecorderError(state, fmt.Errorf("Chromium event recorder stopped: %w", err))
			}
			return
		}
		handleExplorationRecorderMessage(connection, state, message)
	}
}

// awaitExplorationRecorderHealth fences an action result on every attached
// target's fixed Page/Network enable acknowledgements. Target attachment is
// asynchronous; returning an Observation while one of those acknowledgements
// is still pending would make a multi-tab session claim a complete recorder
// even though a popup/tab can still be invisible to the trace.
// recorderCheckpoint is delivered from the recorder loop while eventMu is
// still held for the matching Target.getTargets acknowledgement. This makes the
// observed sequence an exact command-ordered event cut rather than a later,
// racy read after another browser event has been appended.
type recorderCheckpoint struct {
	sequence uint64
	err      error
}

// explorationRecorderBarrier establishes an ordered checkpoint on the one
// recorder connection. Target.getTargets is a fixed, private query: its reply
// cannot overtake prior recorder events. The reply handler captures the event
// sequence under that same eventMu critical section before it lets a later event
// append, so an Observation consumes precisely the prefix it describes.
func explorationRecorderBarrier(ctx context.Context, state *explorationState) (uint64, error) {
	if state == nil {
		return 0, errors.New("browser event recorder is not running")
	}
	state.eventMu.Lock()
	connection := state.eventConnection
	ready := state.eventReady
	if !ready {
		state.eventMu.Unlock()
		return 0, errors.New("browser event recorder is not ready")
	}
	// Unit-level executor seams supply an already-healthy synthetic recorder
	// without a websocket. A real recorder always owns a connection; a stopped
	// real recorder is rejected by eventReady/eventError before this point.
	if connection == nil {
		sequence := state.eventSequence
		state.eventMu.Unlock()
		return sequence, nil
	}
	state.eventNextID++
	commandID := state.eventNextID
	state.eventPending[commandID] = "Target.getTargets"
	if state.eventCheckpointWaiters == nil {
		state.eventCheckpointWaiters = make(map[int]chan recorderCheckpoint)
	}
	waiter := make(chan recorderCheckpoint, 1)
	state.eventCheckpointWaiters[commandID] = waiter
	state.eventMu.Unlock()
	if err := websocket.JSON.Send(connection, devtoolsMessage{ID: commandID, Method: "Target.getTargets", Params: []byte(`{}`)}); err != nil {
		state.eventMu.Lock()
		delete(state.eventPending, commandID)
		delete(state.eventCheckpointWaiters, commandID)
		state.eventMu.Unlock()
		return 0, fmt.Errorf("send Chromium event recorder command Target.getTargets: %w", err)
	}
	select {
	case checkpoint := <-waiter:
		if checkpoint.err != nil {
			return 0, checkpoint.err
		}
		return checkpoint.sequence, nil
	case <-ctx.Done():
		state.eventMu.Lock()
		delete(state.eventCheckpointWaiters, commandID)
		state.eventMu.Unlock()
		return 0, fmt.Errorf("browser event recorder checkpoint: %w", ctx.Err())
	}
}

func awaitExplorationRecorderHealth(ctx context.Context, state *explorationState) error {
	if state == nil {
		return errors.New("browser event recorder is not running")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		state.eventMu.Lock()
		ready := state.eventReady
		pending := len(state.eventPending)
		err := state.eventError
		state.eventMu.Unlock()
		if err != nil {
			return err
		}
		if ready && pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("browser event recorder did not become healthy: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// appendRecorderEvents is called only under state.eventMu. It assigns a
// appendRecorderEventWithOriginLocked is the sole path for provenance-bearing
// recorder events. Callers hold eventMu. Early redirects replayed after a frame
// tree response must pass this same origin gate rather than bypassing deferred
// attribution and fabricating a complete trace with an unknown origin.
func appendRecorderEventWithOriginLocked(state *explorationState, event browserEvent) {
	if event.Origin != "" || !requiresObservedOrigin(event.Kind) {
		appendRecorderEvents(state, []browserEvent{event})
		return
	}
	generation := state.eventOriginGeneration[event.PageID]
	if event.hasOriginGeneration {
		generation = event.originGeneration
		// A redirect belongs to the future document it observed. It may not be
		// rebound to the current document merely because a frame-tree reply won
		// the race with Page.frameNavigated.
		if generation != state.eventOriginGeneration[event.PageID] {
			if state.eventError == nil {
				state.eventError = errors.New("browser redirect generation was superseded before provenance resolved")
			}
			return
		}
	}
	if origin := state.eventOrigins[event.PageID]; origin != "" {
		event.Origin = origin
		appendRecorderEvents(state, []browserEvent{event})
		return
	}
	if state.deferredOriginEvents == nil {
		state.deferredOriginEvents = make(map[string]map[uint64][]browserEvent)
	}
	if state.deferredOriginEvents[event.PageID] == nil {
		state.deferredOriginEvents[event.PageID] = make(map[uint64][]browserEvent)
	}
	deferred := state.deferredOriginEvents[event.PageID][generation]
	if len(deferred) >= maxBrowserEvents {
		if state.eventError == nil {
			state.eventError = errors.New("browser event origin resolution exceeded bounded retention")
		}
		return
	}
	state.deferredOriginEvents[event.PageID][generation] = append(deferred, event)
}

// monotonically increasing private sequence before bounded retention can discard
// old entries, so checkpointSequence remains meaningful even on truncation.
func appendRecorderEvents(state *explorationState, events []browserEvent) {
	for index := range events {
		state.eventSequence++
		events[index].Sequence = state.eventSequence
	}
	// Retention discards from the head. Record exactly which sequence becomes
	// unavailable before mutating the slice, so checkpoint attribution is based
	// on the lost event's sequence rather than a sticky global boolean.
	for _, event := range events {
		if event.Kind == "" || event.PageID == "" {
			continue
		}
		if len(state.events) == maxBrowserEvents {
			dropped := state.events[0].Sequence
			if length := len(state.droppedIntervals); length != 0 && state.droppedIntervals[length-1].last+1 == dropped {
				state.droppedIntervals[length-1].last = dropped
			} else {
				state.droppedIntervals = append(state.droppedIntervals, explorationEventInterval{first: dropped, last: dropped})
			}
			copy(state.events, state.events[1:])
			state.events = state.events[:maxBrowserEvents-1]
		}
		state.events = append(state.events, event)
		state.eventsTruncated = state.eventsTruncated || event.Truncated
	}
}

// invalidateRecorderOrigin advances the document binding before asking CDP for
// the next fixed location.origin value. A rapid second navigation can supersede
// a generation whose navigation/redirect events are still awaiting that fixed
// reply. Those events cannot be safely reassigned to the next document and may
// not be silently deleted: record a bounded recorder fault so the operation
// cannot claim a complete trace.
func invalidateRecorderOrigin(state *explorationState, targetID string) {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	delete(state.eventOrigins, targetID)
	if state.eventOriginGeneration == nil {
		state.eventOriginGeneration = make(map[string]uint64)
	}
	if generations := state.deferredOriginEvents[targetID]; len(generations) != 0 {
		for _, events := range generations {
			if len(events) != 0 {
				if state.eventError == nil {
					state.eventError = errors.New("browser event origin resolution was superseded by a later navigation")
				}
				break
			}
		}
	}
	// The fault above is durable operation state; once emitted the stale private
	// queue is no longer needed and must be released to keep rapid redirects
	// bounded. It is never silently discarded.
	delete(state.deferredOriginEvents, targetID)
	state.eventOriginGeneration[targetID]++
}

// replayPendingRedirectsLocked releases only redirects that predicted this exact
// newly-observed document generation. Callers hold eventMu after Chromium's
// Page.frameNavigated event established the top-level frame.
func replayPendingRedirectsLocked(state *explorationState, targetID, frameID string, generation uint64) {
	key := targetID + "\x00" + frameID
	pending := state.pendingRedirects[key]
	if len(pending) == 0 {
		return
	}
	remaining := pending[:0]
	for _, event := range pending {
		if !event.hasOriginGeneration || event.originGeneration == generation {
			appendRecorderEventWithOriginLocked(state, event)
			continue
		}
		if event.originGeneration < generation {
			if state.eventError == nil {
				state.eventError = errors.New("browser redirect generation was skipped before provenance resolved")
			}
			continue
		}
		remaining = append(remaining, event)
	}
	if len(remaining) == 0 {
		delete(state.pendingRedirects, key)
	} else {
		state.pendingRedirects[key] = remaining
	}
}

func setRecorderError(state *explorationState, err error) {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	if state.eventError == nil {
		state.eventError = err
	}
}

func handleExplorationRecorderMessage(connection *websocket.Conn, state *explorationState, message devtoolsMessage) {
	if message.ID != 0 {
		state.eventMu.Lock()
		method, pending := state.eventPending[message.ID]
		targetID := state.frameTreeRequests[message.ID]
		originRequest := state.originRequests[message.ID]
		checkpointWaiter := state.eventCheckpointWaiters[message.ID]
		baselineTargetFence := state.eventBaselineTargetRequestID == message.ID
		var checkpointErr error
		if pending {
			delete(state.eventPending, message.ID)
			delete(state.frameTreeRequests, message.ID)
			delete(state.originRequests, message.ID)
			if baselineTargetFence {
				state.eventBaselineTargetRequestID = 0
			}
			if message.Error != nil {
				checkpointErr = fmt.Errorf("Chromium event recorder command %s rejected: %s", method, message.Error.Message)
				if state.eventError == nil {
					state.eventError = checkpointErr
				}
			} else if baselineTargetFence {
				var targets struct {
					TargetInfos []struct {
						TargetID string `json:"targetId"`
						Type     string `json:"type"`
					} `json:"targetInfos"`
				}
				if jsonUnmarshal(message.Result, &targets) != nil {
					checkpointErr = errors.New("Chromium event recorder startup target inventory is malformed")
					if state.eventError == nil {
						state.eventError = checkpointErr
					}
				} else {
					if state.knownTargets == nil {
						state.knownTargets = make(map[string]bool)
					}
					for _, target := range targets.TargetInfos {
						if target.Type == "page" && target.TargetID != "" {
							state.knownTargets[target.TargetID] = true
						}
					}
				}
			}
		}
		if checkpointWaiter != nil {
			delete(state.eventCheckpointWaiters, message.ID)
			// This send is deliberately performed before eventMu is released. The
			// sequence belongs to this command acknowledgement, not to any event
			// the recorder appends immediately afterwards.
			checkpointWaiter <- recorderCheckpoint{sequence: state.eventSequence, err: checkpointErr}
		}
		state.eventMu.Unlock()
		if pending && method == "Runtime.evaluate" && message.Error == nil && originRequest.targetID != "" {
			// Runtime.evaluate replies are a CDP RemoteObject envelope, not the
			// expression value itself.  In particular an exception is represented
			// in result.exceptionDetails while the command-level reply succeeds.
			// Never treat that envelope (or a non-string RemoteObject) as an
			// origin; doing so would fabricate provenance for later trace events.
			var reply struct {
				Result struct {
					Type  string          `json:"type"`
					Value json.RawMessage `json:"value"`
				} `json:"result"`
				ExceptionDetails json.RawMessage `json:"exceptionDetails"`
			}
			var origin string
			if jsonUnmarshal(message.Result, &reply) == nil && len(reply.ExceptionDetails) == 0 && reply.Result.Type == "string" && jsonUnmarshal(reply.Result.Value, &origin) == nil && origin != "" {
				state.eventMu.Lock()
				if state.eventOriginGeneration[originRequest.targetID] == originRequest.generation {
					state.eventOrigins[originRequest.targetID] = origin
					if generations := state.deferredOriginEvents[originRequest.targetID]; generations != nil {
						for _, event := range generations[originRequest.generation] {
							event.Origin = origin
							appendRecorderEvents(state, []browserEvent{event})
						}
						delete(generations, originRequest.generation)
						if len(generations) == 0 {
							delete(state.deferredOriginEvents, originRequest.targetID)
						}
					}
				}
				state.eventMu.Unlock()
			} else {
				setRecorderError(state, errors.New("Chromium location.origin evaluation returned no string value"))
			}
		}
		if pending && method == "Page.getFrameTree" && message.Error == nil && targetID != "" {
			var tree struct {
				FrameTree struct {
					Frame struct {
						ID string `json:"id"`
					} `json:"frame"`
				} `json:"frameTree"`
			}
			if jsonUnmarshal(message.Result, &tree) == nil && tree.FrameTree.Frame.ID != "" {
				// Page.getFrameTree is the authoritative top-frame identity even
				// when a redirect arrived before Page.frameNavigated. Replay only
				// the candidates for this exact target/frame pair while holding the
				// recorder lock so a snapshot cannot observe a half-replayed prefix.
				state.eventMu.Lock()
				// A frame-tree response can establish identity for filtering iframe
				// traffic, but is not a navigation boundary. In particular it must
				// not release a redirect which belongs to a future document.
				state.topFrames[targetID] = tree.FrameTree.Frame.ID
				state.eventMu.Unlock()
			}
		}
		return
	}
	if message.Method == "Target.detachedFromTarget" {
		var detached struct {
			SessionID string `json:"sessionId"`
			TargetID  string `json:"targetId"`
		}
		if jsonUnmarshal(message.Params, &detached) == nil {
			state.eventMu.Lock()
			targetID := detached.TargetID
			if targetID == "" {
				targetID = state.eventTargets[detached.SessionID]
			}
			// A detached target may never silently discard provenance-bearing events.
			// If its fixed origin or frame identity had not resolved, the trace is
			// incomplete rather than a shortened complete trace.
			unresolved := false
			for _, events := range state.deferredOriginEvents[targetID] {
				if len(events) != 0 {
					unresolved = true
					break
				}
			}
			for key, events := range state.pendingRedirects {
				if strings.HasPrefix(key, targetID+"\x00") {
					if len(events) != 0 {
						unresolved = true
					}
					delete(state.pendingRedirects, key)
				}
			}
			for _, request := range state.originRequests {
				if request.targetID == targetID {
					unresolved = true
					break
				}
			}
			if unresolved && state.eventError == nil {
				state.eventError = errors.New("browser target detached before recorder provenance resolved")
			}
			delete(state.eventTargets, detached.SessionID)
			delete(state.knownTargets, targetID)
			delete(state.eventOrigins, targetID)
			delete(state.eventOriginGeneration, targetID)
			delete(state.deferredOriginEvents, targetID)
			delete(state.topFrames, targetID)
			state.eventMu.Unlock()
		}
		return
	}
	if message.Method == "Target.attachedToTarget" {
		var attached struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		if jsonUnmarshal(message.Params, &attached) == nil && attached.SessionID != "" && attached.TargetInfo.Type == "page" && attached.TargetInfo.TargetID != "" {
			state.eventMu.Lock()
			state.eventTargets[attached.SessionID] = attached.TargetInfo.TargetID
			state.eventMu.Unlock()
			// These fixed commands are acknowledged in the same recorder loop. A
			// later rejection is retained as a recorder fault rather than ignored.
			if err := sendRecorderCommand(connection, state, devtoolsMessage{Method: "Page.enable", SessionID: attached.SessionID}); err != nil {
				setRecorderError(state, err)
			}
			if err := sendRecorderCommand(connection, state, devtoolsMessage{Method: "Network.enable", Params: []byte(`{}`), SessionID: attached.SessionID}); err != nil {
				setRecorderError(state, err)
			}
			if err := sendRecorderCommand(connection, state, devtoolsMessage{Method: "Runtime.evaluate", Params: []byte(`{"expression":"location.origin","returnByValue":true}`), SessionID: attached.SessionID}); err != nil {
				setRecorderError(state, err)
			}
			if err := sendRecorderCommand(connection, state, devtoolsMessage{Method: "Page.getFrameTree", Params: []byte(`{}`), SessionID: attached.SessionID}); err != nil {
				setRecorderError(state, err)
			}
		}
	}
	// CDP flattened events carry the session ID, which is not a page identity.
	// Replace it only with the target ID learned from Target.attachedToTarget.
	if message.SessionID != "" {
		sessionID := message.SessionID
		state.eventMu.Lock()
		targetID := state.eventTargets[sessionID]
		state.eventMu.Unlock()
		if targetID == "" {
			return
		}
		// Origin is a property of a target's current top-level document, not of
		// its target ID. Invalidate the prior fixed location.origin observation on
		// every top-frame document or same-document navigation and issue the fixed
		// query again through the recorder connection.
		if recorderTopFrameNavigated(state, targetID, message) {
			invalidateRecorderOrigin(state, targetID)
			if err := sendRecorderCommand(connection, state, devtoolsMessage{Method: "Runtime.evaluate", Params: []byte(`{"expression":"location.origin","returnByValue":true}`), SessionID: sessionID}); err != nil {
				setRecorderError(state, err)
			}
		}
		message.SessionID = targetID
	}
	if event, download := browserEventFromRecorder(state, message); event.Kind != "" || download {
		state.eventMu.Lock()
		if download {
			state.downloadBlocked = true
		}
		if event.Kind != "" {
			appendRecorderEventWithOriginLocked(state, event)
		}
		state.eventMu.Unlock()
	}
}

// recorderTopFrameNavigated returns true only for a navigation of the target's
// known top-level frame. Page.frameNavigated can establish that identity; a
// same-document event never guesses it from an iframe or URL.
func requiresObservedOrigin(kind string) bool {
	return kind == "navigation" || kind == "popup" || kind == "redirect"
}

func recorderTopFrameNavigated(state *explorationState, targetID string, message devtoolsMessage) bool {
	if message.Method != "Page.frameNavigated" && message.Method != "Page.navigatedWithinDocument" {
		return false
	}
	var value struct {
		Frame struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
		} `json:"frame"`
		FrameID string `json:"frameId"`
	}
	if jsonUnmarshal(message.Params, &value) != nil {
		return false
	}
	frameID := value.Frame.ID
	if frameID == "" {
		frameID = value.FrameID
	}
	if frameID == "" {
		return false
	}
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	if state.topFrames == nil {
		state.topFrames = make(map[string]string)
	}
	if message.Method == "Page.frameNavigated" && value.Frame.ParentID == "" {
		state.topFrames[targetID] = frameID
		return true
	}
	return state.topFrames[targetID] == frameID
}

// jsonUnmarshal is a seam for recorder tests without exporting the private CDP
// protocol. Keeping it here also makes accidental arbitrary payload handling
// impossible: only fixed event structs are decoded.
var jsonUnmarshal = func(data []byte, value any) error { return json.Unmarshal(data, value) }

// browserEventFromCDP remains a stateless parser seam for unit tests. The live
// recorder supplies state so popup discovery can distinguish startup inventory.
func browserEventFromCDP(message devtoolsMessage) (browserEvent, bool) {
	return browserEventFromRecorder(&explorationState{knownTargets: make(map[string]bool), eventReady: true}, message)
}

func browserEventFromRecorder(state *explorationState, message devtoolsMessage) (browserEvent, bool) {
	if message.Method == "Browser.downloadWillBegin" || message.Method == "Page.downloadWillBegin" {
		return browserEvent{}, true
	}
	pageID := message.SessionID
	switch message.Method {
	case "Target.targetCreated":
		var value struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfo"`
		}
		if jsonUnmarshal(message.Params, &value) == nil && value.TargetInfo.Type == "page" && value.TargetInfo.TargetID != "" {
			state.eventMu.Lock()
			alreadyKnown, ready := state.knownTargets[value.TargetInfo.TargetID], state.eventReady
			state.knownTargets[value.TargetInfo.TargetID] = true
			state.eventMu.Unlock()
			// Discovery's initial inventory describes pages that predate the
			// operation, not popups. targetInfoChanged is deliberately ignored:
			// navigation of an existing page must not look like a new popup.
			if ready && !alreadyKnown {
				return safeBrowserEvent("popup", value.TargetInfo.TargetID, value.TargetInfo.URL, ""), false
			}
		}
	case "Page.frameNavigated", "Page.navigatedWithinDocument":
		var value struct {
			Frame struct {
				ID       string `json:"id"`
				URL      string `json:"url"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}
		if jsonUnmarshal(message.Params, &value) == nil {
			frameID := value.Frame.ID
			if frameID == "" {
				frameID = value.FrameID
			}
			// Chromium normally supplies frame.id. Keep the parser seam tolerant of
			// older synthetic recorder fixtures only for an explicit top-level
			// frameNavigated event; production same-document/iframe events still
			// require their actual frame identity.
			if frameID == "" && message.Method == "Page.frameNavigated" && value.Frame.ParentID == "" {
				frameID = pageID
			}
			if pageID == "" || frameID == "" {
				return browserEvent{}, false
			}
			state.eventMu.Lock()
			if state.topFrames == nil {
				state.topFrames = make(map[string]string)
			}
			topFrame := state.topFrames[pageID]
			if message.Method == "Page.frameNavigated" && value.Frame.ParentID == "" {
				// A frameNavigated event carries parent identity, unlike a
				// same-document event. Only it may establish the top frame and
				// release redirects for its matching document generation.
				state.topFrames[pageID] = frameID
				topFrame = frameID
				replayPendingRedirectsLocked(state, pageID, frameID, state.eventOriginGeneration[pageID])
			}
			state.eventMu.Unlock()
			if topFrame != frameID {
				return browserEvent{}, false
			}
			if value.URL == "" {
				value.URL = value.Frame.URL
			}
			return safeBrowserEvent("navigation", pageID, value.URL, ""), false
		}
	case "Page.javascriptDialogOpening":
		var value struct {
			Type string `json:"type"`
		}
		if jsonUnmarshal(message.Params, &value) == nil {
			return safeBrowserEvent("dialog", pageID, "", value.Type), false
		}
	case "Network.requestWillBeSent":
		// Only top-level Document redirects are model-facing. Iframe/resource
		// redirects must not be presented as a page transition.
		var value struct {
			FrameID          string `json:"frameId"`
			Type             string `json:"type"`
			RedirectResponse *struct {
				URL string `json:"url"`
			} `json:"redirectResponse"`
			Request struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if jsonUnmarshal(message.Params, &value) == nil && value.RedirectResponse != nil && value.Type == "Document" {
			state.eventMu.Lock()
			topFrame := state.topFrames[pageID]
			state.eventMu.Unlock()
			source := safeBrowserEvent("redirect", pageID, value.RedirectResponse.URL, "")
			destination := safeBrowserEvent("redirect", pageID, value.Request.URL, "")
			redirect := browserEvent{Kind: "redirect", PageID: pageID, SourceURL: source.URL, DestinationURL: destination.URL, Truncated: source.Truncated || destination.Truncated}
			if topFrame != "" && topFrame != value.FrameID {
				return browserEvent{}, false
			}
			// A Document redirect predicts the next top-frame document even if a
			// prior frame tree already knows the frame ID. Retain it until that
			// Page.frameNavigated advances the generation; releasing it now would
			// attribute it to the old document (or origin=null during startup).
			key := pageID + "\x00" + value.FrameID
			state.eventMu.Lock()
			if state.pendingRedirects == nil {
				state.pendingRedirects = make(map[string][]browserEvent)
			}
			pending := state.pendingRedirects[key]
			if len(pending) < 32 {
				redirect.originGeneration = state.eventOriginGeneration[pageID] + 1
				redirect.hasOriginGeneration = true
				state.pendingRedirects[key] = append(pending, redirect)
			} else if state.eventError == nil {
				state.eventError = errors.New("browser redirect retention exceeded bounded limit")
			}
			state.eventMu.Unlock()
			return browserEvent{}, false
		}
	}
	return browserEvent{}, false
}
