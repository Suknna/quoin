package artifact

// HTTP text-attachment staging (HTTP-FILE-001/002, DATA-ATTACH-001,
// DATA-ARTIFACT-003): the multipart body streams straight into a staging
// file (never buffered whole in memory), is validated for size, UTF-8 and
// NUL while streaming, then fsync + atomic rename into the content-addressed
// blob directory and one SQLite transaction registering source_materials →
// artifact_blobs → artifacts(kind='attachment', owner='source_material') →
// text_attachments. Attachment bodies are long-term retained
// (DATA-ARTIFACT-005) and never enter the runtime upload ledger — that
// ledger is the gRPC Runtime path's retry authority, not the HTTP one.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// ErrAttachmentTooLarge reports a body over the message-level attachment
// boundary (HTTP-FILE-002 → 413).
var ErrAttachmentTooLarge = errors.New("attachment body exceeds the message-level boundary")

// ErrAttachmentText reports a body that is not valid UTF-8 or carries a NUL
// byte (HTTP-FILE-002 → 422).
var ErrAttachmentText = errors.New("attachment body must be valid UTF-8 without NUL")

// AttachmentRecord is one durable text-attachment row joined with its
// artifact metadata (the TextAttachmentSummary projection source).
type AttachmentRecord struct {
	ID               int64
	ArtifactID       int64
	OriginalFilename string
	MediaType        string
	SizeBytes        int64
	SHA256           string
	BodyExpired      bool
	UploadedBy       int64
	CreatedAt        string
}

// StagedText is one streamed attachment body held in the staging
// directory: validated (size, UTF-8, NUL) and hashed but not yet sealed
// into the blob store. Seal renames it into its content address; Abort
// drops it. The split lets the caller decide idempotency (replay checks)
// before a body becomes a durable blob.
type StagedText struct {
	stagingPath string
	blobDir     string
	file        *os.File
	// SHA256Hex and SizeBytes are measured while streaming.
	SHA256Hex string
	SizeBytes int64
}

// Abort closes and removes the staging file (safe on an already-sealed
// body: the rename consumed the path).
func (staged *StagedText) Abort() {
	if staged.file != nil {
		staged.file.Close()
	}
	os.Remove(staged.stagingPath)
}

