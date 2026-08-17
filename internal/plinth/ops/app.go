package ops

import (
	"context"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

func Run(ctx context.Context, configPath string) error {
	var config contract.PlinthConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return err
	}
	if config.Component != "plinth" {
		return fmt.Errorf("configuration component must be plinth")
	}
	if _, err := os.ReadFile(config.QuoinRuntimeCAFile); err != nil {
		return fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	lock, err := sharedops.AcquireDirectory(config.StateDirectory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := os.MkdirAll(config.WorkspaceDirectory, 0o700); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}
	server, err := sharedops.New("plinth", ":9090", sharedops.RuntimeUnregistered)
	if err != nil {
		return err
	}
	return server.Run(ctx)
}
