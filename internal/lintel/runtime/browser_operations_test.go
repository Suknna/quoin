package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
	"golang.org/x/net/websocket"
)

func fakeBrowserDevTools(pageURL, websocketURL *string, onBrowserClose func()) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/json/list":
			_, _ = fmt.Fprintf(writer, `[{"id":"start","type":"page","url":%q,"webSocketDebuggerUrl":%q}]`, *pageURL, *websocketURL)
		case "/devtools/browser/test":
			websocket.Handler(func(connection *websocket.Conn) {
				var message struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if websocket.JSON.Receive(connection, &message) == nil && message.Method == "Browser.close" && onBrowserClose != nil {
					onBrowserClose()
				}
			}).ServeHTTP(writer, request)
		case "/devtools/page/start":
			websocket.Handler(func(connection *websocket.Conn) {
				for index := 0; index < 4; index++ {
					var message struct {
						ID int `json:"id"`
					}
					if websocket.JSON.Receive(connection, &message) != nil {
						return
					}
					response := map[string]any{"id": message.ID}
					if message.ID == 4 {
						response["result"] = map[string]string{"frameId": "start", "loaderId": "start-load"}
					}
					_ = websocket.JSON.Send(connection, response)
				}
				_ = websocket.JSON.Send(connection, map[string]any{"method": "Network.requestWillBeSent", "params": map[string]any{"frameId": "start", "loaderId": "start-load", "requestId": "start-request", "type": "Document", "request": map[string]string{"url": *pageURL}}})
				_ = websocket.JSON.Send(connection, map[string]any{"method": "Network.loadingFinished", "params": map[string]string{"requestId": "start-request"}})
				_ = websocket.JSON.Send(connection, map[string]any{"method": "Page.lifecycleEvent", "params": map[string]string{"frameId": "start", "loaderId": "start-load", "name": "DOMContentLoaded"}})
			}).ServeHTTP(writer, request)
		case "/json/activate/start":
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	})
}

