package browser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/lintel/browser/exploration"
	"golang.org/x/net/websocket"
)

const explorationActionTimeout = 10 * time.Second

// ExplorationResult contains only the bounded, model-facing projection. The
// executor never exposes an HTML document, CDP endpoint, cookie, network body,
// or arbitrary JavaScript capability.
type ExplorationResult struct {
	Success         bool
	ErrorCode       string
	ErrorDetail     string
	SessionTerminal bool
	Payload         map[string]any
	Screenshot      []byte
}

type observationElement struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Visible  bool   `json:"visible"`
	Enabled  bool   `json:"enabled"`
	Checked  *bool  `json:"checked,omitempty"`
	Selected *bool  `json:"selected,omitempty"`
}

type observationCandidate struct {
	summary       observationElement
	backendNodeID int64
}

type explorationReference struct {
	// backendNodeID is Chromium's stable node identity, retained only in this
	// operation-local ledger. It is never exposed to the model or written into
	// the target DOM. Resolving it again makes an asynchronously removed node
	// unambiguously stale instead of retargeting a later array position.
	backendNodeID int64
	version       int64
	// pageID and documentID bind a reference to the exact target and top-level
	// document that issued it. Neither an observation version nor a backend node
	// ID is portable between tabs or navigations.
	pageID     string
	documentID string
}

type originRequest struct {
	targetID   string
	generation uint64
}

type explorationEventInterval struct {
	first uint64
	last  uint64
}

type browserEvent struct {
	// Sequence is assigned exclusively while eventMu is held by the single
	// Browser-level recorder. It is private trace metadata, never returned to the
	// model; checkpoints use it to prove a monotonic event cut.
	Sequence       uint64
	Kind           string
	PageID         string
	URL            string
	Origin         string
	SourceURL      string
	DestinationURL string
	Detail         string
	Truncated      bool
	// originGeneration is recorder-private provenance metadata. It binds an
	// early redirect to the document generation it predicts; it is never part of
	// a model-visible Observation.
	originGeneration    uint64
	hasOriginGeneration bool
}

type explorationState struct {
	version     int64
	refs        map[string]explorationReference
	currentPage string
	// eventMu isolates the operation-lifetime Browser CDP recorder from the
	// serialized typed-action executor.
	eventMu            sync.Mutex
	events             []browserEvent
	eventsTruncated    bool
	eventSequence      uint64
	checkpointSequence uint64
	// droppedIntervals records every sequence interval evicted by bounded event
	// retention. A maximum evicted sequence loses the temporal relationship to
	// checkpoints when several overflows occur around different barriers.
	droppedIntervals []explorationEventInterval
	downloadBlocked  bool
	eventConnection  *websocket.Conn
	eventDone        chan struct{}
	// eventTargets maps private flattened CDP session IDs to Chromium target IDs.
	// Session IDs never cross the browser boundary into an Observation.
	eventTargets map[string]string
	// eventOrigins is populated only by the recorder's fixed Runtime.evaluate
	// location.origin command; URL strings are never treated as origin evidence.
	eventOrigins map[string]string
	// originRequests binds a fixed location.origin reply to the exact
	// top-frame document generation that issued it; a delayed old reply may
	// never overwrite the origin of a newer navigation.
	originRequests        map[int]originRequest
	eventOriginGeneration map[string]uint64
	// deferredOriginEvents retains navigation/popup/redirect records until the
	// fixed location.origin reply for their exact top-frame generation arrives.
	// URL text is never substituted as origin evidence.
	deferredOriginEvents map[string]map[uint64][]browserEvent
	// recorderReady is set only after the fixed browser-level CDP commands have
	// acknowledged. Target discovery before that point is baseline inventory, not
	// a user-caused popup.
	eventReady             bool
	eventPending           map[int]string
	eventCheckpointWaiters map[int]chan recorderCheckpoint
	eventNextID            int
	// eventBaselineTargetRequestID identifies the startup Target.getTargets
	// fence. Only its reply seeds pre-existing page targets; later action
	// checkpoints must not suppress popups caused by that action.
	eventBaselineTargetRequestID int
	eventError                   error
	knownTargets                 map[string]bool
	// topFrames binds a private page target to its current top-level frame.
	// Redirect records are emitted only for that frame, never an iframe.
	topFrames map[string]string
	// frameTreeRequests binds the fixed Page.getFrameTree response to the target
	// it describes. It may establish the current frame identity but may never
	// release a redirect for a future document generation.
	frameTreeRequests map[int]string
	// pendingRedirects is a bounded per target/frame cache for Document redirects
	// awaiting their matching Page.frameNavigated generation. A frame-tree reply
	// is identity evidence only, never a release fence.
	pendingRedirects map[string][]browserEvent
}

// ExecuteExplorationAction executes one member of the fixed browser-tool
// vocabulary against the operation-private Chromium target. Calls are serialized
// per Manager, which makes action side effects deterministic with respect to the
// Runtime channel's child-attempt idempotency cache.
func (manager *Manager) ExecuteExplorationAction(parent context.Context, operationID int64, action exploration.Action) (result ExplorationResult) {
	if action.Name != "open" && action.SessionID != strconv.FormatInt(operationID, 10) {
		return ExplorationResult{ErrorCode: "ProtocolError", ErrorDetail: "session does not belong to this browser operation", SessionTerminal: true}
	}
	ctx, cancel := context.WithTimeout(parent, explorationActionTimeout)
	defer cancel()
	// Manager.mu protects operation membership/lifecycle only. Do not retain it
	// across CDP I/O: Stop and the crash watcher must be able to fence this
	// operation even while an action's bounded context waits on Chromium.
	manager.mu.Lock()
	op := manager.operations[operationID]
	if op == nil || op.stopping || op.crashed {
		manager.mu.Unlock()
		return ExplorationResult{ErrorCode: "BrowserCrashed", ErrorDetail: "browser operation is not running", SessionTerminal: true}
	}
	manager.mu.Unlock()
	// Typed actions remain serialized per operation, without serializing all
	// browser operations behind the Manager lifecycle mutex.
	op.actionMu.Lock()
	defer op.actionMu.Unlock()
	manager.mu.Lock()
	live := manager.operations[operationID] == op && !op.stopping && !op.crashed
	manager.mu.Unlock()
	if !live {
		return ExplorationResult{ErrorCode: "BrowserCrashed", ErrorDetail: "browser operation is not running", SessionTerminal: true}
	}
	if op.exploration == nil {
		return ExplorationResult{ErrorCode: "RuntimeUnavailable", ErrorDetail: "browser event recorder is not running", SessionTerminal: true}
	}
	// The Browser-level recorder is the only owner of events and downloads.
	// Every typed action (including open, page switching and close) consumes the
	// operation-wide download fence before returning its model-visible result.
	defer func() {
		if !manager.consumeExplorationDownload(op) {
			return
		}
		// Download detection is an operation-wide safety fence, including a
		// terminal action such as close_session. Do not let a close result hide a
		// download that Chromium observed while the action was in flight.
		if result.SessionTerminal {
			result = ExplorationResult{ErrorCode: "DownloadBlocked", ErrorDetail: "browser download was blocked", SessionTerminal: true}
			return
		}
		var observation map[string]any
		if result.Payload != nil {
			observation, _ = result.Payload["observation"].(map[string]any)
		}
		result = explorationFailure(action, "DownloadBlocked", "browser download was blocked", false, observation)
	}()
	if err := awaitExplorationRecorderHealth(ctx, op.exploration); err != nil {
		return ExplorationResult{ErrorCode: "RuntimeUnavailable", ErrorDetail: "browser event recorder is not healthy", SessionTerminal: true}
	}
	// A download observed between model turns is a session-wide safety fence.
	// Consume it before obtaining a page or issuing any next typed action, not
	// only in the deferred post-action path where a side effect could escape.
	if manager.consumeExplorationDownload(op) {
		return ExplorationResult{ErrorCode: "DownloadBlocked", ErrorDetail: "browser download was blocked", SessionTerminal: true}
	}
	pages, page, err := manager.explorationCurrentPage(ctx, op)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return manager.explorationActionTimedOut(op, action)
		}
		return explorationFailure(action, "RuntimeUnavailable", err.Error(), true, nil)
	}
	if action.Name == "close_session" {
		observation, obsErr := manager.explorationObservation(ctx, op)
		if obsErr != nil {
			return explorationFailure(action, "RuntimeUnavailable", obsErr.Error(), true, nil)
		}
		return ExplorationResult{Success: true, SessionTerminal: true, Payload: resultPayload(action, "success", observation, nil)}
	}
	if action.Name == "open" {
		observation, obsErr := manager.explorationObservation(ctx, op)
		if obsErr != nil {
			return explorationFailure(action, "RuntimeUnavailable", obsErr.Error(), true, nil)
		}
		return ExplorationResult{Success: true, Payload: resultPayload(action, "success", observation, nil)}
	}
	if action.Name == "switch_page" || action.Name == "close_page" {
		var target string
		_ = json.Unmarshal(action.Fields["pageId"], &target)
		found, ok := pageByID(pages, target)
		if !ok {
			return manager.explorationRecoverable(ctx, op, action, "ElementNotFound", "page is not present")
		}
		if action.Name == "switch_page" {
			if err := activateExplorationPage(ctx, op.profile, found.ID); err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return manager.explorationActionTimedOut(op, action)
				}
				return manager.explorationRecoverable(ctx, op, action, "NavigationFailed", "cannot activate page")
			}
			page = found
			op.exploration.currentPage = found.ID
		} else {
			if err := closeExplorationPage(ctx, op.profile, found.ID); err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return manager.explorationActionTimedOut(op, action)
				}
				return manager.explorationRecoverable(ctx, op, action, "NavigationFailed", "cannot close page")
			}
			var currentErr error
			pages, page, currentErr = manager.explorationCurrentPage(ctx, op)
			if currentErr != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return manager.explorationActionTimedOut(op, action)
				}
				return explorationFailure(action, "RuntimeUnavailable", currentErr.Error(), true, nil)
			}
			op.exploration.currentPage = page.ID
		}
		observation, obsErr := manager.explorationObservation(ctx, op)
		if obsErr != nil {
			return explorationFailure(action, "RuntimeUnavailable", obsErr.Error(), true, nil)
		}
		return ExplorationResult{Success: true, Payload: resultPayload(action, "success", observation, nil)}
	}

	commandResult, actionErr := manager.executeFixedAction(ctx, op, page, action)
	// explorationObservation performs the sole post-action recorder barrier and
	// captures the page inventory under the same event cut. Do not add a separate
	// pre-snapshot barrier here: its page list could already be stale.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return manager.explorationActionTimedOut(op, action)
	}
	observation, obsErr := manager.explorationObservation(ctx, op)
	if obsErr != nil {
		return explorationFailure(action, "RuntimeUnavailable", obsErr.Error(), true, nil)
	}
	if actionErr != nil {
		return explorationFailure(action, actionErr.code, actionErr.detail, actionErr.terminal, observation)
	}
	// A navigation can create or attach additional targets after the action's
	// initial health fence. Do not report a completed Observation until those
	// recorder commands have either acknowledged or failed.
	if err := awaitExplorationRecorderHealth(ctx, op.exploration); err != nil {
		return ExplorationResult{ErrorCode: "RuntimeUnavailable", ErrorDetail: "browser event recorder is not healthy", SessionTerminal: true}
	}
	result = ExplorationResult{Success: true, Payload: resultPayload(action, "success", observation, nil)}
	if action.Name == "screenshot" {
		result.Screenshot = commandResult.screenshot
	}
	return result
}

