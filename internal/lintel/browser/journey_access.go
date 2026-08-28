// Journey DevTools access (T23): the only Manager surface the versioned
// Journey executor needs. It resolves the running operation's loopback
// DevTools endpoint exactly like the authentication probe does, and exposes
// no browser-control capability beyond it.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// JourneyDevToolsBase resolves http://127.0.0.1:<port> for one running
// operation's Chromium DevTools endpoint.
func (manager *Manager) JourneyDevToolsBase(ctx context.Context, operationID int64) (string, error) {
	manager.mu.Lock()
	op := manager.operations[operationID]
	manager.mu.Unlock()
	if op == nil {
		return "", errors.New("browser operation is not running")
	}
	portPath := filepath.Join(op.profile, "DevToolsActivePort")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		body, err := os.ReadFile(portPath)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) == 0 || lines[0] == "" {
				return "", errors.New("Chromium DevTools endpoint is malformed")
			}
			port, parseErr := strconv.ParseUint(lines[0], 10, 16)
			if parseErr != nil || port == 0 {
				return "", errors.New("Chromium DevTools port is malformed")
			}
			return fmt.Sprintf("http://127.0.0.1:%d", port), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read Chromium DevTools endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errors.New("Chromium DevTools endpoint did not become ready")
		case <-time.After(25 * time.Millisecond):
		}
	}
}