func TestPublishRunsTypedURLProbeAndInstallsProfile(t *testing.T) {
	pageURL := "https://app.example/login"
	websocketURL := ""
	closeFile := filepath.Join(t.TempDir(), "browser-close")
	server := httptest.NewServer(fakeBrowserDevTools(&pageURL, &websocketURL, func() {
		if err := os.WriteFile(closeFile, []byte("close"), 0o600); err != nil {
			t.Errorf("record Browser.close: %v", err)
		}
	}))
	defer server.Close()
	websocketURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/start"
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DEVTOOLS_PORT", parsed.Port())
	t.Setenv("TEST_CLOSE_FILE", closeFile)
	fake := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	idle := fake("idle", "#!/bin/sh\nexec sleep 300\n")
	chromium := fake("chromium", "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --user-data-dir=*) d=${arg#*=};; esac; done\nprintf '%s\\n/devtools/browser/test\\n' \"$TEST_DEVTOOLS_PORT\" > \"$d/DevToolsActivePort\"\nwhile [ ! -e \"$TEST_CLOSE_FILE\" ]; do sleep 0.01; done\nexit 0\n")
	stateDirectory := t.TempDir()
	manager, err := browser.NewManager(browser.Config{StateDirectory: stateDirectory, Capacity: 1, ChromiumBinary: chromium, XvfbBinary: idle, X0VNCBinary: idle})
	if err != nil {
		t.Fatal(err)
	}
	channel := &Channel{Config: ChannelConfig{StateDirectory: stateDirectory, ChromiumRevision: "Chromium-test"}, bootID: "boot", epoch: 1, browser: manager, profiles: profile.NewStore(stateDirectory), started: map[int64]*runtimev1.StartBrowserOperation{}, published: map[int64]*runtimev1.PublishBrowserProfileResult{}, tunnelCancels: map[int64]context.CancelFunc{}}
	input, err := json.Marshal(map[string]any{"schemaKind": "manual_login_v1", "operationId": 7, "identity": map[string]any{"identityId": 4, "identityRevisionId": 8, "startUrl": "https://app.example/login"}, "actorUserId": 11, "actorSessionId": 12, "authenticationProbe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "https://app.example/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	inputDigest := sha256.Sum256(input)
	start := &runtimev1.StartBrowserOperation{OperationId: 7, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN, IdentityId: 4, IdentityRevisionId: 8, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version, Input: &runtimev1.BrowserOperationInput{SchemaKind: "manual_login_v1", CanonicalJson: input, ContentDigest: inputDigest[:]}}
	if err := validateStartInput(start); err != nil {
		t.Fatalf("valid test input rejected by frozen validator: %v", err)
	}
	firstAck := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 1}, start).GetStartBrowserOperationAck()
	if !firstAck.GetAccepted() {
		t.Fatalf("start rejected: %#v", firstAck)
	}
	// Replaying the same Start on a later stream must not create a second
	// process and must retain the original accepted timestamp.
	address, _ := manager.VNCAddress(7)
	replayedAck := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 11}, start).GetStartBrowserOperationAck()
	replayedAddress, _ := manager.VNCAddress(7)
	if !replayedAck.GetAccepted() || !replayedAck.GetStartedAt().AsTime().Equal(firstAck.GetStartedAt().AsTime()) || replayedAddress != address {
		t.Fatalf("Start retry was not stable/idempotent: first=%#v replay=%#v", firstAck, replayedAck)
	}
	request := &runtimev1.PublishBrowserProfile{OperationId: 7, IdentityId: 4, IdentityRevisionId: 8, NewGeneration: 1, CommandId: "publish-command"}
	unauthenticated := channel.publishResponse(&runtimev1.ControlEnvelope{MessageId: 2}, request).GetPublishBrowserProfileResult()
	if unauthenticated.GetAccepted() || unauthenticated.GetProbeResult().GetResult() != runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_UNAUTHENTICATED {
		t.Fatalf("unexpected unauthenticated publish result: %#v", unauthenticated)
	}
	if _, running := manager.VNCAddress(7); !running || channel.started[7] == nil {
		t.Fatal("unauthenticated publish must keep the manual-login browser running")
	}
	pageURL = "https://app.example/authenticated"
	result := channel.publishResponse(&runtimev1.ControlEnvelope{MessageId: 3}, request).GetPublishBrowserProfileResult()
	if !result.GetAccepted() || result.GetProbeResult().GetResult() != runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED || len(result.GetProfileManifestDigest()) != 32 {
		t.Fatalf("unexpected authenticated publish result: %#v", result)
	}
	manifest, digest, err := channel.profiles.Inspect(4, 1)
	if err != nil || manifest.ChromiumRevision != "Chromium-test" || !bytes.Equal(digest, result.GetProfileManifestDigest()) {
		t.Fatalf("published profile is not durable: manifest=%#v digest=%x err=%v", manifest, digest, err)
	}
	generationPath, err := channel.profiles.GenerationPath(4, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(generationPath, "DevToolsActivePort")); !os.IsNotExist(err) {
		t.Fatalf("published profile retained a live DevTools endpoint: %v", err)
	}
	replayed := channel.publishResponse(&runtimev1.ControlEnvelope{MessageId: 4}, request).GetPublishBrowserProfileResult()
	if !replayed.GetAccepted() || !bytes.Equal(replayed.GetProfileManifestDigest(), result.GetProfileManifestDigest()) {
		t.Fatalf("publish retry did not replay stable result: %#v", replayed)
	}
}

func TestInventoryUsesGenerationPathButEchoesDatabaseLocator(t *testing.T) {
	directory := t.TempDir()
	store := profile.NewStore(directory)
	digest, err := store.Publish(profile.Manifest{IdentityID: 19, Generation: 4, IdentityRevision: 8, ChromiumRevision: "Chromium 140"})
	if err != nil {
		t.Fatal(err)
	}
	channel := &Channel{bootID: "boot", epoch: 2, profiles: store}
	request := &runtimev1.ProfileInventoryRequest{InventoryId: "inv-1", Profiles: []*runtimev1.ExpectedBrowserProfile{{IdentityId: 19, ProfileGenerationId: 987654, Generation: 4, ChromiumRevision: "Chromium 140", ProfileManifestDigest: digest}}}
	reply := channel.inventoryResponse(&runtimev1.ControlEnvelope{MessageId: 9}, request).GetProfileInventoryReport()
	if !reply.GetComplete() || len(reply.GetProfiles()) != 1 {
		t.Fatalf("unexpected report: %#v", reply)
	}
	observed := reply.GetProfiles()[0]
	if observed.GetProfileGenerationId() != 987654 || observed.GetStatus() != runtimev1.ProfileInventoryStatus_PROFILE_INVENTORY_STATUS_COMPATIBLE || !bytes.Equal(observed.GetObservedManifestDigest(), digest) {
		t.Fatalf("generation/locator mapping broken: %#v", observed)
	}
}

func TestStartDownloadBlockedUsesTypedRejection(t *testing.T) {
	if got := browserStartRejectReason(browser.ErrDownloadBlocked); got != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_DOWNLOAD_BLOCKED {
		t.Fatalf("download error reject reason=%s", got)
	}
}

