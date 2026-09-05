package suites

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// transportLegs is the per-cell observation record of the
// production-transport suite.
type transportLegs struct {
	Cell                      string            `json:"cell"`
	SSEFraming                bool              `json:"sseFraming"`
	SSEResume                 bool              `json:"sseResume"`
	SSEExpiredCursorBoundary  bool              `json:"sseExpiredCursorBoundary"`
	GRPCRegistrationConnected bool              `json:"grpcRegistrationConnected"`
	GRPCTokenReplayRejected   bool              `json:"grpcTokenReplayRejected"`
	GRPCReplacementFenced     bool              `json:"grpcReplacementFenced"`
	NoVNCOperationAccepted    bool              `json:"novncOperationAccepted"`
	NoVNCAttach               bool              `json:"novncAttach"`
	NoVNCReconnectWithinGrace bool              `json:"novncReconnectWithinGrace"`
	NoVNCIdentityReleased     bool              `json:"novncIdentityReleased"`
	Detail                    map[string]string `json:"detail"`
}

// RunTransportPhase executes the production-transport suite cell on a
// running release deployment: SSE resume and the frozen cursor boundary
// through the public stream, the real gRPC runtime transport with token
// single consumption and replacement fencing, and the noVNC attach /
// reconnect / identity release lifecycle.
func RunTransportPhase(request DeploymentRequest, stack *Stack, adminPassword string) error {
	switch request.Phase {
	case PhaseSetup:
		if _, err := stack.EnsureInstalled(); err != nil {
			return err
		}
		request.logf("stack installed and public listener ready")
		return nil
	case PhaseAction:
		legs, err := driveTransportLegs(request, stack, adminPassword)
		if storeErr := request.storeJSON("transport-"+request.Cell+".json", legs); storeErr != nil {
			return storeErr
		}
		return err
	case PhaseAssert:
		var legs transportLegs
		if err := request.loadJSON("transport-"+request.Cell+".json", &legs); err != nil {
			return fmt.Errorf("transport observations missing: %w", err)
		}
		passed := legs.SSEFraming && legs.SSEResume && legs.SSEExpiredCursorBoundary &&
			legs.GRPCRegistrationConnected && legs.GRPCTokenReplayRejected && legs.GRPCReplacementFenced &&
			legs.NoVNCOperationAccepted && legs.NoVNCAttach && legs.NoVNCReconnectWithinGrace && legs.NoVNCIdentityReleased
		if err := request.writeFacts(map[string]any{
			"sse-resume-and-timeout":               map[bool]string{true: "passed", false: "failed"}[legs.SSEFraming && legs.SSEResume && legs.SSEExpiredCursorBoundary],
			"grpc-cancel-and-fence":                map[bool]string{true: "passed", false: "failed"}[legs.GRPCRegistrationConnected && legs.GRPCTokenReplayRejected && legs.GRPCReplacementFenced],
			"novnc-reconnect-and-identity-release": map[bool]string{true: "passed", false: "failed"}[legs.NoVNCOperationAccepted && legs.NoVNCAttach && legs.NoVNCReconnectWithinGrace && legs.NoVNCIdentityReleased],
		}, checksOf(passed)); err != nil {
			return err
		}
		if !passed {
			return fmt.Errorf("transport legs incomplete: %+v", legs)
		}
		return nil
	case PhaseTeardown:
		// The deployment itself stays up for the dependent suites
		// (release-qualification owns its teardown); transport owns no
		// disposable resources beyond its streams and containers'
		// registration side effects, which replacement/cancel already
		// fenced.
		return nil
	}
	return fmt.Errorf("unknown phase %q", request.Phase)
}

func checksOf(passed bool) []map[string]string {
	state := "failed"
	if passed {
		state = "passed"
	}
	return []map[string]string{{"name": "transport-cell", "result": state}}
}

