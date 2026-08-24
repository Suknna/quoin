// Package browser runs one headed Chromium/Xvfb/x0vncserver process set per
// Browser Operation. It deliberately has no generic DevTools, JavaScript, or
// network API: callers receive only lifecycle and the private VNC endpoint.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
)

type Config struct {
	StateDirectory string
	Capacity       uint32
	ChromiumBinary string
	XvfbBinary     string
	X0VNCBinary    string
}

type Manager struct {
	config     Config
	mu         sync.Mutex
	operations map[int64]*operation
	// reconcileStartURL is the fixed internal launch condition. It is not a
	// caller-visible DevTools or browser-control capability.
	reconcileStartURL func(context.Context, string, string) error
	// OnCrash is invoked after every child process of an unexpectedly ended
	// operation is fenced and its staging is removed. The Runtime Channel turns
	// it into a durable browser_crashed completion.
	OnCrash func(operationID int64)
}

type operation struct {
	id                  int64
	display             string
	profile             string
	vncAddress          string
	xvfb, chromium, vnc *exec.Cmd
	exited              chan struct{}
	stopping            bool
	childExited         bool
	crashed             bool
}

var ErrBusy = errors.New("browser capacity exhausted")

func NewManager(config Config) (*Manager, error) {
	if config.StateDirectory == "" || config.Capacity == 0 {
		return nil, errors.New("browser state directory and positive capacity are required")
	}
	if config.ChromiumBinary == "" {
		config.ChromiumBinary = "chromium"
	}
	if config.XvfbBinary == "" {
		config.XvfbBinary = "Xvfb"
	}
	if config.X0VNCBinary == "" {
		config.X0VNCBinary = "x0vncserver"
	}
	for _, binary := range []string{config.ChromiumBinary, config.XvfbBinary, config.X0VNCBinary} {
		if _, err := exec.LookPath(binary); err != nil {
			return nil, fmt.Errorf("required browser binary %q: %w", binary, err)
		}
	}
	operationsDirectory := filepath.Join(config.StateDirectory, "operations")
	if err := os.MkdirAll(operationsDirectory, 0o700); err != nil {
		return nil, err
	}
	// A new Lintel Manager has no in-memory ownership of a prior boot's
	// browser staging. Remove only that disposable staging root before the
	// control stream opens; published profile generations live elsewhere.
	entries, err := os.ReadDir(operationsDirectory)
	if err != nil {
		return nil, fmt.Errorf("read browser operation staging: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(operationsDirectory, entry.Name())); err != nil {
			return nil, fmt.Errorf("remove stale browser operation staging: %w", err)
		}
	}
	return &Manager{config: config, operations: make(map[int64]*operation), reconcileStartURL: ensureStartURL}, nil
}

// Start opens an isolated headed Chromium profile and an unauthenticated VNC
// listener on loopback. noVNC is intentionally a transport concern: only a
// Quoin-authorized gRPC tunnel may dial VNCAddress.
func (manager *Manager) Start(ctx context.Context, operationID int64, startURL string) (string, error) {
	return manager.start(ctx, operationID, startURL, "")
}

// StartWithProfile opens an isolated writable copy of a published profile.
// The caller supplies the generation path resolved from frozen generation
// sequence, never the Quoin database locator.
func (manager *Manager) StartWithProfile(ctx context.Context, operationID int64, startURL, sourceProfile string) (string, error) {
	if sourceProfile == "" {
		return "", errors.New("published profile path is required")
	}
	return manager.start(ctx, operationID, startURL, sourceProfile)
}

