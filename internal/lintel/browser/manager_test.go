package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/lintel/browser/exploration"
	"golang.org/x/net/websocket"
	"golang.org/x/sys/unix"
)

func stubStartNavigation(t *testing.T) {
	t.Helper()
	original := navigateOperationStartPage
	navigateOperationStartPage = func(context.Context, uint16, devtoolsPage, string) error { return nil }
	t.Cleanup(func() { navigateOperationStartPage = original })
}

func stubDownloadDeny(t *testing.T) {
	t.Helper()
	original := installOperationDownloadDeny
	installOperationDownloadDeny = func(context.Context, string) error { return nil }
	t.Cleanup(func() { installOperationDownloadDeny = original })
}

func TestUnexpectedChildExitFreesSlotAndReportsCrash(t *testing.T) {
	stubDownloadDeny(t)
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

func TestEnsureExplorationNeutralPageCreatesTargetWithoutClosingPages(t *testing.T) {
	pages := []devtoolsPage{{ID: "restored", Type: "page", URL: "https://restored.test/"}}
	created := false
	closed := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/json/list":
			_ = json.NewEncoder(writer).Encode(pages)
		case request.Method == http.MethodPut && request.URL.Path == "/json/new":
			created = true
			target := devtoolsPage{ID: "neutral", Type: "page", URL: "about:blank"}
			pages = append(pages, target)
			_ = json.NewEncoder(writer).Encode(target)
		case strings.HasPrefix(request.URL.Path, "/json/close/"):
			closed = true
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
	if err := ensureExplorationNeutralPage(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if !created || closed || !containsPageID(pages, "neutral") || !containsPageID(pages, "restored") {
		t.Fatalf("neutral preflight mutated startup pages incorrectly: created=%t closed=%t pages=%+v", created, closed, pages)
	}
}

func TestEnsureStartURLReusesNeutralPageWithoutCreatingTarget(t *testing.T) {
	stubStartNavigation(t)
	const startURL = "https://target.test/login"
	pages := []devtoolsPage{
		{ID: "neutral", Type: "page", URL: "about:blank"},
		{ID: "restored", Type: "page", URL: "https://restored.test/"},
	}
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/json/list":
			_ = json.NewEncoder(writer).Encode(pages)
		case request.URL.Path == "/json/new":
			created = true
			http.Error(writer, "neutral page must be reused", http.StatusInternalServerError)
		case request.URL.Path == "/json/close/restored":
			pages = []devtoolsPage{{ID: "neutral", Type: "page", URL: "about:blank"}}
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/json/activate/neutral":
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
	if created || !containsOnlyPageID(pages, "neutral") {
		t.Fatalf("neutral start target was not reused: created=%t pages=%+v", created, pages)
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
	stubDownloadDeny(t)
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
	if err := os.Symlink("/tmp/chromium-runtime-cookie", filepath.Join(source, "SingletonCookie")); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDirectory: root, Capacity: 1, ChromiumBinary: binary, XvfbBinary: binary, X0VNCBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	manager.reconcileStartURL = func(_ context.Context, profile, _ string) error {
		for _, name := range []string{"DevToolsActivePort", "SingletonCookie"} {
			if _, err := os.Lstat(filepath.Join(profile, name)); !os.IsNotExist(err) {
				t.Fatalf("cloned profile retained Chromium runtime entry %s: %v", name, err)
			}
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

func TestEnsureStartURLClosesRestoredPageAfterNavigation(t *testing.T) {
	const startURL = "https://target.test/login"
	original := navigateOperationStartPage
	pages := []devtoolsPage{{ID: "start", Type: "page", URL: startURL}}
	navigateOperationStartPage = func(context.Context, uint16, devtoolsPage, string) error {
		// Chromium can restore a profile tab after the initial reconciliation;
		// it must not remain alongside the operation's fixed start page.
		pages = append(pages, devtoolsPage{ID: "restored", Type: "page", URL: "https://target.test/authenticated"})
		return nil
	}
	t.Cleanup(func() { navigateOperationStartPage = original })
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/json/list":
			_ = json.NewEncoder(writer).Encode(pages)
		case strings.HasPrefix(request.URL.Path, "/json/close/"):
			id := strings.TrimPrefix(request.URL.Path, "/json/close/")
			pages = slices.DeleteFunc(pages, func(page devtoolsPage) bool { return page.ID == id })
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/json/activate/start":
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
	if !containsOnlyPageID(pages, "start") {
		t.Fatalf("post-navigation restored page remained: %+v", pages)
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

func TestExplorationBrowserEventsAreBoundedAndSafe(t *testing.T) {
	events := appendBoundedBrowserEvents(nil, []browserEvent{safeBrowserEvent("navigation", "page-1", "https://example.test/path?token=not-projected", "")})
	// URL sanitization never fabricates an origin. The live recorder fills it
	// only from Chromium's fixed location.origin projection.
	if len(events) != 1 || events[0].Origin != "" || events[0].PageID != "page-1" {
		t.Fatalf("safe event projection=%+v", events)
	}
	var truncated bool
	for index := 0; index < 501; index++ {
		var dropped bool
		events, dropped = appendBoundedBrowserEventsWithTruncation(events, []browserEvent{{Kind: "dialog", PageID: "page-1", Detail: "alert"}})
		truncated = truncated || dropped
	}
	if len(events) != 500 || events[0].Kind != "dialog" || !truncated {
		t.Fatalf("events must retain only bounded latest entries and report truncation: len=%d first=%+v truncated=%t", len(events), events[0], truncated)
	}
	if event := safeBrowserEvent("navigation", "page-1", "https://example.test/"+strings.Repeat("x", 5000), strings.Repeat("d", 1001)); !event.Truncated {
		t.Fatalf("bounded event projection did not retain truncation fact: %+v", event)
	}
}

func TestChromiumCommandHasNoDownloadPath(t *testing.T) {
	command := chromiumCommand(context.Background(), "chromium", t.TempDir())
	for _, argument := range command.Args {
		if strings.Contains(argument, "download") {
			t.Fatalf("Chromium launch must not configure a download path: %q", argument)
		}
	}
}

func TestCDPCallerFailsClosedOnDelayedDownloadEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			var command devtoolsMessage
			if err := websocket.JSON.Receive(connection, &command); err != nil {
				return
			}
			if command.Method != "Browser.setDownloadBehavior" {
				t.Errorf("command=%q, want Browser.setDownloadBehavior", command.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: command.ID})
			// The event is deliberately sent after the command acknowledgement to
			// exercise the event/action interleaving that previously escaped.
			time.Sleep(10 * time.Millisecond)
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Browser.downloadWillBegin"})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", "http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	caller := newCDPCaller(connection, "page-1")
	if _, err := caller.call("Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}); err != nil {
		t.Fatal(err)
	}
	if !caller.observeDownloadWindow(100 * time.Millisecond) {
		t.Fatal("delayed download event was not converted into blocked outcome")
	}
}

func TestNewCDPRequestPinsEphemeralDevToolsPort(t *testing.T) {
	request, err := newCDPRequest(context.Background(), 43210, http.MethodGet, "/json/close/page-1")
	if err != nil || request.URL.Host != "127.0.0.1:43210" || request.URL.Path != "/json/close/page-1" {
		t.Fatalf("request=%v err=%v", request.URL, err)
	}
}

func TestExplorationPageCommandsUseProfileDevToolsPort(t *testing.T) {
	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths <- request.URL.EscapedPath()
		writer.WriteHeader(http.StatusOK)
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
	if err := activateExplorationPage(context.Background(), profile, "page/1"); err != nil {
		t.Fatal(err)
	}
	if err := closeExplorationPage(context.Background(), profile, "page/1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/json/activate/page%2F1", "/json/close/page%2F1"} {
		select {
		case got := <-paths:
			if got != want {
				t.Fatalf("path=%q want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing DevTools command %q", want)
		}
	}
}

func TestBrowserEventRecorderProjectsContinuousCDPEvents(t *testing.T) {
	cases := []struct {
		name     string
		message  devtoolsMessage
		wantKind string
		wantURL  string
		wantDrop bool
	}{
		{"navigation", devtoolsMessage{Method: "Page.frameNavigated", SessionID: "page-1", Params: json.RawMessage(`{"frame":{"url":"https://example.test/next"}}`)}, "navigation", "https://example.test/next", false},
		{"popup", devtoolsMessage{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetInfo":{"targetId":"page-2","type":"page","url":"https://other.test/"}}`)}, "popup", "https://other.test/", false},
		{"dialog", devtoolsMessage{Method: "Page.javascriptDialogOpening", SessionID: "page-1", Params: json.RawMessage(`{"type":"alert"}`)}, "dialog", "", false},
		{"download", devtoolsMessage{Method: "Browser.downloadWillBegin"}, "", "", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event, download := browserEventFromCDP(test.message)
			if download != test.wantDrop || event.Kind != test.wantKind || event.URL != test.wantURL {
				t.Fatalf("event=%#v download=%v", event, download)
			}
			if event.Origin != "" {
				t.Fatalf("stateless parser must not derive a browser security origin: %#v", event)
			}
		})
	}
}

func TestBrowserEventRecorderRetainsEventsAcrossActionGap(t *testing.T) {
	state := &explorationState{refs: make(map[string]explorationReference)}
	state.eventMu.Lock()
	state.events = appendBoundedBrowserEvents(state.events, []browserEvent{
		safeBrowserEvent("navigation", "page-1", "https://example.test/redirect", ""),
		safeBrowserEvent("popup", "page-2", "https://other.test/", ""),
	})
	state.eventMu.Unlock()
	state.eventMu.Lock()
	got := append([]browserEvent(nil), state.events...)
	state.events = nil
	state.eventMu.Unlock()
	if len(got) != 2 || got[0].Kind != "navigation" || got[1].Kind != "popup" {
		t.Fatalf("events were not retained across an action gap: %#v", got)
	}
}

func TestBrowserEventRecorderCollectsCDPEventsBetweenActions(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(websocket.Handler(func(connection *websocket.Conn) {
		// The recorder configures target discovery and takes its startup target
		// inventory fence before accepting events. This synthetic inventory is
		// empty, so the following target is genuinely post-fence and a popup.
		for i := 0; i < 3; i++ {
			var command devtoolsMessage
			_ = websocket.JSON.Receive(connection, &command)
			response := devtoolsMessage{ID: command.ID}
			if command.Method == "Target.getTargets" {
				response.Result = json.RawMessage(`{"targetInfos":[]}`)
			}
			_ = websocket.JSON.Send(connection, response)
		}
		_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Target.targetCreated", Params: json.RawMessage(`{"targetInfo":{"targetId":"popup","type":"page","url":"https://popup.test/"}}`)})
		_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Target.attachedToTarget", Params: json.RawMessage(`{"sessionId":"popup-session","targetInfo":{"targetId":"popup","type":"page"}}`)})
		_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Target.attachedToTarget", Params: json.RawMessage(`{"sessionId":"main","targetInfo":{"targetId":"main-target","type":"page"}}`)})
		_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.frameNavigated", SessionID: "main", Params: json.RawMessage(`{"frame":{"url":"https://example.test/redirect"}}`)})
		_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.frameNavigated", SessionID: "main", Params: json.RawMessage(`{"frame":{"url":"https://iframe.test/secret","parentId":"top"}}`)})
		// Popup/navigation projection remains deferred until the exact document's
		// private location.origin command has replied.
		for i := 0; i < 8; i++ {
			var command devtoolsMessage
			_ = websocket.JSON.Receive(connection, &command)
			response := devtoolsMessage{ID: command.ID}
			if command.Method == "Runtime.evaluate" {
				response.Result = json.RawMessage(`{"result":{"type":"string","value":"https://example.test"}}`)
			}
			_ = websocket.JSON.Send(connection, response)
		}
		received <- struct{}{}
		<-time.After(time.Second)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &explorationState{refs: make(map[string]explorationReference)}
	op := &operation{profile: profile, exploration: state}
	if err := startExplorationEventRecorder(ctx, op); err != nil {
		t.Fatal(err)
	}
	defer stopExplorationEventRecorder(state)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("event recorder did not subscribe")
	}
	deadline := time.Now().Add(time.Second)
	for {
		state.eventMu.Lock()
		count := len(state.events)
		state.eventMu.Unlock()
		if count == 2 {
			state.eventMu.Lock()
			got := append([]browserEvent(nil), state.events...)
			state.eventMu.Unlock()
			if got[1].PageID != "main-target" || got[1].URL != "https://example.test/redirect" {
				t.Fatalf("event target/top-frame projection=%#v", got[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("continuous events were not retained: %#v", state.events)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestChromiumKeyEventsDoNotInjectControlKeyNames(t *testing.T) {
	down, up := chromiumKeyEvents("Enter")
	if down["type"] != "keyDown" || up["type"] != "keyUp" || down["text"] != nil || down["windowsVirtualKeyCode"] != 13 {
		t.Fatalf("Enter events=%#v %#v", down, up)
	}
	printableDown, printableUp := chromiumKeyEvents("a")
	if printableDown["text"] != "a" || printableUp["text"] != nil || printableDown["code"] != "a" {
		t.Fatalf("printable events=%#v %#v", printableDown, printableUp)
	}
}

func TestEnsureStartURLReportsInitialDownloadBlocked(t *testing.T) {
	original := navigateOperationStartPage
	navigateOperationStartPage = func(context.Context, uint16, devtoolsPage, string) error {
		return ErrDownloadBlocked
	}
	t.Cleanup(func() { navigateOperationStartPage = original })

	const startURL = "https://target.test/download"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/json/list":
			_ = json.NewEncoder(writer).Encode([]devtoolsPage{{ID: "start", Type: "page", URL: startURL}})
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
	if err := ensureStartURL(context.Background(), profile, startURL); !errors.Is(err, ErrDownloadBlocked) {
		t.Fatalf("initial download error=%v, want DownloadBlocked", err)
	}
}

// TestOperationPIDExitedBeforeWait proves Stop can observe a real leader exit
// even if the watcher has not yet scheduled exec.Cmd.Wait. A process-group
// signal alone is insufficient when an exited browser leader has living helpers.
func TestOperationPIDExitedBeforeWait(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(command.Process.Pid, 0)
	if err != nil {
		_ = command.Wait()
		t.Skipf("pidfd unsupported: %v", err)
	}
	defer unix.Close(fd)
	defer command.Wait()
	deadline := time.Now().Add(time.Second)
	for !operationPIDExited(fd) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !operationPIDExited(fd) {
		t.Fatal("pidfd did not report an exited leader before Cmd.Wait")
	}
}

func TestBrowserEventFromCDPProjectsTopFrameRedirectEndpointsWithoutRequestData(t *testing.T) {
	state := &explorationState{knownTargets: map[string]bool{}, topFrames: map[string]string{}}
	_, _ = browserEventFromRecorder(state, devtoolsMessage{Method: "Page.frameNavigated", SessionID: "page-1", Params: []byte(`{"frame":{"id":"top","url":"https://redirect.example.test/previous"}}`)})
	event, download := browserEventFromRecorder(state, devtoolsMessage{
		Method: "Network.requestWillBeSent", SessionID: "page-1",
		Params: []byte(`{"frameId":"top","type":"Document","request":{"url":"https://redirect.example.test/next?secret=not-audit"},"redirectResponse":{"url":"https://redirect.example.test/previous?secret=not-audit"}}`),
	})
	if download || event.Kind != "" || len(state.pendingRedirects["page-1\x00top"]) != 1 {
		t.Fatalf("redirect was not held for its next document: %#v, pending=%#v, download=%v", event, state.pendingRedirects, download)
	}
	pending := state.pendingRedirects["page-1\x00top"][0]
	if pending.SourceURL != "https://redirect.example.test/previous" || pending.DestinationURL != "https://redirect.example.test/next" || strings.Contains(pending.SourceURL, "secret") || strings.Contains(pending.DestinationURL, "secret") {
		t.Fatalf("redirect projection retained incorrect endpoint data: %#v", pending)
	}
}

func TestBrowserEventRecorderRequiresInitialCommandAcknowledgements(t *testing.T) {
	server := httptest.NewServer(websocket.Handler(func(connection *websocket.Conn) {
		var command devtoolsMessage
		if err := websocket.JSON.Receive(connection, &command); err != nil {
			return
		}
		_ = websocket.JSON.Send(connection, devtoolsMessage{ID: command.ID, Error: &struct {
			Message string `json:"message"`
		}{Message: "unsupported"}})
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
	state := &explorationState{refs: make(map[string]explorationReference)}
	started := time.Now()
	err = startExplorationEventRecorder(context.Background(), &operation{profile: profile, exploration: state})
	if err == nil {
		t.Fatal("recorder accepted an unacknowledged/rejected setup command")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("recorder setup failure exceeded fixed bound: %v", elapsed)
	}
}

func TestObservationProjectionDoesNotMutateBusinessDOM(t *testing.T) {
	if strings.Contains(fixedObservationScript, "setAttribute") || strings.Contains(fixedObservationScript, "data-quoin-index") {
		t.Fatalf("observation script mutates business DOM: %s", fixedObservationScript)
	}
	if !strings.Contains(fixedCandidateObjectsScript, "querySelectorAll") {
		t.Fatal("observation must retain a fixed private Chromium node ledger query")
	}
}

func TestExplorationElementReferenceIsBoundToItsIssuingPage(t *testing.T) {
	manager := &Manager{}
	op := &operation{exploration: &explorationState{refs: map[string]explorationReference{
		"e:1:1": {backendNodeID: 101, version: 1, pageID: "page-a", documentID: "top\x00loader"},
	}}}
	action := exploration.Action{Name: "read", Fields: map[string]json.RawMessage{"locator": json.RawMessage(`{"kind":"elementRef","ref":"e:1:1","version":1}`)}}
	actionErr := manager.executeDOMAction(context.Background(), &cdpCaller{pageID: "page-b"}, op, action)
	if actionErr == nil || actionErr.code != "ElementReferenceStale" {
		t.Fatalf("cross-page element ref error=%#v, want ElementReferenceStale", actionErr)
	}
}

func TestCDPCallerNavigationWaitsForTopLevelDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			var message devtoolsMessage
			if err := websocket.JSON.Receive(connection, &message); err != nil {
				return
			}
			if message.Method != "Page.navigate" {
				t.Errorf("method=%s want Page.navigate", message.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: message.ID, Result: json.RawMessage(`{"frameId":"top","loaderId":"load"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"frameId":"top","loaderId":"load","requestId":"document","type":"Document"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Network.loadingFinished", Params: json.RawMessage(`{"requestId":"document"}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.lifecycleEvent", Params: json.RawMessage(`{"frameId":"top","loaderId":"load","name":"DOMContentLoaded"}`)})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := (&cdpCaller{connection: connection}).callAndAwaitNavigation("Page.navigate", map[string]any{"url": "https://target.test"}); err != nil {
		t.Fatalf("navigation did not await document: %v", err)
	}
}

func TestStartDoesNotHoldLifecycleMutexDuringReconcile(t *testing.T) {
	stubDownloadDeny(t)
	root := t.TempDir()
	binary := filepath.Join(root, "sleeping-browser")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDirectory: root, Capacity: 1, ChromiumBinary: binary, XvfbBinary: binary, X0VNCBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.reconcileStartURL = func(context.Context, string, string) error {
		close(entered)
		<-release
		return nil
	}
	started := make(chan error, 1)
	go func() { _, err := manager.Start(context.Background(), 44, "https://example.test"); started <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("start did not reach bounded reconcile seam")
	}
	// VNCAddress takes Manager.mu. It must not wait for the private CDP reconcile
	// operation, otherwise Stop/crash could be blocked behind a stalled browser.
	returned := make(chan struct{})
	go func() { _, _ = manager.VNCAddress(44); close(returned) }()
	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("manager lifecycle mutex remained held during reconcile")
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExplorationActionTimeoutReturnsRecoverableObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/json/list" {
			_ = json.NewEncoder(writer).Encode([]devtoolsPage{{ID: "page-1", Type: "page", URL: "https://example.test/", WebSocketDebuggerURL: "ws" + strings.TrimPrefix(serverURLForTest(request), "http") + "/devtools/page-1"}})
			return
		}
		if request.URL.Path != "/devtools/page-1" {
			http.NotFound(writer, request)
			return
		}
		websocket.Handler(func(connection *websocket.Conn) {
			for {
				var command devtoolsMessage
				if websocket.JSON.Receive(connection, &command) != nil {
					return
				}
				var result json.RawMessage
				switch command.Method {
				case "Runtime.evaluate":
					if strings.Contains(string(command.Params), fixedCandidateObjectsScript) {
						result = json.RawMessage(`{"result":{"objectId":"candidates"}}`)
					} else {
						result = json.RawMessage(`{"result":{"type":"object","value":{"url":"https://example.test/","title":"Example","visibleText":"ready","accessibilityText":"","elements":[],"originalSizeBytes":5,"truncated":false}}}`)
					}
				case "Page.getFrameTree":
					result = json.RawMessage(`{"frameTree":{"frame":{"id":"frame-1","loaderId":"loader-1"}}}`)
				case "Accessibility.getFullAXTree":
					result = json.RawMessage(`{"nodes":[{"backendDOMNodeId":9,"ignored":false,"role":{"value":"button"},"name":{"value":"Ready"}}]}`)
				case "Runtime.getProperties":
					result = json.RawMessage(`{"result":[{"name":"0","value":{"objectId":"candidate-1"}}]}`)
				case "Runtime.callFunctionOn":
					result = json.RawMessage(`{"result":{"value":{"role":"textbox","name":"DOM fallback","visible":true,"enabled":true}}}`)
				case "DOM.describeNode":
					result = json.RawMessage(`{"node":{"backendNodeId":9}}`)
				default:
					return
				}
				if websocket.JSON.Send(connection, devtoolsMessage{ID: command.ID, Result: result}) != nil {
					return
				}
			}
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(endpoint.Port()+"\n/devtools/browser/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	op := &operation{profile: profile, exploration: &explorationState{refs: make(map[string]explorationReference), eventReady: true, eventPending: map[int]string{}}}
	result := (&Manager{}).explorationActionTimedOut(op, exploration.Action{Name: "read", SessionID: "12"})
	if result.SessionTerminal || result.ErrorCode != "ActionTimeout" || result.Payload == nil {
		t.Fatalf("timeout result=%#v, want recoverable ActionTimeout observation", result)
	}
	if outcome, _ := result.Payload["outcome"].(string); outcome != "recoverable_error" {
		t.Fatalf("timeout outcome=%q", outcome)
	}
	observation, _ := result.Payload["observation"].(map[string]any)
	if accessibility, _ := observation["accessibilityText"].(string); accessibility != "button Ready" {
		t.Fatalf("accessibility projection=%q, want Chromium AX tree", accessibility)
	}
	elements, _ := observation["elements"].([]map[string]any)
	if len(elements) != 1 || elements[0]["role"] != "button" || elements[0]["name"] != "Ready" {
		t.Fatalf("AX/backend ledger did not replace DOM semantics: %#v", elements)
	}
}

// serverURLForTest reconstructs the httptest server authority from the request;
// operationDevToolsURL replaces its host with the port from DevToolsActivePort.
func serverURLForTest(request *http.Request) string { return "http://" + request.Host }

func TestActiveOperationIDsIncludesStartingAndLiveOperations(t *testing.T) {
	manager := &Manager{operations: map[int64]*operation{9: {}}, starting: map[int64]*operationStart{3: {}}}
	if got := manager.ActiveOperationIDs(); !slices.Equal(got, []int64{3, 9}) {
		t.Fatalf("active IDs=%v", got)
	}
}

func TestFixedInteractionsUseChromiumInputInsteadOfDOMMutation(t *testing.T) {
	for _, forbidden := range []string{".click(", "dispatchEvent("} {
		if strings.Contains(fixedFindElementsScript, forbidden) || strings.Contains(fixedElementInfoFunction, forbidden) {
			t.Fatalf("fixed interaction helper contains DOM mutation %q", forbidden)
		}
	}
	body := fixedFindElementsScript + "\n" + fixedElementInfoFunction
	if !strings.Contains(body, "read-only") && strings.Contains(body, "click") {
		t.Fatal("element resolver must remain read-only")
	}
}

func TestBackendNodeLedgerDoesNotUseCandidatePositions(t *testing.T) {
	body := fixedCandidateObjectsScript + fixedObservationScript
	for _, forbidden := range []string{"candidateIndex", "observationIndex", "data-quoin-index"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("backend-node ledger retained mutable position identity %q", forbidden)
		}
	}
}

func TestActionCheckpointRetainsPopupInterleavedWithAction(t *testing.T) {
	state := &explorationState{refs: make(map[string]explorationReference)}
	state.eventMu.Lock()
	state.events = append(state.events, safeBrowserEvent("popup", "page-after-action", "https://popup.test/", ""))
	// Draining happens under the recorder mutex, the same ownership boundary used
	// after the ordered recorder barrier in explorationObservation.
	drained := append([]browserEvent(nil), state.events...)
	state.events = nil
	state.eventMu.Unlock()
	if len(drained) != 1 || drained[0].Kind != "popup" || drained[0].PageID != "page-after-action" {
		t.Fatalf("interleaved popup was lost from checkpoint: %#v", drained)
	}
}

func TestBackendLedgerReferenceCarriesNodeAndDocumentIdentity(t *testing.T) {
	ref := explorationReference{backendNodeID: 9001, version: 7, pageID: "page", documentID: "top\x00loader"}
	if ref.backendNodeID == 0 || ref.documentID == "" || ref.pageID == "" || ref.version == 0 {
		t.Fatalf("incomplete stable element ledger entry: %#v", ref)
	}
}

func TestSelectRejectsMissingOptionBeforeBrowserInput(t *testing.T) {
	failure := selectExplorationValues(nil, "unused", explorationElementInfo{Tag: "SELECT", Options: []string{"present"}}, []string{"missing"})
	if failure == nil || failure.code != "ElementNotInteractable" {
		t.Fatalf("missing select option result=%#v, want ElementNotInteractable", failure)
	}
}

func TestCDPCallerNavigationAcceptsSameDocumentResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			var message devtoolsMessage
			if websocket.JSON.Receive(connection, &message) != nil {
				return
			}
			// Chromium omits loaderId for fragment/same-document navigation.
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: message.ID, Result: json.RawMessage(`{"frameId":"top"}`)})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := (&cdpCaller{connection: connection}).callAndAwaitNavigation("Page.navigate", map[string]any{"url": "https://target.test/#fragment"}); err != nil {
		t.Fatalf("same-document navigation failed: %v", err)
	}
}

func TestFixedRoleLocatorUsesExactAccessibleNameByDefault(t *testing.T) {
	if !strings.Contains(fixedFindElementsScript, "l.exact===false?n.includes(l.name):n===l.name") {
		t.Fatalf("role locator does not preserve exact-name default: %s", fixedFindElementsScript)
	}
}

func TestCDPCallerHistoryTraversalAcceptsBFCacheTopFrameNavigation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			var frameTree devtoolsMessage
			if websocket.JSON.Receive(connection, &frameTree) != nil {
				return
			}
			if frameTree.Method != "Page.getFrameTree" {
				t.Errorf("first method=%q, want Page.getFrameTree", frameTree.Method)
				return
			}
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: frameTree.ID, Result: json.RawMessage(`{"frameTree":{"frame":{"id":"top"}}}`)})
			var navigate devtoolsMessage
			if websocket.JSON.Receive(connection, &navigate) != nil {
				return
			}
			if navigate.Method != "Page.navigateToHistoryEntry" {
				t.Errorf("method=%q, want Page.navigateToHistoryEntry", navigate.Method)
				return
			}
			// BFCache restore may return an empty command result and no Network
			// Document lifecycle. A verified top-frame navigation is its boundary.
			_ = websocket.JSON.Send(connection, devtoolsMessage{ID: navigate.ID, Result: json.RawMessage(`{}`)})
			_ = websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.frameNavigated", Params: json.RawMessage(`{"frame":{"id":"top","parentId":""}}`)})
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := (&cdpCaller{connection: connection}).callAndAwaitNavigation("Page.navigateToHistoryEntry", map[string]any{"entryId": 1}); err != nil {
		t.Fatalf("BFCache history traversal failed: %v", err)
	}
}

func TestCDPCallerRejectsPreCommandNavigationEventOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		websocket.Handler(func(connection *websocket.Conn) {
			var message devtoolsMessage
			if websocket.JSON.Receive(connection, &message) != nil {
				return
			}
			for range 65 {
				if websocket.JSON.Send(connection, devtoolsMessage{Method: "Page.frameNavigated", Params: json.RawMessage(`{"frame":{"id":"top","parentId":""}}`)}) != nil {
					return
				}
			}
		}).ServeHTTP(writer, request)
	}))
	defer server.Close()
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(server.URL, "http"), "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	err = (&cdpCaller{connection: connection}).callAndAwaitNavigation("Page.navigate", map[string]any{"url": "https://target.test"})
	if err == nil || !strings.Contains(err.Error(), "prefix exceeded") {
		t.Fatalf("overflow error=%v, want bounded prefix failure", err)
	}
}
