package app

import "testing"

func TestClosedTunnelCannotBeReattached(t *testing.T) {
	hub := newBrowserTunnelHub()
	tunnel, err := hub.register(1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := hub.attach(1)
	if err != nil || attached != tunnel {
		t.Fatalf("attach: tunnel=%p err=%v", attached, err)
	}
	hub.closeAttachment(tunnel)
	if _, err := hub.attach(1); err != errBrowserTunnelUnavailable {
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
	if _, err := hub.attach(1); err != errBrowserTunnelUnavailable {
		t.Fatalf("superseded control owner retained a browser tunnel: %v", err)
	}
	// The close is synchronous under the hub lock; this guards against a stale
	// map entry being left behind for a later Lintel control owner.
	if _, err := hub.register(1, 2, make(chan struct{})); err != nil {
		t.Fatalf("new control owner cannot replace stale tunnel: %v", err)
	}
}
