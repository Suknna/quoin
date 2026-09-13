package app

import (
	"time"

	"google.golang.org/grpc/keepalive"
)

// runtimeRelayKeepalivePolicy permits the Plinth partition detector's 20-second
// idle pings. The default gRPC server policy rejects them as too_many_pings,
// repeatedly disconnecting every live runtime control stream.
func runtimeRelayKeepalivePolicy() keepalive.EnforcementPolicy {
	return keepalive.EnforcementPolicy{
		MinTime:             20 * time.Second,
		PermitWithoutStream: true,
	}
}
