// Package investigation owns the Investigation aggregate (DATA-INVEST-001..005):
// the atomic first-message creation, head-fenced sends, the single active
// attempt invariant, assistant-message commit adjudication, the frozen
// investigation_v1 input projection and the transient model-delta feed that
// the ui-message-stream HTTP surface consumes. The package is the only
// product write path for investigations and their messages/attempts.
package investigation

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/artifact"
	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// SchemaKind is the frozen input schema identifier for investigation
// attempt snapshots (DATA-ATTEMPT-001: attempt_type + "_v1").
const SchemaKind = "investigation_v1"

// OutputSchemaKind is the frozen result payload schema identifier
// (RUNTIME-TASK-012).
const OutputSchemaKind = "investigation_output_v1"

// RendererVersion identifies the investigation input renderer generation
// (ARCH-CONTEXT-006).
const RendererVersion = "investigation-renderer-v1"

// AgentVersion is the frozen investigation agent generation recorded on
// the attempt row; the worker binary pins its own copy equal to this.
const AgentVersion = "investigation-v1"

// Errors the HTTP surface maps onto the frozen status codes.
var (
	ErrNotFound             = errors.New("investigation not found")
	ErrModelProviderMissing = errors.New("no enabled qualified model provider")
	ErrCommandReused        = errors.New("client command id reused with a different request")
	ErrActiveAttempt        = errors.New("an active attempt already owns the investigation")
	ErrLateResult           = errors.New("result lost the commit-order race")
	ErrSourceNotFound       = errors.New("investigation source not found")
	ErrInvalidSource        = errors.New("investigation source invalid")
	ErrMessageInvalid       = errors.New("message content invalid")
)

// HeadConflictError reports a stale expected_head_message_id fence miss
// (DATA-INVEST-001); the HTTP surface maps it to the frozen HeadConflict
// envelope (code=head_conflict).
type HeadConflictError struct {
	CurrentHead *int64 // nil = no head (all messages withdrawn)
}

func (err *HeadConflictError) Error() string {
	if err.CurrentHead == nil {
		return "investigation head is null"
	}
	return "investigation head is " + strconv.FormatInt(*err.CurrentHead, 10)
}

// SourceInput is one immutable provenance reference carried by the create
// command (HTTP: InvestigationSourceInput).
type SourceInput struct {
	Type     string // occurrence | initial_analysis | evidence | inspection_report
	SourceID int64
}

// CreateResult carries the durable ids of one created investigation turn.
type CreateResult struct {
	InvestigationID int64
	MessageID       int64
	AttemptID       int64
}

// SendResult carries the durable ids of one appended turn.
type SendResult struct {
	MessageID int64
	AttemptID int64
}

type replayEntry struct {
	investigationID int64
	messageID       int64
	attemptID       int64
	digest          string
}

// Service is the investigation authority.
type Service struct {
	db       *sql.DB
	attempts *attempt.Service
	evidence *evidence.Service
	now      func() time.Time
	// commandReplay is the bounded in-process idempotency ledger
	// (principal, client_command_id) -> original result, mirroring the
	// analysis precedent; the frozen client_commands table is persisted by
	// a later ticket (HTTP-COMMAND-003).
	replayMu sync.Mutex
	replay   map[string]replayEntry
	// streamMu guards the transient delta feeds (one per attempt while an
	// observer exists).
	streamMu sync.Mutex
	streams  map[int64]*feed
	// attachmentMu guards the staging dependency and the message-level
	// attachment boundary (wired once at startup by the app layer).
	attachmentMu    sync.Mutex
	attachments     *artifact.Store
	attachmentLimit int64
	// staged is the bounded in-process idempotency ledger for attachment
	// staging commands ((principal, client_command_id) → attachment id).
	staged map[string]stagedReplay
}

// NewService builds the investigation service and wires the deterministic
// input rebuilder plus the tool observation hooks (grant
// resolution/validation for thanos_query, deterministic Evidence) into the
// shared attempt machine.
func NewService(db *sql.DB) *Service {
	service := &Service{
		db:              db,
		attempts:        attempt.NewService(db),
		now:             func() time.Time { return time.Now().UTC() },
		replay:          map[string]replayEntry{},
		staged:          map[string]stagedReplay{},
		streams:         map[int64]*feed{},
		attachmentLimit: DefaultAttachmentLimitBytes,
	}
	service.attempts.SnapshotRebuilder = service.RebuildInput
	service.evidence = evidence.NewService(db)
	service.evidence.RegisterProjector(thanos.QueryToolName, thanos.EvidenceFor)
	service.attempts.ToolGrantResolver = func(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64, tool attempt.ToolDef) ([]attempt.ToolGrant, error) {
		if tool.Name != thanos.QueryToolName {
			return nil, errors.New("tool " + tool.Name + " has no grant resolver")
		}
		grant, err := thanos.ResolveQueryGrant(ctx, conn, attemptID, toolCallID)
		if err != nil {
			return nil, err
		}
		return []attempt.ToolGrant{grant}, nil
	}
	service.attempts.ToolGrantValidator = func(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64, tool attempt.ToolDef) error {
		if tool.Name != thanos.QueryToolName {
			return errors.New("tool " + tool.Name + " has no grant validator")
		}
		return thanos.ValidateGrantForExecution(ctx, conn, attemptID, toolCallID)
	}
	service.attempts.EvidenceWriter = service.evidence.WriteForToolCall
	return service
}

// Attempts exposes the shared attempt state machine to the runtime slice.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// Evidence exposes the evidence authority to the app layer (read paths).
func (service *Service) Evidence() *evidence.Service { return service.evidence }

// DB exposes the product database to the app layer for read-only routing
// queries (attempt type lookups etc.).
func (service *Service) DB() *sql.DB { return service.db }

func (service *Service) nowText() string { return service.now().Format(time.RFC3339Nano) }

func (service *Service) replayKey(principalID int64, commandID string) string {
	return strconv.FormatInt(principalID, 10) + ":" + commandID
}

// replayLookup returns the original result for a replayed command; a
// reused command id with a different request digest is a deterministic
// conflict (HTTP-COMMAND-003).
func (service *Service) replayLookup(principalID int64, commandID, digest string) (replayEntry, bool, error) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	entry, ok := service.replay[service.replayKey(principalID, commandID)]
	if !ok {
		return replayEntry{}, false, nil
	}
	if entry.digest != digest {
		return replayEntry{}, false, ErrCommandReused
	}
	return entry, true, nil
}

func (service *Service) replayRemember(principalID int64, commandID string, entry replayEntry) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	key := service.replayKey(principalID, commandID)
	if _, exists := service.replay[key]; !exists && len(service.replay) >= 1024 {
		// Bounded in-process replay: evict one arbitrary entry (map order
		// is random; any victim keeps the map at capacity).
		for victim := range service.replay {
			delete(service.replay, victim)
			break
		}
	}
	service.replay[key] = entry
}

func recordAudit(ctx context.Context, conn *sql.Conn, actorType string, actorID int64, action, outcome, targetType string, targetID int64, timestamp string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type, actor_id, action, outcome, domain_ref_type, domain_ref_id, created_at) VALUES(?,?,?,?,?,?,?)`,
		actorType, actorID, action, outcome, targetType, targetID, timestamp)
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id, target_type, target_id) VALUES(?,?,?)`, auditID, targetType, targetID)
	return err
}
