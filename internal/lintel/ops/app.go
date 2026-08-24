package ops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/contract"
	"github.com/Suknna/quoin/internal/lintel/browser"
	"github.com/Suknna/quoin/internal/lintel/runtime"
	sharedops "github.com/Suknna/quoin/internal/ops"
	"golang.org/x/sys/unix"
)

func Run(ctx context.Context, configPath string) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	browserManager, err := browser.NewManager(browser.Config{StateDirectory: config.StateDirectory, Capacity: uint32(config.BrowserSlots)})
	if err != nil {
		return err
	}
	revision, err := chromiumRevision(runCtx)
	if err != nil {
		return err
	}
	channel, err := runtime.NewChannel(runtime.ChannelConfig{
		Slot:               "lintel",
		QuoinEndpoint:      config.QuoinRuntimeEndpoint,
		QuoinRuntimeCAFile: config.QuoinRuntimeCAFile,
		StateDirectory:     config.StateDirectory,
		BrowserSlots:       uint32(config.BrowserSlots),
		ChromiumRevision:   revision,
		Browser:            browserManager,
	})
	if err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			default:
			}
			if err := channel.RunConnect(runCtx, server); err != nil {
				sharedops.LogEvent("lintel", "info", "runtime.reconnect", err.Error())
			}
			select {
			case <-runCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	runErr := server.Run(runCtx)
	cancel()
	cleanupErr := browserManager.Close()
	if runErr != nil {
		return runErr
	}
	return cleanupErr
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
		Browser:            &browser.Manager{}, // registration does not start a browser
	})
	if err != nil {
		return err
	}
	return channel.RunRegister(ctx)
}

func chromiumRevision(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "chromium", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("detect Chromium revision: %w", err)
	}
	revision := strings.TrimSpace(string(output))
	if revision == "" {
		return "", fmt.Errorf("detect Chromium revision: empty output")
	}
	return revision, nil
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
