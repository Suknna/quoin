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
	"sync"
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
	// RuntimeSlot is authenticated at the gRPC boundary and is deliberately
	// not persisted: it narrows the Store's owner closure for Lintel uploads.
	RuntimeSlot     string
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
	// TraceIntegrity is a closed wire value (complete|incomplete) carried only
	// by Lintel trace uploads. It is intentionally part of the commit fence,
	// not inferred later from a mutable ActionResult.
	TraceIntegrity string
}

// Store is the artifact authority.
type Store struct {
	db  *sql.DB
	dir string
	now func() time.Time

	// uploadMu serializes one immutable upload_id from Begin through Commit or
	// Abort. Two simultaneous streams must never truncate the same staging name
	// or mint two logical artifacts for one idempotency capability.
	uploadMu     sync.Mutex
	uploadLeases map[string]chan struct{}
	// blobMu covers only non-replacing blob installation and its directory fsync.
	// It prevents a second digest-equivalent upload from referencing a link before
	// the installing writer has durably synced the parent directory.
	blobMu sync.Mutex
	// gcSuccess projects only successful bounded GC passes to the frozen ops
	// metric. It is injected by the application so the storage package does not
	// own an HTTP/Prometheus dependency.
	gcSuccess func(float64)
	// gcOrphanCursor makes physical orphan cleanup bounded even though expired
	// Artifact history retains the blob reference for auditability.
	gcOrphanCursor string
}

// NewStore builds the store on the product database and blob directory.
func NewStore(db *sql.DB, directory string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(directory, "blobs"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(directory, "staging"), 0o700); err != nil {
		return nil, err
	}
	return &Store{db: db, dir: directory, now: func() time.Time { return time.Now().UTC() }, uploadLeases: make(map[string]chan struct{})}, nil
}

// SetGCSuccessProjector supplies the process-owned metric projection.
func (store *Store) SetGCSuccessProjector(project func(float64)) { store.gcSuccess = project }

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

