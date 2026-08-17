package app

import (
	"strconv"
	"sync"

	"github.com/Suknna/quoin/internal/quoin/alerts"
)

// commandReplay is a minimal in-process idempotency ledger for the create
// alert-source command (HTTP-COMMAND-003 semantics). The frozen
// client_commands table is persisted by a later ticket; this map is keyed by
// (principal, client_command_id) and carries the non-secret result and digest
// only — reveal handles and raw secrets never enter it (SEC-REVEAL-005).
type commandReplay struct {
	mu    sync.Mutex
	items map[string]commandResult
}

// maxReplayEntries bounds the in-process ledger; the frozen client_commands
// table (persisted by a later ticket) is the durable authority.
const maxReplayEntries = 1024

type commandResult struct {
	digest string
	result alerts.CreateSourceResult
}

func newCommandReplay() *commandReplay {
	return &commandReplay{items: map[string]commandResult{}}
}

func (replay *commandReplay) key(principalID int64, commandID string) string {
	return strconv.FormatInt(principalID, 10) + ":" + commandID
}

func (replay *commandReplay) lookup(principalID int64, commandID string) (commandResult, bool) {
	replay.mu.Lock()
	defer replay.mu.Unlock()
	result, ok := replay.items[replay.key(principalID, commandID)]
	return result, ok
}

func (replay *commandReplay) remember(principalID int64, commandID, digest string, result alerts.CreateSourceResult) {
	replay.mu.Lock()
	defer replay.mu.Unlock()
	if _, exists := replay.items[replay.key(principalID, commandID)]; !exists && len(replay.items) >= maxReplayEntries {
		// Evict the oldest entry so the map cannot grow without bound.
		var oldestKey string
		for candidate := range replay.items {
			oldestKey = candidate
			break
		}
		delete(replay.items, oldestKey)
	}
	replay.items[replay.key(principalID, commandID)] = commandResult{digest: digest, result: result}
}