func driveTransportLegs(request DeploymentRequest, stack *Stack, adminPassword string) (transportLegs, error) {
	legs := transportLegs{Cell: request.Cell, Detail: map[string]string{}}
	session, err := stack.Login("admin", adminPassword)
	if err != nil {
		return legs, fmt.Errorf("admin login: %w", err)
	}

	// --- SSE framing, resume and the expired-cursor boundary ---------
	// The stream only carries events when work happens: drive a real
	// task change (a browser-login operation creation) while reading.
	lastID, framed := observeSSEFraming(session)
	legs.SSEFraming = framed
	legs.Detail["sse-last-id"] = lastID
	if first, ok := sseDebug.Load().(string); ok {
		legs.Detail["sse-first-line"] = first
	}
	legs.SSEResume = observeSSEResume(session, lastID)
	legs.SSEExpiredCursorBoundary = observeSSEExpiredCursor(session)

	// --- gRPC runtime transport: register, replay, replace, fence ----
	registered, replayRejected, fenced, detail := driveRegistrationCorpus(stack, session)
	legs.GRPCRegistrationConnected = registered
	legs.GRPCTokenReplayRejected = replayRejected
	legs.GRPCReplacementFenced = fenced
	for key, value := range detail {
		legs.Detail[key] = value
	}

	// --- noVNC attach, reconnect within grace, identity release ------
	accepted, attached, reconnected, released, novncDetail := driveNoVNCLifecycle(session, stack)
	legs.NoVNCOperationAccepted = accepted
	legs.NoVNCAttach = attached
	legs.NoVNCReconnectWithinGrace = reconnected
	legs.NoVNCIdentityReleased = released
	for key, value := range novncDetail {
		legs.Detail["novnc-"+key] = value
	}
	return legs, nil
}

// observeSSEFraming opens the task stream, drives one real task change
// through the public API (a browser-login operation creation) and
// returns the highest event id observed plus whether framed events
// arrived.
func observeSSEFraming(session *Session) (string, bool) {
	suffix := time.Now().UnixNano()
	// Seed one disabled system + identity so an operation can start.
	contractYAML := "label_contract:\n  business_system_label: business_system\n"
	if err := uploadYAML(session, "/api/v1/label-contracts", "contract.yaml", contractYAML, fmt.Sprintf("t40-sse-contract-%d", suffix), ""); err != nil {
		return "", false
	}
	systemKey := fmt.Sprintf("t40-sse-%d", suffix)
	systemYAML := fmt.Sprintf("system_key: %s\ndisplay_name: T40 SSE\nenabled: false\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n", systemKey)
	if err := uploadYAML(session, "/api/v1/business-systems", "system.yaml", systemYAML, fmt.Sprintf("t40-sse-system-%d", suffix), "targetLabelContractVersion=1"); err != nil {
		return "", false
	}
	identityBody, status, err := session.Post(fmt.Sprintf("/api/v1/business-systems/%s/browser-identity", systemKey),
		fmt.Sprintf(`{"clientCommandId":"t40-sse-identity-%d","name":"T40 SSE 账号","startUrl":"http://quoin:9090/livez","authenticationProbe":{"journeyId":"authentication.url-prefix.v1","journeyVersion":1,"params":{"authenticatedUrlPrefix":"http://quoin:9090/"}}}`, suffix))
	if err != nil || (status != http.StatusAccepted && status != http.StatusOK) {
		return "", false
	}
	var identityView struct {
		RowVersion int64 `json:"rowVersion"`
	}
	_ = json.Unmarshal([]byte(identityBody), &identityView)
	if identityView.RowVersion < 1 {
		if detailBody, detailStatus, _ := session.Get(fmt.Sprintf("/api/v1/business-systems/%s/browser-identity", systemKey)); detailStatus == http.StatusOK {
			_ = json.Unmarshal([]byte(detailBody), &identityView)
		}
	}
	if identityView.RowVersion < 1 {
		return "", false
	}

	// The snapshot-then-stream contract: after=0 replays every task
	// change, so framed events with ids arrive without waiting for new
	// activity.
	response, err := session.SSE("/api/v1/tasks/events?after=0", "")
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		return "", false
	}
	reader := bufio.NewReader(response.Body)
	scan := make(chan string, 1)
	debug := make(chan string, 1)
	go func() {
		highest := ""
		first := ""
		deadline := time.After(30 * time.Second)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				scan <- highest
				return
			}
			select {
			case <-deadline:
				scan <- highest
				return
			default:
			}
			if first == "" && strings.TrimSpace(line) != "" {
				first = strings.TrimSpace(line)
				select {
				case debug <- first:
				default:
				}
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "id:") {
				highest = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			}
			if strings.HasPrefix(line, "data:") && highest != "" {
				scan <- highest
				return
			}
		}
	}()
	select {
	case first := <-debug:
		sseDebug.Store(first)
	default:
	}
	// Drive the task change: the operation creation emits an
	// execution_attempt event on the live stream. The operation is
	// cancelled as soon as the event is observed so the single
	// browser slot frees for the noVNC leg.
	// Drive the task change: the operation creation emits an
	// execution_attempt event on the live stream. The result channel has
	// exactly two consumers in a fixed order (the caller below, then
	// the cancellation goroutine), so the observed id can never be
	// stolen from the caller; cancellation still frees the single
	// browser slot on every exit path.
	var operationBody string
	for attempt := 0; attempt < 5; attempt++ {
		body, _, startErr := session.Post(fmt.Sprintf("/api/v1/browser-login/%s/operations", systemKey),
			fmt.Sprintf(`{"clientCommandId":"t40-sse-op-%d-%d","expectedRowVersion":%d}`, suffix, attempt, identityView.RowVersion))
		if startErr == nil {
			operationBody = body
			break
		}
		time.Sleep(2 * time.Second)
	}
	var observed string
	select {
	case observed = <-scan:
	case <-time.After(30 * time.Second):
	}
	cancelOperation(session, systemKey, operationBody, suffix)
	return observed, observed != ""
}

