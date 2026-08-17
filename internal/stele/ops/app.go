package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
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
	webhook := &http.Server{Addr: ":8080", Handler: http.NotFoundHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 2)
	go func() { errCh <- server.Run(ctx) }()
	go func() { errCh <- webhook.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return webhook.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
