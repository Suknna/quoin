package investigation

// Text-attachment staging and consumption (DATA-ATTACH-001, HTTP-FILE-001/
// 002): uploads are independent immutable staging objects owned by the
// uploading principal; the send/create transactions append ordered message
// references inside their own SQLite commit, so a message never persists
// partial references and the same attachment can be referenced again
// without re-uploading. Every staged body lives exactly once as a
// long-term Artifact; the model sees only bounded artifact_read/grep views.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/execution"
)

// DefaultAttachmentLimitBytes is the deployment default for the message-level
// attachment boundary (HTTP-FILE-002: single file and the sum of one
// message's attachments share the same boundary, default 10 MiB).
const DefaultAttachmentLimitBytes = 10 << 20

// Attachment errors the HTTP surface maps onto the frozen codes.
var (
	ErrAttachmentTooLarge   = errors.New("attachment body exceeds the message-level boundary")
	ErrAttachmentText       = errors.New("attachment body must be valid UTF-8 without NUL")
	ErrAttachmentInvalidRef = errors.New("attachment reference is invalid")
)

// AttachmentView is the TextAttachmentSummary wire projection.
type AttachmentView struct {
	ID               string `json:"id"`
	ArtifactID       string `json:"artifactId"`
	OriginalFilename string `json:"originalFilename"`
	MediaType        string `json:"mediaType"`
	SizeBytes        int64  `json:"sizeBytes"`
	Digest           string `json:"digest"`
	BodyExpired      bool   `json:"bodyExpired"`
	CreatedAt        string `json:"createdAt"`
}

// stagedReplay is the guard entry of one committed staging command: it lets
// concurrent same-command uploads converge on the winner's attachment. It is
// process-local best effort and never a durable command-outcome authority —
// no result is ever replayed from it after a restart or eviction.
type stagedReplay struct {
	attachmentID int64
	digest       string
}

// SetAttachmentStore wires the staging dependency and the deployment
// message-level boundary (defaults apply when unset). If the family's
// read-only reader is already installed it is forwarded to the store, so a
// late-wired staging capability shares the one validated read-only gate.
func (service *Service) SetAttachmentStore(store *artifact.Store, limitBytes int64) error {
	service.attachmentMu.Lock()
	service.attachments = store
	if limitBytes > 0 {
		service.attachmentLimit = limitBytes
	} else {
		service.attachmentLimit = DefaultAttachmentLimitBytes
	}
	wired := service.wiredReader
	service.attachmentMu.Unlock()
	if wired != nil {
		return store.SetReader(wired)
	}
	return nil
}

// StagedBody is one streamed-but-unsealed upload body (the multipart
// handler may receive the file part before the command field, so the
// content stages first and seals only after the full form is parsed).
type StagedBody = artifact.StagedText

// BeginAttachment streams and validates one upload body into staging
// without binding it to a command yet (size/UTF-8/NUL enforced while
// streaming).
func (service *Service) BeginAttachment(ctx context.Context, principalID int64, rawFilename string, reader io.Reader) (*StagedBody, error) {
	service.attachmentMu.Lock()
	limit := service.attachmentLimit
	store := service.attachments
	service.attachmentMu.Unlock()
	if store == nil {
		return nil, errors.New("attachment staging is not wired")
	}
	staged, err := store.StageText(reader, limit)
	if err != nil {
		return nil, mapStageError(err)
	}
	return staged, nil
}

// CommitAttachment seals one staged body and registers the durable staging
// object through the execution runner as one audited, transient command
// (Execute: no durable client-command ledger row, and no result payload is
// persisted — the staged body exists only in memory until its bytes land in
// the content-addressed store inside the guarded transaction). The command's
// session proof is re-verified in-transaction and the automatic audit event
// (investigation.attachment.stage) commits with the registration.
//
// stagedMu is a concurrency guard only, never the command-outcome authority:
// it serializes the in-memory staged-body handoff so two concurrent uploads
// sharing a command id converge on one registration — the loser reuses the
// winner's attachment and drops its own staged body instead of minting a
// second long-term artifact. It is process-local best effort (restart or
// eviction simply registers fresh); no durable outcome is ever replayed from
// it. A reused command id with different content stays a deterministic
// conflict (HTTP-COMMAND-003).
func (service *Service) CommitAttachment(ctx context.Context, principalID int64, clientCommandID, rawFilename string, staged *StagedBody) (AttachmentView, error) {
	filename := artifact.SanitizeAttachmentFilename(rawFilename)
	service.attachmentMu.Lock()
	store := service.attachments
	service.attachmentMu.Unlock()
	if store == nil {
		staged.Abort()
		return AttachmentView{}, errors.New("attachment staging is not wired")
	}
	digest := attachmentCommandDigest(filename, staged.SHA256Hex, staged.SizeBytes)
	service.stagedMu.Lock()
	defer service.stagedMu.Unlock()
	if entry, ok := service.staged[service.stagedKey(principalID, clientCommandID)]; ok {
		if entry.digest != digest {
			staged.Abort()
			return AttachmentView{}, ErrCommandReused
		}
		// Same-command convergence inside the guard: the freshly streamed
		// bytes are the same content, so drop them and answer the winner.
		staged.Abort()
		return service.AttachmentFor(ctx, principalID, entry.attachmentID)
	}
	if err := staged.Seal(); err != nil {
		return AttachmentView{}, err
	}
	type registered struct {
		Record artifact.AttachmentRecord
	}
	value, err := execution.Execute(ctx, service.runner, service.opStage, func(tx *execution.Tx) (registered, error) {
		record, err := store.CommitAttachmentTransaction(ctx, tx, principalID, filename, staged.SHA256Hex, staged.SizeBytes)
		return registered{Record: record}, err
	}, func(value registered) int64 { return value.Record.ID })
	if err != nil {
		staged.Abort()
		return AttachmentView{}, err
	}
	service.staged[service.stagedKey(principalID, clientCommandID)] = stagedReplay{attachmentID: value.Record.ID, digest: digest}
	return attachmentView(value.Record), nil
}