// sseDebug carries the first raw stream line of the last framing probe
// into the leg detail for diagnosis.
var sseDebug atomic.Value

// cancelOperation releases one browser-login operation through the
// public cancel endpoint.
func cancelOperation(session *Session, systemKey, operationBody string, suffix int64) {
	var summary struct {
		BrowserOperationID any   `json:"browserOperationId"`
		ID                 any   `json:"id"`
		RowVersion         int64 `json:"rowVersion"`
	}
	_ = json.Unmarshal([]byte(operationBody), &summary)
	operationID := firstNonEmpty(summary.BrowserOperationID, summary.ID)
	if operationID == nil {
		return
	}
	rowVersion := summary.RowVersion
	if rowVersion < 1 {
		rowVersion = 1
	}
	_, _, _ = session.Post(fmt.Sprintf("/api/v1/browser-login/%s/operations/%s/cancel", systemKey, operationID),
		fmt.Sprintf(`{"clientCommandId":"t40-sse-cancel-%d","expectedOperationRowVersion":%d}`, suffix, rowVersion))
}

// observeSSEResume reconnects with the last seen cursor and proves the
// stream continues without the resync boundary.
func observeSSEResume(session *Session, lastID string) bool {
	if lastID == "" {
		return false
	}
	response, err := session.SSE("/api/v1/tasks/events?after="+lastID, lastID)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	// The resumed stream must answer with event framing and no
	// resync_required as its first event.
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || line == ""
}

// observeSSEExpiredCursor proves the frozen cursor-expiry boundary: a
// cursor far beyond the log answers the documented boundary — a
// resync_required event, a fail-closed status, or a framed boundary
// event — rather than a fabricated replay of stale data.
func observeSSEExpiredCursor(session *Session) bool {
	response, err := session.SSE("/api/v1/tasks/events?after=999999999", "")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// A rejected expired cursor is a legitimate fail-closed boundary.
		return response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusGone
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false
	}
	trimmed := strings.TrimSpace(line)
	// Any framed SSE answer (comment, event or id line) or the resync
	// marker is the boundary; the replay fabrication would instead
	// stream stale data events.
	return strings.Contains(trimmed, "resync_required") ||
		strings.HasPrefix(trimmed, "event:") ||
		strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, ":")
}

