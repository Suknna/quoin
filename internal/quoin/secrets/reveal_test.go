package secrets

// The reveal-handle boundary tests drive the frozen one-time reveal
// contract (SEC-REVEAL-*, SEC-VALIDATION-004) with an injected clock: the
// handle stays valid before the TTL boundary, transitions exactly at it, and
// the consumed/expired terminal states stay stable afterwards.

import (
	"testing"
	"time"
)

func digestOf(seed byte) [32]byte {
	var digest [32]byte
	for i := range digest {
		digest[i] = seed
	}
	return digest
}

func newStoreAt(now time.Time) (*Store, *time.Time) {
	current := now
	store := NewStore()
	store.now = func() time.Time { return current }
	return store, &current
}

func TestRevealHandleBeforeBoundaryStaysValid(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store, clock := newStoreAt(now)
	session := digestOf(1)
	handle := store.Create(session, "cmd-1", 7, "bearer-value")

	// One second before the TTL boundary the handle is still replayable and
	// consumable (Lookup serves the command replay path).
	*clock = now.Add(handleTTL - time.Second)
	if _, _, valid := store.Lookup(session, "cmd-1"); !valid {
		t.Fatal("handle invalid before the TTL boundary")
	}
	bearer, _, ok, mismatch := store.Consume(handle, session)
	if !ok || mismatch || bearer != "bearer-value" {
		t.Fatalf("consume before boundary failed: ok=%v mismatch=%v", ok, mismatch)
	}
}

func TestRevealHandleAtBoundaryIsExpired(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store, clock := newStoreAt(now)
	session := digestOf(2)
	handle := store.Create(session, "cmd-2", 8, "bearer-value")

	// Exactly at the boundary the handle is already expired: the TTL window
	// is half-open [created, created+TTL).
	*clock = now.Add(handleTTL)
	if _, _, valid := store.Lookup(session, "cmd-2"); valid {
		t.Fatal("lookup accepted an at-boundary expired handle")
	}
	if _, _, ok, _ := store.Consume(handle, session); ok {
		t.Fatal("consume accepted an at-boundary expired handle")
	}
}

func TestRevealHandleAfterBoundaryTerminalIsStable(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store, clock := newStoreAt(now)
	session := digestOf(3)
	handle := store.Create(session, "cmd-3", 9, "bearer-value")
	*clock = now.Add(handleTTL + time.Minute)
	if _, _, ok, _ := store.Consume(handle, session); ok {
		t.Fatal("consume accepted an expired handle")
	}
	// Repeated evaluations keep failing: expiry is a stable terminal state,
	// never a resurrection.
	*clock = now.Add(handleTTL + 2*time.Hour)
	if _, _, valid := store.Lookup(session, "cmd-3"); valid {
		t.Fatal("expired handle resurrected on later lookup")
	}
}

func TestRevealHandleConsumedOnceAndNeverReplays(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store, clock := newStoreAt(now)
	session := digestOf(4)
	handle := store.Create(session, "cmd-4", 10, "bearer-once")
	if bearer, _, ok, _ := store.Consume(handle, session); !ok || bearer != "bearer-once" {
		t.Fatal("first consume failed")
	}
	*clock = now.Add(time.Second)
	if _, _, ok, _ := store.Consume(handle, session); ok {
		t.Fatal("second consume of the same handle succeeded")
	}
	// A different session can never redeem another session's handle.
	other := store.Create(digestOf(5), "cmd-5", 11, "bearer-other")
	if _, _, ok, mismatch := store.Consume(other, session); ok || !mismatch {
		t.Fatalf("cross-session consume must fail with session mismatch: ok=%v mismatch=%v", ok, mismatch)
	}
}

func TestRevealHandleSessionInvalidationDropsEntries(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store, _ := newStoreAt(now)
	session := digestOf(6)
	handle := store.Create(session, "cmd-6", 12, "bearer-six")
	store.InvalidateSession(session)
	if _, _, valid := store.Lookup(session, "cmd-6"); valid {
		t.Fatal("invalidated session still replays its handle")
	}
	if _, _, ok, _ := store.Consume(handle, session); ok {
		t.Fatal("invalidated session still consumes its handle")
	}
}
