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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
	"golang.org/x/sys/unix"
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
	// starting reserves capacity while fixed startup CDP work runs without
	// Manager.mu. The entry lets duplicate Start and Stop wait for one bounded
	// startup outcome instead of exposing a partially initialized operation.
	starting map[int64]*operationStart
	// reconcileStartURL is the fixed internal launch condition. It is not a
	// caller-visible DevTools or browser-control capability.
	reconcileStartURL func(context.Context, string, string) error
	// OnCrash is invoked after every child process of an unexpectedly ended
	// operation is fenced and its staging is removed. The Runtime Channel turns
	// it into a durable browser_crashed completion.
	OnCrash func(operationID int64)
}

type operationStart struct {
	done chan struct{}
	err  error
}

type operation struct {
	id                  int64
	display             string
	profile             string
	vncAddress          string
	xvfb, chromium, vnc *exec.Cmd
	// pidfds are opened once for the three owned leaders. Unlike an async Wait
	// goroutine, a pidfd becomes readable as soon as the kernel observes exit,
	// including while the leader is a zombie and helper processes keep its group
	// alive. -1 marks test-only operations without an opened pidfd.
	// pidfdMu protects the descriptors from concurrent Stop/crash observation
	// and watcher cleanup. pidfds are kernel handles, but replacing/closing the
	// integer slots while another goroutine polls them is a Go data race.
	pidfdMu      sync.Mutex
	pidfds       [3]int
	pidfdsOpened bool
	exited       chan struct{}
	stopping     bool
	childExited  bool
	crashed      bool
	// actionMu serializes typed actions for this one operation. It deliberately
	// does not hold Manager.mu while a bounded CDP call waits, so Stop/crash and
	// unrelated browser operations can still make lifecycle progress.
	actionMu sync.Mutex
	// exploration is operation-private state used exclusively by the closed
	// typed action executor. It never holds caller inputs or browser contents.
	// The event recorder starts before the frozen initial navigation, so redirects
	// and popups that happen between model actions remain observable.
	exploration *explorationState
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
	return &Manager{config: config, operations: make(map[int64]*operation), starting: make(map[int64]*operationStart), reconcileStartURL: ensureStartURL}, nil
}

// Start opens an isolated headed Chromium profile and an unauthenticated VNC
// listener on loopback. noVNC is intentionally a transport concern: only a
// Quoin-authorized gRPC tunnel may dial VNCAddress.
func (manager *Manager) Start(ctx context.Context, operationID int64, startURL string) (string, error) {
	return manager.start(ctx, operationID, startURL, "", false)
}

// StartWithProfile opens an isolated writable copy of a published profile.
// The caller supplies the generation path resolved from frozen generation
// sequence, never the Quoin database locator.
func (manager *Manager) StartWithProfile(ctx context.Context, operationID int64, startURL, sourceProfile string) (string, error) {
	if sourceProfile == "" {
		return "", errors.New("published profile path is required")
	}
	return manager.start(ctx, operationID, startURL, sourceProfile, false)
}

// StartExplorationWithProfile starts a profile-backed operation with its
// mandatory event recorder armed before the frozen initial navigation.
func (manager *Manager) StartExplorationWithProfile(ctx context.Context, operationID int64, startURL, sourceProfile string) (string, error) {
	if sourceProfile == "" {
		return "", errors.New("published profile path is required")
	}
	return manager.start(ctx, operationID, startURL, sourceProfile, true)
}

