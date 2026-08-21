package ops

import (
	"context"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plinth/runtime"
	"github.com/Suknna/quoin/internal/plinth/supervisor"
)

// RunServe is the long-lived serve path: ops endpoint plus the outbound
// Connect control loop, retried with backoff while unregistered. The process
// stays alive so the deployment keeps it running; readiness stays strict
// (unregistered until a Quoin-accepted handshake).
func RunServe(ctx context.Context, config contract.PlinthConfig, server *sharedops.Server) error {
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:               "plinth",
		QuoinEndpoint:      config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile: config.QuoinRuntimeCAFile,
		StateDirectory:     config.StateDirectory,
	})
	if err != nil {
		return err
	}
	channel.Tasks = &supervisor.Supervisor{Channel: channel, WorkspaceRoot: config.WorkspaceDirectory}
	// Terminal results retry until a ResultAck survives the stream it
	// travelled on (T12, RUNTIME-TASK-008); the loop is boot-scoped.
	go channel.RunResultDeliveryLoop(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := channel.RunConnect(ctx, server); err != nil {
				sharedops.LogEvent("plinth", "info", "runtime.reconnect", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectInterval):
			}
		}
	}()
	return server.Run(ctx)
}

// reconnectInterval is the fixed reconnect cadence for the v1 dev
// projection (RUNTIME-SCOPE-004: the frozen release source owns the value).
const reconnectInterval = 2 * time.Second
