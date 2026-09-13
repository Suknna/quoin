// Package thanos owns the Quoin-side authority of the frozen thanos_query
// Tool (T11, ARCH-TOOL-003/005, ARCH-INPUT-003, DATA-CONN-002/006): the
// deterministic resolution of the single enabled deployment Thanos
// connection inside the Tool Call persistence transaction, the execution
// authorization re-check, the frozen thanos_query_result_v1 shape and the
// deterministic Evidence projection of one observation. The Plinth
// supervisor executes the actual HTTP query; Quoin only ever sees the
// sealed result payload plus the committed Artifact, and never the
// connection secret.
package thanos

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/config"
	"github.com/Suknna/quoin/internal/quoin/evidence"
)

// Frozen tool identity (byte-pinned with the worker-side catalog and the
// provider-facing schema digest; internal/quoin/attempt/tools.go).
const (
	QueryToolName    = "thanos_query"
	QueryToolVersion = "2"
	// QueryResultSchemaKind is the frozen CompleteToolCall payload schema
	// identifier the supervisor seals (RUNTIME-AGENT-008).
	QueryResultSchemaKind = "thanos_query_result_v1"
)

// ErrThanosUnavailable reports that no enabled qualified Thanos connection
// exists when a thanos_query tool call is authorized (RUNTIME-AGENT-005:
// an unresolvable tool route fails the whole model call).
var ErrThanosUnavailable = errors.New("no enabled thanos connection")

// ErrGrantNotCurrent reports a frozen grant whose connection pair or root
// binding lost currency after the grant was created (DATA-CONN-002: the
// execution authorization re-check failed).
var ErrGrantNotCurrent = errors.New("thanos grant is no longer current")