func (manager *Manager) start(ctx context.Context, operationID int64, startURL, sourceProfile string, recordExploration bool) (address string, returnedErr error) {
	if operationID <= 0 || startURL == "" {
		return "", errors.New("operation ID and start URL are required")
	}
	manager.mu.Lock()
	if existing := manager.operations[operationID]; existing != nil {
		manager.mu.Unlock()
		return existing.vncAddress, nil
	}
	if pending := manager.starting[operationID]; pending != nil {
		manager.mu.Unlock()
		select {
		case <-pending.done:
			if pending.err != nil {
				return "", pending.err
			}
			manager.mu.Lock()
			existing := manager.operations[operationID]
			manager.mu.Unlock()
			if existing == nil {
				return "", errors.New("browser operation startup ended without an operation")
			}
			return existing.vncAddress, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if uint32(len(manager.operations)+len(manager.starting)) >= manager.config.Capacity {
		manager.mu.Unlock()
		return "", ErrBusy
	}
	pending := &operationStart{done: make(chan struct{})}
	manager.starting[operationID] = pending
	manager.mu.Unlock()
	defer func() {
		if returnedErr == nil {
			return
		}
		manager.mu.Lock()
		if manager.starting[operationID] == pending {
			pending.err = returnedErr
			delete(manager.starting, operationID)
			close(pending.done)
		}
		manager.mu.Unlock()
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address = listener.Addr().String()
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
	} else if err := removeChromiumRuntimeEntries(profileDirectory); err != nil {
		return "", fmt.Errorf("remove stale Chromium runtime state: %w", err)
	}
	display := ":" + strconv.FormatInt(1000+operationID%50000, 10)
	op := &operation{id: operationID, display: display, profile: profileDirectory, vncAddress: address, exploration: &explorationState{refs: make(map[string]explorationReference)}, pidfds: [3]int{-1, -1, -1}}
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
	// Install the browser-wide deny policy before any navigation. Exploration's
	// per-action CDP guard is defense in depth; it is too late for the frozen
	// start URL, which may itself respond with a download. Every startup CDP
	// phase has its own bound: callers commonly pass the Runtime's long-lived
	// stream context, which must not turn a stalled private endpoint into an
	// indefinitely held capacity reservation.
	downloadCtx, cancelDownload := context.WithTimeout(ctx, operationStartupCDPTimeout)
	err = installOperationDownloadDeny(downloadCtx, profileDirectory)
	cancelDownload()
	if err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("install Chromium download deny policy: %w", err)
	}
	if recordExploration {
		// Establish the neutral operation page before the recorder's startup target
		// inventory fence. ensureStartURL's subsequent frozen-URL reconciliation
		// reuses this about:blank page, so its Target.targetCreated is baseline
		// inventory rather than a fabricated popup in the first Observation.
		neutralCtx, cancelNeutral := context.WithTimeout(ctx, operationStartupCDPTimeout)
		err = ensureExplorationNeutralPage(neutralCtx, profileDirectory)
		cancelNeutral()
		if err != nil {
			manager.kill(op)
			_ = os.RemoveAll(filepath.Dir(profileDirectory))
			return "", fmt.Errorf("establish Chromium exploration neutral page: %w", err)
		}
		// The recorder is mandatory and starts before the frozen start-URL
		// navigation so its operation-lifetime trace includes that redirect chain.
		// Treat an unavailable recorder as an admission failure instead of silently
		// creating an Exploration that cannot produce its mandatory continuous trace.
		recorderCtx, cancelRecorder := context.WithTimeout(ctx, 10*time.Second)
		err = startExplorationEventRecorder(recorderCtx, op)
		cancelRecorder()
		if err != nil {
			manager.kill(op)
			_ = os.RemoveAll(filepath.Dir(profileDirectory))
			return "", fmt.Errorf("start Chromium event recorder: %w", err)
		}
	}
	// A live Chromium PID is not evidence that its requested first page exists.
	// Reconcile the immutable launch URL before creating an attachable RFB
	// surface, so Running can never represent an empty/default page.
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, operationStartupCDPTimeout)
	err = manager.reconcileStartURL(reconcileCtx, profileDirectory, startURL)
	cancelReconcile()
	if err != nil {
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
	if err := openOperationPidfds(op); err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("open browser lifecycle pidfds: %w", err)
	}
	op.exited = make(chan struct{})
	manager.mu.Lock()
	// Stop waits for the startup reservation, so this publication is the first
	// point at which lifecycle operations may observe the fully initialized op.
	manager.operations[operationID] = op
	delete(manager.starting, operationID)
	close(pending.done)
	manager.mu.Unlock()
	manager.watch(op)
	return address, nil
}

