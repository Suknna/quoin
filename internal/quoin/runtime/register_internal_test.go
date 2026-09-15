package runtime

// White-box coverage of the infra-retry treatment of the registration token
// (register.go): an infra failure leaves no durable trace, so the unconsumed
// token returns to the pending window within its original expiry, while an
// expired token stays consumed. Deterministic rejections never re-arm.

import (
	"errors"
	"testing"
	"time"

	"github.com/Suknna/quoin/internal/quoin/execution"
)

// TestRegisterRejectionMapping pins the stable typed error surface: every
// recorded registration precondition rejection surfaces as the package's
// FAILED_PRECONDITION *RegisterError (the gRPC status mapping keys on it),
// and any other recorded rejection passes through verbatim.
func TestRegisterRejectionMapping(t *testing.T) {
	err := registerRejectionError(preconditionRejection("slot is not in a registration window (ALREADY_REGISTERED)"))
	var registerErr *RegisterError
	if !errors.As(err, &registerErr) || registerErr.Status != "FAILED_PRECONDITION" || registerErr.Detail != "slot is not in a registration window (ALREADY_REGISTERED)" {
		t.Fatalf("precondition rejection must map to FAILED_PRECONDITION, got %v", err)
	}
	foreign := &execution.Rejection{Code: "unexpected_code", Detail: "x"}
	if got := registerRejectionError(foreign); !errors.Is(got, foreign) || got != error(foreign) {
		t.Fatalf("a foreign rejection must pass through verbatim, got %v", got)
	}
}

func TestRearmTokenRestoresPendingWithinWindow(t *testing.T) {
	now := time.Now()
	service := &Service{now: func() time.Time { return now }, pending: map[string]*registrationToken{}}
	token := &registrationToken{slot: SlotPlinth, generation: 3, consumed: true, expiresAt: now.Add(registrationTokenTTL)}
	service.rearmToken(token)
	if token.consumed {
		t.Fatal("re-armed token must be unconsumed again")
	}
	if service.pending[SlotPlinth+"\x00"+itoa64(3)] != token {
		t.Fatal("re-armed token must be pending for its slot and generation")
	}
}

func TestRearmTokenKeepsExpiredTokenConsumed(t *testing.T) {
	now := time.Now()
	service := &Service{now: func() time.Time { return now }, pending: map[string]*registrationToken{}}
	token := &registrationToken{slot: SlotPlinth, generation: 4, consumed: true, expiresAt: now.Add(-time.Second)}
	service.rearmToken(token)
	if !token.consumed {
		t.Fatal("expired token must stay consumed")
	}
	if len(service.pending) != 0 {
		t.Fatalf("expired token must not re-enter the pending map, got %d entries", len(service.pending))
	}
}

func TestRearmTokenYieldsToANewerTokenForTheSameWindow(t *testing.T) {
	// A re-mint for the same slot and generation (prepare replay of another
	// session, recovery re-begin) owns the window while the failed execution
	// was in flight; the stale token must never be resurrected over it.
	now := time.Now()
	service := &Service{now: func() time.Time { return now }, pending: map[string]*registrationToken{}}
	newer := &registrationToken{slot: SlotPlinth, generation: 3, expiresAt: now.Add(registrationTokenTTL)}
	service.pending[SlotPlinth+"\x00"+itoa64(3)] = newer
	stale := &registrationToken{slot: SlotPlinth, generation: 3, consumed: true, expiresAt: now.Add(registrationTokenTTL)}
	service.rearmToken(stale)
	if !stale.consumed {
		t.Fatal("stale token must stay consumed when a newer token owns the window")
	}
	if service.pending[SlotPlinth+"\x00"+itoa64(3)] != newer {
		t.Fatal("the newer token must keep the pending window")
	}
}
