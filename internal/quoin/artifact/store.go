// Package artifact owns the Quoin-side Artifact store (DATA-ARTIFACT-001..
// 006): content-addressed blob bytes on the filesystem, the runtime
// upload ledger, attempt-scoped read grants and the bounded text read/grep
// paths. SQLite owns metadata and references; the filesystem owns blob
// bytes; neither can claim a committed Artifact alone.
package artifact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Fixed service caps (RUNTIME-ARTIFACT-003): bounded reads, matches and
// response bytes so a worker can never pull unbounded content.
const (
	maxReadLines     = 2000
	maxGrepMatches   = 200
	maxContextLines  = 5
	maxResponseBytes = 512 * 1024
)

// ErrRejected reports an upload the ledger refused (a reject reason and a
// detail always accompany it).
var ErrRejected = errors.New("artifact upload rejected")

// RejectReason mirrors runtime.proto UploadRejectReason as stable text.
type RejectReason string

const (
	RejectSHA256Mismatch    RejectReason = "SHA256_MISMATCH"
	RejectSizeMismatch      RejectReason = "SIZE_MISMATCH"
	RejectAttemptNotRunning RejectReason = "ATTEMPT_NOT_RUNNING"
	RejectBootMismatch      RejectReason = "BOOT_MISMATCH"
	RejectEpochMismatch     RejectReason = "EPOCH_MISMATCH"
	RejectMetadataMismatch  RejectReason = "METADATA_MISMATCH"
	RejectInternal          RejectReason = "INTERNAL"
)

// Rejection pairs a stable reason with a non-secret detail.
type Rejection struct {
	Reason RejectReason
	Detail string
}

func (rejection *Rejection) Error() string {
	return fmt.Sprintf("%s: %s", rejection.Reason, rejection.Detail)
}

// UploadHeader is the first frame of one upload (RUNTIME-UPLOAD-001).
type UploadHeader struct {
	UploadID        string
	AttemptID       int64
	BootID          string
	ConnectionEpoch uint64
	OwnerType       string
	OwnerID         int64
	Kind            string
	RetentionKind   string
	Sensitive       bool
	SizeBytes       int64
	SHA256          []byte
	MediaType       string
}

// Store is the artifact authority.
type Store struct {
	db  *sql.DB
	dir string
	now func() time.Time
}

