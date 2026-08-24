package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func stubStartNavigation(t *testing.T) {
	t.Helper()
	original := navigateOperationStartPage
	navigateOperationStartPage = func(context.Context, uint16, devtoolsPage, string) error { return nil }
	t.Cleanup(func() { navigateOperationStartPage = original })
}

func TestUnexpectedChildExitFreesSlotAndReportsCrash(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "short-lived-browser")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 0.05\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDirectory: root, Capacity: 1, ChromiumBinary: binary, XvfbBinary: binary, X0VNCBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	crashed := make(chan int64, 1)
	manager.OnCrash = func(id int64) { crashed <- id }
	manager.reconcileStartURL = func(context.Context, string, string) error { return nil }
	if _, err = manager.Start(context.Background(), 1, "https://example.test"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-crashed:
		if id != 1 {
			t.Fatalf("unexpected crashed operation %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("process exit did not free operation or report browser crash")
	}
	if _, err = manager.Start(context.Background(), 2, "https://example.test"); err != nil {
		t.Fatalf("crashed operation retained capacity: %v", err)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestChromiumCommandUsesDisposableProfileEnvironment(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	command := chromiumCommand(context.Background(), "chromium", profile)
	if !slices.Contains(command.Args, "--no-sandbox") {
		t.Fatalf("Chromium command does not use the required container sandbox fallback: %q", command.Args)
	}
	for _, wanted := range []string{
		"--user-data-dir=" + profile,
		"--app=about:blank",
		"--window-position=0,0",
		"--window-size=1280,900",
		"--remote-allow-origins=http://127.0.0.1",
		"HOME=" + profile,
		"XDG_CONFIG_HOME=" + profile,
		"XDG_CACHE_HOME=" + filepath.Join(profile, "cache"),
	} {
		if !slices.Contains(command.Args, wanted) && !slices.Contains(command.Env, wanted) {
			t.Fatalf("Chromium command missing %q: args=%q env=%q", wanted, command.Args, command.Env)
		}
	}
}

func TestNavigateStartPageWaitsForChromiumLoad(t *testing.T) {
	const startURL = "https://target.test/login"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/devtools/page/start" {
			http.NotFound(writer, request)
			return
		}
		websocket.Handler(func(connection *websocket.Conn) {
			var enable devtoolsMessage
			if err := websocket.JSON.Receive(connection, &enable); err != nil {
				t.Errorf("receive Page.enable: %v", err)
				return
			}
			if enable.Method != "Page.enable" {
				t.Errorf("first CDP method=%q, want Page.enable", enable.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: enable.ID})
			var network devtoolsMessage
			if err := websocket.JSON.Receive(connection, &network); err != nil {
				t.Errorf("receive Network.enable: %v", err)
				return
			}
			if network.Method != "Network.enable" {
				t.Errorf("second CDP method=%q, want Network.enable", network.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: network.ID})
			var lifecycle devtoolsMessage
			if err := websocket.JSON.Receive(connection, &lifecycle); err != nil {
				t.Errorf("receive Page.setLifecycleEventsEnabled: %v", err)
				return
			}
			if lifecycle.Method != "Page.setLifecycleEventsEnabled" {
				t.Errorf("third CDP method=%q, want Page.setLifecycleEventsEnabled", lifecycle.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: lifecycle.ID})
			var navigate devtoolsMessage
			if err := websocket.JSON.Receive(connection, &navigate); err != nil {
				t.Errorf("receive Page.navigate: %v", err)
				return
			}
			if navigate.Method != "Page.navigate" || !strings.Contains(string(navigate.Params), startURL) {
				t.Errorf("fixed navigation=%+v, want %q", navigate, startURL)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: navigate.ID, Result: json.RawMessage(`{"frameId":"start","loaderId":"start-load"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"frameId":"start","loaderId":"start-load","requestId":"start-request","type":"Document","request":{"url":"` + startURL + `"}}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.loadingFinished", Params: json.RawMessage(`{"requestId":"start-request"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.lifecycleEvent", Params: json.RawMessage(`{"frameId":"start","loaderId":"start-load","name":"DOMContentLoaded"}`)})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	if err := navigateStartPage(context.Background(), uint16(port), devtoolsPage{WebSocketDebuggerURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/start"}, startURL); err != nil {
		t.Fatal(err)
	}
}

func TestNavigateStartPageRejectsChromiumNavigationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			for index := 0; index < 4; index++ {
				var message devtoolsMessage
				if websocket.JSON.Receive(connection, &message) != nil {
					return
				}
				if message.ID == 4 {
					_ = websocket.JSON.Send(connection, devtoolsMessage{ID: 4, Result: json.RawMessage(`{"frameId":"start","loaderId":"start-load","errorText":"net::ERR_NAME_NOT_RESOLVED"}`)})
					return
				}
				_ = websocket.JSON.Send(connection, devtoolsMessage{ID: message.ID})
			}
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	err = navigateStartPage(context.Background(), uint16(port), devtoolsPage{WebSocketDebuggerURL: "ws" + strings.TrimPrefix(server.URL, "http")}, "https://target.test/login")
	if err == nil || !strings.Contains(err.Error(), "net::ERR_NAME_NOT_RESOLVED") {
		t.Fatalf("navigation error = %v, want Chromium navigation failure", err)
	}
}

func TestNavigateStartPageBindsEarlyMatchingDocumentCompletion(t *testing.T) {
	eventsSent := make(chan struct{})
	releaseNavigateResult := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			for index := 0; index < 4; index++ {
				var message devtoolsMessage
				if websocket.JSON.Receive(connection, &message) != nil {
					return
				}
				if message.ID != 4 {
					_ = websocket.JSON.Send(connection, devtoolsMessage{ID: message.ID})
				}
			}
			// CDP may deliver these lifecycle events before the Page.navigate
			// response. A same-URL Fetch completion must neither replace nor
			// complete the frozen top-level Document navigation.
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"frameId":"start","loaderId":"document-load","requestId":"document-request","type":"Document","request":{"url":"https://target.test/login"}}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.loadingFinished", Params: json.RawMessage(`{"requestId":"document-request"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.lifecycleEvent", Params: json.RawMessage(`{"frameId":"start","loaderId":"document-load","name":"DOMContentLoaded"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"frameId":"start","loaderId":"fetch-load","requestId":"fetch-request","type":"Fetch","request":{"url":"https://target.test/login"}}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.loadingFinished", Params: json.RawMessage(`{"requestId":"fetch-request"}`)})
			close(eventsSent)
			<-releaseNavigateResult
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: 4, Result: json.RawMessage(`{"frameId":"start","loaderId":"document-load"}`)})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- navigateStartPage(context.Background(), uint16(port), devtoolsPage{WebSocketDebuggerURL: "ws" + strings.TrimPrefix(server.URL, "http")}, "https://target.test/login")
	}()
	<-eventsSent
	select {
	case err := <-result:
		t.Fatalf("start completed before Page.navigate response: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseNavigateResult)
	if err := <-result; err != nil {
		t.Fatalf("early document completion was lost: %v", err)
	}
}

func TestEnsureStartURLReplacesDefaultPageBeforeOperationIsAttachable(t *testing.T) {
	stubStartNavigation(t)
	const startURL = "https://target.test/login"
	var mu sync.Mutex
	pages := []devtoolsPage{{ID: "blank", Type: "page", URL: "chrome://newtab/"}}
	created := false
	activated := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.URL.Path == "/json/list":
			_ = json.NewEncoder(writer).Encode(pages)
		case request.Method == http.MethodPut && request.URL.Path == "/json/new":
			got, err := url.QueryUnescape(request.URL.RawQuery)
			if err != nil || got != "about:blank" {
				http.Error(writer, "unexpected start URL", http.StatusBadRequest)
				return
			}
			created = true
			target := devtoolsPage{ID: "start", Type: "page", URL: startURL}
			pages = append(pages, target)
			_ = json.NewEncoder(writer).Encode(target)
		case request.URL.Path == "/json/close/blank":
			pages = []devtoolsPage{{ID: "start", Type: "page", URL: startURL}}
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/json/activate/start":
			activated = true
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(parsed.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartURL(context.Background(), profile, startURL); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !created || !activated || !containsOnlyStartPage(pages, startURL) {
		t.Fatalf("start reconciliation did not leave exactly the requested focused page: created=%t activated=%t pages=%+v", created, activated, pages)
	}
}

func TestEnsureStartURLKeepsCreatedTargetAcrossRedirect(t *testing.T) {
	stubStartNavigation(t)
	const startURL = "https://target.test/login"
	const redirectedURL = "https://target.test/auth/sso"
	pages := []devtoolsPage{{ID: "blank", Type: "page", URL: "chrome://newtab/"}}
	activated := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/json/list":
			_ = json.NewEncoder(writer).Encode(pages)
		case request.URL.Path == "/json/new":
			target := devtoolsPage{ID: "redirected", Type: "page", URL: redirectedURL}
			pages = append(pages, target)
			_ = json.NewEncoder(writer).Encode(target)
		case request.URL.Path == "/json/close/blank":
			pages = []devtoolsPage{{ID: "redirected", Type: "page", URL: redirectedURL}}
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/json/activate/redirected":
			activated = "redirected"
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(parsed.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartURL(context.Background(), profile, startURL); err != nil {
		t.Fatal(err)
	}
	if activated != "redirected" || !containsOnlyPageID(pages, "redirected") {
		t.Fatalf("redirected start target was not retained and activated: activated=%q pages=%+v", activated, pages)
	}
}

func TestStartWithProfileDiscardsStaleDevToolsEndpoint(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "sleep")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "published")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "DevToolsActivePort"), []byte("65535\n/devtools/browser/stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDirectory: root, Capacity: 1, ChromiumBinary: binary, XvfbBinary: binary, X0VNCBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	manager.reconcileStartURL = func(_ context.Context, profile, _ string) error {
		if _, err := os.Stat(filepath.Join(profile, "DevToolsActivePort")); !os.IsNotExist(err) {
			t.Fatalf("cloned profile retained stale DevTools endpoint: %v", err)
		}
		return nil
	}
	if _, err := manager.StartWithProfile(context.Background(), 1, "https://target.test/login", source); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureStartURLConvergesExistingStartPages(t *testing.T) {
	stubStartNavigation(t)
	const startURL = "https://target.test/login"
	for _, initial := range [][]devtoolsPage{
		{{ID: "start", Type: "page", URL: startURL}, {ID: "blank", Type: "page", URL: "chrome://newtab/"}},
		{{ID: "start-one", Type: "page", URL: startURL}, {ID: "start-two", Type: "page", URL: startURL}},
	} {
		pages := append([]devtoolsPage(nil), initial...)
		created := false
		activated := ""
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch {
			case request.URL.Path == "/json/list":
				_ = json.NewEncoder(writer).Encode(pages)
			case request.URL.Path == "/json/new":
				created = true
				writer.WriteHeader(http.StatusOK)
			case strings.HasPrefix(request.URL.Path, "/json/close/"):
				id := strings.TrimPrefix(request.URL.Path, "/json/close/")
				pages = slices.DeleteFunc(pages, func(page devtoolsPage) bool { return page.ID == id })
				writer.WriteHeader(http.StatusOK)
			case strings.HasPrefix(request.URL.Path, "/json/activate/"):
				activated = strings.TrimPrefix(request.URL.Path, "/json/activate/")
				writer.WriteHeader(http.StatusOK)
			default:
				http.NotFound(writer, request)
			}
		}))
		parsed, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		profile := t.TempDir()
		if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(parsed.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ensureStartURL(context.Background(), profile, startURL); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if created || !containsOnlyStartPage(pages, startURL) || activated == "" {
			t.Fatalf("reconciliation did not converge existing pages: initial=%+v created=%t activated=%q pages=%+v", initial, created, activated, pages)
		}
	}
}

func TestEnsureStartURLDoesNotCreateDuplicatePage(t *testing.T) {
	stubStartNavigation(t)
	const startURL = "https://target.test/login"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/json/list":
			_, _ = writer.Write([]byte(`[{"id":"start","type":"page","url":"https://target.test/login"}]`))
		case "/json/activate/start":
			writer.WriteHeader(http.StatusOK)
		case "/json/new":
			created = true
			_ = json.NewEncoder(writer).Encode(devtoolsPage{ID: "unexpected", Type: "page", URL: startURL})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(parsed.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartURL(context.Background(), profile, startURL); err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("start reconciliation created a duplicate page")
	}
}

func TestProbeLoopbackPagesRejectsBackgroundAuthenticatedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/json/list" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`[{"type":"page","url":"https://target.test/authenticated"},{"type":"page","url":"https://target.test/login"}]`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated, err := probeLoopbackPages(context.Background(), uint16(port), "https://target.test/authenticated"); err == nil || authenticated {
		t.Fatalf("background authenticated page must not satisfy the probe: authenticated=%t err=%v", authenticated, err)
	}
}

func TestSameStartRequestURLAcceptsOnlyChromiumRootSlashNormalization(t *testing.T) {
	if !sameStartRequestURL("https://target.test/", "https://target.test") {
		t.Fatal("root trailing slash normalization was not accepted")
	}
	if !sameStartRequestURL("https://target.test/login", "https://target.test/login#section") {
		t.Fatal("fragment omission from HTTP request was not accepted")
	}
	for _, actual := range []string{"https://target.test/other", "https://target.test/?next=1", "http://target.test/"} {
		if sameStartRequestURL(actual, "https://target.test") {
			t.Fatalf("unexpected start request accepted: %q", actual)
		}
	}
}

func TestEnsureStartURLRetriesOnlyTimedOutFixedNavigation(t *testing.T) {
	original := navigateOperationStartPage
	calls := 0
	navigateOperationStartPage = func(context.Context, uint16, devtoolsPage, string) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	t.Cleanup(func() { navigateOperationStartPage = original })
	const startURL = "https://target.test/login"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/json/list":
			_ = json.NewEncoder(writer).Encode([]devtoolsPage{{ID: "start", Type: "page", URL: startURL}})
		case "/json/activate/start":
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(parsed.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartURL(context.Background(), profile, startURL); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("navigation calls=%d, want 2 after one timed-out first navigation", calls)
	}
}
