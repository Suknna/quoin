package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/catalog"
	"github.com/Suknna/quoin/internal/lintel/profile"
	"golang.org/x/net/websocket"
)

func fakeBrowserDevTools(pageURL, websocketURL *string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/json/list":
			_, _ = fmt.Fprintf(writer, `[{"id":"start","type":"page","url":%q,"webSocketDebuggerUrl":%q}]`, *pageURL, *websocketURL)
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
	server := httptest.NewServer(fakeBrowserDevTools(&pageURL, &websocketURL))
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
	server := httptest.NewServer(fakeBrowserDevTools(&pageURL, &websocketURL))
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
