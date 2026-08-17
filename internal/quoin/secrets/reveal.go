// Package secrets owns the in-process, single-consumption reveal capability
// store for one-time secret display (SEC-REVEAL-*). Handles never touch disk.
package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

const handleTTL = 60 * time.Second

type entry struct {
	digest       [32]byte // session identity, not the raw session token
	commandID    string
	credentialID int64
	bearer       string
	expiresAt    time.Time
	consumed     bool
}

// Store keeps reveal handles in process memory only. Process restart, session
// logout/revocation/disable, or expiry invalidates every outstanding handle.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

func NewStore() *Store {
	return &Store{entries: map[string]*entry{}, now: time.Now}
}

// Create binds a fresh handle to the creating session, command, and the raw
// bearer that will be revealed exactly once within handleTTL.
func (store *Store) Create(sessionDigest [32]byte, commandID string, credentialID int64, bearer string) string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	handle := base64.RawURLEncoding.EncodeToString(raw)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.evictLocked()
	store.entries[handle] = &entry{
		digest: sessionDigest, commandID: commandID, credentialID: credentialID,
		bearer: bearer, expiresAt: store.now().Add(handleTTL),
	}
	return handle
}

// Lookup returns the still-valid handle for a command replay, if any
// (SEC-REVEAL-003: replays reuse the original handle or report unavailable).
func (store *Store) Lookup(sessionDigest [32]byte, commandID string) (handle string, credentialID int64, ok bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.evictLocked()
	for candidate, entry := range store.entries {
		if entry.digest == sessionDigest && entry.commandID == commandID && !entry.consumed {
			return candidate, entry.credentialID, true
		}
	}
	return "", 0, false
}

// Consume atomically redeems a handle: only the same session that created it,
// before expiry and exactly once, receives the bearer. The second return says
// why redemption failed: expired/consumed/unknown handle → 410 semantics;
// session mismatch → treat as unauthorized.
func (store *Store) Consume(handle string, sessionDigest [32]byte) (bearer string, credentialID int64, ok bool, sessionMismatch bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.evictLocked()
	entry, exists := store.entries[handle]
	if !exists {
		return "", 0, false, false
	}
	if entry.digest != sessionDigest {
		return "", 0, false, true
	}
	if entry.consumed || !store.now().Before(entry.expiresAt) {
		return "", 0, false, false
	}
	entry.consumed = true
	bearer = entry.bearer
	credentialID = entry.credentialID
	delete(store.entries, handle)
	return bearer, credentialID, true, false
}

// InvalidateSession drops every handle bound to a session (logout/revoke).
func (store *Store) InvalidateSession(sessionDigest [32]byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for handle, entry := range store.entries {
		if entry.digest == sessionDigest {
			delete(store.entries, handle)
		}
	}
}

func SessionDigest(sessionToken string) [32]byte {
	return sha256.Sum256([]byte(sessionToken))
}

func (store *Store) evictLocked() {
	now := store.now()
	for handle, entry := range store.entries {
		if entry.consumed || !now.Before(entry.expiresAt) {
			delete(store.entries, handle)
		}
	}
}