func TestStopTombstoneRejectsLateStart(t *testing.T) {
	channel := &Channel{
		stopAcks:  map[int64]*runtimev1.StopBrowserOperationAck{42: {OperationId: 42}},
		started:   map[int64]*runtimev1.StartBrowserOperation{},
		startAcks: map[int64]*runtimev1.StartBrowserOperationAck{},
	}
	reply := channel.startResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StartBrowserOperation{OperationId: 42}).GetStartBrowserOperationAck()
	if reply.GetAccepted() || reply.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM {
		t.Fatalf("late Start escaped Stop tombstone: %#v", reply)
	}
}

func TestFailedStopCleanupRetriesInsteadOfReplayingFailure(t *testing.T) {
	directory := t.TempDir()
	manager, err := browser.NewManager(browser.Config{StateDirectory: directory, Capacity: 1, ChromiumBinary: "/bin/true", XvfbBinary: "/bin/true", X0VNCBinary: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(directory, "blocked-staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(staging, "still-open")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	channel := &Channel{bootID: "boot", epoch: 1, browser: manager, Config: ChannelConfig{StateDirectory: directory}, started: map[int64]*runtimev1.StartBrowserOperation{}, stopAcks: map[int64]*runtimev1.StopBrowserOperationAck{}, completed: map[int64]*runtimev1.CompleteBrowserOperation{}, traceStaging: map[int64]string{42: staging}}
	first := channel.stopResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StopBrowserOperation{OperationId: 42}).GetStopBrowserOperationAck()
	if first.GetCleanupOutcome() != runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_FAILED {
		t.Fatalf("first stop=%#v, want failed cleanup", first)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	second := channel.stopResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StopBrowserOperation{OperationId: 42}).GetStopBrowserOperationAck()
	if second.GetCleanupOutcome() != runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED || !second.GetTraceStagingDeleted() {
		t.Fatalf("retry did not repair stop cleanup: %#v", second)
	}
}

func TestStartRejectsTamperedFrozenInputBeforeStartingBrowser(t *testing.T) {
	input, err := json.Marshal(map[string]any{"schemaKind": "manual_login_v1", "operationId": 7, "identity": map[string]any{"identityId": 4, "identityRevisionId": 8, "startUrl": "https://app.example/login"}, "actorUserId": 11, "actorSessionId": 12, "authenticationProbe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "https://app.example/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	channel := &Channel{bootID: "boot", epoch: 1, started: map[int64]*runtimev1.StartBrowserOperation{}, startAcks: map[int64]*runtimev1.StartBrowserOperationAck{}}
	request := &runtimev1.StartBrowserOperation{OperationId: 7, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN, IdentityId: 4, IdentityRevisionId: 8, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version, Input: &runtimev1.BrowserOperationInput{SchemaKind: "manual_login_v1", CanonicalJson: input, ContentDigest: make([]byte, 32)}}
	ack := channel.startResponse(&runtimev1.ControlEnvelope{}, request).GetStartBrowserOperationAck()
	if ack.GetAccepted() || ack.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_INPUT_UNSUPPORTED {
		t.Fatalf("tampered frozen input was not rejected: %#v", ack)
	}
}

func TestPublishProbeUnavailableIsIndeterminateAndDoesNotInstallProfile(t *testing.T) {
	pageURL := "https://app.example/login"
	websocketURL := ""
	server := httptest.NewServer(fakeBrowserDevTools(&pageURL, &websocketURL, nil))
	defer server.Close()
	websocketURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/start"
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DEVTOOLS_PORT", parsed.Port())
	fake := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	idle := fake("idle", "#!/bin/sh\nexec sleep 300\n")
	chromium := fake("chromium", "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --user-data-dir=*) d=${arg#*=};; esac; done\nprintf '%s\\n/devtools/browser/test\\n' \"$TEST_DEVTOOLS_PORT\" > \"$d/DevToolsActivePort\"\nexec sleep 300\n")
	stateDirectory := t.TempDir()
	manager, err := browser.NewManager(browser.Config{StateDirectory: stateDirectory, Capacity: 1, ChromiumBinary: chromium, XvfbBinary: idle, X0VNCBinary: idle})
	if err != nil {
		t.Fatal(err)
	}
	channel := &Channel{Config: ChannelConfig{StateDirectory: stateDirectory, ChromiumRevision: "Chromium-test"}, bootID: "boot", epoch: 1, browser: manager, profiles: profile.NewStore(stateDirectory), started: map[int64]*runtimev1.StartBrowserOperation{}, published: map[int64]*runtimev1.PublishBrowserProfileResult{}, tunnelCancels: map[int64]context.CancelFunc{}}
	input, err := json.Marshal(map[string]any{"schemaKind": "manual_login_v1", "operationId": 8, "identity": map[string]any{"identityId": 5, "identityRevisionId": 9, "startUrl": "https://app.example/login"}, "actorUserId": 11, "actorSessionId": 12, "authenticationProbe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "https://app.example/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(input)
	start := &runtimev1.StartBrowserOperation{OperationId: 8, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN, IdentityId: 5, IdentityRevisionId: 9, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version, Input: &runtimev1.BrowserOperationInput{SchemaKind: "manual_login_v1", CanonicalJson: input, ContentDigest: digest[:]}}
	if ack := channel.startResponse(&runtimev1.ControlEnvelope{}, start).GetStartBrowserOperationAck(); !ack.GetAccepted() {
		t.Fatalf("start rejected: %#v", ack)
	}
	server.Close() // The DevTools endpoint is the technical-fault boundary.
	result := channel.publishResponse(&runtimev1.ControlEnvelope{}, &runtimev1.PublishBrowserProfile{OperationId: 8, IdentityId: 5, IdentityRevisionId: 9, NewGeneration: 1, CommandId: "publish-indeterminate"}).GetPublishBrowserProfileResult()
	if result.GetAccepted() || result.GetProbeResult().GetResult() != runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_INDETERMINATE || result.GetRejectReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_AUTHENTICATION_PROBE_UNAVAILABLE {
		t.Fatalf("technical probe failure must be indeterminate and reject publication: %#v", result)
	}
	if _, _, err := channel.profiles.Inspect(5, 1); err == nil {
		t.Fatal("indeterminate publish installed a profile generation")
	}
	_ = manager.Stop(8)
}

func TestTicket21ControlEnvelopeFencesStaleAndDuplicateFrames(t *testing.T) {
	seen := map[uint64]struct{}{}
	current := &runtimev1.ControlEnvelope{BootId: "boot-b", ConnectionEpoch: 1, MessageId: 1}
	if !isCurrentControlEnvelope(current, "boot-b", 1, seen) {
		t.Fatal("current control frame must be admitted exactly once")
	}
	if isCurrentControlEnvelope(current, "boot-b", 1, seen) {
		t.Fatal("physical duplicate must not execute a second time")
	}
	for _, stale := range []*runtimev1.ControlEnvelope{
		{BootId: "boot-a", ConnectionEpoch: 7, MessageId: 2},
		{BootId: "boot-b", ConnectionEpoch: 2, MessageId: 3},
		{BootId: "boot-b", ConnectionEpoch: 1, MessageId: 0},
	} {
		if isCurrentControlEnvelope(stale, "boot-b", 1, seen) {
			t.Fatalf("stale control frame bypassed the active stream fence: %#v", stale)
		}
	}
}

// TestStopTombstonesNeverEvict reproduces an old delayed Start after more than
// the former arbitrary tombstone cap. A same-boot terminal fence must remain
// authoritative until process exit.
func TestStopTombstonesNeverEvict(t *testing.T) {
	channel := &Channel{stopAcks: map[int64]*runtimev1.StopBrowserOperationAck{}, started: map[int64]*runtimev1.StartBrowserOperation{}, startAcks: map[int64]*runtimev1.StartBrowserOperationAck{}}
	for id := int64(1); id <= 2048; id++ {
		channel.operationMu.Lock()
		channel.recordStopTombstoneLocked(id, &runtimev1.StopBrowserOperationAck{OperationId: id})
		channel.operationMu.Unlock()
	}
	if len(channel.stopAcks) != 2048 {
		t.Fatalf("tombstones=%d; a terminal fence was evicted", len(channel.stopAcks))
	}
	ack := channel.startResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StartBrowserOperation{OperationId: 1}).GetStartBrowserOperationAck()
	if ack.GetAccepted() || ack.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM {
		t.Fatalf("evicted tombstone allowed delayed Start: %#v", ack)
	}
}

func TestNoCapacityStartAckIsNotCached(t *testing.T) {
	channel := &Channel{startAcks: map[int64]*runtimev1.StartBrowserOperationAck{}}
	noCapacity := &runtimev1.StartBrowserOperationAck{OperationId: 42, RejectReason: runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_NO_CAPACITY}
	channel.startAckReply(&runtimev1.ControlEnvelope{}, noCapacity)
	if channel.startAcks[42] != nil {
		t.Fatal("NO_CAPACITY must not poison a later FIFO retry on the same boot")
	}
	accepted := &runtimev1.StartBrowserOperationAck{OperationId: 42, Accepted: true}
	channel.startAckReply(&runtimev1.ControlEnvelope{}, accepted)
	if channel.startAcks[42] != accepted {
		t.Fatal("later Start acceptance was not retained for unknown-outcome replay")
	}
}

func TestRejectedStartAckIsCachedForDuplicateStart(t *testing.T) {
	channel := &Channel{
		stopAcks:  map[int64]*runtimev1.StopBrowserOperationAck{42: {OperationId: 42}},
		started:   map[int64]*runtimev1.StartBrowserOperation{},
		startAcks: map[int64]*runtimev1.StartBrowserOperationAck{},
	}
	first := channel.startResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StartBrowserOperation{OperationId: 42}).GetStartBrowserOperationAck()
	second := channel.startResponse(&runtimev1.ControlEnvelope{}, &runtimev1.StartBrowserOperation{OperationId: 42}).GetStartBrowserOperationAck()
	if first != second || first.GetAccepted() || first.GetRejectReason() != runtimev1.BrowserOperationStartRejectReason_BROWSER_OPERATION_START_REJECT_REASON_STALE_STREAM {
		t.Fatalf("rejected Start acknowledgement was not replayed exactly: first=%#v second=%#v", first, second)
	}
}

func TestCrashClaimsTerminalBeforeWaitingForRunningAction(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	channel := &Channel{
		started:   map[int64]*runtimev1.StartBrowserOperation{14: {OperationId: 14, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN}},
		completed: map[int64]*runtimev1.CompleteBrowserOperation{}, completing: map[int64]bool{},
		explorationChildren: map[int64]int64{14: 15}, explorationRunning: map[int64]bool{15: true},
		explorationCancels: map[int64]context.CancelFunc{15: func() { cancelled <- struct{}{} }},
		explorationDone:    map[int64]chan struct{}{15: done},
	}
	returned := make(chan struct{})
	go func() { channel.browserCrashed(14); close(returned) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("crash did not cancel active action")
	}
	channel.operationMu.Lock()
	claimed := channel.completing[14]
	channel.operationMu.Unlock()
	if !claimed {
		t.Fatal("crash waited for active action before claiming operation terminal")
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("crash did not complete after action quiesced")
	}
}

func TestCrashBeforeStartAckRetainsCompletionTombstone(t *testing.T) {
	channel := &Channel{
		started:    map[int64]*runtimev1.StartBrowserOperation{14: {OperationId: 14, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN}},
		completed:  map[int64]*runtimev1.CompleteBrowserOperation{},
		completing: map[int64]bool{},
	}
	channel.browserCrashed(14)
	completion := channel.completed[14]
	if completion == nil || completion.GetOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_FAILED || completion.GetTerminalReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED {
		t.Fatalf("crash before StartAck lost its completion tombstone: %#v", completion)
	}
}

func TestAcceptedStartupFailureWaitsForCurrentStreamStartAck(t *testing.T) {
	start := &runtimev1.StartBrowserOperation{OperationId: 42}
	channel := &Channel{
		started:         map[int64]*runtimev1.StartBrowserOperation{42: start},
		startupFailures: map[int64]*runtimev1.StartBrowserOperation{42: start},
	}
	// startResponse records this durable work item, but it must not begin upload
	// or Completion until RunConnect has successfully written the accepted Ack.
	if channel.completed != nil && channel.completed[42] != nil {
		t.Fatal("startup completion was created before StartAck")
	}
	if got := channel.takeStartupFailure(42); got != start {
		t.Fatalf("post-Ack startup work item=%#v", got)
	}
	if channel.startupFailures[42] != nil {
		t.Fatal("startup failure work item replayed after consumption")
	}
	if active := channel.activeBrowserOperationIDs(); len(active) != 1 || active[0] != 42 {
		t.Fatalf("accepted startup failure disappeared from physical reconciliation: %v", active)
	}
}

func TestStartupFailureCompletionReleasesOnlyTheInProgressFence(t *testing.T) {
	start := &runtimev1.StartBrowserOperation{OperationId: 91, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN}
	var sent *runtimev1.CompleteBrowserOperation
	channel := &Channel{
		completed:  map[int64]*runtimev1.CompleteBrowserOperation{},
		completing: map[int64]bool{91: true},
		controlSend: func(envelope *runtimev1.ControlEnvelope) error {
			sent = envelope.GetCompleteBrowserOperation()
			return nil
		},
	}
	channel.recordStartupFailure(start)
	if sent == nil || sent.GetTerminalReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_RUNTIME_UNAVAILABLE {
		t.Fatalf("startup failure completion=%#v", sent)
	}
	if channel.completed[91] == nil || channel.completing[91] {
		t.Fatalf("startup terminal fence state completed=%#v completing=%#v", channel.completed[91], channel.completing)
	}
}

func TestBrowserCrashCompletionWaitsForAcceptedStartAck(t *testing.T) {
	gate := make(chan struct{})
	sent := make(chan *runtimev1.CompleteBrowserOperation, 1)
	channel := &Channel{
		started:            map[int64]*runtimev1.StartBrowserOperation{92: {OperationId: 92, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN}},
		completed:          map[int64]*runtimev1.CompleteBrowserOperation{},
		completing:         map[int64]bool{},
		browserCrashes:     map[int64]bool{},
		startAckFences:     map[int64]chan struct{}{92: gate},
		explorationCancels: map[int64]context.CancelFunc{},
		explorationDone:    map[int64]chan struct{}{},
		controlSend: func(envelope *runtimev1.ControlEnvelope) error {
			sent <- envelope.GetCompleteBrowserOperation()
			return nil
		},
	}
	go channel.browserCrashed(92)
	select {
	case completion := <-sent:
		t.Fatalf("completion overtook StartAck: %#v", completion)
	case <-time.After(20 * time.Millisecond):
	}
	channel.releaseStartAckFence(92)
	select {
	case completion := <-sent:
		if completion == nil || completion.GetTerminalReason() != runtimev1.BrowserOperationTerminalReason_BROWSER_OPERATION_TERMINAL_REASON_BROWSER_CRASHED {
			t.Fatalf("wrong post-ack crash completion: %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("crash completion did not follow StartAck release")
	}
}

// Deployment verification stops carry the typed resource-zero evidence: the
// frozen clone/start-boot binding is echoed, this cleanup boot and epoch are
// bound, and a failed stop reports a still-running process instead of a
// fabricated zero (VERIFY-BROWSER-003).
func TestDeploymentVerificationStopAckCarriesTypedEvidence(t *testing.T) {
	directory := t.TempDir()
	manager, err := browser.NewManager(browser.Config{StateDirectory: directory, Capacity: 1, ChromiumBinary: "/bin/true", XvfbBinary: "/bin/true", X0VNCBinary: "/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	channel := &Channel{bootID: "boot-successor", epoch: 3, browser: manager, Config: ChannelConfig{StateDirectory: directory},
		started: map[int64]*runtimev1.StartBrowserOperation{}, stopAcks: map[int64]*runtimev1.StopBrowserOperationAck{}, completed: map[int64]*runtimev1.CompleteBrowserOperation{}}
	committed := timestamppb.New(time.Now())
	request := &runtimev1.StopBrowserOperation{
		OperationId: 77, CloneIdentity: "deployment-verification-1-9",
		OriginalStartBootId: "boot-origin", CommittedAt: committed,
	}
	ack := channel.stopResponse(&runtimev1.ControlEnvelope{}, request).GetStopBrowserOperationAck()
	if ack.GetCloneIdentity() != "deployment-verification-1-9" || ack.GetOriginalStartBootId() != "boot-origin" {
		t.Fatalf("typed binding not echoed: %#v", ack)
	}
	if ack.GetCleanupBootId() != "boot-successor" || ack.GetCleanupConnectionEpoch() != 3 {
		t.Fatalf("cleanup boot binding missing: %#v", ack)
	}
	if len(ack.GetStopFenceDigest()) != 32 || len(ack.GetCleanupStateHash()) != 32 {
		t.Fatalf("typed digests missing: %#v", ack)
	}
	// The manager's Stop succeeds against a never-started operation (no
	// process to leak), so the typed counts report zeros.
	if ack.GetOperationProcessCount() != 0 || ack.GetCloneNamespaceCount() != 0 {
		t.Fatalf("clean stop must report zero counts: %#v", ack)
	}
	if ack.GetCleanupOutcome() != runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED {
		t.Fatalf("clean stop outcome: %#v", ack)
	}
	// Replay of the identical Stop replays the exact typed tombstone.
	replay := channel.stopResponse(&runtimev1.ControlEnvelope{}, request).GetStopBrowserOperationAck()
	if replay.GetCloneIdentity() != ack.GetCloneIdentity() || replay.GetCleanupConnectionEpoch() != ack.GetCleanupConnectionEpoch() {
		t.Fatalf("typed tombstone replay diverged: %#v", replay)
	}
}

// TestDeploymentVerificationRunsProbeAgainstCloneAndStopsClean drives the
// complete Deployment Acceptance browser path with the real manager and a
// scripted Chromium: profile publish, deployment_verification Start from the
// published generation clone, the frozen URL-prefix probe completion, and the
// typed resource-zero Stop acknowledgment (VERIFY-BROWSER-001..003).
func TestDeploymentVerificationRunsProbeAgainstCloneAndStopsClean(t *testing.T) {
	pageURL := "https://app.example/login"
	websocketURL := ""
	closeFile := filepath.Join(t.TempDir(), "browser-close")
	server := httptest.NewServer(fakeBrowserDevTools(&pageURL, &websocketURL, func() {
		if err := os.WriteFile(closeFile, []byte("close"), 0o600); err != nil {
			t.Errorf("record Browser.close: %v", err)
		}
	}))
	defer server.Close()
	websocketURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/start"
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DEVTOOLS_PORT", parsed.Port())
	t.Setenv("TEST_CLOSE_FILE", closeFile)
	fake := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	idle := fake("idle", "#!/bin/sh\nexec sleep 300\n")
	chromium := fake("chromium", "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --user-data-dir=*) d=${arg#*=};; esac; done\nprintf '%s\\n/devtools/browser/test\\n' \"$TEST_DEVTOOLS_PORT\" > \"$d/DevToolsActivePort\"\nwhile [ ! -e \"$TEST_CLOSE_FILE\" ]; do sleep 0.01; done\nexit 0\n")
	stateDirectory := t.TempDir()
	manager, err := browser.NewManager(browser.Config{StateDirectory: stateDirectory, Capacity: 1, ChromiumBinary: chromium, XvfbBinary: idle, X0VNCBinary: idle})
	if err != nil {
		t.Fatal(err)
	}
	store := profile.NewStore(stateDirectory)
	channel := &Channel{Config: ChannelConfig{StateDirectory: stateDirectory, ChromiumRevision: "Chromium-test"}, bootID: "boot", epoch: 1, browser: manager, profiles: store,
		started: map[int64]*runtimev1.StartBrowserOperation{}, startAcks: map[int64]*runtimev1.StartBrowserOperationAck{}, stopAcks: map[int64]*runtimev1.StopBrowserOperationAck{},
		completed: map[int64]*runtimev1.CompleteBrowserOperation{}, completing: map[int64]bool{}, published: map[int64]*runtimev1.PublishBrowserProfileResult{}, tunnelCancels: map[int64]context.CancelFunc{}, startAckFences: map[int64]chan struct{}{}}

	// Publish a real generation through the authenticated manual-login path.
	loginInput, err := json.Marshal(map[string]any{"schemaKind": "manual_login_v1", "operationId": 30, "identity": map[string]any{"identityId": 4, "identityRevisionId": 8, "startUrl": "https://app.example/login"}, "actorUserId": 11, "actorSessionId": 12, "authenticationProbe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "https://app.example/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	loginDigest := sha256.Sum256(loginInput)
	loginStart := &runtimev1.StartBrowserOperation{OperationId: 30, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_MANUAL_LOGIN, IdentityId: 4, IdentityRevisionId: 8, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version, Input: &runtimev1.BrowserOperationInput{SchemaKind: "manual_login_v1", CanonicalJson: loginInput, ContentDigest: loginDigest[:]}}
	if ack := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 1}, loginStart).GetStartBrowserOperationAck(); !ack.GetAccepted() {
		t.Fatalf("manual login start rejected: %#v", ack)
	}
	pageURL = "https://app.example/authenticated"
	publish := channel.publishResponse(&runtimev1.ControlEnvelope{MessageId: 2}, &runtimev1.PublishBrowserProfile{OperationId: 30, IdentityId: 4, IdentityRevisionId: 8, NewGeneration: 1, CommandId: "publish-command"}).GetPublishBrowserProfileResult()
	if !publish.GetAccepted() {
		t.Fatalf("publish rejected: %#v", publish)
	}

	// The scripted Chromium exits once the close marker exists; the publish
	// path set it for operation 30, so clear it before the clone launches.
	// The clone reconciles its start page first, so the DevTools view must
	// report the login page at launch and only then flip to the
	// authenticated prefix for the probe.
	if err := os.Remove(closeFile); err != nil {
		t.Fatal(err)
	}
	pageURL = "https://app.example/login"

	// Deployment verification Start from the published generation clone.
	input, err := json.Marshal(map[string]any{
		"schemaKind": "deployment_verification_v1", "operationId": 31,
		"identity":                     map[string]any{"identityId": 4, "identityRevisionId": 8, "profileGenerationId": 1, "profileGeneration": 1, "startUrl": "https://app.example/login"},
		"verificationInvocationItemId": 9, "cloneIdentity": "deployment-verification-1-9",
		"probe": map[string]any{"id": "authentication.url-prefix.v1", "version": 1, "params": map[string]any{"authenticatedUrlPrefix": "https://app.example/authenticated"}, "catalog": map[string]any{"digest": catalog.Digest(), "version": catalog.Version}},
	})
	if err != nil {
		t.Fatal(err)
	}
	inputSum := sha256.Sum256(input)
	start := &runtimev1.StartBrowserOperation{OperationId: 31, Kind: runtimev1.BrowserOperationKind_BROWSER_OPERATION_KIND_DEPLOYMENT_VERIFICATION,
		IdentityId: 4, IdentityRevisionId: 8, ProfileGenerationId: 1, JourneyCatalogDigest: catalog.Digest(), JourneyCatalogVersion: catalog.Version,
		VerificationInvocationItemId: 9, CloneIdentity: "deployment-verification-1-9",
		Input: &runtimev1.BrowserOperationInput{SchemaKind: "deployment_verification_v1", CanonicalJson: input, ContentDigest: inputSum[:]}}
	if err := validateStartInput(start); err != nil {
		t.Fatalf("valid deployment input rejected by frozen validator: %v", err)
	}
	ack := channel.startResponse(&runtimev1.ControlEnvelope{MessageId: 3}, start).GetStartBrowserOperationAck()
	if !ack.GetAccepted() {
		t.Fatalf("deployment start rejected: %#v", ack)
	}
	// The probe observes the authenticated page: flip the DevTools view only
	// after the clone's start reconciliation completed.
	pageURL = "https://app.example/authenticated"
	// The frozen probe runs against the live clone; wait for its typed completion.
	channel.completeRevisionProbe(start)
	deadline := time.Now().Add(15 * time.Second)
	var completion *runtimev1.CompleteBrowserOperation
	for {
		channel.operationMu.Lock()
		completion = channel.completed[31]
		channel.operationMu.Unlock()
		if completion != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if completion == nil {
		if _, running := manager.VNCAddress(31); !running {
			t.Fatal("deployment clone browser is not running")
		}
		t.Fatal("probe completion missing")
	}
	if completion.GetOutcome() != runtimev1.BrowserOperationOutcome_BROWSER_OPERATION_OUTCOME_SUCCEEDED {
		t.Fatalf("probe completion outcome=%s terminal=%s probes=%#v", completion.GetOutcome(), completion.GetTerminalReason(), completion.GetProbeResults())
	}
	found := false
	for _, probe := range completion.GetProbeResults() {
		if probe.GetResult() == runtimev1.AuthenticationProbeResult_AUTHENTICATION_PROBE_RESULT_AUTHENTICATED && probe.GetJourneyId() == "authentication.url-prefix.v1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("authenticated probe observation missing: %#v", completion)
	}
	// Publishing from a deployment verification operation is rejected.
	rejected := channel.publishResponse(&runtimev1.ControlEnvelope{MessageId: 4}, &runtimev1.PublishBrowserProfile{OperationId: 31, IdentityId: 4, IdentityRevisionId: 8, NewGeneration: 2, CommandId: "dep-publish"}).GetPublishBrowserProfileResult()
	if rejected.GetAccepted() {
		t.Fatal("deployment verification operation must not publish profiles")
	}
	// The typed Stop acknowledgment proves resource zero.
	stopAck := channel.stopResponse(&runtimev1.ControlEnvelope{MessageId: 5}, &runtimev1.StopBrowserOperation{OperationId: 31, CloneIdentity: "deployment-verification-1-9", OriginalStartBootId: "boot", CommittedAt: timestamppb.New(time.Now())}).GetStopBrowserOperationAck()
	if stopAck.GetCleanupOutcome() != runtimev1.BrowserCleanupOutcome_BROWSER_CLEANUP_OUTCOME_SUCCEEDED {
		t.Fatalf("typed stop outcome: %#v", stopAck)
	}
	if stopAck.GetCloneIdentity() != "deployment-verification-1-9" || stopAck.GetOriginalStartBootId() != "boot" || stopAck.GetCleanupBootId() != "boot" {
		t.Fatalf("typed binding echo: %#v", stopAck)
	}
	for name, value := range map[string]uint64{
		"operationProcess": stopAck.GetOperationProcessCount(), "chromium": stopAck.GetChromiumProcessCount(),
		"cloneNamespace": stopAck.GetCloneNamespaceCount(), "temporaryFiles": stopAck.GetTemporaryFileCount(),
	} {
		if value != 0 {
			t.Fatalf("clean typed stop reported %s=%d", name, value)
		}
	}
}
