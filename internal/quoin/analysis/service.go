// Package analysis owns the Initial Analysis aggregate (DATA-ANALYSIS-001/
// 002): creation against one alert occurrence, the one-active invariant,
// attempt fan-out, technical-failure retry, the cancellation fence and the
// atomic seal of the first legal result. The package is the only product
// write path for initial_analyses and its attempt fan-out; the HTTP surface
// and the runtime control stream both call through here.
package analysis

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// SchemaKind is the frozen input schema identifier for initial-analysis
// attempt snapshots (DATA-ATTEMPT-001: attempt_type + "_v1").
const SchemaKind = "initial_analysis_v1"

// OutputSchemaKind is the frozen result payload schema identifier
// (RUNTIME-TASK-012).
const OutputSchemaKind = "initial_analysis_output_v1"

// RendererVersion identifies the input renderer generation both sides
// agree on (ARCH-CONTEXT-006).
const RendererVersion = "initial-analysis-renderer-v1"

// Errors the HTTP surface maps onto the frozen status codes.
var (
	ErrNotFound             = errors.New("initial analysis not found")
	ErrModelProviderMissing = errors.New("no enabled qualified model provider")
	ErrActiveConflict       = errors.New("initial analysis is not retryable or the fence lost the race")
	ErrNoOutput             = errors.New("initial analysis has no sealed output")
	ErrLateResult           = errors.New("result proposal lost the commit-order race")
	ErrOutputSealed         = errors.New("initial analysis already sealed an output")
)

// RowVersionError reports a stale expected_row_version fence miss.
type RowVersionError struct {
	Current int64
}

func (err *RowVersionError) Error() string {
	return fmt.Sprintf("analysis row version is %d", err.Current)
}

// Service is the analysis authority.
type Service struct {
	db       *sql.DB
	attempts *attempt.Service
	now      func() time.Time
	// commandReplay is the bounded in-process idempotency ledger
	// (principal, client_command_id) -> analysis result, mirroring the
	// alert-source precedent; the frozen client_commands table is
	// persisted by a later ticket.
	replayMu sync.Mutex
	replay   map[string]replayEntry
}

type replayEntry struct {
	analysisID int64
	attemptID  int64
}

// NewService builds the analysis service on the product database and
// wires the deterministic input rebuilder into the shared attempt machine.
func NewService(db *sql.DB) *Service {
	service := &Service{
		db:       db,
		attempts: attempt.NewService(db),
		now:      func() time.Time { return time.Now().UTC() },
		replay:   map[string]replayEntry{},
	}
	service.attempts.SnapshotRebuilder = service.RebuildInput
	return service
}

// Attempts exposes the shared attempt state machine to the runtime slice.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// DB exposes the product database to the app layer for read-only routing
// queries (attempt type lookups etc.).
func (service *Service) DB() *sql.DB { return service.db }

func (service *Service) nowText() string { return service.now().Format(time.RFC3339Nano) }

func (service *Service) replayKey(principalID int64, commandID string) string {
	return strconv.FormatInt(principalID, 10) + ":" + commandID
}

func (service *Service) replayLookup(principalID int64, commandID string) (replayEntry, bool) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	entry, ok := service.replay[service.replayKey(principalID, commandID)]
	return entry, ok
}

func (service *Service) replayRemember(principalID int64, commandID string, entry replayEntry) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	key := service.replayKey(principalID, commandID)
	if _, exists := service.replay[key]; !exists && len(service.replay) >= 1024 {
		// Evict one arbitrary entry: map order is random, which is exactly
		// the bounded-replay policy (any victim keeps the map at capacity).
		for victim := range service.replay {
			delete(service.replay, victim)
			break
		}
	}
	service.replay[key] = entry
}

// Input is the rendered, immutable input of one analysis.
type Input struct {
	Occurrence    OccurrenceContext `json:"occurrence"`
	ModelContract ModelContract     `json:"modelContract"`
}

// OccurrenceContext is the frozen alert context the model receives.
type OccurrenceContext struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	FirstSeenAt     string            `json:"firstSeenAt"`
	LastStateChange string            `json:"lastStateChangeAt"`
	ResolvedAt      *string           `json:"resolvedAt,omitempty"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// ModelContract is the frozen chat contract of the attempt
// (ARCH-AGENT-003): the worker renders these values, never selects them.
type ModelContract struct {
	ModelID             string `json:"modelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}

