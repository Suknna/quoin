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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
	return &Manager{config: config, operations: make(map[int64]*operation)}, nil
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
	op.chromium = chromiumCommand(ctx, manager.config.ChromiumBinary, profileDirectory, startURL)
	op.chromium.Env = append(op.chromium.Env, "DISPLAY="+display)
	if err := op.chromium.Start(); err != nil {
		manager.kill(op)
		_ = os.RemoveAll(filepath.Dir(profileDirectory))
		return "", fmt.Errorf("start Chromium: %w", err)
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
func chromiumCommand(ctx context.Context, binary, profile, startURL string) *exec.Cmd {
	command := managedCommand(ctx, binary,
		"--no-sandbox",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--disable-background-networking",
		"--disable-sync",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=0",
		startURL,
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
	for _, page := range pages {
		if page.Type == "page" && strings.HasPrefix(page.URL, prefix) {
			return true, nil
		}
	}
	return false, nil
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
func (manager *Manager) ProfilePath(operationID int64) (string, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	op := manager.operations[operationID]
	if op == nil {
		return "", false
	}
	return op.profile, true
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
	if op != nil {
		op.stopping = true
	}
	manager.mu.Unlock()
	if op == nil {
		return "", errors.New("browser operation is not running")
	}
	manager.stopAndWait(op)
	return op.profile, nil
}

func (manager *Manager) watch(op *operation) {
	finished := make(chan struct{}, 3)
	for _, command := range []*exec.Cmd{op.vnc, op.chromium, op.xvfb} {
		go func(command *exec.Cmd) { _ = command.Wait(); finished <- struct{}{} }(command)
	}
	go func() {
		<-finished
		manager.mu.Lock()
		crashed := manager.operations[op.id] == op && !op.stopping
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
		close(op.exited)
		if crashed {
			_ = os.RemoveAll(filepath.Dir(op.profile))
			if manager.OnCrash != nil {
				manager.OnCrash(op.id)
			}
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
