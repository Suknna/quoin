package ops

import (
	"context"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"github.com/Suknna/quoin/internal/plinth/runtime"
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
	return RunServe(ctx, config, server)
}

// RunRegister performs the one-time attached-stdin registration for the
// plinth runtime slot (RUNTIME-REG-002): the revealed token JSON is read
// from stdin only, never from argv or the environment.
func RunRegister(ctx context.Context, configPath string) error {
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
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:               "plinth",
		QuoinEndpoint:      config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile: config.QuoinRuntimeCAFile,
		StateDirectory:     config.StateDirectory,
	})
	if err != nil {
		return err
	}
	return channel.RunRegister(ctx, os.Stdin, os.Stdout)
}