func (store *Store) acquireUpload(ctx context.Context, uploadID string) error {
	for {
		store.uploadMu.Lock()
		if store.uploadLeases == nil {
			store.uploadLeases = make(map[string]chan struct{})
		}
		if _, busy := store.uploadLeases[uploadID]; !busy {
			store.uploadLeases[uploadID] = make(chan struct{})
			store.uploadMu.Unlock()
			return nil
		}
		wait := store.uploadLeases[uploadID]
		store.uploadMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (store *Store) releaseUpload(uploadID string) {
	store.uploadMu.Lock()
	defer store.uploadMu.Unlock()
	if done := store.uploadLeases[uploadID]; done != nil {
		delete(store.uploadLeases, uploadID)
		close(done)
	}
}

// AbortUpload ends one interrupted stream without changing its uploading ledger
// row. A whole-body retry with the same immutable upload_id may then take over.
func (store *Store) AbortUpload(uploadID string) {
	_ = os.Remove(store.stagingPath(uploadID))
	store.releaseUpload(uploadID)
}

// BeginUpload validates and records the short-lived ledger phase, then opens a
// staging file. It deliberately returns no database handle: a slow or stalled
// gRPC upload must never monopolize Quoin's (often single-connection) SQLite
// pool. CommitUpload opens its own short transaction after the bytes are sealed.
func (store *Store) BeginUpload(ctx context.Context, header UploadHeader) (file *os.File, artifactID int64, err error) {
	if header.UploadID == "" || len(header.SHA256) != 32 || header.SizeBytes < 0 {
		return nil, 0, &Rejection{RejectMetadataMismatch, "header fields incomplete"}
	}
	if header.Kind == "trace" && !header.Sensitive {
		return nil, 0, &Rejection{RejectMetadataMismatch, "trace artifacts must be sensitive"}
	}
	if err := store.acquireUpload(ctx, header.UploadID); err != nil {
		return nil, 0, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			store.releaseUpload(header.UploadID)
		}
	}()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, 0, err
	}
	fail := func(rejection *Rejection) (*os.File, int64, error) {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		conn.Close()
		return nil, 0, rejection
	}
	var state string
	var storedArtifact sql.NullInt64
	var storedAttempt sql.NullInt64
	var storedBoot, storedOwnerType, storedKind, storedMediaType, storedRetention, storedSHA string
	var storedTraceIntegrity sql.NullString
	var storedEpoch, storedOwnerID, storedSensitive, storedSize int64
	err = conn.QueryRowContext(ctx, `
		SELECT state, artifact_id, attempt_id, boot_id, connection_epoch, owner_type, owner_id,
			kind, media_type, retention_kind, sensitive, size_bytes, sha256, trace_integrity
		FROM runtime_artifact_uploads WHERE upload_id=?`, header.UploadID).Scan(
		&state, &storedArtifact, &storedAttempt, &storedBoot, &storedEpoch, &storedOwnerType, &storedOwnerID,
		&storedKind, &storedMediaType, &storedRetention, &storedSensitive, &storedSize, &storedSHA, &storedTraceIntegrity)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New upload: validate the attempt fence and the owner closure.
		if rejection, ok := store.validateUploadOn(ctx, conn, header); !ok {
			return fail(rejection)
		}
		now := store.now().Format(time.RFC3339Nano)
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO runtime_artifact_uploads(upload_id,attempt_id,boot_id,connection_epoch,
				owner_type,owner_id,kind,media_type,retention_kind,sensitive,size_bytes,sha256,trace_integrity,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'uploading',?)`,
			header.UploadID, nullableUploadAttempt(header.AttemptID), header.BootID, header.ConnectionEpoch,
			header.OwnerType, header.OwnerID, header.Kind, header.MediaType, header.RetentionKind,
			boolInt(header.Sensitive), header.SizeBytes, hex.EncodeToString(header.SHA256), nullableTraceIntegrity(header), now); err != nil {
			return fail(&Rejection{RejectInternal, err.Error()})
		}
	case err != nil:
		return fail(&Rejection{RejectInternal, err.Error()})
	case state == "committed":
		if !storedArtifact.Valid || !sameCommittedUploadMetadata(storedAttempt, storedBoot, storedEpoch, storedOwnerType, storedOwnerID, storedKind, storedMediaType, storedRetention, storedSensitive, storedSize, storedSHA, storedTraceIntegrity, header) {
			return fail(&Rejection{RejectMetadataMismatch, "committed upload metadata differs"})
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return fail(&Rejection{RejectInternal, err.Error()})
		}
		conn.Close()
		return nil, storedArtifact.Int64, nil
	case state == "rejected":
		return fail(&Rejection{RejectMetadataMismatch, "upload id was rejected"})
	default:
		// A crashed upload retrying: the upload ID is a complete immutable
		// capability, not merely a body digest. The browser-start fence and the
		// live upload transport are intentionally distinct: a same-boot successor
		// stream may retransmit an interrupted Lintel trace/screenshot without
		// rewriting the ledger's original epoch. Reject every other metadata or
		// epoch change before opening a new staging file.
		if !sameRetryUploadMetadata(storedAttempt, storedBoot, storedEpoch, storedOwnerType, storedOwnerID, storedKind, storedMediaType, storedRetention, storedSensitive, storedSize, storedSHA, storedTraceIntegrity, header) {
			return fail(&Rejection{RejectMetadataMismatch, "retry metadata differs"})
		}
		if rejection, ok := store.validateUploadOn(ctx, conn, header); !ok {
			return fail(rejection)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fail(&Rejection{RejectInternal, err.Error()})
	}
	// The transaction is over before the upload body is received. Do not hold a
	// pool connection across network I/O.
	conn.Close()
	file, err = os.OpenFile(store.stagingPath(header.UploadID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, 0, &Rejection{RejectInternal, err.Error()}
	}
	keepLease = true
	return file, 0, nil
}

// sameUploadMetadata enforces the ledger's idempotency contract: retries of a
// stable upload ID must repeat the entire immutable capability, including its
// owner and runtime fence, rather than only the bytes.
func nullableUploadAttempt(attemptID int64) any {
	if attemptID == 0 {
		return nil
	}
	return attemptID
}

func nullableTraceIntegrity(header UploadHeader) any {
	if header.TraceIntegrity == "" {
		return nil
	}
	return header.TraceIntegrity
}

func sameUploadMetadata(attempt sql.NullInt64, boot string, epoch int64, ownerType string, ownerID int64, kind, mediaType, retention string, sensitive, size int64, digest string, traceIntegrity sql.NullString, header UploadHeader) bool {
	return sameUploadMetadataIgnoringEpoch(attempt, boot, ownerType, ownerID, kind, mediaType, retention, sensitive, size, digest, traceIntegrity, header) && epoch == int64(header.ConnectionEpoch)
}

func sameUploadMetadataIgnoringEpoch(attempt sql.NullInt64, boot, ownerType string, ownerID int64, kind, mediaType, retention string, sensitive, size int64, digest string, traceIntegrity sql.NullString, header UploadHeader) bool {
	return attempt.Valid == (header.AttemptID != 0) && (!attempt.Valid || attempt.Int64 == header.AttemptID) &&
		boot == header.BootID && ownerType == header.OwnerType && ownerID == header.OwnerID && kind == header.Kind &&
		mediaType == header.MediaType && retention == header.RetentionKind &&
		sensitive == int64(boolInt(header.Sensitive)) && size == header.SizeBytes && digest == hex.EncodeToString(header.SHA256) &&
		traceIntegrity.Valid == (header.TraceIntegrity != "") && (!traceIntegrity.Valid || traceIntegrity.String == header.TraceIntegrity)
}

// sameCommittedUploadMetadata permits the one retry that can legitimately cross
// a Runtime stream boundary: Lintel replaying an already committed browser
// artifact over a later epoch of the same boot. A reply can be lost after either
// a trace or screenshot commit; both must replay the existing Artifact ID rather
// than turn an unknown outcome into a second logical upload. The capability
// remains otherwise fully immutable, and no new body is accepted on this path.
func sameCommittedUploadMetadata(attempt sql.NullInt64, boot string, epoch int64, ownerType string, ownerID int64, kind, mediaType, retention string, sensitive, size int64, digest string, traceIntegrity sql.NullString, header UploadHeader) bool {
	return sameRetryUploadMetadata(attempt, boot, epoch, ownerType, ownerID, kind, mediaType, retention, sensitive, size, digest, traceIntegrity, header)
}

// sameRetryUploadMetadata permits a same-boot Lintel successor transport for
// both unknown (uploading) and committed trace/screenshot uploads. The ledger
// itself remains immutable: this predicate never updates storedEpoch. Non-Lintel
// uploads, a different boot, a lower epoch, or any capability mismatch remain
// rejected.
func sameRetryUploadMetadata(attempt sql.NullInt64, boot string, epoch int64, ownerType string, ownerID int64, kind, mediaType, retention string, sensitive, size int64, digest string, traceIntegrity sql.NullString, header UploadHeader) bool {
	if sameUploadMetadata(attempt, boot, epoch, ownerType, ownerID, kind, mediaType, retention, sensitive, size, digest, traceIntegrity, header) {
		return true
	}
	return header.RuntimeSlot == "lintel" && (kind == "trace" || kind == "screenshot") && epoch < int64(header.ConnectionEpoch) &&
		sameUploadMetadataIgnoringEpoch(attempt, boot, ownerType, ownerID, kind, mediaType, retention, sensitive, size, digest, traceIntegrity, header)
}

// validateUploadOn checks the attempt fence and owner closure inside the
// ledger transaction (RUNTIME-UPLOAD-004).
func (store *Store) validateUploadOn(ctx context.Context, conn *sql.Conn, header UploadHeader) (*Rejection, bool) {
	// The operation and child fields record the epoch that started the physical
	// browser and are immutable audit facts. A same-boot reconnect may transport
	// a pending Lintel upload on a later epoch, but must never rewrite that start
	// fence. The authenticated Runtime bearer establishes the live transport
	// epoch; below we accept only a monotonic same-boot successor.
	if header.AttemptID != 0 {
		var state string
		var boot sql.NullString
		var epoch sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT state,boot_id,connection_epoch FROM execution_attempts WHERE id=?`, header.AttemptID).Scan(&state, &boot, &epoch); err != nil {
			return &Rejection{RejectAttemptNotRunning, "attempt unknown"}, false
		}
		// A Lintel cancellation trace is the sole exception: SQLite has already
		// fenced the child to Cancelling, but the continuous trace must be
		// committed before its parent Tool Call trigger can terminalize it.
		if state != "Running" && !(state == "Cancelling" && header.RuntimeSlot == "lintel" && header.Kind == "trace" && header.OwnerType == "browser_operation") {
			return &Rejection{RejectAttemptNotRunning, fmt.Sprintf("attempt is %s", state)}, false
		}
		if !boot.Valid || boot.String != header.BootID {
			return &Rejection{RejectBootMismatch, "boot fence"}, false
		}
		if !epoch.Valid || (header.RuntimeSlot == "lintel" && epoch.Int64 > int64(header.ConnectionEpoch)) || (header.RuntimeSlot != "lintel" && epoch.Int64 != int64(header.ConnectionEpoch)) {
			return &Rejection{RejectEpochMismatch, "epoch fence"}, false
		}
	}
	switch header.Kind {
	case "trace", "screenshot":
		if header.RuntimeSlot != "lintel" {
			break
		}
		if header.OwnerType != "browser_operation" || header.RetentionKind != "generated" || (header.AttemptID == 0 && header.Kind != "trace") {
			return &Rejection{RejectMetadataMismatch, "Lintel browser artifacts require a generated browser operation owner and child attempt; only traces may use operation ownership"}, false
		}
		if header.Kind == "trace" && !header.Sensitive {
			return &Rejection{RejectMetadataMismatch, "Lintel trace artifacts must be sensitive"}, false
		}
		if header.Kind == "trace" && header.TraceIntegrity != "complete" && header.TraceIntegrity != "incomplete" {
			return &Rejection{RejectMetadataMismatch, "Lintel trace integrity must be complete or incomplete"}, false
		}
		if header.Kind != "trace" && header.TraceIntegrity != "" {
			return &Rejection{RejectMetadataMismatch, "only Lintel traces carry trace integrity"}, false
		}
		var bound int
		// A crash can happen after StartAck but before the first action row, or
		// between two terminal child attempts. An attempt_id=0 trace is therefore
		// authorized by the still-Running operation itself (a Journey's mandatory
		// whole-run trace or an Exploration's operation-owned trace); a nonzero
		// attempt keeps the stronger child/action fence used by ordinary action
		// artifacts.
		query := `SELECT EXISTS(SELECT 1 FROM browser_operations operation
			WHERE operation.id=? AND operation.kind IN ('journey','exploration') AND operation.state='Running'
				AND operation.lintel_boot_id=? AND operation.lintel_connection_epoch<=?)`
		args := []any{header.OwnerID, header.BootID, header.ConnectionEpoch}
		if header.AttemptID != 0 {
			query = `SELECT EXISTS(SELECT 1 FROM execution_attempts child
				JOIN browser_exploration_actions action ON action.child_attempt_id=child.id
				JOIN browser_operations operation ON operation.id=action.operation_id
				WHERE child.id=? AND child.attempt_type='browser_exploration'
				  AND child.state IN ('Running','Cancelling')
				  AND child.boot_id=? AND child.connection_epoch<=? AND action.operation_id=?
				  AND operation.kind='exploration' AND operation.state='Running')`
			args = []any{header.AttemptID, header.BootID, header.ConnectionEpoch, header.OwnerID}
		}
		err := conn.QueryRowContext(ctx, query, args...).Scan(&bound)
		if err != nil || bound != 1 {
			return &Rejection{RejectMetadataMismatch, "Lintel browser artifact is not bound to a running journey/exploration operation or exploration action"}, false
		}
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
// non-replacing installation into the content-addressed blob directory, fsync
// of the parent, then the SQLite reference transaction (RUNTIME-UPLOAD-003).
func (store *Store) CommitUpload(ctx context.Context, header UploadHeader, staged *os.File) (int64, error) {
	// BeginUpload owns this idempotency lease; every terminal Commit path releases
	// it so a whole-body retry can linearize after an interrupted attempt.
	defer store.releaseUpload(header.UploadID)
	// The staging body is already sealed. Acquire a database connection only for
	// the short reference transaction below, never while the stream is open.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		_ = staged.Close()
		return 0, err
	}
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
	// Rename would replace a concurrent writer's blob on Unix and the old
	// failure cleanup could then delete a blob already referenced by another
	// upload. Link provides non-replacing installation: either our sealed staging
	// inode becomes the blob, or an equal digest blob already owns the path.
	// Keep blobMu through the reference commit. GC takes the same lock only
	// after reserving its DB connection, so an unreferenced existing digest
	// cannot be unlinked between installation and its first durable reference.
	store.blobMu.Lock()
	installErr := store.installVerifiedBlob(header, blobPath)
	if installErr != nil {
		store.blobMu.Unlock()
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, installErr
	}
	// The staging name is ours even when a concurrent upload had already
	// installed the shared blob. Never unlink blobPath on transaction failure:
	// it might now be referenced by that concurrent transaction. Orphan content
	// is safe and collectible; deleting shared content is not.
	_ = os.Remove(store.stagingPath(header.UploadID))
	artifactID, err := store.commitReferences(ctx, conn, header)
	store.blobMu.Unlock()
	if err != nil {
		closeConn()
		store.rejectUpload(ctx, header.UploadID, RejectInternal)
		return 0, err
	}
	return artifactID, nil
}