// StageAttachment is the one-shot staging path (begin + commit) for
// callers that already hold the command identity.
func (service *Service) StageAttachment(ctx context.Context, principalID int64, clientCommandID, rawFilename string, reader io.Reader) (AttachmentView, error) {
	staged, err := service.BeginAttachment(ctx, principalID, rawFilename, reader)
	if err != nil {
		return AttachmentView{}, err
	}
	return service.CommitAttachment(ctx, principalID, clientCommandID, rawFilename, staged)
}

func mapStageError(err error) error {
	switch {
	case errors.Is(err, artifact.ErrAttachmentTooLarge):
		return ErrAttachmentTooLarge
	case errors.Is(err, artifact.ErrAttachmentText):
		return ErrAttachmentText
	default:
		return err
	}
}

// AttachmentFor loads one attachment owned by the requesting principal;
// any other attachment behaves as not found (the read path projects only
// the current user's staging objects).
func (service *Service) AttachmentFor(ctx context.Context, principalID, attachmentID int64) (AttachmentView, error) {
	service.attachmentMu.Lock()
	store := service.attachments
	service.attachmentMu.Unlock()
	if store == nil {
		return AttachmentView{}, errors.New("attachment staging is not wired")
	}
	record, err := store.AttachmentByID(ctx, attachmentID)
	if errors.Is(err, artifact.ErrNotFound) {
		return AttachmentView{}, ErrNotFound
	}
	if err != nil {
		return AttachmentView{}, err
	}
	if record.UploadedBy != principalID {
		return AttachmentView{}, ErrNotFound
	}
	return attachmentView(record), nil
}

// resolvedAttachment carries one validated message-attachment reference in
// the user's order (the ordinal position inside the send/create command).
type resolvedAttachment struct {
	AttachmentID int64
	ArtifactID   int64
	Filename     string
	SizeBytes    int64
}

// resolveAttachments validates the send/create attachment references
// inside the caller's transaction: no duplicates, every id exists and was
// uploaded by the same principal, each file and the message total stay
// within the boundary (sizes are immutable, so a single read decides).
func (service *Service) resolveAttachments(ctx context.Context, queries queryer, principalID int64, ids []int64) ([]resolvedAttachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	service.attachmentMu.Lock()
	limit := service.attachmentLimit
	service.attachmentMu.Unlock()
	seen := make(map[int64]bool, len(ids))
	resolved := make([]resolvedAttachment, 0, len(ids))
	var total int64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return nil, ErrAttachmentInvalidRef
		}
		seen[id] = true
		var item resolvedAttachment
		var uploadedBy sql.NullInt64
		var bodyExpired int
		err := queries.QueryRowContext(ctx, `
			SELECT t.id, t.artifact_id, t.original_filename, t.size_bytes, t.uploaded_by, a.body_expired
			FROM text_attachments t JOIN artifacts a ON a.id=t.artifact_id
			WHERE t.id=?`, id).
			Scan(&item.AttachmentID, &item.ArtifactID, &item.Filename, &item.SizeBytes, &uploadedBy, &bodyExpired)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAttachmentInvalidRef
		}
		if err != nil {
			return nil, err
		}
		if !uploadedBy.Valid || uploadedBy.Int64 != principalID {
			return nil, ErrAttachmentInvalidRef
		}
		if bodyExpired == 1 {
			return nil, ErrAttachmentInvalidRef
		}
		if item.SizeBytes > limit {
			return nil, ErrAttachmentTooLarge
		}
		total += item.SizeBytes
		if total > limit {
			return nil, ErrAttachmentTooLarge
		}
		resolved = append(resolved, item)
	}
	return resolved, nil
}

// attachments.go keeps the other staged-attachment helpers; the removed
// placeholder comment about newBool is gone with the type itself.

func (service *Service) stagedKey(principalID int64, commandID string) string {
	return strconv.FormatInt(principalID, 10) + ":" + commandID
}

func attachmentView(record artifact.AttachmentRecord) AttachmentView {
	return AttachmentView{
		ID: strconv.FormatInt(record.ID, 10), ArtifactID: strconv.FormatInt(record.ArtifactID, 10),
		OriginalFilename: record.OriginalFilename, MediaType: record.MediaType,
		SizeBytes: record.SizeBytes, Digest: record.SHA256,
		BodyExpired: record.BodyExpired, CreatedAt: record.CreatedAt,
	}
}

// attachmentCommandDigest fingerprints one staging command's semantic
// fields (filename + content identity; no secret ever exists here).
func attachmentCommandDigest(filename, shaHex string, size int64) string {
	sum := sha256.Sum256([]byte(filename + "\n" + shaHex + "\n" + strconv.FormatInt(size, 10)))
	return hex.EncodeToString(sum[:])
}

// parseAttachmentIDs converts the wire's decimal locator strings; any
// malformed value is a deterministic validation failure.
// ParseAttachmentIDs converts the wire's decimal locator strings; any
// malformed value is a deterministic validation failure (HTTP surface).
func ParseAttachmentIDs(values []string) ([]int64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, ErrAttachmentInvalidRef
		}
		ids = append(ids, id)
	}
	return ids, nil
}
