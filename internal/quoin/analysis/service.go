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
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/tools/kubernetes"
	"github.com/Suknna/quoin/internal/quoin/tools/thanos"
)

// SchemaKind is the frozen input schema identifier for initial-analysis
// attempt snapshots (DATA-ATTEMPT-001: attempt_type + "_v1").
const SchemaKind = "initial_analysis_v1"

// OutputSchemaKind is the frozen result payload schema identifier
// (RUNTIME-TASK-012).
const OutputSchemaKind = "initial_analysis_output_v1"

// RendererVersion identifies the input renderer generation both sides
// agree on (ARCH-CONTEXT-006).
// Renderer v3 replaced Label Contract context with declaration resource scopes.
// Renderer v4 makes the declaration optional (ADR-0004): attempts created
// without an eligible business view freeze the enabled integrations as their
// source-level authority instead. Rebuild retains v1/v2/v3 paths so
// historical snapshot bytes stay exact.
const RendererVersion = "initial-analysis-renderer-v4"

// Errors the HTTP surface maps onto the frozen status codes.
var (
	ErrNotFound              = errors.New("initial analysis not found")
	ErrModelProviderMissing  = errors.New("no enabled qualified model provider")
	ErrActiveConflict        = errors.New("initial analysis is not retryable or the fence lost the race")
	ErrCommandReplayMismatch = errors.New("client command id is already bound to a different analysis operation or target")
	ErrNoOutput              = errors.New("initial analysis has no sealed output")
	ErrLateResult            = errors.New("result proposal lost the commit-order race")
	ErrOutputSealed          = errors.New("initial analysis already sealed an output")
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
	evidence *evidence.Service
	now      func() time.Time
	// ProjectTerminalOutcome receives the terminal execution sequence while its
	// SQLite transaction is still open. It projects independent platform facts
	// atomically with the authoritative attempt transition.
	ProjectTerminalOutcome func(ctx context.Context, conn *sql.Conn, commitSequence int64, succeeded bool, termination string) error
	// commandReplay is the bounded in-process idempotency ledger
	// (principal, client_command_id) -> analysis result, mirroring the
	// alert-source precedent; the frozen client_commands table is
	// persisted by a later ticket.
	replayMu sync.Mutex
	replay   map[string]replayEntry
}

type replayEntry struct {
	operation  string
	analysisID int64
	attemptID  int64
	// targetAnalysisID distinguishes a retry/cancel command's requested
	// historical record from the newly created recovery analysis it returns.
	targetAnalysisID int64
	occurrenceID     int64
}

// NewService builds the analysis service on the product database and
// wires the deterministic input rebuilder and the tool observation hooks
// (grant resolution/validation for thanos_query, deterministic Evidence)
// into the shared attempt machine.
func NewService(db *sql.DB) *Service {
	service := &Service{
		db:       db,
		attempts: attempt.NewService(db),
		now:      func() time.Time { return time.Now().UTC() },
		replay:   map[string]replayEntry{},
	}
	service.attempts.SnapshotRebuilder = service.RebuildInput
	service.evidence = evidence.NewService(db)
	service.evidence.RegisterProjector(thanos.QueryToolName, thanos.EvidenceFor)
	service.evidence.RegisterProjector(kubernetes.ReadToolName, kubernetes.EvidenceFor)
	service.attempts.ToolGrantResolver = func(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64, tool attempt.ToolDef) (attempt.ToolResolution, error) {
		switch tool.Name {
		case thanos.QueryToolName:
			// ResolveQueryGrant returns the full resolution (grants + preflight).
			return thanos.ResolveQueryGrant(ctx, conn, attemptID, toolCallID)
		case kubernetes.ReadToolName:
			return kubernetes.ResolveRead(ctx, conn, attemptID, toolCallID)
		default:
			return attempt.ToolResolution{}, fmt.Errorf("tool %s has no grant resolver", tool.Name)
		}
	}
	service.attempts.ToolGrantValidator = func(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64, tool attempt.ToolDef) error {
		switch tool.Name {
		case thanos.QueryToolName:
			return thanos.ValidateGrantForExecution(ctx, conn, attemptID, toolCallID)
		case kubernetes.ReadToolName:
			// The TOCTOU fence lives at fulfillment, not here: every
			// FetchCredentialGrant for purpose kubernetes_read re-validates
			// enabled/revision/generation/root binding per grant inside
			// FulfillGrant's IMMEDIATE transaction (connections/grant.go ->
			// kubernetes.ValidateGrantForFulfillment, pinned by
			// TestValidateGrantForFulfillment*). Checking every mapping here
			// would let one invalid connection reject valid siblings before
			// partial results reach the model.
			return nil
		default:
			return fmt.Errorf("tool %s has no grant validator", tool.Name)
		}
	}
	service.attempts.EvidenceWriter = service.evidence.WriteForToolCall
	return service
}

