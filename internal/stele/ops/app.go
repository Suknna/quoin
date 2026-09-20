package ops

// Stele 装配（ADR-0011）：本地 SQLite 队列 + webhook 入站 + 转发循环 +
// 出向网关流 + ops/webhook 两个监听面。readiness = 凭据快照已加载（本地
// 库打不开时进程根本起不来，是启动前置而非运行态条件）；网关流只作指标
// 观测，不阻塞入向就绪。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Suknna/quoin/internal/buildinfo"
	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plugins"
	"github.com/Suknna/quoin/internal/stele"
)

func Run(ctx context.Context, configPath string) error {
	var config contract.SteleConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return err
	}
	if config.Component != "stele" {
		return fmt.Errorf("configuration component must be stele")
	}
	if config.DataDirectory == "" {
		return fmt.Errorf("configuration dataDirectory is required for the local state queue")
	}
	if _, err := os.ReadFile(config.QuoinRuntimeCAFile); err != nil {
		return fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	if _, err := os.Stat(config.QuoinRuntimeClientCertificateFile); err != nil {
		return fmt.Errorf("read Stele client certificate: %w", err)
	}
	if _, err := os.Stat(config.QuoinRuntimeClientPrivateKeyFile); err != nil {
		return fmt.Errorf("read Stele client private key: %w", err)
	}
	server, err := sharedops.New("stele", ":9090", sharedops.DependencyUnavailable)
	if err != nil {
		return err
	}
	// 本地状态库是入站可靠性承诺（202 即 ACK）的载体：打不开就不启动。
	queue, err := stele.OpenQueue(config.DataDirectory)
	if err != nil {
		return err
	}
	defer queue.Close()
	relay, err := stele.NewRelay(config.QuoinRuntimeEndpoint, config.QuoinRuntimeCAFile, config.QuoinRuntimeClientCertificateFile, config.QuoinRuntimeClientPrivateKeyFile)
	if err != nil {
		return fmt.Errorf("connect Quoin Runtime: %w", err)
	}
	defer relay.Close()

	metrics := stele.NewMetrics()
	go relay.Run(ctx)
	forwarder := stele.NewForwarder(queue, relay, metrics)
	go forwarder.Run(ctx)
	gateway := stele.NewGateway(relay, queue, metrics)
	go gateway.Run(ctx)

	webhook := stele.NewWebhook(queue, relay, plugins.Default(), metrics)
	webhookServer := &http.Server{
		Addr: ":8080", Handler: webhook.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	// Transition readiness once the credential snapshot is loaded; before
	// that the webhook returns 503 (OPS-HEALTH-005, RUNTIME-STELE-002).
	// 网关流是否建立单独观测（gateway.Ready），不阻塞入向。
	go func() {
		for {
			if relay.Ready() {
				server.SetReadiness(sharedops.Readiness{Component: "stele", Release: buildinfo.Release, Mode: "normal", AcceptingWork: true, Reason: sharedops.Ready})
			} else {
				server.SetReadiness(sharedops.Readiness{Component: "stele", Release: buildinfo.Release, Mode: "normal", AcceptingWork: false, Reason: sharedops.DependencyUnavailable})
			}
			time.Sleep(time.Second)
		}
	}()
	errCh := make(chan error, 2)
	opsDone := make(chan error, 1)
	go func() { opsDone <- server.Run(ctx) }()
	go func() { errCh <- webhookServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		webhookErr := webhookServer.Shutdown(shutdownCtx)
		// The ops listener owns the drained readiness window after SIGTERM;
		// the process must not exit before that surface has closed.
		opsErr := <-opsDone
		if webhookErr != nil {
			return webhookErr
		}
		return opsErr
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