func (store *Store) installVerifiedBlob(header UploadHeader, blobPath string) error {
	return store.installVerifiedBlobFrom(store.stagingPath(header.UploadID), header, blobPath)
}

func (store *Store) installVerifiedBlobFrom(stagingPath string, header UploadHeader, blobPath string) error {
	if err := os.Link(stagingPath, blobPath); err != nil {
		if !os.IsExist(err) {
			return err
		}
		// A pre-existing digest path may be shared by a concurrent upload. Verify
		// it before creating another logical reference: filename equality alone is
		// not integrity evidence after a torn/manual filesystem write.
		return verifyBlob(blobPath, header.SizeBytes, header.SHA256)
	}
	directory, err := os.Open(filepath.Dir(blobPath))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return err
	}
	return verifyBlob(blobPath, header.SizeBytes, header.SHA256)
}

// MaterializeEvidenceTransaction creates the content-addressed, attempt-readable
// report source for one immutable Evidence. The caller owns the SQLite transaction.
func (store *Store) MaterializeEvidenceTransaction(ctx context.Context, conn *sql.Conn, evidenceID int64, body []byte) (int64, error) {
	if evidenceID < 1 {
		return 0, errors.New("evidence id must be positive")
	}
	sum := sha256.Sum256(body)
	shaHex := hex.EncodeToString(sum[:])
	var existing int64
	err := conn.QueryRowContext(ctx, `SELECT a.id FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.owner_type='evidence' AND a.owner_id=? AND a.kind='report_file' AND b.sha256=?`, evidenceID, shaHex).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	staged, err := os.CreateTemp(filepath.Join(store.dir, "staging"), "evidence-*.part")
	if err != nil {
		return 0, err
	}
	stagingPath := staged.Name()
	defer os.Remove(stagingPath)
	if _, err = staged.Write(body); err != nil {
		staged.Close()
		return 0, err
	}
	if err = staged.Sync(); err != nil {
		staged.Close()
		return 0, err
	}
	if err = staged.Close(); err != nil {
		return 0, err
	}
	header := UploadHeader{SizeBytes: int64(len(body)), SHA256: sum[:]}
	store.blobMu.Lock()
	err = store.installVerifiedBlobFrom(stagingPath, header, store.blobPath(shaHex))
	store.blobMu.Unlock()
	if err != nil {
		return 0, err
	}
	var blobID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM artifact_blobs WHERE sha256=?`, shaHex).Scan(&blobID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `INSERT INTO artifact_blobs(sha256,size_bytes,storage_key,created_at) VALUES(?,?,?,?)`, shaHex, len(body), "blobs/"+shaHex+".blob", store.now().Format(time.RFC3339Nano))
		if insertErr != nil {
			return 0, insertErr
		}
		blobID, err = insert.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	insert, err := conn.ExecContext(ctx, `INSERT INTO artifacts(blob_id,kind,media_type,sensitive,retention_kind,owner_type,owner_id,created_at) VALUES(?,'report_file','application/json',0,'long_term','evidence',?,?)`, blobID, evidenceID, store.now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return insert.LastInsertId()
}

func verifyBlob(path string, expectedSize int64, expectedHash []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("content-addressed blob size differs from upload header")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !bytesEqual(hash.Sum(nil), expectedHash) {
		return fmt.Errorf("content-addressed blob hash differs from upload header")
	}
	return nil
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
	// BeginUpload's validation deliberately precedes network I/O, but it cannot
	// authorize the final reference: cancellation, boot replacement, or an
	// operation terminal transition may commit while bytes are staged. Re-read the
	// complete owner/action/boot fence inside this same transaction, immediately
	// before any artifact rows can be created.
	if rejection, ok := store.validateUploadOn(ctx, conn, header); !ok {
		return 0, rejection
	}
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
		// Retention is resolved once at creation and then frozen on the logical
		// artifact row; later setting changes never rewrite historical expiry.
		var days int
		if err := conn.QueryRowContext(ctx, `SELECT generated_retention_days FROM artifact_retention_settings WHERE id=1`).Scan(&days); err != nil {
			return 0, fmt.Errorf("read generated artifact retention: %w", err)
		}
		expiresAt = sql.NullString{String: store.now().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano), Valid: true}
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
	// The irreversible Artifact commit is the only point where a trace may make
	// a normal terminal claim durable. Do not infer integrity from a later ActionResult:
	// cancellation/crash can race that message. A complete trace must consume the
	// one matching pre-upload claim; an incomplete trace can only downgrade it.
	if header.RuntimeSlot == "lintel" && header.Kind == "trace" && header.AttemptID > 0 {
		var update sql.Result
		if header.TraceIntegrity == "complete" {
			update, err = conn.ExecContext(ctx, `UPDATE browser_exploration_terminal_claims
				SET state='artifact_committed_complete',trace_artifact_id=?,trace_digest=?,finalized_at=?
				WHERE child_attempt_id=? AND operation_id=? AND state='claimed_complete'
				  AND NOT EXISTS (
					SELECT 1 FROM browser_exploration_child_bindings binding
					JOIN execution_attempts parent ON parent.id=binding.parent_attempt_id
					WHERE binding.child_attempt_id=browser_exploration_terminal_claims.child_attempt_id
					  AND parent.state <> 'Running'
				  )`,
				artifactID, header.SHA256, store.now().Format(time.RFC3339Nano), header.AttemptID, header.OwnerID)
		} else {
			// An incomplete trace may follow an already committed complete artifact
			// when parent cancellation wins before ActionResult. Do not transition or
			// erase that claim here: the ActionResult transaction atomically moves the
			// immutable complete binding into historical columns while attaching this
			// distinct incomplete trace to the operation.
			update, err = conn.ExecContext(ctx, `UPDATE browser_exploration_terminal_claims
				SET finalized_at=finalized_at
				WHERE child_attempt_id=? AND operation_id=? AND state IN ('claimed_complete','artifact_committed_complete')`,
				header.AttemptID, header.OwnerID)
		}
		if err != nil {
			return 0, err
		}
		if rows, rowsErr := update.RowsAffected(); rowsErr != nil || (header.TraceIntegrity == "complete" && rows != 1) {
			if rowsErr != nil {
				return 0, rowsErr
			}
			return 0, &Rejection{RejectAttemptNotRunning, "complete trace has no accepted terminal claim"}
		}
	}
	updated, err := conn.ExecContext(ctx, `
		UPDATE runtime_artifact_uploads SET state='committed', artifact_id=?, committed_at=?, row_version=row_version+1
		WHERE upload_id=? AND state='uploading'`, artifactID, store.now().Format(time.RFC3339Nano), header.UploadID)
	if err != nil {
		return 0, err
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, fmt.Errorf("upload ledger %q lost its commit linearization", header.UploadID)
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