type actionError struct {
	code, detail string
	terminal     bool
}

var errExplorationHitTarget = errors.New("target is not the current hit-test target")

func (err *actionError) Error() string { return err.detail }

func (manager *Manager) consumeExplorationDownload(op *operation) bool {
	if op == nil || op.exploration == nil {
		return false
	}
	op.exploration.eventMu.Lock()
	defer op.exploration.eventMu.Unlock()
	blocked := op.exploration.downloadBlocked
	op.exploration.downloadBlocked = false
	return blocked
}

func explorationFailure(action exploration.Action, code, detail string, terminal bool, observation map[string]any) ExplorationResult {
	if terminal {
		return ExplorationResult{ErrorCode: code, ErrorDetail: boundedDetail(detail), SessionTerminal: true}
	}
	return ExplorationResult{ErrorCode: code, ErrorDetail: boundedDetail(detail), Payload: resultPayload(action, "recoverable_error", observation, map[string]any{"code": code, "message": boundedDetail(detail), "retryableInSession": true})}
}

// explorationActionTimedOut turns exhaustion of the operation's fixed action
// deadline into the documented recoverable ActionTimeout result. It obtains a
// fresh bounded Observation rather than claiming the stale pre-action page.
func (manager *Manager) explorationActionTimedOut(op *operation, action exploration.Action) ExplorationResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observation, err := manager.explorationObservation(ctx, op)
	if err != nil {
		return explorationFailure(action, "RuntimeUnavailable", err.Error(), true, nil)
	}
	return explorationFailure(action, "ActionTimeout", "browser action exceeded its fixed deadline", false, observation)
}

func (manager *Manager) explorationRecoverable(ctx context.Context, op *operation, action exploration.Action, code, detail string) ExplorationResult {
	observation, err := manager.explorationObservation(ctx, op)
	if err != nil {
		return explorationFailure(action, "RuntimeUnavailable", err.Error(), true, nil)
	}
	return explorationFailure(action, code, detail, false, observation)
}

func resultPayload(action exploration.Action, outcome string, observation map[string]any, failure map[string]any) map[string]any {
	payload := map[string]any{"outcome": outcome, "action": action.Name}
	if action.Name == "open" || action.SessionID != "" {
		payload["sessionId"] = action.SessionID
	}
	if action.Name == "open" { /* caller replaces session id at runtime */
	}
	if observation != nil {
		payload["observation"] = observation
	}
	if failure != nil {
		payload["error"] = failure
	}
	return payload
}

func boundedDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "browser action failed"
	}
	if len(detail) > 2000 {
		return detail[:2000]
	}
	return detail
}

func (manager *Manager) explorationCurrentPage(ctx context.Context, op *operation) ([]devtoolsPage, devtoolsPage, error) {
	port, err := waitDevToolsPort(ctx, op.profile)
	if err != nil {
		return nil, devtoolsPage{}, err
	}
	client := devtoolsHTTPClient(port)
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	pages, err := devtoolsPages(ctx, client)
	if err != nil {
		return nil, devtoolsPage{}, err
	}
	for _, page := range pages {
		if page.Type == "page" && page.ID == op.exploration.currentPage {
			return pages, page, nil
		}
	}
	for _, page := range pages {
		if page.Type == "page" {
			op.exploration.currentPage = page.ID
			return pages, page, nil
		}
	}
	return nil, devtoolsPage{}, errors.New("browser operation has no page")
}

func newCDPRequest(ctx context.Context, port uint16, method, path string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
}

func explorationPageCommand(ctx context.Context, profile, path string) error {
	port, err := waitDevToolsPort(ctx, profile)
	if err != nil {
		return err
	}
	client := devtoolsHTTPClient(port)
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	request, err := newCDPRequest(ctx, port, http.MethodGet, path)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("DevTools page command: HTTP %d", response.StatusCode)
	}
	return nil
}

func activateExplorationPage(ctx context.Context, profile, pageID string) error {
	return explorationPageCommand(ctx, profile, "/json/activate/"+url.PathEscape(pageID))
}

func closeExplorationPage(ctx context.Context, profile, pageID string) error {
	return explorationPageCommand(ctx, profile, "/json/close/"+url.PathEscape(pageID))
}

// explorationTargetOrigin reads location.origin from each private Chromium
// target. It is intentionally a fixed, no-argument projection; a page URL is
// not a reliable substitute for opaque or inherited origins.
func explorationTargetOrigin(ctx context.Context, profile string, page devtoolsPage) (string, error) {
	port, err := waitDevToolsPort(ctx, profile)
	if err != nil {
		return "", err
	}
	endpoint, err := operationDevToolsURL(port, page.WebSocketDebuggerURL)
	if err != nil {
		return "", err
	}
	connection, err := dialCDP(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	stopCloseOnContext := closeCDPOnContext(ctx, connection)
	defer stopCloseOnContext()
	caller := newCDPCaller(connection, page.ID)
	raw, err := caller.evaluate(`function(){return location.origin}`, nil)
	if err != nil {
		return "", err
	}
	var origin string
	if err := json.Unmarshal(raw, &origin); err != nil {
		return "", errors.New("invalid Chromium target origin")
	}
	return origin, nil
}

type fixedActionOutput struct{ screenshot []byte }

// installDownloadDeny establishes Chromium's browser-wide deny policy before
// the frozen start URL is navigated. It intentionally uses the private
// DevTools page only to invoke this one fixed command; no endpoint escapes.
func installDownloadDeny(ctx context.Context, profile string) error {
	port, err := waitDevToolsPort(ctx, profile)
	if err != nil {
		return err
	}
	client := devtoolsHTTPClient(port)
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	pages, err := devtoolsPages(ctx, client)
	if err != nil {
		return err
	}
	for _, page := range pages {
		if page.Type != "page" {
			continue
		}
		endpoint, err := operationDevToolsURL(port, page.WebSocketDebuggerURL)
		if err != nil {
			return err
		}
		connection, err := dialCDP(ctx, endpoint)
		if err != nil {
			return err
		}
		defer connection.Close()
		stopCloseOnContext := closeCDPOnContext(ctx, connection)
		defer stopCloseOnContext()
		caller := newCDPCaller(connection, page.ID)
		_, callErr := caller.call("Browser.setDownloadBehavior", map[string]any{"behavior": "deny"})
		return callErr
	}
	return errors.New("browser has no page for download deny policy")
}

func (manager *Manager) executeFixedAction(ctx context.Context, op *operation, page devtoolsPage, action exploration.Action) (fixedActionOutput, *actionError) {
	port, err := waitDevToolsPort(ctx, op.profile)
	if err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", err.Error(), true}
	}
	endpoint, err := operationDevToolsURL(port, page.WebSocketDebuggerURL)
	if err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", err.Error(), true}
	}
	connection, err := dialCDP(ctx, endpoint)
	if err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "cannot connect to private browser target", true}
	}
	defer connection.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stop:
		}
	}()
	call := newCDPCaller(connection, page.ID)
	// Downloads are never a browser-tool capability. Chromium must acknowledge
	// the deny policy before any action is issued; a missing/unsupported command
	// is a terminal runtime fault, never permission to continue unprotected.
	if _, err := call.call("Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}); err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "cannot install browser download deny policy", true}
	}
	if _, err := call.call("Page.enable", nil); err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "cannot enable browser event collection", true}
	}
	if _, err := call.call("Network.enable", nil); err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "cannot enable browser navigation collection", true}
	}
	if _, err := call.call("Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}); err != nil {
		return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "cannot enable browser navigation lifecycle", true}
	}
	switch action.Name {
	case "goto":
		var target string
		_ = json.Unmarshal(action.Fields["url"], &target)
		if err := call.callAndAwaitNavigation("Page.navigate", map[string]any{"url": target}); err != nil {
			return fixedActionOutput{}, &actionError{"NavigationFailed", err.Error(), false}
		}
	case "back", "forward":
		raw, err := call.call("Page.getNavigationHistory", nil)
		if err != nil {
			return fixedActionOutput{}, &actionError{"NavigationFailed", err.Error(), false}
		}
		var history struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(raw, &history)
		next := history.CurrentIndex - 1
		if action.Name == "forward" {
			next = history.CurrentIndex + 1
		}
		if next < 0 || next >= len(history.Entries) {
			return fixedActionOutput{}, &actionError{"NavigationFailed", "history entry is unavailable", false}
		}
		if err := call.callAndAwaitNavigation("Page.navigateToHistoryEntry", map[string]any{"entryId": history.Entries[next].ID}); err != nil {
			return fixedActionOutput{}, &actionError{"NavigationFailed", err.Error(), false}
		}
	case "reload":
		if err := call.callAndAwaitNavigation("Page.reload", nil); err != nil {
			return fixedActionOutput{}, &actionError{"NavigationFailed", err.Error(), false}
		}
	case "screenshot":
		var full bool
		_ = json.Unmarshal(action.Fields["fullPage"], &full)
		raw, err := call.call("Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": full})
		if err != nil {
			return fixedActionOutput{}, &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		var image struct {
			Data string `json:"data"`
		}
		if json.Unmarshal(raw, &image) != nil {
			return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "invalid screenshot response", false}
		}
		bytes, decodeErr := base64.StdEncoding.DecodeString(image.Data)
		if decodeErr != nil || len(bytes) == 0 {
			return fixedActionOutput{}, &actionError{"RuntimeUnavailable", "invalid screenshot bytes", false}
		}
		return fixedActionOutput{screenshot: bytes}, nil
	case "accept_dialog", "dismiss_dialog":
		params := map[string]any{"accept": action.Name == "accept_dialog"}
		if action.Name == "accept_dialog" && len(action.Fields["promptText"]) > 0 {
			var value string
			_ = json.Unmarshal(action.Fields["promptText"], &value)
			params["promptText"] = value
		}
		if _, err := call.call("Page.handleJavaScriptDialog", params); err != nil {
			return fixedActionOutput{}, &actionError{"DialogBlocked", "no matching browser dialog", false}
		}
	default:
		if err := manager.executeDOMAction(ctx, call, op, action); err != nil {
			return fixedActionOutput{}, err
		}
	}
	// The operation-level event recorder owns the event projection. This
	// per-action command socket only consumes replies to fixed commands; merging
	// its event stream would create a second, nondeterministic trace writer.
	// It still observes an immediately delivered download event as a fail-closed
	// safety fence; it does not record that event in the trace.
	if call.observeDownloadWindow(75*time.Millisecond) || call.downloadBlocked {
		return fixedActionOutput{}, &actionError{"DownloadBlocked", "browser download was blocked", false}
	}
	return fixedActionOutput{}, nil
}