// Summary is the list projection (InitialAnalysisSummary).
type Summary struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	RowVersion int64  `json:"rowVersion"`
	CreatedAt  string `json:"createdAt"`
}

// Output is the sealed first successful result (AnalysisOutput).
type Output struct {
	ID          string   `json:"id"`
	ModelID     string   `json:"modelId"`
	Content     string   `json:"content"`
	EvidenceIDs []string `json:"evidenceIds"`
	CreatedAt   string   `json:"createdAt"`
}

// Detail is the get/list create/retry/cancel response projection
// (InitialAnalysisDetail).
type Detail struct {
	Summary
	AttemptCount int64   `json:"attemptCount"`
	Output       *Output `json:"output,omitempty"`
}

// AttemptItem is the listInitialAnalysisAttempts projection (AttemptSummary).
type AttemptItem struct {
	ID                string  `json:"id"`
	Type              string  `json:"type"`
	State             string  `json:"state"`
	RowVersion        int64   `json:"rowVersion"`
	StartedAt         *string `json:"startedAt,omitempty"`
	EndedAt           *string `json:"endedAt,omitempty"`
	TerminationReason *string `json:"terminationReason,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

// CreateResult carries the durable ids of one created (or replayed)
// analysis.
type CreateResult struct {
	AnalysisID int64
	AttemptID  int64
}

// Create renders the input snapshot, creates the analysis and its first
// attempt and freezes the chat_model grant in one transaction. A second
// create for the same occurrence returns the active analysis (the unique
// partial index is the authority; DATA-ANALYSIS-001), and a replayed
// client command returns its original record.
func (service *Service) Create(ctx context.Context, occurrenceID, principalID int64, clientCommandID string) (CreateResult, error) {
	if entry, ok := service.replayLookup(principalID, clientCommandID); ok {
		return CreateResult{AnalysisID: entry.analysisID, AttemptID: entry.attemptID}, nil
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return CreateResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	var occurrenceIDRow int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM alert_occurrences WHERE id=?`, occurrenceID).Scan(&occurrenceIDRow); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CreateResult{}, ErrNotFound
		}
		return CreateResult{}, err
	}
	var activeID int64
	err = conn.QueryRowContext(ctx, `SELECT id FROM initial_analyses WHERE occurrence_id=? AND state IN ('Queued','Running')`, occurrenceID).Scan(&activeID)
	if err == nil {
		var attemptID int64
		if attemptErr := conn.QueryRowContext(ctx, `SELECT id FROM execution_attempts WHERE scope_type='analysis' AND scope_id=? AND state IN ('Queued','Assigned','Running','Cancelling')`, activeID).Scan(&attemptID); attemptErr != nil {
			return CreateResult{}, attemptErr
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return CreateResult{}, err
		}
		committed = true
		result := CreateResult{AnalysisID: activeID, AttemptID: attemptID}
		service.replayRemember(principalID, clientCommandID, replayEntry{analysisID: activeID, attemptID: attemptID})
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CreateResult{}, err
	}
	input, _, selected, err := service.renderInput(ctx, conn, occurrenceID)
	if err != nil {
		return CreateResult{}, err
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return CreateResult{}, err
	}
	digest := sha256.Sum256(canonical)
	digestHex := hex.EncodeToString(digest[:])
	now := service.nowText()
	analysisInsert, err := conn.ExecContext(ctx, `
		INSERT INTO initial_analyses(occurrence_id,state,input_snapshot_digest,created_by,created_at)
		VALUES(?,?,?,?,?)`, occurrenceID, "Queued", digestHex, principalID, now)
	if err != nil {
		return CreateResult{}, err
	}
	analysisID, err := analysisInsert.LastInsertId()
	if err != nil {
		return CreateResult{}, err
	}
	attemptID, err := insertAttempt(ctx, conn, analysisID, digestHex, input, selected, now)
	if err != nil {
		return CreateResult{}, err
	}
	if err := recordAudit(ctx, conn, "user", principalID, "initial_analysis.create", "success", "initial_analysis", analysisID, now); err != nil {
		return CreateResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return CreateResult{}, err
	}
	committed = true
	result := CreateResult{AnalysisID: analysisID, AttemptID: attemptID}
	service.replayRemember(principalID, clientCommandID, replayEntry{analysisID: analysisID, attemptID: attemptID})
	return result, nil
}

