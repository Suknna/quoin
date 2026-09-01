package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"golang.org/x/sys/unix"
)

var ErrInsufficientCapacity = errors.New("backup target has insufficient capacity")

// StorageFailure identifies the durable storage target that prevented a backup
// from proceeding. Its stable outer error code is persisted as
// storage_unavailable; details remain bounded in the Backup Run.
type StorageFailure struct {
	Target string
	Cause  error
}

func (err *StorageFailure) Error() string {
	return fmt.Sprintf("%s storage: %v", err.Target, err.Cause)
}

func (err *StorageFailure) Unwrap() error { return err.Cause }

type preflightRequirement struct {
	DatabaseBytes     uint64
	ArtifactBytes     uint64
	ArtifactFileBytes []uint64
	ManifestBytes     uint64
	BackupBytes       uint64
}

type capacityFunc func(directory string) (available, blockSize uint64, err error)

func filesystemCapacity(directory string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(directory, &stat); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stat.Frsize)
	if blockSize == 0 {
		blockSize = uint64(stat.Bsize)
	}
	if blockSize == 0 {
		return 0, 0, errors.New("filesystem reports a zero block size")
	}
	if uint64(stat.Bavail) > math.MaxUint64/blockSize {
		return math.MaxUint64, blockSize, nil
	}
	return uint64(stat.Bavail) * blockSize, blockSize, nil
}