// ActiveOperationIDs returns the exact operation IDs that still own a local
// Chromium lifecycle. The control channel includes this snapshot in Hello and
// heartbeats so Quoin can reconcile its capacity projection with physical fact.
func (manager *Manager) ActiveOperationIDs() []int64 {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	ids := make([]int64, 0, len(manager.operations)+len(manager.starting))
	for id := range manager.operations {
		ids = append(ids, id)
	}
	for id := range manager.starting {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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

// chromiumRuntimeEntry is process-local state, not published profile data.
// Chromium leaves these files (and, on Linux, symlinks) behind after a clean
// stop. A generation must neither publish them nor reject an otherwise valid
// profile when cloning it for an isolated operation.
func chromiumRuntimeEntry(relative string) bool {
	if filepath.Dir(relative) != "." {
		return false
	}
	switch filepath.Base(relative) {
	case "DevToolsActivePort", "SingletonCookie", "SingletonLock", "SingletonSocket":
		return true
	default:
		return false
	}
}

func removeChromiumRuntimeEntries(profile string) error {
	for _, name := range []string{"DevToolsActivePort", "SingletonCookie", "SingletonLock", "SingletonSocket"} {
		if err := os.Remove(filepath.Join(profile, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
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
		if chromiumRuntimeEntry(relative) {
			return nil
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
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var (
	navigateOperationStartPage   = navigateStartPage
	installOperationDownloadDeny = installDownloadDeny

	// ErrDownloadBlocked is returned when the immutable operation start URL
	// attempts a download. It is deliberately distinct from navigation failure:
	// admission must report the forbidden download rather than expose a Running
	// browser that has already attempted it.
	ErrDownloadBlocked = errors.New("browser download was blocked")
)

const (
	startPageNavigationAttemptTimeout = 3 * time.Second
	operationStartupCDPTimeout        = 10 * time.Second
)

// ensureExplorationNeutralPage makes sure a neutral page exists before an
// Exploration recorder takes its Target.getTargets baseline fence. It does not
// close pages or navigate: the subsequent frozen-URL reconciliation owns both
// operations, while the recorder must see this target as startup inventory.
func ensureExplorationNeutralPage(parent context.Context, profile string) error {
	ctx, cancel := context.WithTimeout(parent, operationStartupCDPTimeout)
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
	for _, page := range pages {
		if page.Type == "page" && page.URL == "about:blank" {
			return nil
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://127.0.0.1/json/new?"+url.QueryEscape("about:blank"), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("open Chromium exploration neutral page: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("open Chromium exploration neutral page: HTTP %d", response.StatusCode)
	}
	var target devtoolsPage
	err = json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<20)).Decode(&target)
	response.Body.Close()
	if err != nil || target.ID == "" || target.Type != "page" {
		return fmt.Errorf("read Chromium exploration neutral page target: %w", err)
	}
	for {
		pages, err = devtoolsPages(ctx, client)
		if err == nil && containsPageID(pages, target.ID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Chromium exploration neutral page: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

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
		// A recorder-backed operation creates a neutral page before its baseline
		// target inventory fence. Reuse that known page for the frozen start URL
		// instead of creating a post-fence Target.targetCreated which would be a
		// false popup. This also avoids a redundant page for ordinary starts.
		for _, page := range pages {
			if page.Type == "page" && page.URL == "about:blank" {
				startPageID = page.ID
				break
			}
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
	if err := closeNonStartPages(ctx, client, pages, startPageID); err != nil {
		return err
	}
	pages, err = waitForOnlyStartPage(ctx, client, startPageID)
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
	// Profile restoration may publish a page after the first cleanup even
	// though --app=about:blank already created this operation's target. Close
	// such delayed restored pages after the fixed navigation too, then wait for
	// the DevTools target list to converge before declaring the operation live.
	pages, err = devtoolsPages(ctx, client)
	if err != nil {
		return err
	}
	if err := closeNonStartPages(ctx, client, pages, startPageID); err != nil {
		return err
	}
	if _, err = waitForOnlyStartPage(ctx, client, startPageID); err != nil {
		return err
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

func closeNonStartPages(ctx context.Context, client *http.Client, pages []devtoolsPage, startPageID string) error {
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
	return nil
}

// waitForOnlyStartPage accounts for the asynchronous /json/close endpoint:
// observing the close HTTP response does not mean that /json/list has already
// stopped publishing the closing target. A Browser Operation is not Running
// until that public process state converges to exactly its frozen start page.
func waitForOnlyStartPage(ctx context.Context, client *http.Client, startPageID string) ([]devtoolsPage, error) {
	for {
		pages, err := devtoolsPages(ctx, client)
		if err != nil {
			return nil, err
		}
		if containsOnlyPageID(pages, startPageID) {
			return pages, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("Chromium browser operation does not have exactly one start page")
		case <-time.After(25 * time.Millisecond):
		}
	}
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
	connection, err := dialCDP(ctx, endpoint)
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
		if message.Method == "Page.downloadWillBegin" || message.Method == "Browser.downloadWillBegin" {
			return ErrDownloadBlocked
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
	ids := make([]int64, 0, len(manager.operations)+len(manager.starting))
	for id := range manager.operations {
		ids = append(ids, id)
	}
	for id := range manager.starting {
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
	pending := manager.starting[operationID]
	manager.mu.Unlock()
	if pending != nil {
		// Startup may be waiting on bounded private CDP calls. Do not delete its
		// staging underneath it; wait until it publishes or reports failure, then
		// apply the normal stop fence to the fully initialized operation.
		<-pending.done
		return manager.Stop(operationID)
	}
	manager.mu.Lock()
	op := manager.operations[operationID]
	if op != nil {
		// The watcher records every observed child exit under this same mutex.
		// Do not turn a child that has already exited into a clean stop merely
		// because Stop won the next scheduling turn.
		if op.childExited || op.crashed {
			op.crashed = true
			manager.mu.Unlock()
			return errors.New("browser operation crashed before clean stop")
		}
	}
	manager.mu.Unlock()
	operationDirectory := filepath.Join(manager.config.StateDirectory, "operations", strconv.FormatInt(operationID, 10))
	if op != nil {
		stopExplorationEventRecorder(op.exploration)
		// A successful Stop is linearized by delivering SIGTERM to every child
		// process group. If a child already exited, Kill reports ESRCH even when
		// its Wait watcher has not yet acquired manager.mu; that is a crash, not a
		// clean stop whose complete trace may be sealed.
		cleanStop := manager.stopAndWait(op)
		manager.mu.Lock()
		if !cleanStop {
			op.crashed = true
		}
		crashed := op.crashed
		manager.mu.Unlock()
		if crashed {
			return errors.New("browser operation crashed during clean stop")
		}
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
	manager.mu.Unlock()
	stopExplorationEventRecorder(op.exploration)
	// A published identity must survive a Chromium restart. SIGTERM is a
	// process-lifetime fence but does not make Chromium synchronously flush its
	// profile databases; Browser.close is Chromium's graceful close path and
	// commits cookie/storage mutations before the profile becomes immutable.
	cleanStop := manager.closeForProfilePublish(op)
	manager.mu.Lock()
	if !cleanStop {
		op.crashed = true
	}
	crashed := op.crashed
	manager.mu.Unlock()
	if crashed {
		return "", errors.New("browser operation crashed while publishing profile")
	}
	// Process-local Chromium state is not profile data. In particular the
	// Singleton* symlinks point outside this generation and must not be published
	// or copied into a later operation.
	if err := removeChromiumRuntimeEntries(op.profile); err != nil {
		return "", fmt.Errorf("remove Chromium runtime state before publish: %w", err)
	}
	return op.profile, nil
}

// closeForProfilePublish first asks Chromium itself to close through its
// operation-private CDP endpoint and waits for that process to exit before it
// terminates Xvfb/VNC. Chromium requires its display to remain available while
// it flushes Cookie/storage databases during Browser.close. This preserves the
// durable profile state while retaining the existing process-exit crash fence.
func (manager *Manager) closeForProfilePublish(op *operation) bool {
	manager.mu.Lock()
	if manager.operations[op.id] != op || op.childExited || op.crashed {
		op.crashed = true
		manager.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	endpoint, err := browserDevToolsURL(ctx, op.profile)
	if err == nil {
		var connection *websocket.Conn
		connection, err = dialCDP(ctx, endpoint)
		if err == nil {
			_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
			err = json.NewEncoder(connection).Encode(devtoolsMessage{ID: 1, Method: "Browser.close"})
			_ = connection.Close()
		}
	}
	if err != nil {
		op.crashed = true
		manager.mu.Unlock()
		return false
	}
	// The successful CDP write is the intentional-close linearization point;
	// the watcher is serialized on this mutex and cannot label the expected
	// Chromium exit as a crash. Do not stop Xvfb yet: that would turn the
	// Browser.close storage flush into an abrupt browser exit.
	op.stopping = true
	manager.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for !browserMainExited(op, 1) {
		if time.Now().After(deadline) {
			manager.mu.Lock()
			op.crashed = true
			manager.mu.Unlock()
			manager.signal(op, syscall.SIGKILL)
			if op.exited != nil {
				<-op.exited
			}
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}

	manager.mu.Lock()
	crashed := op.crashed
	for _, child := range []*exec.Cmd{op.vnc, op.xvfb} {
		// The watcher may already have terminated auxiliaries after observing the
		// intentional Chromium exit. Their absence is therefore not a failure.
		if child != nil && child.Process != nil {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGTERM)
		}
	}
	manager.mu.Unlock()
	if crashed {
		return false
	}
	if op.exited == nil {
		return false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Millisecond
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-op.exited:
		return true
	case <-timer.C:
		manager.signal(op, syscall.SIGKILL)
		<-op.exited
		return false
	}
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
		defer closeOperationPidfds(op)
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

// stopAndWait returns whether SIGTERM was delivered to every child process
// group before any child had exited. This signal-delivery barrier is the Stop
// linearization point: relying only on the asynchronously scheduled Wait
// watcher would allow a prior crash to be relabelled as a clean Stop.
func (manager *Manager) stopAndWait(op *operation) bool {
	// Linearize Stop with child exit observation under manager.mu: watcher
	// cannot see `stopping` until SIGTERM has been successfully delivered to
	// every process group. A pre-existing exit therefore stays a crash rather
	// than being relabelled as a clean stop by goroutine scheduling.
	manager.mu.Lock()
	if manager.operations[op.id] != op || op.childExited || op.crashed {
		op.crashed = true
		manager.mu.Unlock()
		return false
	}
	clean := manager.signalLive(op, syscall.SIGTERM)
	if !clean {
		op.crashed = true
		manager.mu.Unlock()
		return false
	}
	op.stopping = true
	manager.mu.Unlock()
	if op.exited != nil {
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-op.exited:
			timer.Stop()
		case <-timer.C:
			manager.signal(op, syscall.SIGKILL)
			<-op.exited
		}
		return clean
	}
	manager.kill(op)
	return clean
}

// signalLive reports false when any owned main process has already exited
// before the first Stop signal. Checking only the process group is insufficient:
// a Chromium helper can keep its group alive after Chromium itself exits, while
// the asynchronous Wait watcher has not yet recorded that exit. The main-PID
// liveness check and the group SIGTERM occur under manager.mu, making Stop's
// clean/crashed decision linearizable with an observed physical process exit.
func browserMainExited(op *operation, index int) bool {
	op.pidfdMu.Lock()
	defer op.pidfdMu.Unlock()
	if op.pidfdsOpened {
		return operationPIDExited(op.pidfds[index])
	}
	commands := []*exec.Cmd{op.vnc, op.chromium, op.xvfb}
	return commands[index] == nil || commands[index].ProcessState != nil
}

func (manager *Manager) signalLive(op *operation, signal syscall.Signal) bool {
	commands := []*exec.Cmd{op.vnc, op.chromium, op.xvfb}
	clean := true
	op.pidfdMu.Lock()
	defer op.pidfdMu.Unlock()
	for index, command := range commands {
		if command == nil || command.Process == nil || (op.pidfdsOpened && operationPIDExited(op.pidfds[index])) {
			clean = false
			continue
		}
		if err := syscall.Kill(-command.Process.Pid, signal); err != nil {
			clean = false
		}
	}
	return clean
}

func openOperationPidfds(op *operation) error {
	op.pidfdMu.Lock()
	defer op.pidfdMu.Unlock()
	fds := [3]int{-1, -1, -1}
	for index, command := range []*exec.Cmd{op.vnc, op.chromium, op.xvfb} {
		if command == nil || command.Process == nil {
			for _, fd := range fds {
				if fd >= 0 {
					_ = unix.Close(fd)
				}
			}
			return errors.New("browser leader is missing")
		}
		fd, err := unix.PidfdOpen(command.Process.Pid, 0)
		if err != nil {
			for _, opened := range fds {
				if opened >= 0 {
					_ = unix.Close(opened)
				}
			}
			return err
		}
		fds[index] = fd
	}
	op.pidfds = fds
	op.pidfdsOpened = true
	return nil
}

func closeOperationPidfds(op *operation) {
	op.pidfdMu.Lock()
	defer op.pidfdMu.Unlock()
	if !op.pidfdsOpened {
		return
	}
	for index, fd := range op.pidfds {
		if fd >= 0 {
			_ = unix.Close(fd)
			op.pidfds[index] = -1
		}
	}
	op.pidfdsOpened = false
}

// operationPIDExited tests the kernel-owned pidfd before Stop marks the
// operation clean. Poll reports an exited leader even when exec.Cmd.Wait has
// not yet been scheduled, which is the physical crash/Stop race we must not
// classify as a clean close. Test-only operations that lack pidfds retain the
// conservative process-group behavior.
func operationPIDExited(fd int) bool {
	if fd < 0 {
		return false
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, err := unix.Poll(poll, 0)
	return err != nil || poll[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0
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