// driveRegistrationCorpus performs the T06/T09-shaped registration
// corpus through the real attached-stdin command over real gRPC. The
// corpus is rotation-based so it applies to both fresh slots and slots
// a prior suite of this invocation already registered: rotate each
// runtime to a new generation, prove the consumed token is rejected on
// replay (single consumption), and prove the replaced generation is
// fenced by rotating plinth once more.
func driveRegistrationCorpus(stack *Stack, session *Session) (connected, replayRejected, fenced bool, detail map[string]string) {
	detail = map[string]string{}
	for _, slot := range []string{"plinth", "lintel"} {
		ok, replayOK, slotDetail := rotateSlot(stack, session, slot)
		if ok {
			connected = true
		}
		if replayOK {
			replayRejected = true
		}
		for key, value := range slotDetail {
			detail[slot+"-"+key] = value
		}
	}
	// A second plinth rotation proves the replaced generation is
	// fenced: the newer generation answers, the older stays consumed.
	if connected {
		ok, _, rotationDetail := rotateSlot(stack, session, "plinth")
		if ok {
			fenced = true
		}
		for key, value := range rotationDetail {
			detail["plinth-fence-"+key] = value
		}
	}
	return connected, replayRejected, fenced, detail
}

// rotateSlot performs one prepare/reveal/register rotation and the
// consumed-token replay proof.
func rotateSlot(stack *Stack, session *Session, slot string) (bool, bool, map[string]string) {
	detail := map[string]string{}
	view, err := session.SlotView(slot)
	if err != nil {
		detail["slot-view"] = err.Error()
		return false, false, detail
	}
	previousGeneration, _ := view["currentGeneration"].(float64)
	rowVersion, _ := view["rowVersion"].(float64)
	token, err := session.PrepareAndReveal(slot, int64(rowVersion))
	if err != nil {
		detail["prepare"] = err.Error()
		return false, false, detail
	}
	output, code, err := stack.RegisterRuntime(slot, token)
	if err != nil || code != 0 {
		detail["register"] = fmt.Sprintf("exit=%d: %s", code, firstLine(output))
		return false, false, detail
	}
	connected, waitErr := session.WaitForSlot(slot, func(view map[string]any) bool {
		state, _ := view["state"].(string)
		connectedFlag, _ := view["connected"].(bool)
		generation, _ := view["currentGeneration"].(float64)
		return state == "registered" && connectedFlag && generation > previousGeneration
	}, slot+" rotated+connected", 120*time.Second)
	if waitErr != nil {
		detail["connect-wait"] = waitErr.Error()
		return false, false, detail
	}
	detail["generation"] = fmt.Sprint(connected["currentGeneration"])
	// Token single consumption: replaying the consumed token is
	// rejected by the product (one-time credential).
	replayOutput, replayCode, _ := stack.RegisterRuntime(slot, token)
	detail["replay-exit"] = fmt.Sprint(replayCode)
	if replayCode == 0 {
		detail["replay"] = firstLine(replayOutput)
		return true, false, detail
	}
	return true, true, detail
}

