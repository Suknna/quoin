package knowledge

// Knowledge domain (T27): Knowledge Candidates created or returned from
// the three immutable diagnosis sources, revisioned drafts, single
// confirmation producing the first immutable KnowledgeVersion, exclusion,
// and the read projections for the knowledge workbench. The candidate
// state machine, immutability and concurrency fences are the frozen
// schema's; this service only performs the deterministic transitions
// (DATA-KNOWLEDGE-001..008).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
)

// Candidate states (knowledge_candidates.state CHECK).
const (
	StateAwaiting      = "AwaitingConfirmation"
	StateConfirmed     = "Confirmed"
	StateExcluded      = "Excluded"
	StateSuperseded    = "Superseded"
	StateSourceInvalid = "SourceInvalid"
)

// Candidate source types (knowledge_candidates.source_type CHECK); this
// ticket owns the three diagnosis sources (the import-batch and revision
// flows belong to T28).
const (
	SourceAnalysisOutput = "initial_analysis_output"
	SourceReport         = "inspection_report"
	SourceMessage        = "investigation_message"
)

var (
	// ErrNotFound maps to 404.
	ErrNotFound = errors.New("knowledge object not found")
	// ErrCommandReused maps to 409 command_id_reused.
	ErrCommandReused = errors.New("client command id reused with a different request")
	// ErrSourceRejected maps to 409: the source has a rejected feedback
	// event and can no longer create candidates (DATA-KNOWLEDGE-006).
	ErrSourceRejected = errors.New("rejected diagnosis source cannot create a knowledge candidate")
	// ErrSourceShape maps to 422: the source reference is not an immutable
	// active assistant output of the named scope.
	ErrSourceShape = errors.New("knowledge candidate source must be an immutable diagnosis output")
)

// RevisionConflict reports a stale expected draft revision (409) with the
// authoritative current revision for the conflict envelope.
type RevisionConflict struct {
	Current int64
}

func (err *RevisionConflict) Error() string {
	return "candidate draft revision is stale"
}

// RowVersionConflict reports a stale expected row version (409).
type RowVersionConflict struct {
	Current int64
}

func (err *RowVersionConflict) Error() string {
	return "candidate row version is stale"
}

// StateConflict reports an operation against a candidate whose state no
// longer permits it (409).
type StateConflict struct {
	State string
}

func (err *StateConflict) Error() string {
	return "candidate state no longer permits this operation"
}

// CandidateSummary is the wire projection of one candidate.
type CandidateSummary struct {
	ID                   string `json:"id"`
	SourceType           string `json:"sourceType"`
	SourceID             string `json:"sourceId"`
	State                string `json:"state"`
	RowVersion           int64  `json:"rowVersion"`
	Generation           int64  `json:"generation"`
	DraftRevision        int64  `json:"draftRevision"`
	DraftTitle           string `json:"draftTitle,omitempty"`
	DraftBody            string `json:"draftBody,omitempty"`
	TargetKnowledgeID    string `json:"targetKnowledgeId,omitempty"`
	ConfirmedKnowledgeID string `json:"confirmedKnowledgeId,omitempty"`
}

// CandidateDetail adds the immutable original model suggestion.
type CandidateDetail struct {
	CandidateSummary
	OriginalSuggestion json.RawMessage `json:"originalSuggestion"`
}

// KnowledgeSummary is the browse/detail projection of one Reusable
// Knowledge aggregate.
type KnowledgeSummary struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	CurrentVersionID  string `json:"currentVersionId"`
	CurrentVersionSeq int64  `json:"currentVersionSeq"`
	Eligible          bool   `json:"eligible"`
	RowVersion        int64  `json:"rowVersion"`
}

// KnowledgeDetail is KnowledgeSummary plus the bounded version count.
type KnowledgeDetail struct {
	KnowledgeSummary
	VersionCount int64 `json:"versionCount"`
}

// VersionSummary is one immutable version row.
type VersionSummary struct {
	ID                       string `json:"id"`
	VersionSeq               int64  `json:"versionSeq"`
	Title                    string `json:"title"`
	SourceCandidateID        string `json:"sourceCandidateId"`
	EmbeddingState           string `json:"embeddingState"`
	CreatedAt                string `json:"createdAt"`
	Eligible                 bool   `json:"eligible"`
	RetrievalStateRowVersion int64  `json:"retrievalStateRowVersion"`
}

// VersionDetail is the full immutable version body.
type VersionDetail struct {
	ID                       string          `json:"id"`
	VersionSeq               int64           `json:"versionSeq"`
	Title                    string          `json:"title"`
	Body                     string          `json:"body"`
	Scope                    json.RawMessage `json:"scope,omitempty"`
	Conditions               json.RawMessage `json:"conditions,omitempty"`
	Limitations              json.RawMessage `json:"limitations,omitempty"`
	SourceCandidateID        string          `json:"sourceCandidateId"`
	CreatedAt                string          `json:"createdAt"`
	Eligible                 bool            `json:"eligible"`
	RetrievalStateRowVersion int64           `json:"retrievalStateRowVersion"`
	EmbeddingState           string          `json:"embeddingState"`
	ExitedAt                 string          `json:"exitedAt,omitempty"`
	ExitReason               string          `json:"exitReason,omitempty"`
}

// Service owns the knowledge domain writes and reads.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService builds the knowledge domain service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

func (service *Service) nowText() string {
	return service.now().UTC().Format(time.RFC3339Nano)
}

// authRecord aliases the durable ledger row; the outcome constants mirror
// the frozen ledger CHECK values.
type authRecord = auth.CommandRecord

const (
	authOutcomeCommitted = auth.OutcomeCommitted
	authOutcomeRejected  = auth.OutcomeRejectedKnown
)

func recordAudit(ctx context.Context, conn *sql.Conn, actorID int64, action, targetType string, targetID int64, timestamp string) error {
	result, err := conn.ExecContext(ctx, `INSERT INTO audit_events(actor_type,actor_id,action,outcome,domain_ref_type,domain_ref_id,created_at) VALUES('user',?,?,'success',?,?,?)`,
		actorID, action, targetType, targetID, timestamp)
	if err != nil {
		return err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO audit_event_targets(audit_event_id,target_type,target_id) VALUES(?,?,?)`, auditID, targetType, targetID)
	return err
}

func validSourceType(sourceType string) bool {
	switch sourceType {
	case SourceAnalysisOutput, SourceReport, SourceMessage:
		return true
	}
	return false
}

// commandDigest wraps the durable ledger digest helper.
func commandDigest(commandType string, fields map[string]any) string {
	return auth.DigestCommand(commandType, fields)
}

// authLookup reads the ledger through an open transaction connection.
func authLookup(ctx context.Context, conn *sql.Conn, principalID int64, commandID string) (auth.CommandRecord, bool, error) {
	return auth.LookupCommandOn(ctx, conn, principalID, commandID)
}

// recordCommand wraps the durable ledger insert.
func recordCommand(ctx context.Context, conn *sql.Conn, principalID int64, commandID, commandType, digest, outcome, objectType string, objectID int64, payload string) error {
	return auth.RecordCommand(ctx, conn, principalID, commandID, commandType, digest, outcome, objectType, objectID, payload)
}

// createReplayPayload carries the original create outcome (the summary
// plus whether the command created the candidate) for exact replay.
type createReplayPayload struct {
	Summary CandidateSummary `json:"summary"`
	Created bool             `json:"created"`
}
