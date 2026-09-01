package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrArchiveNotReady = errors.New("backup archive is not ready")
var ErrArchiveUnavailable = errors.New("backup archive is unavailable")

// PrepareArchive materializes a verified archive before HTTP commits headers.
// This turns every structural archive failure (including a corrupt member) into
// a normal problem response instead of a partial 200 response.
func (s *Service) PrepareArchive(ctx context.Context, id int64) (*os.File, func(), error) {
	value, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if value.Status != "succeeded" || value.ManifestSHA256 == nil {
		return nil, nil, ErrArchiveNotReady
	}
	root := filepath.Join(s.config.BackupDirectory, value.ID)
	entries, err := verifiedArchiveEntries(root, *value.ManifestSHA256)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrArchiveUnavailable, err)
	}
	file, err := os.CreateTemp(s.config.BackupDirectory, ".archive-*.tar")
	if err != nil {
		return nil, nil, &StorageFailure{Target: "backup", Cause: err}
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	archive := tar.NewWriter(file)
	for _, relative := range entries {
		path := filepath.Join(root, relative)
		info, statErr := os.Stat(path)
		if statErr != nil {
			err = statErr
			break
		}
		header, headerErr := tar.FileInfoHeader(info, "")
		if headerErr != nil {
			err = headerErr
			break
		}
		header.Name = relative
		if err = archive.WriteHeader(header); err != nil {
			break
		}
		input, openErr := os.Open(path)
		if openErr != nil {
			err = openErr
			break
		}
		_, err = io.Copy(archive, input)
		closeErr := input.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			break
		}
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if _, seekErr := file.Seek(0, io.SeekStart); err == nil {
		err = seekErr
	}
	if err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, &StorageFailure{Target: "backup", Cause: err}
	}
	return file, func() { _ = file.Close(); cleanup() }, nil
}

// WriteArchive is retained for non-HTTP consumers; HTTP uses PrepareArchive so
// all structural failures happen before response headers are committed.
func (s *Service) WriteArchive(ctx context.Context, id int64, writer io.Writer) error {
	file, cleanup, err := s.PrepareArchive(ctx, id)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = io.Copy(writer, file)
	return err
}

// RecordDownloadStart durably records a verified, authorized transfer before
// headers. It deliberately does not claim that bytes reached the client.
func (s *Service) RecordDownloadStart(ctx context.Context, actorID, backupID int64) error {
	return s.recordDownloadAudit(ctx, actorID, backupID, "backup.download_started", "success")
}

// RecordDownloadAudit is retained for internal callers that only need to
// authorize a transfer; HTTP must use start then completion.
func (s *Service) RecordDownloadAudit(ctx context.Context, actorID, backupID int64) error {
	return s.RecordDownloadStart(ctx, actorID, backupID)
}