// Seal finishes the frozen durability order (fsync → rename → fsync of the
// parent; HTTP-FILE-001).
func (staged *StagedText) Seal() error {
	if err := staged.file.Sync(); err != nil {
		return err
	}
	if err := staged.file.Close(); err != nil {
		return err
	}
	staged.file = nil
	blobPath := filepath.Join(staged.blobDir, staged.SHA256Hex+".blob")
	if err := os.Rename(staged.stagingPath, blobPath); err != nil {
		return err
	}
	if directory, err := os.Open(filepath.Dir(blobPath)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// StageText streams one attachment body into a staging file with size,
// UTF-8 and NUL enforced while streaming (HTTP-FILE-001/002). No SQLite
// write and no blob rename happens here.
func (store *Store) StageText(reader io.Reader, limitBytes int64) (*StagedText, error) {
	if limitBytes <= 0 {
		return nil, errors.New("attachment limit must be positive")
	}
	staging, err := os.CreateTemp(filepath.Join(store.dir, "staging"), "attach-*.part")
	if err != nil {
		return nil, err
	}
	staged := &StagedText{stagingPath: staging.Name(), blobDir: filepath.Join(store.dir, "blobs"), file: staging}
	hash := sha256.New()
	var size int64
	var pending []byte // a possibly-incomplete UTF-8 rune tail
	buffer := make([]byte, 32*1024)
	validate := func(chunk []byte) error {
		if size+int64(len(chunk)) > limitBytes {
			return ErrAttachmentTooLarge
		}
		for _, b := range chunk {
			if b == 0 {
				return ErrAttachmentText
			}
		}
		// Validate UTF-8 across chunk boundaries: pair the previous tail
		// with the new chunk, decode whole runes, keep whatever may still
		// complete a rune (a truncated valid prefix is not an error yet).
		window := append(pending, chunk...)
		consumed := 0
		for consumed < len(window) {
			if !utf8.FullRune(window[consumed:]) {
				break // incomplete trailing rune: wait for the next chunk
			}
			symbol, size := utf8.DecodeRune(window[consumed:])
			if size <= 1 && symbol == utf8.RuneError {
				return ErrAttachmentText
			}
			consumed += size
		}
		pending = append(pending[:0], window[consumed:]...)
		if len(pending) > utf8.UTFMax {
			return ErrAttachmentText
		}
		return nil
	}
	for {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			if err := validate(buffer[:read]); err != nil {
				staged.Abort()
				return nil, err
			}
			if _, err := staging.Write(buffer[:read]); err != nil {
				staged.Abort()
				return nil, err
			}
			if _, err := hash.Write(buffer[:read]); err != nil {
				staged.Abort()
				return nil, err
			}
			size += int64(read)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			staged.Abort()
			return nil, readErr
		}
	}
	if len(pending) > 0 {
		staged.Abort()
		return nil, ErrAttachmentText
	}
	staged.SHA256Hex = hex.EncodeToString(hash.Sum(nil))
	staged.SizeBytes = size
	return staged, nil
}

// CommitAttachmentTransaction registers one staged body as a durable
// text attachment on the CALLER's already-open BEGIN IMMEDIATE transaction
// (DATA-ARTIFACT-003 ownership order: the source material exists first,
// the artifact closes onto it, then text_attachments completes the
// association — never a circular owner). The caller owns COMMIT/ROLLBACK
// so the staging replay re-check and this write serialize atomically.
func (store *Store) CommitAttachmentTransaction(ctx context.Context, conn *sql.Conn, principalID int64, filename, shaHex string, sizeBytes int64) (AttachmentRecord, error) {
	now := store.now().Format(time.RFC3339Nano)
	materialInsert, err := conn.ExecContext(ctx, `
		INSERT INTO source_materials(kind,digest,size_bytes,content,created_by,created_at)
		VALUES('text_attachment',?,?,NULL,?,?)`, shaHex, sizeBytes, principalID, now)
	if err != nil {
		return AttachmentRecord{}, err
	}
	materialID, err := materialInsert.LastInsertId()
	if err != nil {
		return AttachmentRecord{}, err
	}
	var blobID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM artifact_blobs WHERE sha256=?`, shaHex).Scan(&blobID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at)
			VALUES(?,?,?,?)`, shaHex, sizeBytes, "blobs/"+shaHex+".blob", now)
		if err != nil {
			return AttachmentRecord{}, err
		}
		blobID, err = insert.LastInsertId()
		if err != nil {
			return AttachmentRecord{}, err
		}
	} else if err != nil {
		return AttachmentRecord{}, err
	}
	artifactInsert, err := conn.ExecContext(ctx, `
		INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_by,created_at)
		VALUES(?,'attachment','text/plain',0,'long_term','source_material',?,NULL,?,?)`,
		blobID, materialID, principalID, now)
	if err != nil {
		return AttachmentRecord{}, err
	}
	artifactID, err := artifactInsert.LastInsertId()
	if err != nil {
		return AttachmentRecord{}, err
	}
	attachmentInsert, err := conn.ExecContext(ctx, `
		INSERT INTO text_attachments(source_material_id,artifact_id,original_filename,size_bytes,digest,uploaded_by,uploaded_at)
		VALUES(?,?,?,?,?,?,?)`, materialID, artifactID, filename, sizeBytes, shaHex, principalID, now)
	if err != nil {
		return AttachmentRecord{}, err
	}
	attachmentID, err := attachmentInsert.LastInsertId()
	if err != nil {
		return AttachmentRecord{}, err
	}
	return AttachmentRecord{
		ID: attachmentID, ArtifactID: artifactID, OriginalFilename: filename,
		MediaType: "text/plain", SizeBytes: sizeBytes, SHA256: shaHex,
		UploadedBy: principalID, CreatedAt: now,
	}, nil
}

// AttachmentByID loads one attachment with its artifact metadata
// (ErrNotFound when the row does not exist).
func (store *Store) AttachmentByID(ctx context.Context, attachmentID int64) (AttachmentRecord, error) {
	var record AttachmentRecord
	var uploadedBy sql.NullInt64
	err := store.db.QueryRowContext(ctx, `
		SELECT t.id, t.artifact_id, t.original_filename, a.media_type, t.size_bytes,
		       t.digest, a.body_expired, t.uploaded_by, t.uploaded_at
		FROM text_attachments t JOIN artifacts a ON a.id=t.artifact_id
		WHERE t.id=?`, attachmentID).
		Scan(&record.ID, &record.ArtifactID, &record.OriginalFilename, &record.MediaType,
			&record.SizeBytes, &record.SHA256, &record.BodyExpired, &uploadedBy, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachmentRecord{}, ErrNotFound
	}
	if err != nil {
		return AttachmentRecord{}, err
	}
	if uploadedBy.Valid {
		record.UploadedBy = uploadedBy.Int64
	}
	return record, nil
}

// SanitizeAttachmentFilename reduces one multipart filename to the frozen
// display/audit value (HTTP-FILE-002): basename only, control characters
// and CR/LF stripped, bounded to 512 runes, never empty. It never reaches a
// storage path or a content decision.
func SanitizeAttachmentFilename(raw string) string {
	base := filepath.Base(filepath.FromSlash(raw))
	var builder []rune
	for _, symbol := range base {
		if symbol < 0x20 || symbol == 0x7f || symbol == '\r' || symbol == '\n' {
			continue
		}
		builder = append(builder, symbol)
	}
	name := string(builder)
	if len([]rune(name)) > 512 {
		name = string([]rune(name)[:512])
	}
	if name == "" || name == "." || name == "/" || name == "\\" {
		return "attachment.txt"
	}
	return name
}
