package ops

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type DirectoryLock struct {
	file *os.File
}

func AcquireDirectory(path string) (*DirectoryLock, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	lockPath := filepath.Join(path, ".quoin.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("state directory is already owned: %w", err)
	}
	if err := probeWritable(path); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return &DirectoryLock{file: file}, nil
}

func (lock *DirectoryLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func probeWritable(path string) error {
	file, err := os.CreateTemp(path, ".write-probe-")
	if err != nil {
		return fmt.Errorf("state directory is not writable: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.WriteString("quoin-storage-probe\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write state probe: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync state probe: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