type cdpCaller struct {
	connection      *websocket.Conn
	next            int
	pageID          string
	downloadBlocked bool
	events          []browserEvent
	eventsTruncated bool
}

func newCDPCaller(connection *websocket.Conn, pageID ...string) *cdpCaller {
	caller := &cdpCaller{connection: connection}
	if len(pageID) > 0 {
		caller.pageID = pageID[0]
	}
	return caller
}
func (caller *cdpCaller) call(method string, params any) (json.RawMessage, error) {
	caller.next++
	body, _ := json.Marshal(params)
	if err := websocket.JSON.Send(caller.connection, devtoolsMessage{ID: caller.next, Method: method, Params: body}); err != nil {
		return nil, err
	}
	for {
		var reply devtoolsMessage
		if err := websocket.JSON.Receive(caller.connection, &reply); err != nil {
			return nil, err
		}
		if caller.recordEvent(reply) {
			continue
		}
		if reply.ID != caller.next {
			continue
		}
		if reply.Error != nil {
			return nil, errors.New(reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// callAndAwaitNavigation binds the navigation command to its top-level
// Document request and waits for both network completion and DOMContentLoaded.
// Returning directly from Page.navigate (or reload/history navigation) races
// the subsequent observation against the prior page and makes a successful
// browser Tool result claim a transition Chromium has not completed.
func (caller *cdpCaller) callAndAwaitNavigation(method string, params any) error {
	// Page.navigate reports its target frame/loader in the command reply. Reload
	// and history traversal deliberately return {}, so capture the current top
	// frame before issuing either command and discover its new loader from events.
	knownFrameID := ""
	if method != "Page.navigate" {
		raw, err := caller.call("Page.getFrameTree", nil)
		if err != nil {
			return err
		}
		var tree struct {
			FrameTree struct {
				Frame struct {
					ID string `json:"id"`
				} `json:"frame"`
			} `json:"frameTree"`
		}
		if err := json.Unmarshal(raw, &tree); err != nil || tree.FrameTree.Frame.ID == "" {
			return errors.New("navigation frame tree is unavailable")
		}
		knownFrameID = tree.FrameTree.Frame.ID
	}
	caller.next++
	commandID := caller.next
	body, _ := json.Marshal(params)
	if err := websocket.JSON.Send(caller.connection, devtoolsMessage{ID: commandID, Method: method, Params: body}); err != nil {
		return err
	}
	commandAccepted := false
	var requestID, frameID, loaderID, failure string
	finished, domContentLoaded, sameDocument, topFrameNavigated := false, false, false, false
	// CDP does not guarantee the command result precedes its matching Network or
	// Page event. Retain this bounded prefix and replay it after Page.navigate
	// identifies the actual top-level frame/loader instead of silently losing an
	// early document completion (or mistaking an iframe for it).
	pending := make([]devtoolsMessage, 0, 16)
	observe := func(message devtoolsMessage) {
		switch message.Method {
		case "Page.navigatedWithinDocument":
			var event struct {
				FrameID string `json:"frameId"`
			}
			if json.Unmarshal(message.Params, &event) == nil && event.FrameID == frameID {
				sameDocument = true
			}
		case "Page.frameNavigated":
			var frame struct {
				Frame struct {
					ID       string `json:"id"`
					LoaderID string `json:"loaderId"`
					ParentID string `json:"parentId"`
				} `json:"frame"`
			}
			if json.Unmarshal(message.Params, &frame) == nil && frame.Frame.ID == frameID && frame.Frame.ParentID == "" {
				topFrameNavigated = true
				if frame.Frame.LoaderID != "" {
					loaderID = frame.Frame.LoaderID
				}
			}
		case "Network.requestWillBeSent":
			var request struct {
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				RequestID string `json:"requestId"`
				Type      string `json:"type"`
			}
			if json.Unmarshal(message.Params, &request) == nil && request.Type == "Document" && request.FrameID == frameID && request.RequestID != "" {
				if loaderID == "" {
					loaderID = request.LoaderID
				}
				if request.LoaderID == loaderID {
					requestID, finished, domContentLoaded, failure = request.RequestID, false, false, ""
				}
			}
		case "Network.loadingFinished":
			var event struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(message.Params, &event) == nil && event.RequestID == requestID {
				finished = true
			}
		case "Network.loadingFailed":
			var event struct {
				RequestID string `json:"requestId"`
				ErrorText string `json:"errorText"`
			}
			if json.Unmarshal(message.Params, &event) == nil && event.RequestID == requestID {
				failure = event.ErrorText
				if failure == "" {
					failure = "document load failed"
				}
			}
		case "Page.lifecycleEvent":
			var event struct {
				FrameID  string `json:"frameId"`
				LoaderID string `json:"loaderId"`
				Name     string `json:"name"`
			}
			if json.Unmarshal(message.Params, &event) == nil && event.Name == "DOMContentLoaded" && event.FrameID == frameID && event.LoaderID == loaderID {
				domContentLoaded = true
			}
		}
	}
	for {
		// History traversal can restore a BFCache document without emitting a
		// Network Document request or lifecycle event. The verified top-frame
		// navigation is Chromium's completion boundary in that case; requiring a
		// synthetic network sequence would turn a successful back/forward action
		// into a timeout.
		if commandAccepted && ((requestID != "" && finished && domContentLoaded) || (method != "Page.navigate" && (sameDocument || topFrameNavigated))) {
			return nil
		}
		var message devtoolsMessage
		if err := websocket.JSON.Receive(caller.connection, &message); err != nil {
			return err
		}
		if message.ID == commandID {
			if message.Error != nil {
				return errors.New(message.Error.Message)
			}
			if method != "Page.navigate" {
				// Reload/history responses have no navigation shape. Their fixed
				// top-frame identity was captured before dispatch; observe events
				// to bind the new loader rather than rejecting Chromium's valid {}.
				frameID, commandAccepted = knownFrameID, true
				for _, early := range pending {
					observe(early)
				}
				pending = nil
				if failure != "" {
					return errors.New(failure)
				}
				continue
			}
			var navigate struct {
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				ErrorText string `json:"errorText"`
			}
			if json.Unmarshal(message.Result, &navigate) != nil || navigate.ErrorText != "" {
				if navigate.ErrorText == "" {
					navigate.ErrorText = "invalid navigation result"
				}
				return errors.New(navigate.ErrorText)
			}
			if navigate.FrameID == "" {
				return errors.New("navigation result omitted top-level frame")
			}
			// Chromium omits loaderId for a same-document navigation. It is already
			// complete; waiting for a Document request would time out incorrectly.
			if navigate.LoaderID == "" {
				return nil
			}
			frameID, loaderID, commandAccepted = navigate.FrameID, navigate.LoaderID, true
			for _, early := range pending {
				observe(early)
			}
			pending = nil
			if failure != "" {
				return errors.New(failure)
			}
			continue
		}
		if message.Method == "Page.downloadWillBegin" || message.Method == "Browser.downloadWillBegin" {
			caller.downloadBlocked = true
			continue
		}
		if !commandAccepted {
			// Events can legitimately precede a CDP command reply, but retaining a
			// partial prefix makes the eventual completion unverifiable. Fail
			// recoverably instead of silently dropping the 65th event and hanging
			// until the action deadline.
			if len(pending) == 64 {
				return errors.New("navigation event prefix exceeded bounded retention")
			}
			pending = append(pending, message)
			continue
		}
		observe(message)
		if failure != "" {
			return errors.New(failure)
		}
	}
}

// observeDownloadWindow catches download events that Chromium delivers after the
// action reply. It uses a bounded delay so one action cannot hold the session
// indefinitely. Other safe browser events are retained for the next projection.
func (caller *cdpCaller) observeDownloadWindow(window time.Duration) bool {
	_ = caller.connection.SetDeadline(time.Now().Add(window))
	defer caller.connection.SetDeadline(time.Time{})
	for {
		var message devtoolsMessage
		if err := websocket.JSON.Receive(caller.connection, &message); err != nil {
			return caller.downloadBlocked
		}
		caller.recordEvent(message)
		if caller.downloadBlocked {
			return true
		}
	}
}

func (caller *cdpCaller) recordEvent(message devtoolsMessage) bool {
	// Browser-level event recording is the sole trace authority. This command
	// socket must skip asynchronous frames so it can continue waiting for its
	// matching reply, but it never projects or owns them.
	if message.Method == "Page.downloadWillBegin" || message.Method == "Browser.downloadWillBegin" {
		caller.downloadBlocked = true
		return true
	}
	if message.Method != "" {
		return true
	}
	var event browserEvent
	switch message.Method {
	case "Page.frameNavigated", "Page.navigatedWithinDocument":
		var value struct {
			Frame struct {
				URL string `json:"url"`
			} `json:"frame"`
			URL string `json:"url"`
		}
		if json.Unmarshal(message.Params, &value) == nil {
			if value.URL == "" {
				value.URL = value.Frame.URL
			}
			event = safeBrowserEvent("navigation", caller.pageID, value.URL, "")
		}
	case "Page.windowOpen":
		// Popup identity belongs to Target.targetCreated on the operation-wide
		// recorder. This per-page event identifies only the opener and would
		// duplicate/misattribute the popup.
		return true
	case "Page.javascriptDialogOpening":
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Params, &value) == nil {
			event = safeBrowserEvent("dialog", caller.pageID, "", value.Type)
		}
	}
	if event.Kind != "" {
		var truncated bool
		caller.events, truncated = appendBoundedBrowserEventsWithTruncation(caller.events, []browserEvent{event})
		caller.eventsTruncated = caller.eventsTruncated || truncated || event.Truncated
		return true
	}
	return false
}

func safeBrowserEvent(kind, pageID, rawURL, detail string) browserEvent {
	parsed, _ := url.Parse(rawURL)
	projectedURL := ""
	if parsed != nil && parsed.Scheme != "" && parsed.Host != "" {
		// Query and fragment content frequently carries tokens or user input.
		// Events need only identify the path-level transition, never the full
		// navigation request.
		parsed.RawQuery, parsed.Fragment = "", ""
		projectedURL = parsed.String()
	}
	return browserEvent{
		// Origin is assigned only from Chromium's fixed location.origin query
		// by the operation recorder. URL parsing cannot represent opaque or
		// inherited origins safely.
		Kind: kind, PageID: pageID, URL: bounded(projectedURL, 4096), Detail: bounded(detail, 1000),
		Truncated: len(projectedURL) > 4096 || len(detail) > 1000,
	}
}

func appendBoundedBrowserEvents(existing, add []browserEvent) []browserEvent {
	events, _ := appendBoundedBrowserEventsWithTruncation(existing, add)
	return events
}

func appendBoundedBrowserEventsWithTruncation(existing, add []browserEvent) ([]browserEvent, bool) {
	const maxBrowserEvents = 500
	truncated := false
	for _, event := range add {
		if event.Kind == "" || event.PageID == "" {
			continue
		}
		if len(existing) == maxBrowserEvents {
			copy(existing, existing[1:])
			existing = existing[:maxBrowserEvents-1]
			truncated = true
		}
		existing = append(existing, event)
	}
	return existing, truncated
}

// awaitLocatorActionability is the sole bounded polling path for typed locator
// actions. A page can render a target after a model action starts, and a target
// can become actionable after a transition; resolving once would turn those
// normal states into spurious ElementNotFound/ElementNotInteractable failures.
// The caller's single action deadline remains the only bound.
func (manager *Manager) awaitLocatorActionability(ctx context.Context, call *cdpCaller, op *operation, locator locator, requireActionable bool) (string, explorationElementInfo, *actionError) {
	const pollInterval = 100 * time.Millisecond
	for {
		objectID, stale, count, err := manager.explorationElementObject(call, op, locator)
		if err != nil {
			return "", explorationElementInfo{}, &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		if stale {
			return "", explorationElementInfo{}, &actionError{"ElementReferenceStale", "element reference is stale", false}
		}
		if count > 1 {
			return "", explorationElementInfo{}, &actionError{"ElementNotUnique", "locator matched multiple elements", false}
		}
		if count == 1 && objectID != "" {
			info, inspectErr := inspectExplorationElement(call, objectID)
			if inspectErr != nil {
				return "", explorationElementInfo{}, &actionError{"RuntimeUnavailable", inspectErr.Error(), false}
			}
			if !requireActionable || (info.Visible && info.Enabled) {
				return objectID, info, nil
			}
		}
		select {
		case <-ctx.Done():
			if requireActionable {
				return "", explorationElementInfo{}, &actionError{"ActionTimeout", "target did not become actionable", false}
			}
			return "", explorationElementInfo{}, &actionError{"ActionTimeout", "target element was not found", false}
		case <-time.After(pollInterval):
		}
	}
}

func (manager *Manager) executeDOMAction(ctx context.Context, call *cdpCaller, op *operation, action exploration.Action) *actionError {
	if action.Name == "scroll" {
		var x, y int
		_ = json.Unmarshal(action.Fields["deltaX"], &x)
		_ = json.Unmarshal(action.Fields["deltaY"], &y)
		if _, err := call.call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": 0, "y": 0, "deltaX": x, "deltaY": y}); err != nil {
			return &actionError{"RuntimeUnavailable", "cannot dispatch browser wheel input", false}
		}
		return nil
	}
	if action.Name == "read" && len(action.Fields["locator"]) == 0 {
		return nil
	}
	if action.Name == "wait_for" {
		return manager.waitFor(ctx, call, op, action)
	}
	locator, err := locatorData(action.Fields["locator"])
	if err != nil {
		return &actionError{"ProtocolError", err.Error(), true}
	}
	objectID, info, resolveFailure := manager.awaitLocatorActionability(ctx, call, op, locator, action.Name != "read")
	if resolveFailure != nil {
		return resolveFailure
	}
	switch action.Name {
	case "read":
		return nil
	case "press":
		if err := focusExplorationElement(call, objectID); err != nil {
			return &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		var key string
		_ = json.Unmarshal(action.Fields["key"], &key)
		return dispatchExplorationKey(call, key, 0)
	case "click":
		if err := dispatchExplorationClick(call, objectID); err != nil {
			if errors.Is(err, errExplorationHitTarget) {
				return &actionError{"ElementNotInteractable", err.Error(), false}
			}
			return &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		return nil
	case "check", "uncheck":
		if info.Tag != "INPUT" || (info.Type != "checkbox" && info.Type != "radio") {
			return &actionError{"ElementNotInteractable", "target is not a checkable control", false}
		}
		want := action.Name == "check"
		// check/uncheck are desired-state operations, not blind toggles. Retrying
		// an acknowledged-but-lost action must therefore leave an already-correct
		// native control untouched.
		if info.Checked == want {
			return nil
		}
		if err := dispatchExplorationClick(call, objectID); err != nil {
			if errors.Is(err, errExplorationHitTarget) {
				return &actionError{"ElementNotInteractable", err.Error(), false}
			}
			return &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		after, inspectErr := inspectExplorationElement(call, objectID)
		if inspectErr != nil || after.Checked != want {
			return &actionError{"ElementNotInteractable", "check postcondition was not reached", false}
		}
		return nil
	case "fill":
		if (info.Tag != "INPUT" && info.Tag != "TEXTAREA") || info.ReadOnly {
			return &actionError{"ElementNotInteractable", "target is not an editable control", false}
		}
		var value string
		_ = json.Unmarshal(action.Fields["value"], &value)
		if err := focusExplorationElement(call, objectID); err != nil {
			return &actionError{"RuntimeUnavailable", err.Error(), false}
		}
		if failure := dispatchExplorationKey(call, "a", 2); failure != nil {
			return failure
		}
		if _, err := call.call("Input.insertText", map[string]any{"text": value}); err != nil {
			return &actionError{"RuntimeUnavailable", "cannot insert browser text", false}
		}
		after, inspectErr := inspectExplorationElement(call, objectID)
		if inspectErr != nil || after.Value != value {
			return &actionError{"ElementNotInteractable", "fill postcondition was not reached", false}
		}
		return nil
	case "select":
		if info.Tag != "SELECT" {
			return &actionError{"ElementNotInteractable", "target is not a select control", false}
		}
		var values []string
		_ = json.Unmarshal(action.Fields["values"], &values)
		return selectExplorationValues(call, objectID, info, values)
	default:
		return &actionError{"ProtocolError", "unsupported fixed browser interaction", true}
	}
}

// chromiumKeyEvents emits real Chromium Input-domain keyboard transitions.
// Printable keys carry text only on keyDown; control keys must never inject
// their symbolic name (for example, pressing Enter must not type "Enter").
func chromiumKeyEvents(key string) (map[string]any, map[string]any) {
	code, virtualKey := key, 0
	switch key {
	case "Enter":
		code, virtualKey = "Enter", 13
	case "Tab":
		code, virtualKey = "Tab", 9
	case "Escape":
		code, virtualKey = "Escape", 27
	case "Backspace":
		code, virtualKey = "Backspace", 8
	case "Delete":
		code, virtualKey = "Delete", 46
	case "ArrowUp":
		code, virtualKey = "ArrowUp", 38
	case "ArrowDown":
		code, virtualKey = "ArrowDown", 40
	case "ArrowLeft":
		code, virtualKey = "ArrowLeft", 37
	case "ArrowRight":
		code, virtualKey = "ArrowRight", 39
	case "Home":
		code, virtualKey = "Home", 36
	case "End":
		code, virtualKey = "End", 35
	}
	down := map[string]any{"type": "keyDown", "key": key, "code": code}
	up := map[string]any{"type": "keyUp", "key": key, "code": code}
	if virtualKey != 0 {
		down["windowsVirtualKeyCode"], up["windowsVirtualKeyCode"] = virtualKey, virtualKey
	}
	if len([]rune(key)) == 1 {
		down["text"] = key
	}
	return down, up
}

type locator struct {
	Kind, Ref, Selector, Role, Name, Label, Text, TestID string
	Exact                                                bool
	Version                                              int64
}

func locatorData(raw json.RawMessage) (locator, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return locator{}, errors.New("invalid locator")
	}
	var value locator
	value.Exact = true
	_ = json.Unmarshal(fields["kind"], &value.Kind)
	_ = json.Unmarshal(fields["ref"], &value.Ref)
	_ = json.Unmarshal(fields["role"], &value.Role)
	_ = json.Unmarshal(fields["name"], &value.Name)
	_ = json.Unmarshal(fields["label"], &value.Label)
	_ = json.Unmarshal(fields["text"], &value.Text)
	_ = json.Unmarshal(fields["testId"], &value.TestID)
	_ = json.Unmarshal(fields["exact"], &value.Exact)
	_ = json.Unmarshal(fields["observationVersion"], &value.Version)
	return value, nil
}
func (caller *cdpCaller) evaluate(script string, argument any) (json.RawMessage, error) {
	raw, err := caller.call("Runtime.evaluate", map[string]any{"expression": "(" + script + ")(" + mustJSON(argument) + ")", "returnByValue": true, "awaitPromise": true})
	if err != nil {
		return nil, err
	}
	// CDP returns a Runtime.evaluate envelope containing a RemoteObject.  The
	// fixed evaluator is only used for returnByValue projections, so unwrap the
	// object's JSON value here rather than handing callers a fake-looking CDP
	// envelope.  ExceptionDetails is a successful protocol response with a
	// failed JavaScript evaluation and must never be interpreted as projection
	// data.
	var reply struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errors.New("invalid Chromium Runtime.evaluate reply")
	}
	if len(reply.ExceptionDetails) != 0 && string(reply.ExceptionDetails) != "null" {
		return nil, errors.New("Chromium Runtime.evaluate raised an exception")
	}
	if reply.Result.Type == "" || len(reply.Result.Value) == 0 || string(reply.Result.Value) == "null" {
		return nil, errors.New("Chromium Runtime.evaluate returned no by-value result")
	}
	return reply.Result.Value, nil
}
func mustJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }

type explorationElementInfo struct {
	Tag      string   `json:"tag"`
	Type     string   `json:"type"`
	Visible  bool     `json:"visible"`
	Enabled  bool     `json:"enabled"`
	ReadOnly bool     `json:"readOnly"`
	Checked  bool     `json:"checked"`
	Value    string   `json:"value"`
	Multiple bool     `json:"multiple"`
	Options  []string `json:"options"`
	Selected []string `json:"selected"`
}

// explorationElementObject resolves a closed locator to exactly one private
// Chromium object handle. Element refs bypass selector matching entirely and
// resolve their issued backend-node identity after verifying page, document and
// observation-version fences.
func (manager *Manager) explorationElementObject(call *cdpCaller, op *operation, locator locator) (objectID string, stale bool, count int, err error) {
	if locator.Kind == "elementRef" {
		ref, ok := op.exploration.refs[locator.Ref]
		if !ok || ref.version != locator.Version || ref.pageID != call.pageID {
			return "", true, 0, nil
		}
		documentID, err := explorationDocumentID(call)
		if err != nil || documentID != ref.documentID {
			return "", true, 0, nil
		}
		raw, err := call.call("DOM.resolveNode", map[string]any{"backendNodeId": ref.backendNodeID})
		if err != nil {
			return "", true, 0, nil
		}
		var resolved struct {
			Object struct {
				ObjectID string `json:"objectId"`
			} `json:"object"`
		}
		if json.Unmarshal(raw, &resolved) != nil || resolved.Object.ObjectID == "" {
			return "", true, 0, nil
		}
		return resolved.Object.ObjectID, false, 1, nil
	}
	raw, err := call.call("Runtime.evaluate", map[string]any{"expression": "(" + fixedFindElementsScript + ")(" + mustJSON(locator) + ")", "returnByValue": false, "awaitPromise": true})
	if err != nil {
		return "", false, 0, err
	}
	var evaluated struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &evaluated) != nil || evaluated.Result.ObjectID == "" {
		return "", false, 0, errors.New("invalid Chromium locator result")
	}
	propsRaw, err := call.call("Runtime.getProperties", map[string]any{"objectId": evaluated.Result.ObjectID, "ownProperties": true})
	if err != nil {
		return "", false, 0, err
	}
	var properties struct {
		Result []struct {
			Name  string `json:"name"`
			Value struct {
				ObjectID string `json:"objectId"`
			} `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(propsRaw, &properties) != nil {
		return "", false, 0, errors.New("invalid Chromium locator properties")
	}
	objectID = ""
	count = 0
	for _, property := range properties.Result {
		if _, parseErr := strconv.Atoi(property.Name); parseErr == nil && property.Value.ObjectID != "" {
			count++
			objectID = property.Value.ObjectID
		}
	}
	if count != 1 {
		return "", false, count, nil
	}
	return objectID, false, count, nil
}

func (caller *cdpCaller) callFunctionOn(objectID, declaration string) (json.RawMessage, error) {
	return caller.callFunctionOnWithArguments(objectID, declaration, nil)
}

func (caller *cdpCaller) callFunctionOnWithArguments(objectID, declaration string, arguments []any) (json.RawMessage, error) {
	params := map[string]any{"objectId": objectID, "functionDeclaration": declaration, "returnByValue": true, "awaitPromise": true}
	if len(arguments) > 0 {
		params["arguments"] = arguments
	}
	raw, err := caller.call("Runtime.callFunctionOn", params)
	if err != nil {
		return nil, err
	}
	var response struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Result.Value) == 0 {
		return nil, errors.New("invalid Chromium element inspection")
	}
	return response.Result.Value, nil
}

func inspectExplorationElement(caller *cdpCaller, objectID string) (explorationElementInfo, error) {
	raw, err := caller.callFunctionOn(objectID, fixedElementInfoFunction)
	if err != nil {
		return explorationElementInfo{}, err
	}
	var info explorationElementInfo
	if json.Unmarshal(raw, &info) != nil {
		return explorationElementInfo{}, errors.New("invalid Chromium element state")
	}
	return info, nil
}

func focusExplorationElement(caller *cdpCaller, objectID string) error {
	_, err := caller.call("DOM.focus", map[string]any{"objectId": objectID})
	return err
}

func dispatchExplorationKey(caller *cdpCaller, key string, modifiers int) *actionError {
	down, up := chromiumKeyEvents(key)
	if modifiers != 0 {
		down["modifiers"], up["modifiers"] = modifiers, modifiers
	}
	if _, err := caller.call("Input.dispatchKeyEvent", down); err != nil {
		return &actionError{"RuntimeUnavailable", "cannot dispatch browser key event", false}
	}
	if _, err := caller.call("Input.dispatchKeyEvent", up); err != nil {
		return &actionError{"RuntimeUnavailable", "cannot dispatch browser key event", false}
	}
	return nil
}

func dispatchExplorationClick(caller *cdpCaller, objectID string) error {
	raw, err := caller.call("DOM.getBoxModel", map[string]any{"objectId": objectID})
	if err != nil {
		return err
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if json.Unmarshal(raw, &box) != nil || len(box.Model.Content) < 8 {
		return errors.New("target has no clickable box")
	}
	x := (box.Model.Content[0] + box.Model.Content[2] + box.Model.Content[4] + box.Model.Content[6]) / 4
	y := (box.Model.Content[1] + box.Model.Content[3] + box.Model.Content[5] + box.Model.Content[7]) / 4
	// A visible/enabled target can still be covered by another element. Verify
	// Chromium's actual hit target immediately before emitting Input events, so a
	// successful click never means "some element at these coordinates was hit".
	hit, err := caller.callFunctionOnWithArguments(objectID, `function(x,y){const hit=document.elementFromPoint(x,y);return !!hit&&(hit===this||this.contains(hit));}`, []any{map[string]any{"value": x}, map[string]any{"value": y}})
	if err != nil {
		return err
	}
	var hitsTarget bool
	if json.Unmarshal(hit, &hitsTarget) != nil || !hitsTarget {
		return errExplorationHitTarget
	}
	for _, eventType := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		params := map[string]any{"type": eventType, "x": x, "y": y, "button": "left", "clickCount": 1}
		if _, err := caller.call("Input.dispatchMouseEvent", params); err != nil {
			return err
		}
	}
	return nil
}

func selectExplorationValues(caller *cdpCaller, objectID string, info explorationElementInfo, values []string) *actionError {
	wanted := make(map[string]bool, len(values))
	indices := make(map[string]int, len(values))
	for _, value := range values {
		if wanted[value] {
			return &actionError{"ElementNotInteractable", "requested select values must be unique", false}
		}
		wanted[value] = true
		index := -1
		for i, candidate := range info.Options {
			if candidate == value {
				index = i
				break
			}
		}
		if index < 0 {
			return &actionError{"ElementNotInteractable", "requested select option is absent", false}
		}
		indices[value] = index
	}
	if !info.Multiple && len(wanted) != 1 {
		return &actionError{"ElementNotInteractable", "single select accepts exactly one value", false}
	}
	selected := make(map[string]bool, len(info.Selected))
	for _, value := range info.Selected {
		selected[value] = true
	}
	if sameStringSet(selected, wanted) {
		return nil // exact desired state is already realized; retries are no-ops.
	}
	if err := focusExplorationElement(caller, objectID); err != nil {
		return &actionError{"RuntimeUnavailable", err.Error(), false}
	}
	if !info.Multiple {
		if failure := chooseExplorationSelectOption(caller, indices[values[0]], 0); failure != nil {
			return failure
		}
	} else {
		// First unselect precisely the extras, then select precisely the missing
		// values. This fixed Input-domain sequence expresses a set difference; it
		// never blindly toggles every requested value.
		for value := range selected {
			if !wanted[value] {
				if failure := chooseExplorationSelectOption(caller, indexOfExplorationOption(info.Options, value), 2); failure != nil {
					return failure
				}
			}
		}
		for value := range wanted {
			if !selected[value] {
				if failure := chooseExplorationSelectOption(caller, indices[value], 2); failure != nil {
					return failure
				}
			}
		}
	}
	after, err := inspectExplorationElement(caller, objectID)
	if err != nil || !sameStringSet(stringSet(after.Selected), wanted) {
		return &actionError{"ElementNotInteractable", "select postcondition was not reached", false}
	}
	return nil
}

func chooseExplorationSelectOption(caller *cdpCaller, index, modifiers int) *actionError {
	if index < 0 {
		return &actionError{"ElementNotInteractable", "requested select option is absent", false}
	}
	if failure := dispatchExplorationKey(caller, "Home", 0); failure != nil {
		return failure
	}
	for i := 0; i < index; i++ {
		if failure := dispatchExplorationKey(caller, "ArrowDown", 0); failure != nil {
			return failure
		}
	}
	return dispatchExplorationKey(caller, "Space", modifiers)
}

func indexOfExplorationOption(options []string, value string) int {
	for index, candidate := range options {
		if candidate == value {
			return index
		}
	}
	return -1
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sameStringSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

// explorationDocumentID is Chromium's top-level frame/loader pair. It fences a
// ledger entry across navigation even when Chromium happens to retain a backend
// node number in a recycled renderer.
func explorationDocumentID(caller *cdpCaller) (string, error) {
	raw, err := caller.call("Page.getFrameTree", nil)
	if err != nil {
		return "", err
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil || tree.FrameTree.Frame.ID == "" || tree.FrameTree.Frame.LoaderID == "" {
		return "", errors.New("invalid Chromium top-level frame identity")
	}
	return tree.FrameTree.Frame.ID + "\x00" + tree.FrameTree.Frame.LoaderID, nil
}

// accessibilityProjection obtains the browser-owned accessibility tree through
// the fixed CDP Accessibility domain. It exposes only a bounded text projection.
// Backend DOM IDs remain private, but join AX semantics to the exact candidate
// nodes that own the operation-local opaque references.
func accessibilityProjection(caller *cdpCaller, candidates []observationCandidate) (string, int, bool, error) {
	const maxAXNodes = 2000
	const maxAccessibilityBytes = 200000
	raw, err := caller.call("Accessibility.getFullAXTree", nil)
	if err != nil {
		return "", 0, false, err
	}
	var tree struct {
		Nodes []struct {
			BackendNodeID int64 `json:"backendDOMNodeId"`
			Ignored       bool  `json:"ignored"`
			Role          struct {
				Value string `json:"value"`
			} `json:"role"`
			Name struct {
				Value string `json:"value"`
			} `json:"name"`
			Description struct {
				Value string `json:"value"`
			} `json:"description"`
			Value struct {
				Value string `json:"value"`
			} `json:"value"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return "", 0, false, errors.New("invalid Chromium accessibility tree")
	}
	byBackend := make(map[int64]int, len(candidates))
	for index := range candidates {
		byBackend[candidates[index].backendNodeID] = index
	}
	var text strings.Builder
	totalBytes := 0
	truncated := len(tree.Nodes) > maxAXNodes
	for index, node := range tree.Nodes {
		if index >= maxAXNodes {
			break
		}
		if node.Ignored {
			continue
		}
		role, name := node.Role.Value, node.Name.Value
		if candidateIndex, ok := byBackend[node.BackendNodeID]; ok {
			// AX roles/names are what assistive technology actually receives. Do
			// not overwrite a non-empty summary with a malformed empty AX value.
			if role != "" {
				candidates[candidateIndex].summary.Role = bounded(role, 100)
			}
			if name != "" {
				candidates[candidateIndex].summary.Name = bounded(name, 500)
			}
		}
		line := strings.TrimSpace(strings.Join([]string{role, name, node.Description.Value, node.Value.Value}, " "))
		if line == "" {
			continue
		}
		lineBytes := len(line)
		totalBytes += lineBytes
		if text.Len() > 0 {
			totalBytes++
		}
		separatorBytes := 0
		if text.Len() > 0 {
			separatorBytes = 1
		}
		if text.Len()+lineBytes+separatorBytes > maxAccessibilityBytes {
			truncated = true
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(line)
	}
	return text.String(), totalBytes, truncated, nil
}

// observationCandidates enumerates the DOM exactly once. Every visible summary
// and every backend-node identity derives from the same private remote object;
// no second positional DOM query can rebind an opaque reference after reordering.
// It returns the unbounded candidate count separately from the bounded ledger so
// truncation is explicit rather than silently hiding actionable controls.
func observationCandidates(caller *cdpCaller) ([]observationCandidate, int, error) {
	raw, err := caller.call("Runtime.evaluate", map[string]any{"expression": "(" + fixedCandidateObjectsScript + ")()", "returnByValue": false, "awaitPromise": true})
	if err != nil {
		return nil, 0, err
	}
	var evaluated struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &evaluated) != nil || evaluated.Result.ObjectID == "" {
		return nil, 0, errors.New("invalid Chromium candidate container")
	}
	containerRaw, err := caller.call("Runtime.getProperties", map[string]any{"objectId": evaluated.Result.ObjectID, "ownProperties": true})
	if err != nil {
		return nil, 0, err
	}
	var container struct {
		Result []struct {
			Name  string `json:"name"`
			Value struct {
				Value    int    `json:"value"`
				ObjectID string `json:"objectId"`
			} `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(containerRaw, &container) != nil {
		return nil, 0, errors.New("invalid Chromium candidate container")
	}
	candidateObjectID, total, hasTotal := "", 0, false
	for _, property := range container.Result {
		switch property.Name {
		case "total":
			total, hasTotal = property.Value.Value, true
		case "candidates":
			candidateObjectID = property.Value.ObjectID
		}
	}
	if total < 0 {
		return nil, 0, errors.New("invalid Chromium candidate container")
	}
	propsRaw := containerRaw
	if candidateObjectID == "" {
		// Test-only/older fixed CDP simulators return the candidate array directly.
		// Treat it as a bounded array with its observed count; production's fixed
		// script always provides the total/candidates container above.
		candidateObjectID = evaluated.Result.ObjectID
	} else {
		propsRaw, err = caller.call("Runtime.getProperties", map[string]any{"objectId": candidateObjectID, "ownProperties": true})
		if err != nil {
			return nil, 0, err
		}
	}
	var properties struct {
		Result []struct {
			Name  string `json:"name"`
			Value struct {
				ObjectID string `json:"objectId"`
			} `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(propsRaw, &properties) != nil {
		return nil, 0, errors.New("invalid Chromium candidate properties")
	}
	type remoteCandidate struct {
		index    int
		objectID string
	}
	remote := make([]remoteCandidate, 0, len(properties.Result))
	for _, property := range properties.Result {
		index, parseErr := strconv.Atoi(property.Name)
		if parseErr == nil && property.Value.ObjectID != "" {
			remote = append(remote, remoteCandidate{index: index, objectID: property.Value.ObjectID})
		}
	}
	slices.SortFunc(remote, func(a, b remoteCandidate) int { return a.index - b.index })
	if !hasTotal {
		total = len(remote)
	}
	if len(remote) > 2000 {
		remote = remote[:2000]
	}
	result := make([]observationCandidate, 0, len(remote))
	for _, candidate := range remote {
		summaryRaw, summaryErr := caller.call("Runtime.callFunctionOn", map[string]any{"objectId": candidate.objectID, "functionDeclaration": fixedCandidateSummaryFunction, "returnByValue": true, "awaitPromise": true})
		if summaryErr != nil {
			return nil, 0, summaryErr
		}
		var summaryEnvelope struct {
			Result struct {
				Value observationElement `json:"value"`
			} `json:"result"`
		}
		if json.Unmarshal(summaryRaw, &summaryEnvelope) != nil {
			return nil, 0, errors.New("invalid Chromium candidate summary")
		}
		described, describeErr := caller.call("DOM.describeNode", map[string]any{"objectId": candidate.objectID})
		if describeErr != nil {
			return nil, 0, describeErr
		}
		var node struct {
			Node struct {
				BackendNodeID int64 `json:"backendNodeId"`
			} `json:"node"`
		}
		if json.Unmarshal(described, &node) != nil || node.Node.BackendNodeID == 0 {
			return nil, 0, errors.New("invalid Chromium backend node identity")
		}
		result = append(result, observationCandidate{summary: summaryEnvelope.Result.Value, backendNodeID: node.Node.BackendNodeID})
	}
	return result, total, nil
}

// observationBackendNodeIDs builds a private stable-node ledger from the same
// fixed candidate query used by the bounded projection. Node/object identities
// never cross the Lintel boundary; only a random opaque ref is exposed.
func observationBackendNodeIDs(caller *cdpCaller) ([]int64, error) {
	raw, err := caller.call("Runtime.evaluate", map[string]any{"expression": "(" + fixedCandidateObjectsScript + ")()", "returnByValue": false, "awaitPromise": true})
	if err != nil {
		return nil, err
	}
	var evaluated struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &evaluated); err != nil || evaluated.Result.ObjectID == "" {
		return nil, errors.New("invalid Chromium candidate object list")
	}
	propsRaw, err := caller.call("Runtime.getProperties", map[string]any{"objectId": evaluated.Result.ObjectID, "ownProperties": true})
	if err != nil {
		return nil, err
	}
	var properties struct {
		Result []struct {
			Name  string `json:"name"`
			Value struct {
				ObjectID string `json:"objectId"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(propsRaw, &properties); err != nil {
		return nil, err
	}
	type candidate struct {
		index    int
		objectID string
	}
	candidates := make([]candidate, 0, len(properties.Result))
	for _, property := range properties.Result {
		index, parseErr := strconv.Atoi(property.Name)
		if parseErr == nil && property.Value.ObjectID != "" {
			candidates = append(candidates, candidate{index, property.Value.ObjectID})
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return a.index - b.index })
	if len(candidates) > 2000 {
		candidates = candidates[:2000]
	}
	ids := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		described, describeErr := caller.call("DOM.describeNode", map[string]any{"objectId": candidate.objectID})
		if describeErr != nil {
			return nil, describeErr
		}
		var node struct {
			Node struct {
				BackendNodeID int64 `json:"backendNodeId"`
			} `json:"node"`
		}
		if json.Unmarshal(described, &node) != nil || node.Node.BackendNodeID == 0 {
			return nil, errors.New("invalid Chromium backend node identity")
		}
		ids = append(ids, node.Node.BackendNodeID)
	}
	return ids, nil
}

func (manager *Manager) waitFor(ctx context.Context, call *cdpCaller, op *operation, action exploration.Action) *actionError {
	locator, err := locatorData(action.Fields["locator"])
	if err != nil {
		return &actionError{"ProtocolError", err.Error(), true}
	}
	state := rawString(action.Fields["state"])
	// The action context is the single, shared deadline for locator polling.
	// A fixed retry count creates a second hidden deadline and incorrectly makes
	// a temporarily absent `hidden` target fail before the action deadline.
	for {
		objectID, stale, count, resolveErr := manager.explorationElementObject(call, op, locator)
		if resolveErr != nil {
			return &actionError{"RuntimeUnavailable", resolveErr.Error(), false}
		}
		if stale {
			return &actionError{"ElementReferenceStale", "element reference is stale", false}
		}
		if count == 0 {
			if state == "hidden" {
				return nil
			}
			// Absence can be transient. Poll until the one action deadline rather
			// than manufacturing an independent locator deadline.
			select {
			case <-ctx.Done():
				return &actionError{"ActionTimeout", "target state was not reached", false}
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		if count > 1 {
			return &actionError{"ElementNotUnique", "locator matched multiple elements", false}
		}
		if objectID != "" {
			info, inspectErr := inspectExplorationElement(call, objectID)
			if inspectErr != nil {
				return &actionError{"RuntimeUnavailable", inspectErr.Error(), false}
			}
			if (state == "visible" && info.Visible) || (state == "hidden" && !info.Visible) || (state == "enabled" && info.Enabled) || (state == "disabled" && !info.Enabled) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return &actionError{"ActionTimeout", "browser action deadline exceeded", false}
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func (manager *Manager) explorationObservation(ctx context.Context, op *operation) (map[string]any, error) {
	// One retry turns a concurrent target/document transition into a coherent
	// projection instead of attaching a later event checkpoint to stale DOM.
	return manager.explorationObservationAttempt(ctx, op, 1)
}

func (manager *Manager) explorationObservationAttempt(ctx context.Context, op *operation, retries int) (map[string]any, error) {
	// A projection is a recorder checkpoint, not merely a DOM snapshot. Waiting
	// here also covers recoverable-error paths, which otherwise could expose an
	// observation while a newly attached top-level target was not yet traced.
	if err := awaitExplorationRecorderHealth(ctx, op.exploration); err != nil {
		return nil, err
	}
	if _, err := explorationRecorderBarrier(ctx, op.exploration); err != nil {
		return nil, err
	}
	// Do not accept a caller-supplied pre-barrier page list: it can describe a
	// different target set than the event prefix we just fenced. Capture the
	// current target inventory after that initial barrier. The recorder remains
	// live during bounded DOM work; a second ordered barrier below defines the
	// event prefix returned with this finished page/DOM snapshot.
	pages, page, err := manager.explorationCurrentPage(ctx, op)
	if err != nil {
		return nil, err
	}
	port, err := waitDevToolsPort(ctx, op.profile)
	if err != nil {
		return nil, err
	}
	endpoint, err := operationDevToolsURL(port, page.WebSocketDebuggerURL)
	if err != nil {
		return nil, err
	}
	connection, err := dialCDP(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	stopCloseOnContext := closeCDPOnContext(ctx, connection)
	defer stopCloseOnContext()
	caller := newCDPCaller(connection, page.ID)
	// The document projection and element ledger are deliberately separated:
	// candidate elements are enumerated once as private remote objects, then both
	// their bounded summaries and backend identities are derived from those exact
	// objects. A second positional query could silently bind a ref to a different
	// element after a live DOM reorder.
	raw, err := caller.evaluate(fixedDocumentObservationScript, nil)
	if err != nil {
		return nil, err
	}
	var projection struct {
		URL               string               `json:"url"`
		Origin            string               `json:"origin"`
		Title             string               `json:"title"`
		VisibleText       string               `json:"visibleText"`
		AccessibilityText string               `json:"accessibilityText"`
		Elements          []observationElement `json:"elements"`
		OriginalSizeBytes int                  `json:"originalSizeBytes"`
		Truncated         bool                 `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &projection); err != nil {
		return nil, err
	}
	documentID := ""
	documentID, identityErr := explorationDocumentID(caller)
	if identityErr != nil {
		return nil, identityErr
	}
	candidates, candidateTotal, identityErr := observationCandidates(caller)
	if identityErr != nil {
		return nil, identityErr
	}
	// Accessibility.getFullAXTree is Chromium's semantic projection, rather
	// than a lossy reconstruction from DOM attributes. The AX nodes which map to
	// action candidates update those candidates in-place, preserving the existing
	// backend-node ledger as the only authority for opaque refs.
	accessibilityText, accessibilityBytes, accessibilityTruncated, accessibilityErr := accessibilityProjection(caller, candidates)
	if accessibilityErr != nil {
		return nil, accessibilityErr
	}
	projection.Elements = projection.Elements[:0]
	for _, candidate := range candidates {
		projection.Elements = append(projection.Elements, candidate.summary)
	}
	projection.AccessibilityText = accessibilityText
	projection.OriginalSizeBytes += accessibilityBytes
	op.exploration.version++
	truncated := projection.Truncated || accessibilityTruncated || candidateTotal > len(candidates) || len(projection.URL) > 4096 || len(projection.Title) > 1000 || len(projection.VisibleText) > 200000 || len(projection.AccessibilityText) > 200000
	refs := make(map[string]explorationReference, len(projection.Elements))
	elements := make([]map[string]any, 0, len(projection.Elements))
	for _, element := range projection.Elements {
		if len(element.Role) > 100 || len(element.Name) > 500 {
			truncated = true
		}
		ref, refErr := opaqueExplorationRef()
		if refErr != nil {
			return nil, refErr
		}
		refs[ref] = explorationReference{backendNodeID: candidates[len(elements)].backendNodeID, version: op.exploration.version, pageID: page.ID, documentID: documentID}
		item := map[string]any{"ref": ref, "role": bounded(element.Role, 100), "name": bounded(element.Name, 500), "visible": element.Visible, "enabled": element.Enabled}
		if element.Checked != nil {
			item["checked"] = *element.Checked
		}
		if element.Selected != nil {
			item["selected"] = *element.Selected
		}
		elements = append(elements, item)
	}
	op.exploration.refs = refs
	summary := make([]map[string]any, 0, len(pages))
	for _, candidate := range pages {
		if candidate.Type != "page" {
			continue
		}
		if len(summary) >= 100 {
			truncated = true
			continue
		}
		origin := projection.Origin
		if candidate.ID != page.ID {
			var originErr error
			origin, originErr = explorationTargetOrigin(ctx, op.profile, candidate)
			if originErr != nil {
				return nil, originErr
			}
		}
		if len(candidate.ID) > 100 || len(candidate.URL) > 4096 || len(origin) > 2048 {
			truncated = true
		}
		summary = append(summary, map[string]any{"pageId": bounded(candidate.ID, 100), "url": bounded(candidate.URL, 4096), "origin": bounded(origin, 2048), "title": "", "current": candidate.ID == page.ID})
	}
	// location.origin is Chromium's target-derived origin and correctly reports
	// opaque origins as "null". Deriving it from a URL string would manufacture
	// misleading origins for about:, data:, blob:, and inherited-origin targets.
	origin := projection.Origin
	// The second command-ordered checkpoint occurs after page and DOM capture.
	// Its acknowledgement proves that every recorder event at or below its
	// sequence precedes this Observation boundary; events cannot be silently lost
	// while DOM inspection is in progress.
	checkpoint, barrierErr := explorationRecorderBarrier(ctx, op.exploration)
	if barrierErr != nil {
		return nil, barrierErr
	}
	// The recorder barrier orders events, but it cannot freeze Chromium's DOM.
	// Verify that the captured page inventory and top-level document are still
	// the same after the barrier; if not, retry from a fresh barrier rather than
	// publishing a mixed snapshot.
	stable, stableErr := manager.explorationObservationStillCurrent(ctx, op, page, pages, documentID)
	if stableErr != nil {
		return nil, stableErr
	}
	if !stable {
		if retries > 0 {
			return manager.explorationObservationAttempt(ctx, op, retries-1)
		}
		return nil, errors.New("browser page changed during observation checkpoint")
	}
	capturedEvents, eventsTruncated := consumeRecorderEventsThroughCheckpoint(op.exploration, checkpoint)
	truncated = truncated || eventsTruncated
	events := make([]map[string]any, 0, len(capturedEvents))
	for _, event := range capturedEvents {
		if len(events) >= 500 {
			truncated = true
			break
		}
		if event.Truncated || len(event.PageID) > 100 || len(event.URL) > 4096 || len(event.Origin) > 2048 || len(event.SourceURL) > 4096 || len(event.DestinationURL) > 4096 || len(event.Detail) > 1000 {
			truncated = true
		}
		item := map[string]any{"kind": event.Kind, "pageId": bounded(event.PageID, 100)}
		if event.URL != "" {
			item["url"] = bounded(event.URL, 4096)
		}
		if event.Origin != "" {
			item["origin"] = bounded(event.Origin, 2048)
		}
		if event.SourceURL != "" {
			item["sourceUrl"] = bounded(event.SourceURL, 4096)
		}
		if event.DestinationURL != "" {
			item["destinationUrl"] = bounded(event.DestinationURL, 4096)
		}
		if event.Detail != "" {
			item["detail"] = bounded(event.Detail, 1000)
		}
		events = append(events, item)
	}
	if len(origin) > 2048 {
		truncated = true
	}
	return map[string]any{"version": op.exploration.version, "url": bounded(projection.URL, 4096), "origin": bounded(origin, 2048), "title": bounded(projection.Title, 1000), "pages": summary, "visibleText": bounded(projection.VisibleText, 200000), "accessibilityText": bounded(projection.AccessibilityText, 200000), "elements": elements, "candidateTotal": candidateTotal, "events": events, "originalSizeBytes": projection.OriginalSizeBytes, "truncated": truncated}, nil
}

// opaqueExplorationRef deliberately has no DOM identity, selector, page ID, or
// observation counter encoded in it. The corresponding selector remains local
// to Lintel and expires with its observation version.
// consumeRecorderEventsThroughCheckpoint consumes exactly the event prefix that
// the second ordered recorder barrier observed. Later events stay queued for the
// next Observation instead of being accidentally attached to an older DOM/page
// snapshot.
func consumeRecorderEventsThroughCheckpoint(state *explorationState, checkpoint uint64) ([]browserEvent, bool) {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	captured := make([]browserEvent, 0, len(state.events))
	remaining := make([]browserEvent, 0, len(state.events))
	for _, event := range state.events {
		if event.Sequence <= checkpoint {
			captured = append(captured, event)
		} else {
			remaining = append(remaining, event)
		}
	}
	// A loss taints exactly the checkpoint interval that contains it. Preserve
	// intervals rather than one maximum evicted sequence: multiple retention
	// overflows can straddle successive barriers before either projection consumes
	// its prefix.
	truncated := false
	remainingLosses := state.droppedIntervals[:0]
	for _, loss := range state.droppedIntervals {
		if loss.last > state.checkpointSequence && loss.first <= checkpoint {
			truncated = true
		}
		if loss.last > checkpoint {
			remainingLosses = append(remainingLosses, loss)
		}
	}
	state.droppedIntervals = remainingLosses
	for _, event := range captured {
		truncated = truncated || event.Truncated
	}
	state.checkpointSequence = checkpoint
	state.events = remaining
	state.eventsTruncated = false
	return captured, truncated
}

// explorationObservationStillCurrent validates that the page set and selected
// top-level document did not change across the second recorder checkpoint.
func (manager *Manager) explorationObservationStillCurrent(ctx context.Context, op *operation, page devtoolsPage, pages []devtoolsPage, documentID string) (bool, error) {
	currentPages, currentPage, err := manager.explorationCurrentPage(ctx, op)
	if err != nil {
		return false, err
	}
	if currentPage.ID != page.ID || !sameExplorationPageInventory(pages, currentPages) {
		return false, nil
	}
	port, err := waitDevToolsPort(ctx, op.profile)
	if err != nil {
		return false, err
	}
	endpoint, err := operationDevToolsURL(port, currentPage.WebSocketDebuggerURL)
	if err != nil {
		return false, err
	}
	connection, err := dialCDP(ctx, endpoint)
	if err != nil {
		return false, err
	}
	defer connection.Close()
	stopCloseOnContext := closeCDPOnContext(ctx, connection)
	defer stopCloseOnContext()
	currentDocumentID, err := explorationDocumentID(newCDPCaller(connection, currentPage.ID))
	if err != nil {
		return false, err
	}
	return currentDocumentID == documentID, nil
}

func sameExplorationPageInventory(left, right []devtoolsPage) bool {
	// Target identity alone is insufficient: Chromium keeps a target ID across
	// reloads and same-target navigations. The URL is the stable public target
	// projection; the selected page's document ID is verified separately.
	leftPages, rightPages := make(map[string]string), make(map[string]string)
	for _, page := range left {
		if page.Type == "page" {
			leftPages[page.ID] = page.URL
		}
	}
	for _, page := range right {
		if page.Type == "page" {
			rightPages[page.ID] = page.URL
		}
	}
	if len(leftPages) != len(rightPages) {
		return false
	}
	for id, leftURL := range leftPages {
		if rightURL, found := rightPages[id]; !found || rightURL != leftURL {
			return false
		}
	}
	return true
}

func opaqueExplorationRef() (string, error) {
	bytes := make([]byte, 18)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate opaque exploration element reference: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	// Preserve a valid UTF-8 boundary. Byte slicing a multi-byte rune makes the
	// JSON encoder silently replace it, so the reported original byte count would
	// no longer truthfully explain the projection's truncation.
	end := limit
	for end > 0 && (value[end]&0xc0) == 0x80 {
		end--
	}
	return value[:end]
}

// fixedFindElementsScript resolves only the frozen typed locator vocabulary. It
// is read-only and returns real DOM objects privately to CDP, never a selector
// or object handle to a caller.
const fixedFindElementsScript = `function(l){if(l.kind==='testId')return Array.from(document.querySelectorAll('[data-testid]')).filter(e=>e.getAttribute('data-testid')===l.testId);if(l.kind==='label')return Array.from(document.querySelectorAll('label')).filter(e=>(l.exact===false?e.textContent.includes(l.label):e.textContent.trim()===l.label)).map(e=>document.getElementById(e.htmlFor)).filter(Boolean);if(l.kind==='text')return Array.from(document.querySelectorAll('body *')).filter(e=>(l.exact===false?(e.innerText||'').includes(l.text):(e.innerText||'').trim()===l.text));if(l.kind==='role')return Array.from(document.querySelectorAll('[role],button,input,select,textarea,a')).filter(e=>{const r=e.getAttribute('role')||(e.tagName==='BUTTON'?'button':e.tagName==='A'?'link':e.tagName.toLowerCase());const n=(e.getAttribute('aria-label')||e.innerText||e.value||'').trim();return r===l.role&&(!l.name||(l.exact===false?n.includes(l.name):n===l.name))});return []}`

// fixedElementInfoFunction only observes a resolved element. All state changes
// below are made through Chromium Input-domain events, never click(), assigned
// values, or synthetic DOM events.
const fixedElementInfoFunction = `function(){const x=this,s=getComputedStyle(x),visible=!!(x.offsetWidth||x.offsetHeight||x.getClientRects().length)&&s.visibility!=='hidden'&&s.display!=='none';return{tag:x.tagName,type:(x.type||'').toLowerCase(),visible:visible,enabled:!x.disabled&&x.getAttribute('aria-disabled')!=='true',readOnly:!!x.readOnly,checked:!!x.checked,value:typeof x.value==='string'?x.value:'',multiple:!!x.multiple,options:x.tagName==='SELECT'?Array.from(x.options).map(o=>o.value):[],selected:x.tagName==='SELECT'?Array.from(x.options).filter(o=>o.selected).map(o=>o.value):[]}}`

// fixedCandidateObjectsScript is the single fixed candidate enumeration used to
// bind opaque refs to Chromium backend-node identities. It performs no mutation
// and returns live node handles only to the private CDP caller.
const fixedCandidateObjectsScript = `function(){const all=Array.from(document.querySelectorAll('button,a,input,select,textarea,[role]'));return{total:all.length,candidates:all.slice(0,2000)}}`

// fixedCandidateSummaryFunction observes one object returned by the fixed
// candidate enumeration. It never writes target DOM or accepts model code.
const fixedCandidateSummaryFunction = `function(){const e=this,role=e.getAttribute('role')||(e.tagName==='BUTTON'?'button':e.tagName==='A'?'link':e.tagName.toLowerCase()),name=(e.getAttribute('aria-label')||e.innerText||'').trim(),s=getComputedStyle(e),visible=!!(e.offsetWidth||e.offsetHeight||e.getClientRects().length)&&s.visibility!=='hidden'&&s.display!=='none';return{role:role.slice(0,100),name:name.slice(0,500),visible:visible,enabled:!e.disabled&&e.getAttribute('aria-disabled')!=='true',checked:(typeof e.checked==='boolean'?e.checked:null),selected:(typeof e.selected==='boolean'?e.selected:null)}}`

// fixedDocumentObservationScript contains only document-level projection. Element
// summaries are obtained from the exact remote object ledger above, not a second
// querySelectorAll positional snapshot.
const fixedDocumentObservationScript = `function(){const cap=200000,text=(document.body&&document.body.innerText||''),chars=Array.from(text);return{url:location.href,origin:location.origin,title:document.title||'',visibleText:chars.slice(0,cap).join(''),accessibilityText:'',elements:[],originalSizeBytes:new TextEncoder().encode(text).length,truncated:chars.length>cap}}`

// Kept as a static safety corpus for tests that assert all fixed observation
// scripts remain read-only. Runtime uses fixedDocumentObservationScript.
const fixedObservationScript = fixedDocumentObservationScript
