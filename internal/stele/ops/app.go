package ops

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
	"github.com/Suknna/quoin/internal/stele"
	"github.com/prometheus/client_golang/prometheus"
)

func Run(ctx context.Context, configPath string) error {
	var config contract.SteleConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return err
	}
	if config.Component != "stele" {
		return fmt.Errorf("configuration component must be stele")
	}
	if _, err := os.ReadFile(config.QuoinRuntimeCAFile); err != nil {
		return fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	token, err := os.ReadFile(config.ServiceTokenFile)
	if err != nil {
		return fmt.Errorf("read Stele service token: %w", err)
	}
	if len(token) != 32 {
		return fmt.Errorf("Stele service token must contain exactly 32 bytes")
	}
	server, err := sharedops.New("stele", ":9090", sharedops.DependencyUnavailable)
	if err != nil {
		return err
	}
	relay, err := stele.NewRelay(config.QuoinRuntimeEndpoint, config.QuoinRuntimeCAFile, config.ServiceTokenFile)
	if err != nil {
		return fmt.Errorf("connect Quoin Runtime: %w", err)
	}
	defer relay.Close()
	webhook := &http.Server{Addr: ":8080", Handler: stele.NewWebhook(relay, prometheus.NewRegistry()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second}
	// Transition readiness once the credential snapshot is loaded; before
	// that the webhook returns 503 (OPS-HEALTH-005, RUNTIME-STELE-002).
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
	go func() { errCh <- webhook.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		webhookErr := webhook.Shutdown(shutdownCtx)
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
