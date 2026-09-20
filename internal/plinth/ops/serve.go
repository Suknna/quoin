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
// mTLS-authenticated Connect control loop, retried with backoff. The process
// stays alive so the deployment keeps it running; readiness stays strict
// (not ready until a Quoin-accepted handshake).
// ADR-0011 之后 Plinth 是纯推理沙箱:这里不再装配任何插件注册表或类型化
// 执行表(执行已移交 Quoin),supervisor 只带通道与工作区根目录。
func RunServe(ctx context.Context, config contract.PlinthConfig, server *sharedops.Server) error {
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:                              "plinth",
		QuoinEndpoint:                     config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile:                config.QuoinRuntimeCAFile,
		QuoinRuntimeClientCertificateFile: config.QuoinRuntimeClientCertificateFile,
		QuoinRuntimeClientPrivateKeyFile:  config.QuoinRuntimeClientPrivateKeyFile,
		StateDirectory:                    config.StateDirectory,
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