// RecordDownloadCompletion appends a terminal immutable result after copy.
func (s *Service) RecordDownloadCompletion(ctx context.Context, actorID, backupID int64, transferErr error) error {
	action, outcome := "backup.download_completed", "success"
	if transferErr != nil {
		action, outcome = "backup.download_failed", "rejected"
	}
	return s.recordDownloadAudit(ctx, actorID, backupID, action, outcome)
}
func (s *Service) recordDownloadAudit(ctx context.Context, actorID, backupID int64, action, outcome string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,?,?,?,?)`, actorID, action, outcome, "backup", backupID, timestamp(s.now()))
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?,?,?)`, auditID, "backup", backupID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) recordRetentionAttempt(ctx context.Context, cleanupErr error) error {
	now := timestamp(s.now())
	if cleanupErr == nil {
		_, err := s.db.ExecContext(ctx, `INSERT INTO backup_retention_health(id,last_attempt_at,last_failure_at,error_detail) VALUES(1,?,NULL,NULL) ON CONFLICT(id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at,last_failure_at=NULL,error_detail=NULL`, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO backup_retention_health(id,last_attempt_at,last_failure_at,error_detail) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at,last_failure_at=excluded.last_failure_at,error_detail=excluded.error_detail`, now, now, truncate(cleanupErr.Error(), 4096))
	return err
}

func (s *Service) GC(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcLocked(ctx)
}

// gcLocked removes every succeeded Run outside the durable retention window.
// Selection is by immutable completed order, never by a readable manifest: a
// failed earlier deletion may already have removed the manifest while leaving
// descendants behind, and that physical residue must remain a retry target.
func (s *Service) gcLocked(ctx context.Context) (result error) {
	defer func() {
		// Retention health is its own durable outcome. Never clear a known
		// failure merely because this pass was cancelled or could not inspect
		// settings; only a complete, synced cleanup clears it.
		if recordErr := s.recordRetentionAttempt(context.Background(), result); recordErr != nil && result == nil {
			result = fmt.Errorf("record retention cleanup outcome: %w", recordErr)
		}
		s.refreshMetrics(context.Background())
	}()

	settings, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM backups WHERE status='succeeded' ORDER BY completed_at DESC, id DESC`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	keep := int(settings.RetentionCount)
	if keep > len(ids) {
		keep = len(ids)
	}
	expired := ids[keep:]
	for _, id := range expired {
		if err = s.removeAll(filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))); err != nil {
			return fmt.Errorf("remove expired backup %d: %w", id, err)
		}
	}
	// Even when a prior partial delete has already made a root disappear, a
	// successful retry must sync the parent before health can become healthy.
	if len(expired) > 0 {
		if err = syncDirectory(s.config.BackupDirectory); err != nil {
			return fmt.Errorf("sync expired backup cleanup: %w", err)
		}
	}
	return nil
}

func Verify(directory string) error { return VerifyWithManifestHash(directory, "") }

func VerifyWithManifestHash(directory, expectedManifestSHA256 string) error {
	_, err := verifiedArchiveEntries(directory, expectedManifestSHA256)
	return err
}

// verifiedArchiveEntries makes the manifest an exact archive set: duplicate,
// traversal, missing and extra files are all integrity failures.
func verifiedArchiveEntries(directory, expectedManifestSHA256 string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if expectedManifestSHA256 != "" {
		actual := sha256.Sum256(body)
		expected, decodeErr := hex.DecodeString(expectedManifestSHA256)
		if decodeErr != nil || len(expected) != sha256.Size || !equalBytes(actual[:], expected) {
			return nil, errors.New("backup manifest checksum mismatch")
		}
	}
	var value manifest
	if err = json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	if value.Version != 1 {
		return nil, errors.New("unsupported backup manifest version")
	}
	files := append([]fileEntry{value.Database}, value.Artifacts...)
	expected := map[string]fileEntry{"manifest.json": {Path: "manifest.json"}}
	for _, entry := range files {
		clean := filepath.Clean(entry.Path)
		if entry.Path == "" || filepath.IsAbs(entry.Path) || clean == "." || clean != entry.Path || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			return nil, fmt.Errorf("invalid backup manifest path: %q", entry.Path)
		}
		if _, found := expected[entry.Path]; found {
			return nil, fmt.Errorf("duplicate backup manifest path: %s", entry.Path)
		}
		expected[entry.Path] = entry
		sum, size, hashErr := hashFileSize(filepath.Join(directory, entry.Path))
		if hashErr != nil {
			return nil, hashErr
		}
		if sum != entry.SHA256 || size != entry.SizeBytes {
			return nil, fmt.Errorf("backup file checksum mismatch: %s", entry.Path)
		}
	}
	actual := map[string]bool{}
	err = filepath.Walk(directory, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(directory, path)
		if relErr != nil || strings.HasPrefix(relative, "..") {
			return errors.New("invalid backup archive path")
		}
		actual[filepath.ToSlash(relative)] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(actual) != len(expected) {
		return nil, errors.New("backup archive contains files absent from manifest")
	}
	entries := make([]string, 0, len(expected))
	for path := range expected {
		if !actual[path] {
			return nil, fmt.Errorf("backup manifest file missing: %s", path)
		}
		entries = append(entries, path)
	}
	sort.Strings(entries)
	return entries, nil
}

func equalBytes(first, second []byte) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
func hashFile(path string) (string, error) { sum, _, err := hashFileSize(path); return sum, err }
func hashFileSize(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