// preflightRequirement computes the first, conservative capacity gate from
// the logical SQLite page image, every currently referenced artifact body, and
// the actual serialized manifest. The post-snapshot gate measures the actual
// VACUUM output and frozen Artifact set before copying any body.
func (s *Service) preflightRequirement(ctx context.Context) (preflightRequirement, error) {
	stat, err := os.Stat(filepath.Join(s.config.DataDirectory, "quoin.db"))
	if err != nil {
		return preflightRequirement{}, &StorageFailure{Target: "data", Cause: err}
	}
	if stat.IsDir() {
		return preflightRequirement{}, &StorageFailure{Target: "data", Cause: errors.New("quoin.db is a directory")}
	}

	entries := make([]fileEntry, 0)
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.sha256,b.size_bytes
		FROM artifact_blobs b
		WHERE EXISTS (
			SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.body_expired=0
		)
		ORDER BY b.sha256`)
	if err != nil {
		return preflightRequirement{}, err
	}
	for rows.Next() {
		var sha string
		var size int64
		if err := rows.Scan(&sha, &size); err != nil {
			rows.Close()
			return preflightRequirement{}, err
		}
		if size < 0 {
			rows.Close()
			return preflightRequirement{}, errors.New("artifact size is negative")
		}
		entries = append(entries, fileEntry{Path: "artifacts/" + sha + ".blob", SHA256: sha, SizeBytes: size})
	}
	if err := rows.Close(); err != nil {
		return preflightRequirement{}, err
	}

	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return preflightRequirement{}, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return preflightRequirement{}, err
	}
	if pageCount < 0 || pageSize <= 0 || uint64(pageCount) > math.MaxUint64/uint64(pageSize) {
		return preflightRequirement{}, errors.New("database snapshot size overflows uint64")
	}
	// A VACUUM INTO snapshot cannot exceed the current logical page image. This
	// upper bound avoids underestimating a WAL-resident change before the second,
	// exact post-snapshot gate below measures the file actually produced.
	databaseBytes := uint64(pageCount) * uint64(pageSize)
	body, err := json.Marshal(manifest{
		Version:   1,
		Database:  fileEntry{Path: "quoin.db", SHA256: strings.Repeat("0", 64), SizeBytes: int64(databaseBytes)},
		Artifacts: entries,
		CreatedAt: timestamp(s.now()),
	})
	if err != nil {
		return preflightRequirement{}, err
	}
	requirement := preflightRequirement{DatabaseBytes: databaseBytes, ManifestBytes: uint64(len(body) + 1)}
	for _, entry := range entries {
		size := uint64(entry.SizeBytes)
		requirement.ArtifactBytes += size
		requirement.ArtifactFileBytes = append(requirement.ArtifactFileBytes, size)
	}
	requirement.BackupBytes = requirement.DatabaseBytes + requirement.ArtifactBytes + requirement.ManifestBytes
	return requirement, nil
}

func allocatedBytes(size, blockSize uint64) (uint64, error) {
	if blockSize == 0 {
		return 0, errors.New("filesystem reports a zero block size")
	}
	if size == 0 {
		return 0, nil
	}
	blocks := (size-1)/blockSize + 1
	if blocks > math.MaxUint64/blockSize {
		return 0, errors.New("required storage size overflows uint64")
	}
	return blocks * blockSize, nil
}

func (s *Service) preflight(ctx context.Context) error {
	requirement, err := s.preflightRequirement(ctx)
	if err != nil {
		s.recordStorageFailure(err)
		return err
	}
	if err := s.checkTarget("data", s.config.DataDirectory, 0); err != nil {
		return err
	}
	available, blockSize, err := s.capacity(s.config.BackupDirectory)
	if err != nil {
		failure := &StorageFailure{Target: "backup", Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	needed := uint64(0)
	for _, size := range append(append([]uint64{requirement.DatabaseBytes}, requirement.ArtifactFileBytes...), requirement.ManifestBytes, uint64(len("quoin\\n"))) {
		allocated, allocationErr := allocatedBytes(size, blockSize)
		if allocationErr != nil {
			return allocationErr
		}
		if needed > math.MaxUint64-allocated {
			return errors.New("required backup storage size overflows uint64")
		}
		needed += allocated
	}
	if available < needed {
		failure := &StorageFailure{Target: "backup", Cause: fmt.Errorf("%w: need %d bytes, have %d", ErrInsufficientCapacity, needed, available)}
		s.recordStorageFailure(failure)
		return failure
	}
	if err := s.probeDirectory(s.config.BackupDirectory); err != nil {
		failure := &StorageFailure{Target: "backup", Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	s.recordStorageWritable("backup")
	return nil
}

// preflightCopiedSet is the exact second capacity gate: SQLite has completed
// VACUUM INTO and the fixed snapshot has enumerated every referenced blob, but
// no artifact body has yet been copied. Available space therefore excludes the
// actual snapshot already written and needs only the exact remaining files.
func (s *Service) preflightCopiedSet(databaseBytes int64, files []artifact.SnapshotFile) error {
	if databaseBytes < 0 {
		return errors.New("snapshot database size is negative")
	}
	entries := make([]fileEntry, 0, len(files))
	for _, file := range files {
		if file.SizeBytes < 0 {
			return errors.New("snapshot artifact size is negative")
		}
		entries = append(entries, fileEntry{Path: "artifacts/" + file.SHA256 + ".blob", SHA256: file.SHA256, SizeBytes: file.SizeBytes})
	}
	body, err := json.Marshal(manifest{
		Version: 1, Database: fileEntry{Path: "quoin.db", SHA256: strings.Repeat("0", 64), SizeBytes: databaseBytes},
		Artifacts: entries, CreatedAt: timestamp(s.now()),
	})
	if err != nil {
		return err
	}
	available, blockSize, err := s.capacity(s.config.BackupDirectory)
	if err != nil {
		failure := &StorageFailure{Target: "backup", Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	needed := uint64(0)
	for _, file := range files {
		allocated, allocationErr := allocatedBytes(uint64(file.SizeBytes), blockSize)
		if allocationErr != nil {
			return allocationErr
		}
		if needed > math.MaxUint64-allocated {
			return errors.New("required backup storage size overflows uint64")
		}
		needed += allocated
	}
	manifestBytes, err := allocatedBytes(uint64(len(body)+1), blockSize)
	if err != nil {
		return err
	}
	probeBytes, err := allocatedBytes(uint64(len("quoin\n")), blockSize)
	if err != nil {
		return err
	}
	if needed > math.MaxUint64-manifestBytes-probeBytes {
		return errors.New("required backup storage size overflows uint64")
	}
	needed += manifestBytes + probeBytes
	if available < needed {
		failure := &StorageFailure{Target: "backup", Cause: fmt.Errorf("%w: need %d bytes, have %d", ErrInsufficientCapacity, needed, available)}
		s.recordStorageFailure(failure)
		return failure
	}
	if err := s.probeDirectory(s.config.BackupDirectory); err != nil {
		failure := &StorageFailure{Target: "backup", Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	s.recordStorageWritable("backup")
	return nil
}

func (s *Service) checkTarget(target, directory string, required uint64) error {
	available, _, err := s.capacity(directory)
	if err != nil {
		failure := &StorageFailure{Target: target, Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	if available < required {
		failure := &StorageFailure{Target: target, Cause: fmt.Errorf("%w: need %d bytes, have %d", ErrInsufficientCapacity, required, available)}
		s.recordStorageFailure(failure)
		return failure
	}
	if err := s.probeDirectory(directory); err != nil {
		failure := &StorageFailure{Target: target, Cause: err}
		s.recordStorageFailure(failure)
		return failure
	}
	s.recordStorageWritable(target)
	return nil
}

func durableDirectoryProbe(directory string) error {
	stamp := fmt.Sprintf(".quoin-storage-probe-%d", time.Now().UnixNano())
	temporary := filepath.Join(directory, stamp+".tmp")
	final := filepath.Join(directory, stamp)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write([]byte("quoin\n")); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = os.Rename(temporary, final); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = syncDirectory(directory); err != nil {
		_ = os.Remove(final)
		return err
	}
	if err = os.Remove(final); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func (s *Service) recordStorageFailure(err error) {
	var failure *StorageFailure
	if !errors.As(err, &failure) || s.metrics == nil || s.metrics.Storage == nil {
		return
	}
	s.metrics.Storage.Set(failure.Target, false)
}

func (s *Service) recordStorageWritable(target string) {
	if s.metrics != nil && s.metrics.Storage != nil {
		s.metrics.Storage.Set(target, true)
	}
}

func (s *Service) backupWriteFailure(err error) error {
	failure := &StorageFailure{Target: "backup", Cause: err}
	s.recordStorageFailure(failure)
	return failure
}

func (s *Service) classifyBackupFailure(err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.EROFS) {
		return s.backupWriteFailure(err)
	}
	return err
}
