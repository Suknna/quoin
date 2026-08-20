package ops

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/runtime"
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
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:               "lintel",
		QuoinEndpoint:      config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile: config.QuoinRuntimeCAFile,
		StateDirectory:     config.StateDirectory,
		BrowserSlots:       uint32(config.BrowserSlots),
		// No Chromium runtime ships in this stage; the browser runtime
		// release that owns the real revision fills this in later.
		ChromiumRevision: "",
	})
	if err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := channel.RunConnect(ctx, server); err != nil {
				sharedops.LogEvent("lintel", "info", "runtime.reconnect", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return server.Run(ctx)
}

// RunRegister performs the one-time attached-stdin registration for the
// lintel runtime slot: the revealed token JSON is read from stdin only,
// never from argv or the environment (RUNTIME-REG-002).
func RunRegister(ctx context.Context, configPath string) error {
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
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:               "lintel",
		QuoinEndpoint:      config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile: config.QuoinRuntimeCAFile,
		StateDirectory:     config.StateDirectory,
		BrowserSlots:       uint32(config.BrowserSlots),
		ChromiumRevision:   "",
	})
	if err != nil {
		return err
	}
	return channel.RunRegister(ctx)
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
