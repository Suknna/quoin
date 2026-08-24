package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClosedTunnelCannotBeReattached(t *testing.T) {
	hub := newBrowserTunnelHub()
	tunnel, err := hub.register(1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := hub.reserveAttachment(1)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := hub.attach(1, reservation)
	if err != nil || attached != tunnel {
		t.Fatalf("attach: tunnel=%p err=%v", attached, err)
	}
	hub.closeAttachment(tunnel)
	reservation, err = hub.reserveAttachment(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.attach(1, reservation); err != errBrowserTunnelUnavailable {
		t.Fatalf("closed transport reattached: %v", err)
	}
}

func TestSupersededControlOwnerCannotBeAttached(t *testing.T) {
	hub := newBrowserTunnelHub()
	ownerClosing := make(chan struct{})
	if _, err := hub.register(1, 1, ownerClosing); err != nil {
		t.Fatal(err)
	}
	close(ownerClosing)
	reservation, err := hub.reserveAttachment(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.attach(1, reservation); err != errBrowserTunnelUnavailable {
		t.Fatalf("superseded control owner retained a browser tunnel: %v", err)
	}
	// The close is synchronous under the hub lock; this guards against a stale
	// map entry being left behind for a later Lintel control owner.
	if _, err := hub.register(1, 2, make(chan struct{})); err != nil {
		t.Fatalf("new control owner cannot replace stale tunnel: %v", err)
	}
}

func TestAttachmentReservationRejectsSecondUpgradeBeforeAttach(t *testing.T) {
	hub := newBrowserTunnelHub()
	first, err := hub.reserveAttachment(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.reserveAttachment(1); err == nil {
		t.Fatal("second WebSocket upgrade did not receive an attachment conflict")
	}
	hub.releaseReservation(1, first)
	if _, err := hub.reserveAttachment(1); err != nil {
		t.Fatalf("released upgrade reservation was not reusable: %v", err)
	}
}

func TestUnavailableTunnelProducesPreUpgradeServiceUnavailable(t *testing.T) {
	hub := newBrowserTunnelHub()
	reservation, err := hub.reserveAttachment(17)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.attachAwaitFor(context.Background(), 17, reservation, time.Millisecond); err != errBrowserTunnelUnavailable {
		t.Fatalf("unavailable tunnel error = %v, want %v", err, errBrowserTunnelUnavailable)
	}
	hub.releaseReservation(17, reservation)

	response := httptest.NewRecorder()
	browserTunnelUnavailable(response)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable tunnel status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable *bool  `json:"retryable"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "runtime_unavailable" || body.Message == "" || body.Retryable == nil || !*body.Retryable {
		t.Fatalf("unavailable tunnel response = %#v", body)
	}
}