func (manager *Manager) start(ctx context.Context, operationID int64, startURL, sourceProfile string) (string, error) {
	if operationID <= 0 || startURL == "" {
		return "", errors.New("operation ID and start URL are required")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if existing := manager.operations[operationID]; existing != nil {
		return existing.vncAddress, nil
	}
	if uint32(len(manager.operations)) >= manager.config.Capacity {
		return "", ErrBusy
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	profileDirectory := filepath.Join(manager.config.StateDirectory, "operations", strconv.FormatInt(operationID, 10), "profile")
	if sourceProfile == "" {
		if err := os.MkdirAll(profileDirectory, 0o700); err != nil {
			return "", err
		}
	} else if err := copyDirectory(sourceProfile, profileDirectory); err != nil {
		return "", fmt.Errorf("clone published browser profile: %w", err)
	} else if err := os.Remove(filepath.Join(profileDirectory, "DevToolsActivePort")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove stale Chromium DevTools endpoint: %w", err)
	}
	display := ":" + strconv.FormatInt(1000+operationID%50000, 10)
	op := &operation{id: operationID, display: display, profile: profileDirectory, vncAddress: address}
	// The display has no TCP listener and lives in Lintel's private container
	// namespace. -ac permits the separately spawned loopback-only VNC server to
	// attach without a host Xauthority file.
	op.xvfb = managedCommand(ctx, manager.config.XvfbBinary, display, "-screen", "0", "1280x900x24", "-nolisten", "tcp", "-ac")
	if err := op.xvfb.Start(); err != nil {
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("start Xvfb: %w", err)
	}
	// Remote debugging is an internal typed probe implementation only. The
	// endpoint is Chromium loopback-only and no caller receives its port,
	// websocket URL, or arbitrary CDP/JavaScript capability.
	op.chromium = chromiumCommand(ctx, manager.config.ChromiumBinary, profileDirectory)
	op.chromium.Env = append(op.chromium.Env, "DISPLAY="+display)
	if err := op.chromium.Start(); err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("start Chromium: %w", err)
	}
	// A live Chromium PID is not evidence that its requested first page exists.
	// Reconcile the immutable launch URL before creating an attachable RFB
	// surface, so Running can never represent an empty/default page.
	if err := manager.reconcileStartURL(ctx, profileDirectory, startURL); err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("reconcile Chromium start URL: %w", err)
	}
	// RFB is intentionally unauthenticated only on this loopback socket: the
	// authenticated Quoin BrowserTunnel is its sole transport boundary. TigerVNC
	// otherwise looks for an interactive password helper and exits.
	op.vnc = managedCommand(ctx, manager.config.X0VNCBinary, "-fg", "-display", display, "-localhost", "-SecurityTypes", "None", "-rfbport", address[stringsLastColon(address)+1:])
	op.vnc.Env = append(os.Environ(),
		"HOME="+profileDirectory,
		"XDG_CONFIG_HOME="+profileDirectory,
		"XDG_CACHE_HOME="+filepath.Join(profileDirectory, "cache"),
	)
	if err := op.vnc.Start(); err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("start x0vncserver: %w", err)
	}
	op.exited = make(chan struct{})
	manager.operations[operationID] = op
	manager.watch(op)
	return address, nil
}

// managedCommand binds every process to the Lintel supervisor's lifetime. The
// Pdeathsig guard covers abrupt supervisor exit; Close provides the blocking,
// observable graceful shutdown path.
func managedCommand(ctx context.Context, name string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	return command
}

// chromiumCommand deliberately runs with Chromium's internal sandbox disabled.
// Lintel is itself a constrained container (no extra capabilities, no-new-
// privileges, internal-only networking); the Docker default seccomp profile
// blocks the unprivileged user-namespace setup Chromium's sandbox requires.
// Supplying the per-operation profile as HOME/XDG state also gives Crashpad a
// writable, disposable database rather than inheriting an unwritable container
// home directory.
func chromiumCommand(ctx context.Context, binary, profile string) *exec.Cmd {
	command := managedCommand(ctx, binary,
		"--no-sandbox",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--disable-background-networking",
		"--disable-sync",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=0",
		"--remote-allow-origins=http://127.0.0.1",
		// The Xvfb display is fixed and has no window manager. Fix the app
		// surface to that display so the VNC framebuffer has one predictable,
		// operator-visible input target instead of a background or offset window.
		"--window-position=0,0",
		"--window-size=1280,900",
		// App mode launches one browser surface. The probe below refuses to
		// classify any multi-page session, so a background authenticated tab
		// can never make a foreground logged-out page publishable.
		// Start neutral. ensureStartURL performs the only navigation to the
		// frozen URL after DevTools is ready, avoiding a process-start race.
		"--app=about:blank",
	)
	command.Env = append(os.Environ(),
		"HOME="+profile,
		"XDG_CONFIG_HOME="+profile,
		"XDG_CACHE_HOME="+filepath.Join(profile, "cache"),
	)
	return command
}

func copyDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported profile entry %q", relative)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	})
}

func stringsLastColon(value string) int {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == ':' {
			return i
		}
	}
	return -1
}

type devtoolsPage struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type devtoolsMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var navigateOperationStartPage = navigateStartPage

const startPageNavigationAttemptTimeout = 3 * time.Second