// provider is the resolved enabled model provider contract for one attempt.
type provider struct {
	ConnectionID      int64
	RevisionID        int64
	CredentialGen     int64
	ProbeResultID     int64
	ChatModelID       string
	ContextBudget     int
	MaxOutput         int
	Streaming         bool
	NativeToolCalling bool
}

// renderInput loads the occurrence context and resolves the current
// enabled model provider (ARCH-AGENT-003). No enabled provider is a
// deterministic 503, not a stored analysis.
func (service *Service) renderInput(ctx context.Context, conn *sql.Conn, occurrenceID int64) (Input, ModelContract, provider, error) {
	var input Input
	var labelsJSON string
	err := conn.QueryRowContext(ctx, `
		SELECT id,state,first_seen_at,last_state_change_at,resolved_at,labels_canonical
		FROM alert_occurrences WHERE id=?`, occurrenceID).
		Scan(&occurrenceID, &input.Occurrence.State, &input.Occurrence.FirstSeenAt, &input.Occurrence.LastStateChange,
			&input.Occurrence.ResolvedAt, &labelsJSON)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	input.Occurrence.ID = strconv.FormatInt(occurrenceID, 10)
	if err := json.Unmarshal([]byte(labelsJSON), &input.Occurrence.Labels); err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	selected, err := selectModelProvider(ctx, conn)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	contract := ModelContract{
		ModelID:             selected.ChatModelID,
		ContextBudgetTokens: selected.ContextBudget,
		MaxOutputTokens:     selected.MaxOutput,
	}
	input.ModelContract = contract
	return input, contract, selected, nil
}

// selectModelProvider resolves the single enabled model provider and its
// qualification (DATA-CONN-003: one enabled provider; the explicit
// qualification must close onto the current pair).
func selectModelProvider(ctx context.Context, conn *sql.Conn) (provider, error) {
	var selected provider
	var qualificationRowVersion, connectionRowVersion int64
	var probeOutcome string
	err := conn.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       q.probe_result_id, q.enabled_row_version, c.row_version, p.outcome
		FROM connections c
		JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0
		ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen,
			&selected.ProbeResultID, &qualificationRowVersion, &connectionRowVersion, &probeOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return provider{}, ErrModelProviderMissing
	}
	if err != nil {
		return provider{}, err
	}
	if qualificationRowVersion != connectionRowVersion || probeOutcome != "passed" {
		return provider{}, ErrModelProviderMissing
	}
	if err := conn.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens,
		       streaming_supported, native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`,
		selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput,
			&selected.Streaming, &selected.NativeToolCalling); err != nil {
		return provider{}, err
	}
	if selected.ChatModelID == "" || !selected.NativeToolCalling {
		return provider{}, ErrModelProviderMissing
	}
	return selected, nil
}

// insertAttempt persists one Queued attempt with its frozen input snapshot,
// input items and chat_model grant (DATA-ATTEMPT-001/002).
func insertAttempt(ctx context.Context, conn *sql.Conn, analysisID int64, digestHex string, input Input, selected provider, now string) (int64, error) {
	attemptInsert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('initial_analysis','analysis',?,'Queued',?,?,?)`, analysisID, attempt.ReleaseVersion(), attempt.AgentVersion, now)
	if err != nil {
		return 0, err
	}
	attemptID, err := attemptInsert.LastInsertId()
	if err != nil {
		return 0, err
	}
	snapshotInsert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, SchemaKind, RendererVersion, digestHex, now)
	if err != nil {
		return 0, err
	}
	snapshotID, err := snapshotInsert.LastInsertId()
	if err != nil {
		return 0, err
	}
	occurrenceID, err := strconv.ParseInt(input.Occurrence.ID, 10, 64)
	if err != nil {
		return 0, err
	}
	occurrenceDigest := sha256.Sum256([]byte("occurrence:" + input.Occurrence.ID))
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,occurrence_id)
		VALUES(?,1,'user',?,?)`, snapshotID, hex.EncodeToString(occurrenceDigest[:]), occurrenceID); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		attemptID, "chat_model", selected.ConnectionID, selected.RevisionID, selected.CredentialGen, selected.ProbeResultID, now); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// recordAudit appends the narrow audit event in the caller's transaction
// (audit co-commit; a failure rolls the domain write back).
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
