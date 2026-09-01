package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

func (s *Service) GC(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcLocked(ctx)
}
func (s *Service) gcLocked(ctx context.Context) error {
	settings, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,manifest_sha256 FROM backups WHERE status='succeeded'`)
	if err != nil {
		return err
	}
	type retainedBackup struct {
		id       int64
		modified time.Time
	}
	var valid []retainedBackup
	for rows.Next() {
		var id int64
		var manifestSHA sql.NullString
		if err = rows.Scan(&id, &manifestSHA); err != nil {
			rows.Close()
			return err
		}
		root := filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", id))
		if !manifestSHA.Valid || VerifyWithManifestHash(root, manifestSHA.String) != nil {
			continue
		}
		info, statErr := os.Stat(filepath.Join(root, "manifest.json"))
		if statErr == nil {
			valid = append(valid, retainedBackup{id: id, modified: info.ModTime()})
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].modified.After(valid[j].modified) })
	keep := int(settings.RetentionCount)
	if keep > len(valid) {
		keep = len(valid)
	}
	for _, backup := range valid[keep:] {
		if err = os.RemoveAll(filepath.Join(s.config.BackupDirectory, fmt.Sprintf("%d", backup.id))); err != nil {
			return err
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