// ensureStartURL makes the frozen initial URL an observable Browser start
// condition. It only uses Chromium's operation-private loopback endpoint and
// accepts no caller-provided DevTools command or target identifier.
func ensureStartURL(parent context.Context, profile, startURL string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
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
	var startPageID string
	for _, page := range pages {
		if page.Type == "page" && page.URL == startURL {
			startPageID = page.ID
			break
		}
	}
	if startPageID == "" {
		// /json/new returns the target identity created by this fixed navigation.
		// We keep that identity through redirects instead of treating a normal
		// canonicalization or SSO redirect as a failed Browser start.
		// Do not let /json/new race the controlled navigation with the frozen
		// URL. Chromium creates a neutral operation-private target first; the
		// single Page.navigate below is the only start-URL navigation.
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://127.0.0.1/json/new?"+url.QueryEscape("about:blank"), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("open Chromium start page: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("open Chromium start page: HTTP %d", response.StatusCode)
		}
		var target devtoolsPage
		err = json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<20)).Decode(&target)
		response.Body.Close()
		if err != nil || target.ID == "" || target.Type != "page" {
			return fmt.Errorf("read Chromium start page target: %w", err)
		}
		startPageID = target.ID
		for {
			pages, err = devtoolsPages(ctx, client)
			if err == nil && containsPageID(pages, startPageID) {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for Chromium start page: %w", ctx.Err())
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	for _, page := range pages {
		if page.Type != "page" || page.ID == startPageID {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/json/close/"+url.PathEscape(page.ID), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("close non-start Chromium page: %w", err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("close non-start Chromium page: HTTP %d", response.StatusCode)
		}
	}
	pages, err = devtoolsPages(ctx, client)
	if err != nil {
		return err
	}
	startPage, found := pageByID(pages, startPageID)
	if !found {
		return errors.New("Chromium start page disappeared before navigation")
	}
	// Chromium can expose the newly-created CDP target before its first
	// navigation is live. Retry only the fixed navigation against that same
	// operation-private target; never report Running until its request is seen.
	var navigationErr error
	for attempt := 0; attempt < 3; attempt++ {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, startPageNavigationAttemptTimeout)
		navigationErr = navigateOperationStartPage(attemptCtx, port, startPage, startURL)
		cancelAttempt()
		if navigationErr == nil {
			break
		}
		if !errors.Is(navigationErr, context.DeadlineExceeded) || ctx.Err() != nil {
			return navigationErr
		}
	}
	if navigationErr != nil {
		return navigationErr
	}
	pages, err = devtoolsPages(ctx, client)
	if err != nil {
		return err
	}
	if !containsOnlyPageID(pages, startPageID) {
		return errors.New("Chromium browser operation does not have exactly one start page")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/json/activate/"+url.PathEscape(startPageID), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("activate Chromium start page: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("activate Chromium start page: HTTP %d", response.StatusCode)
	}
	return nil
}