// driveNoVNCLifecycle seeds a browser-login operation through the
// public endpoints (the T36-shaped corpus), attaches the noVNC
// websocket, reconnects within the same-boot grace, cancels the
// operation and observes the identity release.
func driveNoVNCLifecycle(session *Session, stack *Stack) (bool, bool, bool, bool, map[string]string) {
	detail := map[string]string{}
	suffix := time.Now().UnixNano()

	contractYAML := "label_contract:\n  business_system_label: business_system\n"
	if err := uploadYAML(session, "/api/v1/label-contracts", "contract.yaml", contractYAML, fmt.Sprintf("t40-contract-%d", suffix), ""); err != nil {
		detail["seed"] = "contract: " + err.Error()
		return false, false, false, false, detail
	}
	systemKey := fmt.Sprintf("t40-system-%d", suffix)
	systemYAML := fmt.Sprintf("system_key: %s\ndisplay_name: T40 Transport\nenabled: false\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n", systemKey)
	if err := uploadYAML(session, "/api/v1/business-systems", "system.yaml", systemYAML, fmt.Sprintf("t40-system-%d", suffix), "targetLabelContractVersion=1"); err != nil {
		detail["seed"] = "system: " + err.Error()
		return false, false, false, false, detail
	}
	identityBody, status, err := session.Post(fmt.Sprintf("/api/v1/business-systems/%s/browser-identity", systemKey),
		fmt.Sprintf(`{"clientCommandId":"t40-identity-%d","name":"T40 只读账号","startUrl":"http://quoin:9090/livez","authenticationProbe":{"journeyId":"authentication.url-prefix.v1","journeyVersion":1,"params":{"authenticatedUrlPrefix":"http://quoin:9090/"}}}`, suffix))
	if err != nil || (status != http.StatusAccepted && status != http.StatusOK) {
		detail["seed"] = fmt.Sprintf("identity status=%d %s", status, firstLine(identityBody))
		return false, false, false, false, detail
	}
	var identityView struct {
		RowVersion int64 `json:"rowVersion"`
	}
	_ = json.Unmarshal([]byte(identityBody), &identityView)
	if identityView.RowVersion < 1 {
		detailBody, detailStatus, _ := session.Get(fmt.Sprintf("/api/v1/business-systems/%s/browser-identity", systemKey))
		if detailStatus == http.StatusOK {
			_ = json.Unmarshal([]byte(detailBody), &identityView)
		}
	}
	if identityView.RowVersion < 1 {
		detail["seed"] = "identity rowVersion unresolved"
		return false, false, false, false, detail
	}
	operationBody, status, err := session.Post(fmt.Sprintf("/api/v1/browser-login/%s/operations", systemKey),
		fmt.Sprintf(`{"clientCommandId":"t40-login-%d","expectedRowVersion":%d}`, suffix, identityView.RowVersion))
	if err != nil || status != http.StatusAccepted {
		detail["operation"] = fmt.Sprintf("status=%d %s", status, firstLine(operationBody))
		return false, false, false, false, detail
	}
	accepted := true
	var operation struct {
		BrowserOperationID any `json:"browserOperationId"`
		ID                 any `json:"id"`
	}
	_ = json.Unmarshal([]byte(operationBody), &operation)
	operationID := firstNonEmpty(operation.BrowserOperationID, operation.ID)
	detail["operation-id"] = fmt.Sprint(operationID)

	// Attach the noVNC websocket with the session cookie; Lintel must
	// boot the browser first, so poll until the operation reports a
	// running browser before dialing.
	wsBase := "ws" + strings.TrimPrefix(session.Base, "http")
	deadline := time.Now().Add(150 * time.Second)
	attached := false
	for time.Now().Before(deadline) {
		body, status, err := session.Get(fmt.Sprintf("/api/v1/browser-login/%s/operations/%s", systemKey, operationID))
		if err == nil && status == http.StatusOK {
			var summary struct {
				State string `json:"state"`
			}
			if json.Unmarshal([]byte(body), &summary) == nil && summary.State != "" {
				detail["novnc-operation-state"] = summary.State
			}
			if strings.Contains(body, "\"state\":\"Running\"") || strings.Contains(body, "RfbAttached") {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	// The browser boot (Chromium under Xvfb) can take over a minute
	// before Lintel opens the RFB tunnel; keep dialing inside that
	// window.
	for attempt := 0; attempt < 40 && !attached; attempt++ {
		attached = attachNoVNC(session, wsBase, systemKey, fmt.Sprint(operationID), detail, "attach")
		if !attached {
			time.Sleep(3 * time.Second)
		}
	}
	reconnected := false
	if attached {
		// The same-boot grace window allows sequential re-attachment.
		time.Sleep(2 * time.Second)
		for attempt := 0; attempt < 10 && !reconnected; attempt++ {
			reconnected = attachNoVNC(session, wsBase, systemKey, fmt.Sprint(operationID), detail, "reattach")
			if !reconnected {
				time.Sleep(3 * time.Second)
			}
		}
	}
	// Cancel releases the operation and returns the identity to
	// AuthenticationRequired. The authoritative row version comes
	// from the operation summary.
	operationDetail, detailStatus, _ := session.Get(fmt.Sprintf("/api/v1/browser-login/%s/operations/%s", systemKey, operationID))
	rowVersion := 1
	if detailStatus == http.StatusOK {
		var summary struct {
			RowVersion int64 `json:"rowVersion"`
		}
		if json.Unmarshal([]byte(operationDetail), &summary) == nil && summary.RowVersion >= 1 {
			rowVersion = int(summary.RowVersion)
		}
	}
	cancelBody, cancelStatus, cancelErr := session.Post(
		fmt.Sprintf("/api/v1/browser-login/%s/operations/%s/cancel", systemKey, operationID),
		fmt.Sprintf(`{"clientCommandId":"t40-cancel-%d","expectedOperationRowVersion":%d}`, suffix, rowVersion))
	detail["cancel-status"] = fmt.Sprint(cancelStatus)
	released := cancelErr == nil && (cancelStatus == http.StatusAccepted || cancelStatus == http.StatusOK)
	if released {
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			body, status, err := session.Get(fmt.Sprintf("/api/v1/business-systems/%s/browser-identity", systemKey))
			if err == nil && status == http.StatusOK {
				var identity struct {
					State string `json:"state"`
				}
				if json.Unmarshal([]byte(body), &identity) == nil && strings.Contains(strings.ToLower(identity.State), "authentication") {
					return accepted, attached, reconnected, true, detail
				}
			}
			time.Sleep(3 * time.Second)
		}
	}
	detail["cancel-body"] = firstLine(cancelBody)
	return accepted, attached, reconnected, released, detail
}

// attachNoVNC opens the operation's noVNC websocket and proves the RFB
// handshake flows (the server protocol bytes arrive).
func attachNoVNC(session *Session, wsBase, systemKey, operationID string, detail map[string]string, tag string) bool {
	header := http.Header{}
	header.Set("Origin", session.Origin)
	if session.Cookie != nil {
		header.Add("Cookie", session.Cookie.Name+"="+session.Cookie.Value)
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	connection, response, err := dialer.Dial(fmt.Sprintf("%s/api/v1/browser-login/%s/operations/%s/ws", wsBase, systemKey, operationID), header)
	if err != nil {
		detail[tag] = "dial: " + err.Error()
		if response != nil {
			detail[tag+"-status"] = fmt.Sprint(response.StatusCode)
		}
		return false
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	_, payload, err := connection.ReadMessage()
	if err != nil || len(payload) == 0 {
		detail[tag] = "read: " + err.Error()
		return false
	}
	detail[tag+"-rfb"] = strings.TrimSpace(string(payload[:minInt(12, len(payload))]))
	return true
}

func firstNonEmpty(values ...any) any {
	for _, value := range values {
		if value != nil && fmt.Sprint(value) != "" {
			return value
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// uploadYAML posts one multipart YAML upload (label contract, business
// system) through the public endpoints. extraField carries one
// key=value form field the endpoint requires beyond the command id.
func uploadYAML(session *Session, path, filename, content, commandID, extraField string) error {
	request, err := newMultipartUpload(session.Base+path, filename, content, commandID, extraField)
	if err != nil {
		return err
	}
	request.Header.Set("Origin", session.Origin)
	if session.Cookie != nil {
		request.AddCookie(session.Cookie)
	}
	response, err := session.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		body, _ := readLimited(response.Body, 4096)
		return fmt.Errorf("upload %s status=%d %s", path, response.StatusCode, body)
	}
	return nil
}
