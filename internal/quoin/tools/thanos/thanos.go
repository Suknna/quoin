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
	"database/sql"
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
	QueryToolVersion = "1"
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

// ResolveQueryGrant derives authority exclusively from the analysis snapshot:
// the occurrence must have an immutable attributed business and that business's
// exact published configuration/versioned Label Contract must be pinned in the
// attempt input. It rejects a missing, stale, unpublished, disabled, or unsafe
// context before a grant row exists; the model never chooses a connection or
// relaxes the Label Contract selector scope.
func ResolveQueryGrant(ctx context.Context, conn *sql.Conn, attemptID, toolCallID int64) (attempt.ToolGrant, error) {
	var (
		businessSystemID, configVersionID, connectionID        int64
		systemKey, businessLabel, query                        string
		revisionID, generationID, bindingRevision, rootBinding int64
	)
	if err := conn.QueryRowContext(ctx, `
		SELECT occurrence.business_system_id, config_item.business_system_config_version_id,
		       config.system_key,
		       json_extract(contract.contract_json, '$.label_contract.business_system_label'),
		       json_extract(tool.arguments_json, '$.query')
		FROM tool_calls tool
		JOIN execution_attempts attempt ON attempt.id=tool.attempt_id
		JOIN attempt_input_snapshots snapshot ON snapshot.attempt_id=attempt.id
		JOIN attempt_input_items occurrence_item ON occurrence_item.snapshot_id=snapshot.id AND occurrence_item.occurrence_id IS NOT NULL
		JOIN alert_occurrences occurrence ON occurrence.id=occurrence_item.occurrence_id
		JOIN attempt_input_items config_item ON config_item.snapshot_id=snapshot.id AND config_item.business_system_config_version_id IS NOT NULL
		JOIN business_system_config_versions config ON config.id=config_item.business_system_config_version_id
		JOIN attempt_input_items contract_item ON contract_item.snapshot_id=snapshot.id AND contract_item.label_contract_version_id IS NOT NULL
		JOIN label_contracts contract ON contract.id=contract_item.label_contract_version_id
		WHERE tool.id=? AND tool.attempt_id=?
		  AND occurrence.business_system_id=config.business_system_id
		  AND config.label_contract_version_id=contract.id
		  -- Publication is immutable; a later publish may supersede this frozen
		  -- version without changing authority already bound to this attempt.
		  AND config.published_at IS NOT NULL`,
		toolCallID, attemptID,
	).Scan(&businessSystemID, &configVersionID, &systemKey, &businessLabel, &query); err != nil {

		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ToolGrant{}, fmt.Errorf("%w: analysis has no eligible published business configuration context", ErrThanosUnavailable)
		}
		return attempt.ToolGrant{}, err
	}
	if fields := config.ValidateCheckExpression(query, businessLabel, systemKey, "query"); len(fields) != 0 {
		return attempt.ToolGrant{}, fmt.Errorf("%w: query violates business Label Contract scope: %s", ErrThanosUnavailable, fields[0].Reason)
	}
	if err := conn.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       g.key_binding_revision, s.binding_revision
		FROM business_system_config_versions config
		JOIN connections c ON c.id=config.metrics_connection_id
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE config.id=? AND config.business_system_id=?
		  AND c.type IN ('thanos','prometheus') AND c.enabled=1 AND c.revalidation_required=0`,
		configVersionID, businessSystemID,
	).Scan(&connectionID, &revisionID, &generationID, &bindingRevision, &rootBinding); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ToolGrant{}, fmt.Errorf("%w: configured metrics connection is unavailable", ErrThanosUnavailable)
		}
		return attempt.ToolGrant{}, err
	}
	if bindingRevision != rootBinding {
		return attempt.ToolGrant{}, fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRevision, rootBinding)
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,business_system_id,connection_id,connection_revision_id,
			credential_generation_id,created_by_tool_call_id,created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		attemptID, "thanos_query", businessSystemID, connectionID, revisionID, generationID, toolCallID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return attempt.ToolGrant{}, err
	}
	grantID, err := insert.LastInsertId()
	if err != nil {
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