// Attempts exposes the shared attempt state machine to the runtime slice.
func (service *Service) Attempts() *attempt.Service { return service.attempts }

// Evidence exposes the evidence authority to the app layer (read paths
// and the deterministic projector registry).
func (service *Service) Evidence() *evidence.Service { return service.evidence }

// DB exposes the product database to the app layer for read-only routing
// queries (attempt type lookups etc.).
func (service *Service) DB() *sql.DB { return service.db }

func (service *Service) nowText() string { return service.now().Format(time.RFC3339Nano) }

func (service *Service) replayKey(principalID int64, commandID string) string {
	return strconv.FormatInt(principalID, 10) + ":" + commandID
}

func (service *Service) replayLookup(principalID int64, commandID, operation string, targetAnalysisID, occurrenceID int64) (replayEntry, bool, error) {
	service.replayMu.Lock()
	defer service.replayMu.Unlock()
	entry, ok := service.replay[service.replayKey(principalID, commandID)]
	if !ok {
		return replayEntry{}, false, nil
	}
	// A client command id is an idempotency key for exactly one semantic
	// operation and target. Returning a prior create/cancel/retry result for a
	// different request would direct a caller to unrelated mutable work.
	if entry.operation != operation || entry.targetAnalysisID != targetAnalysisID ||
		(occurrenceID != 0 && entry.occurrenceID != occurrenceID) {
		return replayEntry{}, false, ErrCommandReplayMismatch
	}
	return entry, true, nil
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

// Input is the rendered, immutable input of one analysis. BusinessContext is
// the published declaration that scopes every metrics observation proposed by
// this attempt; when no eligible business view exists (ADR-0004) it is absent
// and Integrations carries the source-level authority instead.
type Input struct {
	Occurrence OccurrenceContext `json:"occurrence"`
	// BusinessContext stays nil exactly when the attempt froze integrations;
	// the declaration view narrows, it never widens source-level authority.
	BusinessContext *BusinessContext      `json:"businessContext,omitempty"`
	Integrations    []RenderedIntegration `json:"integrations,omitempty"`
	ModelContract   ModelContract         `json:"modelContract"`
	// ToolCatalog is the attempt's frozen model tool catalog (ADR-0004);
	// the snapshot digest covers it via this embedding.
	ToolCatalog *attempt.FrozenCatalog `json:"toolCatalog,omitempty"`
}

// RenderedIntegration is one admin-enabled integration frozen into the
// attempt input as its source-level read-only authority. Kind is
// "metrics" (Prometheus/Thanos) or "kubernetes"; credentials never appear.
type RenderedIntegration struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// BusinessContext is the declaration-derived scope exposed to the model. It
// excludes connections and secrets; resource policies are frozen inside the
// version's declaration_json and are the only metrics authority for new work.
type BusinessContext struct {
	SystemKey       string                      `json:"systemKey"`
	ConfigVersionID string                      `json:"configVersionId"`
	Resources       []config.ResourceProjection `json:"resources,omitempty"`
	// Historical snapshots retain these fields solely for byte-exact rebuild.
	LabelContractVersionID string `json:"labelContractVersionId,omitempty"`
	BusinessSystemLabel    string `json:"businessSystemLabel,omitempty"`
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
	return service.create(ctx, occurrenceID, principalID, clientCommandID, "create", 0)
}

// create performs a create or recovery-retry using the same durable admission
// path, while keeping their idempotency identities distinct.
func (service *Service) create(ctx context.Context, occurrenceID, principalID int64, clientCommandID, operation string, targetAnalysisID int64) (CreateResult, error) {
	if entry, ok, err := service.replayLookup(principalID, clientCommandID, operation, targetAnalysisID, occurrenceID); err != nil {
		return CreateResult{}, err
	} else if ok {
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
		service.replayRemember(principalID, clientCommandID, replayEntry{operation: operation, targetAnalysisID: targetAnalysisID, occurrenceID: occurrenceID, analysisID: activeID, attemptID: attemptID})
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CreateResult{}, err
	}
	input, _, selected, err := service.renderInput(ctx, conn, occurrenceID)
	if err != nil {
		return CreateResult{}, err
	}
	// Freeze THIS attempt's tool catalog at creation: the identical document
	// travels in the digested input and in attempt_input_snapshots.
	catalogDocument, catalog, err := attempt.FrozenCatalogJSONForCreation(service.attempts.Catalogs, attempt.AgentVersion)
	if err != nil {
		return CreateResult{}, err
	}
	input.ToolCatalog = catalog
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
	attemptID, err := insertAttempt(ctx, conn, analysisID, digestHex, input, selected, now, string(catalogDocument))
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
	service.replayRemember(principalID, clientCommandID, replayEntry{operation: operation, targetAnalysisID: targetAnalysisID, occurrenceID: occurrenceID, analysisID: analysisID, attemptID: attemptID})
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
	// Creation and dispatch rebuild must project the exact same first accepted
	// Alertmanager item. Otherwise its frozen digest cannot pass the dispatch
	// immutability fence after annotations are added to the input contract.
	if err := populateOccurrenceAnnotations(ctx, conn, occurrenceID, &input.Occurrence); err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	// ADR-0004: the business view is descriptive context only; the enabled
	// integrations are ALWAYS the attempt's source-level authority. A
	// published declaration can never grant or scope new work.
	integrations, err := enabledIntegrations(ctx, conn)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	input.Integrations = integrations
	businessContext, err := resolveBusinessContext(ctx, conn, occurrenceID)
	if err != nil {
		return Input{}, ModelContract{}, provider{}, err
	}
	input.BusinessContext = businessContext
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

// resolveBusinessContext closes occurrence attribution onto an eligible
// published declaration. ADR-0004: a missing or structurally empty view is
// the source-level mainline (nil, nil) — no longer an admission error. A
// malformed frozen declaration stays a hard error.
func resolveBusinessContext(ctx context.Context, conn *sql.Conn, occurrenceID int64) (*BusinessContext, error) {
	var configVersionID int64
	var declarationJSON string
	err := conn.QueryRowContext(ctx, `
		SELECT config.id, config.declaration_json
		FROM alert_occurrences occurrence
		JOIN business_systems business ON business.id=occurrence.business_system_id
		JOIN business_system_config_versions config ON config.id=business.current_config_version_id
		WHERE occurrence.id=? AND business.enabled=1 AND config.state='published'
		  AND config.published_at IS NOT NULL AND config.declaration_json IS NOT NULL`, occurrenceID).
		Scan(&configVersionID, &declarationJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		return nil, fmt.Errorf("decode frozen business declaration: %w", err)
	}
	if declaration.SystemKey == "" || declaration.MetricsConnectionID <= 0 || len(declaration.Resources) == 0 {
		return nil, nil
	}
	return &BusinessContext{
		SystemKey:       declaration.SystemKey,
		ConfigVersionID: strconv.FormatInt(configVersionID, 10),
		Resources:       append([]config.ResourceProjection(nil), declaration.Resources...),
	}, nil
}

// enabledIntegrations lists the admin-enabled observation integrations in
// deterministic name order. Each entry freezes the connection's current
// revision when the attempt items are written, so the model-visible source
// authority is exactly the grant-eligible set.
func enabledIntegrations(ctx context.Context, conn *sql.Conn) ([]RenderedIntegration, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT name, type FROM connections
		WHERE type IN ('thanos','prometheus','kubernetes') AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var name, connectionType string
		if err := rows.Scan(&name, &connectionType); err != nil {
			return nil, err
		}
		kind := "metrics"
		if connectionType == "kubernetes" {
			kind = "kubernetes"
		}
		integrations = append(integrations, RenderedIntegration{Kind: kind, Name: name})
	}
	return integrations, rows.Err()
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
func insertAttempt(ctx context.Context, conn *sql.Conn, analysisID int64, digestHex string, input Input, selected provider, now, toolCatalogJSON string) (int64, error) {
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
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,tool_catalog_json,created_at)
		VALUES(?,?,?,?,?,?)`, attemptID, SchemaKind, RendererVersion, digestHex, toolCatalogJSON, now)
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
		VALUES(?,1,'occurrence',?,?)`, snapshotID, hex.EncodeToString(occurrenceDigest[:]), occurrenceID); err != nil {
		return 0, err
	}
	// Every attempt freezes the enabled integrations at their current
	// revisions — the authoritative grant-eligible set (ADR-0004). An
	// attributed occurrence additionally freezes its business config version
	// lineage as descriptive model context; it never grants authority.
	if input.BusinessContext != nil {
		configVersionID, err := strconv.ParseInt(input.BusinessContext.ConfigVersionID, 10, 64)
		if err != nil || configVersionID <= 0 {
			return 0, fmt.Errorf("analysis business configuration context is missing")
		}
		configDigest := sha256.Sum256([]byte("business-system-config-version:" + input.BusinessContext.ConfigVersionID))
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
			VALUES(?,2,'business_config',?,?)`, snapshotID, hex.EncodeToString(configDigest[:]), configVersionID); err != nil {
			return 0, err
		}
	}
	if err := insertSourceLineageItems(ctx, conn, snapshotID, 3, input.Integrations); err != nil {
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

// insertSourceLineageItems freezes one input item per enabled integration at
// its current revision. The revision pointer is the frozen fact: later
// rotations create new revisions, so the attempt's authorized set stays
// reconstructible byte-for-byte (and later grants must match these items).
func insertSourceLineageItems(ctx context.Context, conn *sql.Conn, snapshotID, firstSeq int64, integrations []RenderedIntegration) error {
	if len(integrations) == 0 {
		return nil
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT name, current_revision_id FROM connections
		WHERE type IN ('thanos','prometheus','kubernetes') AND enabled=1 AND revalidation_required=0
		ORDER BY name, type`)
	if err != nil {
		return err
	}
	type frozen struct {
		name       string
		revisionID int64
	}
	var frozenIntegrations []frozen
	for rows.Next() {
		var item frozen
		if err := rows.Scan(&item.name, &item.revisionID); err != nil {
			rows.Close()
			return err
		}
		frozenIntegrations = append(frozenIntegrations, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The integration list rendered into the digest and the frozen items must
	// be the same set; a concurrent enablement inside this IMMEDIATE
	// transaction is impossible, so a size mismatch is a programming error.
	if len(frozenIntegrations) != len(integrations) {
		return fmt.Errorf("integration snapshot drift: %d rendered vs %d frozen", len(integrations), len(frozenIntegrations))
	}
	for index, item := range frozenIntegrations {
		role := "metrics_source"
		if integrations[index].Kind == "kubernetes" {
			role = "kubernetes_source"
		}
		digest := sha256.Sum256([]byte("connection-revision:" + strconv.FormatInt(item.revisionID, 10)))
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,connection_revision_id)
			VALUES(?,?,?, ?,?)`, snapshotID, firstSeq+int64(index), role, hex.EncodeToString(digest[:]), item.revisionID); err != nil {
			return err
		}
	}
	return nil
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