// navigateStartPage performs the fixed, operation-private navigation and
// waits until Chromium reports both the frozen start request and that page's
// load completion. A target's presence in /json/list (or even its first HTTP
// request) is insufficient: Chromium can create it before the page becomes a
// usable manual-login surface.
func navigateStartPage(ctx context.Context, port uint16, page devtoolsPage, startURL string) error {
	endpoint, err := operationDevToolsURL(port, page.WebSocketDebuggerURL)
	if err != nil {
		return err
	}
	connection, err := websocket.Dial(endpoint, "", "http://127.0.0.1")
	if err != nil {
		return fmt.Errorf("dial Chromium start page: %w", err)
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
	for _, command := range []devtoolsMessage{{ID: 1, Method: "Page.enable"}, {ID: 2, Method: "Network.enable"}, {ID: 3, Method: "Page.setLifecycleEventsEnabled", Params: json.RawMessage(`{"enabled":true}`)}, {ID: 4, Method: "Page.navigate", Params: json.RawMessage(fmt.Sprintf(`{"url":%q}`, startURL))}} {
		if err := websocket.JSON.Send(connection, command); err != nil {
			return fmt.Errorf("send Chromium %s: %w", command.Method, err)
		}
	}
	var pageEnabled, networkEnabled, lifecycleEnabled, navigated bool
	var navigationFrame, navigationLoader, requestFrame, requestID string
	var documentFrame, documentLoader, documentRequestID, documentFailure string
	var documentFinished, documentDOMContentLoaded bool
	for {
		if documentFailure != "" && requestID == documentRequestID {
			return fmt.Errorf("navigate Chromium start page: %s", documentFailure)
		}
		loaded := navigated && navigationFrame != "" && navigationLoader != "" && requestFrame == navigationFrame && requestID != "" && documentFinished && documentDOMContentLoaded
		if loaded && pageEnabled && networkEnabled && lifecycleEnabled {
			return nil
		}
		var message devtoolsMessage
		if err := websocket.JSON.Receive(connection, &message); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("wait for Chromium start page: %w", ctx.Err())
			}
			return fmt.Errorf("read Chromium start page: %w", err)
		}
		if message.Error != nil {
			return fmt.Errorf("Chromium %d: %s", message.ID, message.Error.Message)
		}
		switch message.ID {
		case 1:
			pageEnabled = true
		case 2:
			networkEnabled = true
		case 3:
			lifecycleEnabled = true
		case 4:
			var result struct {
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				ErrorText string `json:"errorText"`
			}
			if json.Unmarshal(message.Result, &result) != nil || result.ErrorText != "" || result.FrameID == "" {
				if result.ErrorText == "" {
					result.ErrorText = "missing navigation target"
				}
				return fmt.Errorf("navigate Chromium start page: %s", result.ErrorText)
			}
			navigated, navigationFrame, navigationLoader = true, result.FrameID, result.LoaderID
			if documentFrame == navigationFrame && documentLoader == navigationLoader {
				requestFrame, requestID = documentFrame, documentRequestID
			}
		}
		if message.Method == "Network.requestWillBeSent" {
			var request struct {
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				RequestID string `json:"requestId"`
				Type      string `json:"type"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			if json.Unmarshal(message.Params, &request) == nil && requestID == "" && request.Type == "Document" && sameStartRequestURL(request.Request.URL, startURL) {
				documentFrame, documentLoader, documentRequestID, documentFinished, documentDOMContentLoaded, documentFailure = request.FrameID, request.LoaderID, request.RequestID, false, false, ""
				if request.FrameID == navigationFrame && request.LoaderID == navigationLoader {
					requestFrame, requestID = request.FrameID, request.RequestID
				}
			}
		}
		if message.Method == "Page.lifecycleEvent" {
			var lifecycle struct {
				FrameID  string `json:"frameId"`
				LoaderID string `json:"loaderId"`
				Name     string `json:"name"`
			}
			if json.Unmarshal(message.Params, &lifecycle) == nil && lifecycle.Name == "DOMContentLoaded" && lifecycle.FrameID == documentFrame && lifecycle.LoaderID == documentLoader {
				documentDOMContentLoaded = true
			}
		}
		if message.Method == "Network.loadingFailed" {
			var failed struct {
				RequestID string `json:"requestId"`
				ErrorText string `json:"errorText"`
			}
			if json.Unmarshal(message.Params, &failed) == nil && failed.RequestID != "" && failed.RequestID == documentRequestID {
				documentFailure = failed.ErrorText
			}
		}
		if message.Method == "Network.loadingFinished" {
			var finished struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(message.Params, &finished) == nil && finished.RequestID == documentRequestID {
				documentFinished = true
			}
		}
	}

}

// operationDevToolsURL only permits the operation-private loopback CDP endpoint.
// Chromium sometimes omits its ephemeral debugging port from /json/list, so the
// port is derived from DevToolsActivePort rather than trusted from the target.
// sameStartRequestURL accepts only URL spelling normalizations Chromium applies
// before it sends the first request (not redirects). In particular a root URL
// may gain a trailing slash and fragments are absent from HTTP requests; query
// semantics remain exact.
func sameStartRequestURL(actual, frozen string) bool {
	actualURL, actualErr := url.Parse(actual)
	frozenURL, frozenErr := url.Parse(frozen)
	if actualErr != nil || frozenErr != nil || actualURL.Scheme != frozenURL.Scheme || actualURL.Host != frozenURL.Host || actualURL.RawQuery != frozenURL.RawQuery {
		return false
	}
	actualPath, frozenPath := actualURL.EscapedPath(), frozenURL.EscapedPath()
	if actualPath == "" {
		actualPath = "/"
	}
	if frozenPath == "" {
		frozenPath = "/"
	}
	return actualPath == frozenPath
}

func operationDevToolsURL(port uint16, raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "ws" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost") {
		return "", errors.New("Chromium start page has an invalid DevTools endpoint")
	}
	endpoint.User = nil
	endpoint.Host = net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10))
	return endpoint.String(), nil
}

func pageByID(pages []devtoolsPage, targetID string) (devtoolsPage, bool) {
	for _, page := range pages {
		if page.Type == "page" && page.ID == targetID {
			return page, true
		}
	}
	return devtoolsPage{}, false
}

func containsPageID(pages []devtoolsPage, targetID string) bool {
	for _, page := range pages {
		if page.Type == "page" && page.ID == targetID {
			return true
		}
	}
	return false
}

func containsOnlyPageID(pages []devtoolsPage, targetID string) bool {
	pageCount := 0
	for _, page := range pages {
		if page.Type != "page" {
			continue
		}
		pageCount++
		if page.ID != targetID {
			return false
		}
	}
	return pageCount == 1
}

func containsOnlyStartPage(pages []devtoolsPage, startURL string) bool {
	pageCount := 0
	for _, page := range pages {
		if page.Type != "page" {
			continue
		}
		pageCount++
		if page.URL != startURL {
			return false
		}
	}
	return pageCount == 1
}

func waitDevToolsPort(ctx context.Context, profile string) (uint16, error) {
	portPath := filepath.Join(profile, "DevToolsActivePort")
	for {
		body, err := os.ReadFile(portPath)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) == 0 || lines[0] == "" {
				return 0, errors.New("Chromium DevTools endpoint is malformed")
			}
			port, parseErr := strconv.ParseUint(lines[0], 10, 16)
			if parseErr != nil || port == 0 {
				return 0, errors.New("Chromium DevTools port is malformed")
			}
			return uint16(port), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("read Chromium probe endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("Chromium probe endpoint did not become ready: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func devtoolsHTTPClient(port uint16) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)))
	}}, Timeout: 3 * time.Second}
}

func devtoolsPages(ctx context.Context, client *http.Client) ([]devtoolsPage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/json/list", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read Chromium page list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read Chromium page list: HTTP %d", response.StatusCode)
	}
	var pages []devtoolsPage
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<20)).Decode(&pages); err != nil {
		return nil, fmt.Errorf("decode Chromium page list: %w", err)
	}
	return pages, nil
}

// ProbeURLPrefix executes the one fixed authentication.url-prefix.v1 probe.
// It reads Chromium's private loopback target list solely to compare the
// foreground page URL with the configured prefix; it exposes no general CDP
// or browser-automation API.
func (manager *Manager) ProbeURLPrefix(ctx context.Context, operationID int64, prefix string) (bool, error) {
	if prefix == "" {
		return false, errors.New("authenticated URL prefix is required")
	}
	manager.mu.Lock()
	op := manager.operations[operationID]
	manager.mu.Unlock()
	if op == nil {
		return false, errors.New("browser operation is not running")
	}
	portPath := filepath.Join(op.profile, "DevToolsActivePort")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		body, err := os.ReadFile(portPath)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) == 0 || lines[0] == "" {
				return false, errors.New("Chromium DevTools endpoint is malformed")
			}
			port, parseErr := strconv.ParseUint(lines[0], 10, 16)
			if parseErr != nil || port == 0 {
				return false, errors.New("Chromium DevTools port is malformed")
			}
			return probeLoopbackPages(ctx, uint16(port), prefix)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("read Chromium probe endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, errors.New("Chromium probe endpoint did not become ready")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func probeLoopbackPages(ctx context.Context, port uint16, prefix string) (bool, error) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)))
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/json/list", nil)
	if err != nil {
		return false, err
	}
	response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return false, fmt.Errorf("read Chromium page list: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("read Chromium page list: HTTP %d", response.StatusCode)
	}
	var pages []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<20)).Decode(&pages); err != nil {
		return false, fmt.Errorf("decode Chromium page list: %w", err)
	}
	var activeURL string
	for _, page := range pages {
		if page.Type != "page" {
			continue
		}
		if activeURL != "" {
			return false, errors.New("Chromium browser operation has more than one page")
		}
		activeURL = page.URL
	}
	if activeURL == "" {
		return false, errors.New("Chromium browser operation has no page")
	}
	return strings.HasPrefix(activeURL, prefix), nil
}

func (manager *Manager) VNCAddress(operationID int64) (string, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	op := manager.operations[operationID]
	if op == nil {
		return "", false
	}
	return op.vncAddress, true
}

// Close blocks until every process owned by this Lintel boot is stopped and
// its disposable operation staging is removed. It is safe to call repeatedly.
func (manager *Manager) Close() error {
	manager.mu.Lock()
	ids := make([]int64, 0, len(manager.operations))
	for id := range manager.operations {
		ids = append(ids, id)
	}
	manager.mu.Unlock()
	var first error
	for _, id := range ids {
		if err := manager.Stop(id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (manager *Manager) Stop(operationID int64) error {
	manager.mu.Lock()
	op := manager.operations[operationID]
	if op != nil {
		op.stopping = true
	}
	manager.mu.Unlock()
	operationDirectory := filepath.Join(manager.config.StateDirectory, "operations", strconv.FormatInt(operationID, 10))
	if op != nil {
		manager.stopAndWait(op)
		operationDirectory = filepath.Dir(op.profile)
	}
	if err := os.RemoveAll(operationDirectory); err != nil {
		return err
	}
	if _, err := os.Stat(operationDirectory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("operation staging remains after cleanup")
		}
		return err
	}
	return nil
}

// DetachProfile stops an operation without deleting its browser data so the
// profile store can atomically adopt it as a published generation.
func (manager *Manager) DetachProfile(operationID int64) (string, error) {
	manager.mu.Lock()
	op := manager.operations[operationID]
	if op == nil {
		manager.mu.Unlock()
		return "", errors.New("browser operation is not running")
	}
	if op.childExited || op.crashed {
		manager.mu.Unlock()
		return "", errors.New("browser operation crashed while publishing profile")
	}
	op.stopping = true
	manager.mu.Unlock()
	manager.stopAndWait(op)
	manager.mu.Lock()
	crashed := op.crashed
	manager.mu.Unlock()
	if crashed {
		return "", errors.New("browser operation crashed while publishing profile")
	}
	// The endpoint is process-local control state, not profile data. A published
	// generation must never retain a port or browser identifier that could be
	// used to recover arbitrary DevTools access.
	if err := os.Remove(filepath.Join(op.profile, "DevToolsActivePort")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove Chromium DevTools endpoint before publish: %w", err)
	}
	return op.profile, nil
}

func (manager *Manager) watch(op *operation) {
	finished := make(chan struct{}, 3)
	for _, command := range []*exec.Cmd{op.vnc, op.chromium, op.xvfb} {
		go func(command *exec.Cmd) {
			_ = command.Wait()
			manager.mu.Lock()
			if manager.operations[op.id] == op {
				op.childExited = true
				if !op.stopping {
					op.crashed = true
				}
			}
			manager.mu.Unlock()
			finished <- struct{}{}
		}(command)
	}
	go func() {
		<-finished
		manager.mu.Lock()
		crashed := manager.operations[op.id] == op && op.crashed
		manager.mu.Unlock()
		manager.signal(op, syscall.SIGTERM)
		for remaining := 1; remaining < 3; remaining++ {
			<-finished
		}
		manager.mu.Lock()
		if manager.operations[op.id] == op {
			delete(manager.operations, op.id)
		}
		manager.mu.Unlock()
		if crashed {
			_ = os.RemoveAll(filepath.Dir(op.profile))
		}
		// A publishing DetachProfile holds the Channel operation mutex. Wake it
		// after marking the crash, before enqueueing completion, so it refuses the
		// profile rather than deadlocking with the completion handler.
		close(op.exited)
		if crashed && manager.OnCrash != nil {
			manager.OnCrash(op.id)
		}
	}()
}

func (manager *Manager) signal(op *operation, signal syscall.Signal) {
	for _, command := range []*exec.Cmd{op.vnc, op.chromium, op.xvfb} {
		if command != nil && command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, signal)
		}
	}
}

func (manager *Manager) stopAndWait(op *operation) {
	manager.signal(op, syscall.SIGTERM)
	if op.exited != nil {
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-op.exited:
			timer.Stop()
		case <-timer.C:
			manager.signal(op, syscall.SIGKILL)
			<-op.exited
		}
		return
	}
	manager.kill(op)
}

func (manager *Manager) kill(op *operation) {
	// Each command owns a process group: Chromium can fork helper processes
	// which must not survive a terminal operation.
	manager.signal(op, syscall.SIGTERM)
	for _, command := range []*exec.Cmd{op.vnc, op.chromium, op.xvfb} {
		if command == nil || command.Process == nil {
			continue
		}
		done := make(chan struct{})
		go func(c *exec.Cmd) { _ = c.Wait(); close(done) }(command)
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
}
