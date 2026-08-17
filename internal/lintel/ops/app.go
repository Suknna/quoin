package ops

import (
	"context"
	"fmt"
	"os"

	"github.com/Suknna/quoin/internal/contract"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"golang.org/x/sys/unix"
)

func Run(ctx context.Context, configPath string) error {
	var config contract.LintelConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return err
	}
	if config.Component != "lintel" {
		return fmt.Errorf("configuration component must be lintel")
	}
	if _, err := os.ReadFile(config.QuoinRuntimeCAFile); err != nil {
		return fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	if err := verifySharedMemory(config.MinimumShmBytes); err != nil {
		return err
	}
	lock, err := sharedops.AcquireDirectory(config.StateDirectory)
	if err != nil {
		return err
	}
	defer lock.Close()
	server, err := sharedops.New("lintel", ":9090", sharedops.RuntimeUnregistered)
	if err != nil {
		return err
	}
	return server.Run(ctx)
}

func verifySharedMemory(minimum int64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs("/dev/shm", &stat); err != nil {
		return fmt.Errorf("inspect /dev/shm: %w", err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available < minimum {
		return fmt.Errorf("/dev/shm has %d bytes available; need at least %d", available, minimum)
	}
	return nil
}