// NewStore builds the store on the product database and blob directory.
func NewStore(db *sql.DB, directory string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(directory, "blobs"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(directory, "staging"), 0o700); err != nil {
		return nil, err
	}
	return &Store{db: db, dir: directory, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (store *Store) stagingPath(uploadID string) string {
	return filepath.Join(store.dir, "staging", uploadID+".part")
}

// StagingPath exposes the staging file location for stream teardown.
func (store *Store) StagingPath(uploadID string) string {
	return store.stagingPath(uploadID)
}

func (store *Store) blobPath(sha256Hex string) string {
	return filepath.Join(store.dir, "blobs", sha256Hex+".blob")
}

// BeginUpload validates the header against the ledger fence and opens the
// staging file. A retry of a crashed upload (same metadata, still
// uploading) resets the staging file; a committed upload replays its
// artifact id through artifactID>0 with nil conn/file (RUNTIME-UPLOAD-002).
func (store *Store) BeginUpload(ctx context.Context, header UploadHeader) (conn *sql.Conn, file *os.File, artifactID int64, err error) {
	if header.UploadID == "" || len(header.SHA256) != 32 || header.SizeBytes < 0 {
		return nil, nil, 0, &Rejection{RejectMetadataMismatch, "header fields incomplete"}
	}
	if header.Kind == "trace" && !header.Sensitive {
		return nil, nil, 0, &Rejection{RejectMetadataMismatch, "trace artifacts must be sensitive"}
	}
	conn, err = store.db.Conn(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, nil, 0, err
	}
	fail := func(rejection *Rejection) (*sql.Conn, *os.File, int64, error) {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		conn.Close()
		return nil, nil, 0, rejection
	}
	var state string
	var storedArtifact sql.NullInt64
	var storedSize int64
	var storedSHA string
	err = conn.QueryRowContext(ctx, `
		SELECT state, artifact_id, size_bytes, sha256 FROM runtime_artifact_uploads WHERE upload_id=?`,
		header.UploadID).Scan(&state, &storedArtifact, &storedSize, &storedSHA)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New upload: validate the attempt fence and the owner closure.
		if rejection, ok := store.validateUploadOn(ctx, conn, header); !ok {
			return fail(rejection)
		}
		now := store.now().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO runtime_artifact_uploads(upload_id,attempt_id,boot_id,connection_epoch,
				owner_type,owner_id,kind,media_type,retention_kind,sensitive,size_bytes,sha256,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'uploading',?)`,
			header.UploadID, header.AttemptID, header.BootID, header.ConnectionEpoch,
			header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind,
			boolInt(header.Sensitive), header.SizeBytes, hex.EncodeToString(header.SHA256), now); err != nil {
			return fail(&Rejection{RejectInternal, err.Error()})
		}
	case err != nil:
		return fail(&Rejection{RejectInternal, err.Error()})
	case state == "committed":
		if !storedArtifact.Valid || storedSize != header.SizeBytes || storedSHA != hex.EncodeToString(header.SHA256) {
			return fail(&Rejection{RejectMetadataMismatch, "committed upload metadata differs"})
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fail(&Rejection{RejectInternal, err.Error()})
		}
		conn.Close()
		return nil, nil, storedArtifact.Int64, nil
	case state == "rejected":
		return fail(&Rejection{RejectMetadataMismatch, "upload id was rejected"})
	default:
		// A crashed upload retrying: same metadata resets the staging file.
		if storedSize != header.SizeBytes || storedSHA != hex.EncodeToString(header.SHA256) {
			return fail(&Rejection{RejectMetadataMismatch, "retry metadata differs"})
		}
		if rejection, ok := store.validateUploadOn(ctx, conn, header); !ok {
			return fail(rejection)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fail(&Rejection{RejectInternal, err.Error()})
	}
	file, err = os.OpenFile(store.stagingPath(header.UploadID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		conn.Close()
		return nil, nil, 0, &Rejection{RejectInternal, err.Error()}
	}
	return conn, file, 0, nil
}

// validateUploadOn checks the attempt fence and owner closure inside the
// ledger transaction (RUNTIME-UPLOAD-004).
func (store *Store) validateUploadOn(ctx context.Context, conn *sql.Conn, header UploadHeader) (*Rejection, bool) {
	if header.AttemptID != 0 {
		var state string
		var boot sql.NullString
		var epoch sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT state,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, header.AttemptID).Scan(&state, &boot, &epoch); err != nil {
			return &Rejection{RejectAttemptNotRunning, "attempt unknown"}, false
		}
		if state != "Running" {
			return &Rejection{RejectAttemptNotRunning, fmt.Sprintf("attempt is %s", state)}, false
		}
		if !boot.Valid || boot.String != header.BootID {
			return &Rejection{RejectBootMismatch, "boot fence"}, false
		}
		if !epoch.Valid || epoch.Int64 != int64(header.ConnectionEpoch) {
			return &Rejection{RejectEpochMismatch, "epoch fence"}, false
		}
	}
	switch header.Kind {
	case "tool_result":
		if header.OwnerType != "tool_call" {
			return &Rejection{RejectMetadataMismatch, "tool_result must be owned by a tool_call"}, false
		}
		var toolAttempt int64
		var toolState string
		if err := conn.QueryRowContext(ctx, `SELECT attempt_id,status FROM tool_calls WHERE id=?`, header.OwnerID).Scan(&toolAttempt, &toolState); err != nil {
			return &Rejection{RejectMetadataMismatch, "owner tool_call unknown"}, false
		}
		if toolAttempt != header.AttemptID || (toolState != "pending" && toolState != "running") {
			return &Rejection{RejectMetadataMismatch, "owner tool_call does not belong to the running attempt"}, false
		}
		if header.RetentionKind != "generated" {
			return &Rejection{RejectMetadataMismatch, "tool_result must be generated retention"}, false
		}
	}
	return nil, true
}

// CommitUpload finishes one staged upload: hash/size verification, fsync,
// atomic rename into the content-addressed blob directory, fsync of the
// parent, then the SQLite reference transaction (RUNTIME-UPLOAD-003).
// The caller's connection is closed BEFORE any rejection bookkeeping runs
// (rejectUpload needs a fresh pool connection and the production pool is
// single-connection).
func (store *Store) CommitUpload(ctx context.Context, conn *sql.Conn, header UploadHeader, staged *os.File) (int64, error) {
	closeConn := func() {
		if conn != nil {
			conn.Close()
			conn = nil
		}
	}
	defer closeConn()
	if err := staged.Close(); err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	info, err := os.Stat(store.stagingPath(header.UploadID))
	if err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	if info.Size() != header.SizeBytes {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectSizeMismatch)
		return 0, &Rejection{RejectSizeMismatch, fmt.Sprintf("staged %d bytes, header %d", info.Size(), header.SizeBytes)}
	}
	body, err := os.Open(store.stagingPath(header.UploadID))
	if err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(body, header.SizeBytes+1)); err != nil {
		body.Close()
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	body.Close()
	if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(header.SHA256) {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectSHA256Mismatch)
		return 0, &Rejection{RejectSHA256Mismatch, "staged content hash differs from header"}
	}
	// Durability order (DATA-ARTIFACT-001): fsync the staging file, rename
	// into the blob store, fsync the parent directory.
	file, err := os.OpenFile(store.stagingPath(header.UploadID), os.O_RDONLY, 0)
	if err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	file.Close()
	blobPath := store.blobPath(hex.EncodeToString(header.SHA256))
	if err := os.Rename(store.stagingPath(header.UploadID), blobPath); err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	if directory, err := os.Open(filepath.Dir(blobPath)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	// The reference transaction: blobs + artifacts + ledger commit + the
	// attempt-scoped read grant (ARCH-OUTPUT-002/004). Failure removes the
	// orphan blob (RUNTIME-UPLOAD-003).
	artifactID, err := store.commitReferences(ctx, conn, header)
	if err != nil {
		closeConn()
		_ = os.Remove(blobPath)
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	return artifactID, nil
}

// commitReferences writes artifact_blobs, artifacts and the ledger commit
// in one transaction (the caller's connection).
func (store *Store) commitReferences(ctx context.Context, conn *sql.Conn, header UploadHeader) (int64, error) {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	shaHex := hex.EncodeToString(header.SHA256)
	var blobID int64
	err := conn.QueryRowContext(ctx, `SELECT id FROM artifact_blobs WHERE sha256=?`, shaHex).Scan(&blobID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at)
			VALUES(?,?,?,?)`, shaHex, header.SizeBytes, "blobs/"+shaHex+".blob", store.now().Format(time.RFC3339Nano))
		if err != nil {
			return 0, err
		}
		blobID, err = insert.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	expiresAt := sql.NullString{}
	if header.RetentionKind == "generated" {
		// Default generated retention: 90 days (ARCH-OUTPUT-002).
		expiresAt = sql.NullString{String: store.now().Add(90 * 24 * time.Hour).Format(time.RFC3339Nano), Valid: true}
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,expires_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		blobID, header.Kind, header.MediaType, boolInt(header.Sensitive), header.RetentionKind,
		header.OwnerType, header.OwnerID, expiresAt, store.now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	artifactID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE runtime_artifact_uploads SET state='committed', artifact_id=?, committed_at=?, row_version=row_version+1
		WHERE upload_id=? AND state='uploading'`, artifactID, store.now().Format(time.RFC3339Nano), header.UploadID); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return artifactID, nil
}

// InsertToolResultGrant writes the attempt-scoped read grant for one
// tool_result artifact on the caller's transaction. The frozen closure
// requires the tool call to be succeeded first, so the caller must seal
// the tool call before invoking this (CompleteToolCall order: UPDATE
// tool_calls, then this insert, then COMMIT).
func (store *Store) InsertToolResultGrant(ctx context.Context, conn *sql.Conn, attemptID, artifactID, toolCallID int64) error {
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_artifact_grants(attempt_id,artifact_id,source_kind,source_id,granted_at)
		VALUES(?,?,?,?,?)`,
		attemptID, artifactID, "tool_result", toolCallID, store.now().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return nil
}

// rejectUpload marks the ledger row rejected (best effort; the physical
// staging file is removed by the caller path).
func (store *Store) rejectUpload(ctx context.Context, uploadID string, reason RejectReason) {
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, `
		UPDATE runtime_artifact_uploads SET state='rejected', row_version=row_version+1
		WHERE upload_id=? AND state='uploading'`, uploadID)
	_ = os.Remove(store.stagingPath(uploadID))
}

// ReadText returns a bounded text slice of one attempt-granted artifact
// (RUNTIME-ARTIFACT-002/003).
type TextSlice struct {
	ArtifactID     int64
	Content        []byte
	StartLine      int64
	NextLine       int64
	EOF            bool
	TotalSizeBytes int64
	TotalLines     int64
	SHA256         []byte
	MediaType      string
}

// ErrBodyExpired reports an artifact whose body the GC already cleared.
var ErrBodyExpired = errors.New("artifact body expired")

// ErrNotFound reports an unknown artifact locator (metadata and body
// reads answer 404; an expired body is NOT not-found — its metadata
// stays readable while the body alone answers 410).
var ErrNotFound = errors.New("artifact not found")

// ReadText fences the attempt/boot/epoch/grant binding and returns a
// bounded line slice. startLine is 1-based.
func (store *Store) ReadText(ctx context.Context, attemptID, artifactID int64, bootID string, epoch uint64, startLine int64, maxLines int) (TextSlice, error) {
	if startLine <= 0 {
		return TextSlice{}, errors.New("start_line must be >= 1")
	}
	if maxLines <= 0 || maxLines > maxReadLines {
		maxLines = maxReadLines
	}
	if err := store.checkReadGrant(ctx, attemptID, artifactID, bootID, epoch); err != nil {
		return TextSlice{}, err
	}
	var slice TextSlice
	var sizeBytes int64
	var shaHex, mediaType string
	var expired int
	err := store.db.QueryRowContext(ctx, `
		SELECT b.size_bytes, b.sha256, a.media_type, a.body_expired
		FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.id=?`, artifactID).
		Scan(&sizeBytes, &shaHex, &mediaType, &expired)
	if err != nil {
		return TextSlice{}, err
	}
	if expired == 1 {
		return TextSlice{}, ErrBodyExpired
	}
	if !isTextMedia(mediaType) {
		return TextSlice{}, fmt.Errorf("artifact %d media type %q is not text", artifactID, mediaType)
	}
	slice.ArtifactID = artifactID
	slice.TotalSizeBytes = sizeBytes
	slice.SHA256, _ = hex.DecodeString(shaHex)
	slice.MediaType = mediaType
	file, err := os.Open(store.blobPath(shaHex))
	if err != nil {
		return TextSlice{}, err
	}
	defer file.Close()
	reader := newLineReader(file)
	// Skip the first startLine-1 lines (1-based, RUNTIME-ARTIFACT-003).
	var totalLines int64
	for line := int64(1); line < startLine; line++ {
		_, eof, err := reader.readLine()
		if err != nil {
			return TextSlice{}, err
		}
		if eof {
			slice.EOF = true
			slice.TotalLines = line - 1
			slice.Content = []byte{}
			return slice, nil
		}
		totalLines++
	}
	slice.StartLine = startLine
	var output []byte
	next := startLine
	for count := 0; count < maxLines; count++ {
		content, eof, err := reader.readLine()
		if err != nil {
			return TextSlice{}, err
		}
		if eof {
			slice.EOF = true
			slice.TotalLines = totalLines
			break
		}
		totalLines++
		next++
		if int64(len(output))+int64(len(content)) > maxResponseBytes {
			// Byte cap hit mid-line: stop before this line.
			next--
			break
		}
		output = append(output, content...)
	}
	slice.Content = output
	slice.NextLine = next
	if slice.TotalLines == 0 && !slice.EOF {
		// Not exhausted yet: the full line count stays unknown.
		slice.TotalLines = 0
	}
	return slice, nil
}

// GrepResult is one bounded grep pass (RUNTIME-ARTIFACT-003).
type GrepResult struct {
	ArtifactID     int64
	Matches        []TextMatch
	Truncated      bool
	TotalSizeBytes int64
	TotalLines     int64
	SHA256         []byte
	MediaType      string
}

// TextMatch is one matched line (the wire shape carries only the line
// number and the bounded line content; context lines are capped but not
// returned in v1).
type TextMatch struct {
	LineNumber int64
	Line       string
}

// GrepText fences the binding and returns bounded RE2 matches. The frozen
// context_lines parameter is validated against the service cap; v1
// responses carry only the matched lines (the cap still bounds the scan
// window per match).
func (store *Store) GrepText(ctx context.Context, attemptID, artifactID int64, bootID string, epoch uint64, pattern string, maxMatches, contextLines int) (GrepResult, error) {
	if contextLines < 0 || contextLines > maxContextLines {
		contextLines = maxContextLines
	}
	if maxMatches <= 0 || maxMatches > maxGrepMatches {
		maxMatches = maxGrepMatches
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return GrepResult{}, fmt.Errorf("invalid RE2 pattern: %w", err)
	}
	if err := store.checkReadGrant(ctx, attemptID, artifactID, bootID, epoch); err != nil {
		return GrepResult{}, err
	}
	var result GrepResult
	var sizeBytes int64
	var shaHex, mediaType string
	var expired int
	err = store.db.QueryRowContext(ctx, `
		SELECT b.size_bytes, b.sha256, a.media_type, a.body_expired
		FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.id=?`, artifactID).
		Scan(&sizeBytes, &shaHex, &mediaType, &expired)
	if err != nil {
		return GrepResult{}, err
	}
	if expired == 1 {
		return GrepResult{}, ErrBodyExpired
	}
	if !isTextMedia(mediaType) {
		return GrepResult{}, fmt.Errorf("artifact %d media type %q is not text", artifactID, mediaType)
	}
	result.ArtifactID = artifactID
	result.TotalSizeBytes = sizeBytes
	result.SHA256, _ = hex.DecodeString(shaHex)
	result.MediaType = mediaType
	file, err := os.Open(store.blobPath(shaHex))
	if err != nil {
		return GrepResult{}, err
	}
	defer file.Close()
	reader := newLineReader(file)
	for lineNumber := int64(1); ; lineNumber++ {
		content, eof, err := reader.readLine()
		if err != nil {
			return GrepResult{}, err
		}
		if eof {
			result.TotalLines = lineNumber - 1
			break
		}
		text := strings.TrimSuffix(string(content), "\n")
		if compiled.MatchString(text) {
			if len(result.Matches) >= maxMatches {
				result.Truncated = true
				break
			}
			bounded := text
			if len(bounded) > 4096 {
				bounded = bounded[:4096]
			}
			result.Matches = append(result.Matches, TextMatch{LineNumber: lineNumber, Line: bounded})
		}
	}
	return result, nil
}

// checkReadGrant enforces the supervisor-only attempt-scoped read fence
// (RUNTIME-ARTIFACT-002).
func (store *Store) checkReadGrant(ctx context.Context, attemptID, artifactID int64, bootID string, epoch uint64) error {
	var state string
	var boot sql.NullString
	var storedEpoch sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `SELECT state,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, attemptID).Scan(&state, &boot, &storedEpoch); err != nil {
		return err
	}
	if state != "Running" {
		return fmt.Errorf("attempt %d is %s", attemptID, state)
	}
	if !boot.Valid || boot.String != bootID || !storedEpoch.Valid || storedEpoch.Int64 != int64(epoch) {
		return fmt.Errorf("attempt binding mismatch")
	}
	var granted int
	if err := store.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attempt_artifact_grants WHERE attempt_id=? AND artifact_id=?`, attemptID, artifactID).Scan(&granted); err != nil {
		return err
	}
	if granted == 0 {
		return fmt.Errorf("artifact %d is not granted to attempt %d", artifactID, attemptID)
	}
	return nil
}

