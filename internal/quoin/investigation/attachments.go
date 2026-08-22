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

// stagedReplay is one committed staging command's durable result
// (the in-process ledger mirrors the create/send precedent; the frozen
// client_commands table is persisted by a later ticket).
type stagedReplay struct {
	attachmentID int64
	digest       string
}

// SetAttachmentStore wires the staging dependency and the deployment
// message-level boundary (defaults apply when unset).
func (service *Service) SetAttachmentStore(store *artifact.Store, limitBytes int64) {
	service.attachmentMu.Lock()
	defer service.attachmentMu.Unlock()
	service.attachments = store
	if limitBytes > 0 {
		service.attachmentLimit = limitBytes
	} else {
		service.attachmentLimit = DefaultAttachmentLimitBytes
	}
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
// object for the command. The replay ledger keys on (principal,
// client_command_id); the digest covers the semantic fields — the
// sanitized filename and the content digest/size (HTTP-COMMAND-002/003).
// The whole seal+register runs inside one BEGIN IMMEDIATE transaction
// with an in-transaction replay re-check: two concurrent uploads sharing
// a command id serialize here, the loser replays the winner's attachment
// and drops its own staged body instead of minting a second long-term
// artifact (the same double-check pattern as Create/Send).
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
	if entry, ok, err := service.stagedLookup(principalID, clientCommandID, digest); err != nil {
		staged.Abort()
		return AttachmentView{}, err
	} else if ok {
		// Identical replay: the original staging object is the answer; the
		// freshly streamed bytes are the same content, so drop them.
		staged.Abort()
		return service.AttachmentFor(ctx, principalID, entry.attachmentID)
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		staged.Abort()
		return AttachmentView{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		staged.Abort()
		return AttachmentView{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// In-transaction replay re-check: the serialized writer decides.
	if entry, ok, err := service.stagedLookup(principalID, clientCommandID, digest); err != nil {
		return AttachmentView{}, err
	} else if ok {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return AttachmentView{}, err
		}
		committed = true
		staged.Abort()
		return service.AttachmentFor(ctx, principalID, entry.attachmentID)
	}
	if err := staged.Seal(); err != nil {
		return AttachmentView{}, err
	}
	record, err := store.CommitAttachmentTransaction(ctx, conn, principalID, filename, staged.SHA256Hex, staged.SizeBytes)
	if err != nil {
		return AttachmentView{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return AttachmentView{}, err
	}
	committed = true
	service.stagedRemember(principalID, clientCommandID, stagedReplay{attachmentID: record.ID, digest: digest})
	return attachmentView(record), nil
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

func (service *Service) stagedLookup(principalID int64, commandID, digest string) (stagedReplay, bool, error) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	entry, ok := service.staged[service.replayKey(principalID, commandID)]
	if !ok {
		return stagedReplay{}, false, nil
	}
	if entry.digest != digest {
		return stagedReplay{}, false, ErrCommandReused
	}
	return entry, true, nil
}

func (service *Service) stagedRemember(principalID int64, commandID string, entry stagedReplay) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	key := service.replayKey(principalID, commandID)
	if _, exists := service.staged[key]; !exists && len(service.staged) >= 1024 {
		for victim := range service.staged {
			delete(service.staged, victim)
			break
		}
	}
	service.staged[key] = entry
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