// ResolveQueryGrant derives authority solely from the config-version input item
// frozen for this attempt. New attempts never consult Label Contracts or resolve
// names: declaration_json carries the reviewed connection locator and compiled
// resource allowlist. The model must name one resource explicitly; Quoin scopes
// the query AST before handing the canonical arguments to Plinth.
func ResolveQueryGrant(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64) (attempt.ToolGrant, error) {
	var (
		businessSystemID, configVersionID, connectionID        int64
		declarationJSON, resourceRef, query                    string
		revisionID, generationID, bindingRevision, rootBinding int64
	)
	if err := conn.QueryRowContext(ctx, `
		SELECT config.business_system_id, config_item.business_system_config_version_id,
		       config.declaration_json, json_extract(tool.arguments_json, '$.resourceRef'),
		       json_extract(tool.arguments_json, '$.query')
		FROM tool_calls tool
		JOIN attempt_input_snapshots snapshot ON snapshot.attempt_id=tool.attempt_id
		JOIN attempt_input_items config_item ON config_item.snapshot_id=snapshot.id
			AND config_item.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions config ON config.id=config_item.business_system_config_version_id
		WHERE tool.id=? AND tool.attempt_id=?
		  AND config.published_at IS NOT NULL
		  AND config.declaration_json IS NOT NULL`, toolCallID, attemptID,
	).Scan(&businessSystemID, &configVersionID, &declarationJSON, &resourceRef, &query); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ToolGrant{}, fmt.Errorf("%w: attempt has no migrated published configuration declaration", ErrThanosUnavailable)
		}
		return attempt.ToolGrant{}, err
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		return attempt.ToolGrant{}, fmt.Errorf("%w: frozen declaration is invalid: %v", ErrThanosUnavailable, err)
	}
	if declaration.MetricsConnectionID <= 0 || resourceRef == "" || query == "" {
		return attempt.ToolGrant{}, fmt.Errorf("%w: frozen declaration or explicit resourceRef is missing", ErrThanosUnavailable)
	}
	// Every vector selector must satisfy the frozen resource policy before a
	// connection grant can authorize execution, including nested expressions.
	scope, err := declaration.CompileResourceScope(resourceRef)
	if err != nil {
		return attempt.ToolGrant{}, fmt.Errorf("%w: resourceRef is not declared: %v", ErrThanosUnavailable, err)
	}
	normalizedQuery, err := scope.ScopeExpression(query)
	if err != nil {
		return attempt.ToolGrant{}, fmt.Errorf("%w: query violates declared resource scope: %v", ErrThanosUnavailable, err)
	}
	// Preserve the model proposal on tool_calls and freeze the independently
	// authorized execution request. Plinth must receive this exact canonical JSON.
	executionArgs, err := json.Marshal(map[string]string{"resourceRef": resourceRef, "query": normalizedQuery})
	if err != nil {
		return attempt.ToolGrant{}, err
	}
	digest := sha256.Sum256(executionArgs)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO tool_call_execution_inputs(tool_call_id,arguments_json,arguments_digest,created_at)
		VALUES(?,?,?,?)`, toolCallID, string(executionArgs), hex.EncodeToString(digest[:]), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return attempt.ToolGrant{}, err
	}
	connectionID = declaration.MetricsConnectionID
	if err := conn.QueryRowContext(ctx, `
		SELECT c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM connections c
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE c.id=? AND c.type IN ('thanos','prometheus') AND c.enabled=1 AND c.revalidation_required=0`,
		connectionID,
	).Scan(&revisionID, &generationID, &bindingRevision, &rootBinding); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ToolGrant{}, fmt.Errorf("%w: declared metrics connection is unavailable", ErrThanosUnavailable)
		}
		return attempt.ToolGrant{}, err
	}
	if bindingRevision != rootBinding {
		return attempt.ToolGrant{}, fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRevision, rootBinding)
	}
	// A single AgentComplete can propose several metrics calls. The binding
	// identity deliberately excludes the Tool Call because every call made by
	// this Attempt against this exact immutable connection pair has identical
	// authority. Reuse that one grant and record the per-call relationship in
	// tool_call_connection_grants; the first creating Tool Call remains auditable
	// in created_by_tool_call_id without weakening the frozen binding constraint.
	var grantID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM attempt_connection_grants
		WHERE attempt_id=? AND purpose='thanos_query' AND business_system_id=?
		  AND connection_id=? AND connection_revision_id=? AND credential_generation_id=?`,
		attemptID, businessSystemID, connectionID, revisionID, generationID).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `
			INSERT INTO attempt_connection_grants(attempt_id,purpose,business_system_id,connection_id,connection_revision_id,
				credential_generation_id,created_by_tool_call_id,created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			attemptID, "thanos_query", businessSystemID, connectionID, revisionID, generationID, toolCallID, time.Now().UTC().Format(time.RFC3339Nano))
		if insertErr != nil {
			return attempt.ToolGrant{}, insertErr
		}
		grantID, insertErr = insert.LastInsertId()
		if insertErr != nil {
			return attempt.ToolGrant{}, insertErr
		}
	} else if err != nil {
		return attempt.ToolGrant{}, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO tool_call_connection_grants(tool_call_id,connection_grant_id,ordinal) VALUES(?,?,0)`, toolCallID, grantID); err != nil {
		return attempt.ToolGrant{}, err
	}
	return attempt.ToolGrant{GrantID: grantID, ConnectionRevisionID: revisionID, CredentialGenerationID: generationID, Purpose: "thanos_query"}, nil
}

// ResolveConfigGrantForConnection freezes one exact Prometheus-compatible
// connection for a deterministic Config Verification or Inspection attempt.
// The declaration locator is mandatory: historical attempts retain their
// already-created immutable grants rather than re-resolving a global default.
func ResolveConfigGrantForConnection(ctx context.Context, conn *sql.Conn, attemptID, requiredConnectionID int64) (attempt.ToolGrant, error) {
	var connectionID, revisionID, generationID, bindingRevision, rootBinding int64
	if err := conn.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM connections c
		JOIN credential_generations g ON g.id = c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE c.type IN ('thanos','prometheus') AND c.enabled = 1 AND c.revalidation_required = 0
		  AND c.id = ?
		LIMIT 1`, requiredConnectionID).Scan(&connectionID, &revisionID, &generationID, &bindingRevision, &rootBinding); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ToolGrant{}, fmt.Errorf("%w: create or enable a thanos connection first", ErrThanosUnavailable)
		}
		return attempt.ToolGrant{}, err
	}
	if bindingRevision != rootBinding {
		return attempt.ToolGrant{}, fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRevision, rootBinding)
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,created_at)
		VALUES(?,?,?,?,?,?)`, attemptID, "config_thanos_query", connectionID, revisionID, generationID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return attempt.ToolGrant{}, err
	}
	grantID, err := insert.LastInsertId()
	if err != nil {
		return attempt.ToolGrant{}, err
	}
	return attempt.ToolGrant{GrantID: grantID, ConnectionRevisionID: revisionID, CredentialGenerationID: generationID, Purpose: "config_thanos_query"}, nil
}

