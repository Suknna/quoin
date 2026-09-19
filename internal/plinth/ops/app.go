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
	var config contract.PlinthConfig
	if err := contract.DecodeFile(configPath, &config); err != nil {
		return err
	}
	if config.Component != "plinth" {
		return fmt.Errorf("configuration component must be plinth")
	}
	// The supervisor makes itself non-dumpable so the sandboxed worker can
	// never read its /proc environ/fd/mem/maps/ns (ARCH-WORKER-007).
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("set supervisor non-dumpable: %w", err)
	}
	if _, err := os.ReadFile(config.QuoinRuntimeCAFile); err != nil {
		return fmt.Errorf("read Quoin Runtime CA: %w", err)
	}
	if _, err := os.Stat(config.QuoinRuntimeClientCertificateFile); err != nil {
		return fmt.Errorf("read Plinth client certificate: %w", err)
	}
	if _, err := os.Stat(config.QuoinRuntimeClientPrivateKeyFile); err != nil {
		return fmt.Errorf("read Plinth client private key: %w", err)
	}
	lock, err := sharedops.AcquireDirectory(config.StateDirectory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := os.MkdirAll(config.WorkspaceDirectory, 0o700); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}
	server, err := sharedops.New("plinth", ":9090", sharedops.Starting)
	if err != nil {
		return err
	}
	return RunServe(ctx, config, server)
}
