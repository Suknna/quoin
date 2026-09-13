package app

import (
	"testing"
	"time"
)

// TestRuntimeRelayKeepalivePolicy keeps the server-side contract aligned with
// Plinth's 20-second idle partition detector so live control streams cannot be
// severed by gRPC's default five-minute enforcement policy.
func TestRuntimeRelayKeepalivePolicy(t *testing.T) {
	policy := runtimeRelayKeepalivePolicy()
	if policy.MinTime > 20*time.Second {
		t.Fatalf("minimum ping interval %s rejects Plinth's 20s detector", policy.MinTime)
	}
	if !policy.PermitWithoutStream {
		t.Fatal("runtime relay must permit Plinth's idle partition-detector pings")
	}
}