// ValidateConfigGrantForExecution re-checks the frozen config execution
// grant just before the supervisor starts the query. A connection disable,
// rotation or root-key rebind committed first wins the race.
func ValidateConfigGrantForExecution(ctx context.Context, conn *sql.Conn, attemptID int64) error {
	var grantRevisionID, grantGenerationID, enabled, revalidation, bindingRevision, rootBinding int64
	var currentRevisionID, currentGenID sql.NullInt64
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_revision_id, ag.credential_generation_id,
		       c.enabled, c.revalidation_required, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM attempt_connection_grants ag
		JOIN connections c ON c.id = ag.connection_id
		JOIN credential_generations g ON g.id = ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE ag.attempt_id=? AND ag.purpose='config_thanos_query'`, attemptID).
		Scan(&grantRevisionID, &grantGenerationID, &enabled, &revalidation, &currentRevisionID, &currentGenID, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: config grant binding missing", ErrGrantNotCurrent)
	}
	if err != nil {
		return err
	}
	if enabled != 1 || revalidation != 0 || !currentRevisionID.Valid || !currentGenID.Valid ||
		currentRevisionID.Int64 != grantRevisionID || currentGenID.Int64 != grantGenerationID || bindingRevision != rootBinding {
		return ErrGrantNotCurrent
	}
	return nil
}

// ValidateGrantForExecution re-checks the frozen binding before a pending
// thanos_query tool call may begin executing (DATA-CONN-002: the execution
// authorization transaction re-reads the connection state; a disable,
// rotation or root rebind committed first refuses execution).
func ValidateGrantForExecution(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64) error {
	var (
		grantRevisionID, grantGenerationID int64
		enabled, revalidation              int
		currentRevisionID, currentGenID    sql.NullInt64
		bindingRevision, rootBinding       int64
	)
	err := conn.QueryRowContext(ctx, `
		SELECT ag.connection_revision_id, ag.credential_generation_id,
		       c.enabled, c.revalidation_required, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM tool_call_connection_grants tcg
		JOIN attempt_connection_grants ag ON ag.id = tcg.connection_grant_id
		JOIN connections c ON c.id = ag.connection_id
		JOIN credential_generations g ON g.id = ag.credential_generation_id
		CROSS JOIN root_key_state s
		WHERE tcg.tool_call_id = ? AND ag.attempt_id = ? AND ag.purpose = 'thanos_query'`,
		toolCallID, attemptID,
	).Scan(&grantRevisionID, &grantGenerationID, &enabled, &revalidation,
		&currentRevisionID, &currentGenID, &bindingRevision, &rootBinding)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: grant binding missing", ErrGrantNotCurrent)
	}
	if err != nil {
		return err
	}
	if enabled != 1 || revalidation != 0 {
		return fmt.Errorf("%w: connection disabled or pending revalidation", ErrGrantNotCurrent)
	}
	if !currentRevisionID.Valid || !currentGenID.Valid ||
		currentRevisionID.Int64 != grantRevisionID || currentGenID.Int64 != grantGenerationID {
		return fmt.Errorf("%w: connection pair rotated since the grant", ErrGrantNotCurrent)
	}
	if bindingRevision != rootBinding {
		return fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRevision, rootBinding)
	}
	return nil
}

// ArtifactRef is the bounded artifact locator embedded in a spilled
// result payload (ARCH-OUTPUT-003: the model context only receives the
// locator and size facts, never the full body).
type ArtifactRef struct {
	ID         string `json:"id"`
	MediaType  string `json:"mediaType"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	TotalLines int64  `json:"totalLines"`
}