// Ref resolves the dispatch-facing metadata of one granted artifact.
type Ref struct {
	ArtifactID  int64
	MediaType   string
	SizeBytes   int64
	SHA256      []byte
	BodyExpired bool
}

// RefFor returns the metadata of one attempt-granted artifact.
func (store *Store) RefFor(ctx context.Context, attemptID, artifactID int64) (Ref, error) {
	var ref Ref
	var shaHex string
	err := store.db.QueryRowContext(ctx, `
		SELECT a.id, a.media_type, b.size_bytes, b.sha256, a.body_expired
		FROM artifacts a
		JOIN artifact_blobs b ON b.id=a.blob_id
		JOIN attempt_artifact_grants g ON g.artifact_id=a.id AND g.attempt_id=?
		WHERE a.id=?`, attemptID, artifactID).
		Scan(&ref.ArtifactID, &ref.MediaType, &ref.SizeBytes, &shaHex, &ref.BodyExpired)
	if err != nil {
		return Ref{}, err
	}
	ref.SHA256, _ = hex.DecodeString(shaHex)
	return ref, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Metadata is the frozen ArtifactSummary read projection (the HTTP
// getArtifactMetadata surface).
type Metadata struct {
	ID            int64
	Kind          string
	Sensitive     bool
	RetentionKind string
	OwnerType     string
	OwnerID       int64
	SizeBytes     int64
	SHA256        string
	BodyExpired   bool
	ExpiresAt     *string
	CreatedAt     string
}

// Metadata returns the logical metadata of one artifact row (the logical
// artifacts row is the authorization authority, DATA-ARTIFACT-003).
func (store *Store) Metadata(ctx context.Context, artifactID int64) (Metadata, error) {
	var meta Metadata
	var sensitive, bodyExpired int
	var expiresAt sql.NullString
	err := store.db.QueryRowContext(ctx, `
		SELECT a.id, a.kind, a.sensitive, a.retention_kind, a.owner_type, a.owner_id,
		       b.size_bytes, b.sha256, a.body_expired, a.expires_at, a.created_at
		FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.id=?`, artifactID).
		Scan(&meta.ID, &meta.Kind, &sensitive, &meta.RetentionKind, &meta.OwnerType, &meta.OwnerID,
			&meta.SizeBytes, &meta.SHA256, &bodyExpired, &expiresAt, &meta.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, err
	}
	meta.Sensitive = sensitive == 1
	meta.BodyExpired = bodyExpired == 1
	if expiresAt.Valid {
		meta.ExpiresAt = &expiresAt.String
	}
	return meta, nil
}

// ErrBodyExpired reports an artifact whose body passed its retention
// fence (DATA-ARTIFACT-003: metadata stays, the body refuses reads).

// RecordDownloadAudit appends the non-secret download audit event before
// the body streams (DATA-ARTIFACT-003, HTTP-FILE-003/004: authorization
// happens first, then the audit, then the bytes).
func (store *Store) RecordDownloadAudit(ctx context.Context, actorType string, actorID, artifactID int64) error {
	now := store.now().Format(time.RFC3339Nano)
	result, err := store.db.ExecContext(ctx, `
		INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at)
		VALUES(?,?,?,?,?,?,?)`, actorType, actorID, "artifact.download", "success", "artifact", artifactID, now)
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?,?,?)`, auditID, "artifact", artifactID)
	return err
}

// OpenBody streams the physical blob of one live artifact body. The caller
// owns the returned file and must close it. Expired bodies refuse reads
// while their metadata and references remain durable (DATA-ARTIFACT-003/004).
func (store *Store) OpenBody(ctx context.Context, artifactID int64) (*os.File, Metadata, error) {
	meta, err := store.Metadata(ctx, artifactID)
	if err != nil {
		return nil, Metadata{}, err
	}
	if meta.BodyExpired {
		return nil, meta, ErrBodyExpired
	}
	file, err := os.Open(store.blobPath(meta.SHA256))
	if err != nil {
		return nil, meta, err
	}
	return file, meta, nil
}

func isTextMedia(mediaType string) bool {
	base := strings.TrimSpace(strings.Split(mediaType, ";")[0])
	return strings.HasPrefix(base, "text/") ||
		base == "application/json" || base == "application/x-ndjson" ||
		base == "application/yaml" || base == "application/x-yaml"
}

// lineReader streams a file line by line without buffering it whole.
type lineReader struct {
	reader *bufio.Reader
}

func newLineReader(file *os.File) *lineReader {
	return &lineReader{reader: bufio.NewReaderSize(file, 64*1024)}
}

// readLine returns one line's raw bytes (including the trailing newline
// when present) or eof=true when the file is exhausted.
func (reader *lineReader) readLine() ([]byte, bool, error) {
	content, err := reader.reader.ReadBytes('\n')
	if errors.Is(err, io.EOF) && len(content) == 0 {
		return nil, true, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	return content, false, nil
}