// Result is the frozen thanos_query_result_v1 payload shape. Success
// carries the bounded output preview (or the full inline body when it fit
// the spill thresholds) plus, when truncated, the Artifact locator of the
// complete raw response. Failure is a structured return_to_model error the
// model sees as a committed Tool Result.
type Result struct {
	Success     bool         `json:"success"`
	Status      string       `json:"status,omitempty"`
	ResultType  string       `json:"resultType,omitempty"`
	SampleCount int          `json:"sampleCount,omitempty"`
	StartedAt   string       `json:"startedAt"`
	FinishedAt  string       `json:"finishedAt"`
	Truncated   bool         `json:"truncated"`
	TotalBytes  int64        `json:"totalBytes"`
	TotalLines  int64        `json:"totalLines"`
	Output      string       `json:"output"`
	Artifact    *ArtifactRef `json:"artifact,omitempty"`
	ErrorCode   string       `json:"errorCode,omitempty"`
	ErrorDetail string       `json:"errorDetail,omitempty"`
}

// ParseResult validates the sealed payload against the frozen result
// schema (RUNTIME-AGENT-008: Quoin validates the fixed result schema in
// the CompleteToolCall transaction).
func ParseResult(canonical []byte) (Result, error) {
	var result Result
	if err := json.Unmarshal(canonical, &result); err != nil {
		return Result{}, fmt.Errorf("thanos_query result unparseable: %w", err)
	}
	if result.Success {
		if result.Status == "" || result.ResultType == "" {
			return Result{}, errors.New("thanos_query success result requires status and resultType")
		}
		if result.StartedAt == "" || result.FinishedAt == "" {
			return Result{}, errors.New("thanos_query success result requires startedAt and finishedAt")
		}
		if _, err := time.Parse(time.RFC3339Nano, result.FinishedAt); err != nil {
			return Result{}, fmt.Errorf("thanos_query finishedAt is not RFC3339: %w", err)
		}
		if result.Output == "" {
			return Result{}, errors.New("thanos_query success result requires output")
		}
		if result.Truncated != (result.Artifact != nil) {
			return Result{}, errors.New("thanos_query truncated flag must pair with the artifact locator")
		}
		if result.Artifact != nil && (result.Artifact.ID == "" || result.Artifact.SHA256 == "" || result.Artifact.SizeBytes < 0) {
			return Result{}, errors.New("thanos_query artifact locator is incomplete")
		}
		return result, nil
	}
	if result.ErrorCode == "" || result.ErrorDetail == "" {
		return Result{}, errors.New("thanos_query failure result requires errorCode and errorDetail")
	}
	if result.StartedAt == "" {
		result.StartedAt = result.FinishedAt
	}
	return result, nil
}

// EvidenceFor derives the deterministic evidence projection of one
// succeeded thanos_query result. The frozen arguments are the canonical
// params; the observation time is the supervisor-observed finish time
// carried by the validated payload.
func EvidenceFor(argumentsJSON, payloadJSON []byte, artifactID int64) (evidence.Projection, error) {
	result, err := ParseResult(payloadJSON)
	if err != nil {
		return evidence.Projection{}, err
	}
	projection := evidence.Projection{
		ParamsJSON: argumentsJSON,
		ObservedAt: result.FinishedAt,
		Integrity:  "complete",
	}
	if result.Truncated {
		if artifactID <= 0 {
			return evidence.Projection{}, errors.New("spilled thanos_query result lacks the committed artifact")
		}
		// The payload's artifact locator must close onto the Artifact the
		// Tool Call completion commits (ARCH-TOOL-003): a locator for any
		// other artifact is a protocol conflict, never a second authority.
		if result.Artifact == nil || result.Artifact.ID != strconv.FormatInt(artifactID, 10) {
			return evidence.Projection{}, errors.New("thanos_query artifact locator does not match the committed artifact")
		}
		projection.ArtifactID = artifactID
	} else {
		projection.ResultJSON = payloadJSON
	}
	return projection, nil
}
