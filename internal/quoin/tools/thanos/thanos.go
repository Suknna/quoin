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
	"strings"
	"time"

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/evidence"
	"github.com/Suknna/quoin/internal/quoin/execution"
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

// Recoverable preflight codes (the frozen tool_calls.preflight_error_code
// vocabulary): routing misses return these as model-visible Tool Results
// instead of failing the attempt, so the model can ask the user or retry
// with an explicit sourceRef (ADR-0004: ambiguity is never resolved by
// picking the first source or querying all of them).
const (
	PreflightTargetNotFound  = "target_not_found"
	PreflightTargetAmbiguous = "target_ambiguous"
	PreflightNoMapping       = "no_mapping"
)

// QueryToolPurpose is the grant purpose of per-tool-call thanos_query
// authorizations.
const QueryToolPurpose = "thanos_query"

// frozenSource is one source binding frozen as an attempt input item when
// the attempt was created: the exact connection revision that was current
// and enabled at freeze time.
type frozenSource struct {
	ConnectionID int64
	RevisionID   int64
	Name         string
}

// frozenDeclarationSourceItems loads the attempt's frozen source items of
// one role, joined to their stable connection names.
func frozenDeclarationSourceItems(ctx context.Context, conn execution.Executor, attemptID int64, itemRole string) ([]frozenSource, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.id, item.connection_revision_id, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=? AND item.item_role=?
		ORDER BY c.name`, attemptID, itemRole)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []frozenSource
	for rows.Next() {
		var source frozenSource
		if err := rows.Scan(&source.ConnectionID, &source.RevisionID, &source.Name); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// sourcePreflight renders a recoverable routing miss.
func sourcePreflight(code string, detail string) attempt.ToolResolution {
	return attempt.ToolResolution{PreflightCode: code, PreflightDetail: detail}
}

// namesOf renders the bounded candidate list carried by preflight details.
func namesOf(sources []frozenSource) string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return strings.Join(names, "、")
}

// currentConnectionPair re-reads the connection's current binding and root
// state inside the caller's transaction. enabled=false reports an admin
// disable/revalidation without an error so callers can preflight.
func currentConnectionPair(ctx context.Context, conn execution.Executor, connectionID int64) (revisionID, generationID int64, enabled bool, err error) {
	var (
		revalidation int
		bindingRev   int64
		rootBinding  int64
	)
	err = conn.QueryRowContext(ctx, `
		SELECT c.current_revision_id, c.current_credential_generation_id, c.enabled, c.revalidation_required,
		       g.key_binding_revision, s.binding_revision
		FROM connections c
		JOIN credential_generations g ON g.id=c.current_credential_generation_id
		CROSS JOIN root_key_state s
		WHERE c.id=?`, connectionID).
		Scan(&revisionID, &generationID, &enabled, &revalidation, &bindingRev, &rootBinding)
	if err != nil {
		return 0, 0, false, err
	}
	if !enabled || revalidation != 0 {
		return 0, 0, false, nil
	}
	if bindingRev != rootBinding {
		return 0, 0, false, fmt.Errorf("%w: credential root binding %d does not match %d", ErrGrantNotCurrent, bindingRev, rootBinding)
	}
	return revisionID, generationID, true, nil
}

// freezeSourceExecution persists the canonical execution request and the
// per-call grant binding for one resolved source. The grant deliberately
// reuses the frozen (connection, revision, generation) triple: one binding
// per attempt and connection authorizes every identical call, while each
// Tool Call keeps its own auditable association row（ADR-0004 来源级授权；
// business_system 授权列已随 ADR-0012 整域退役）。
func freezeSourceExecution(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, executionArgs any, source frozenSource, generationID, frozenRevisionID int64) (attempt.ToolGrant, error) {
	canonical, err := json.Marshal(executionArgs)
	if err != nil {
		return attempt.ToolGrant{}, err
	}
	digest := sha256.Sum256(canonical)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO tool_call_execution_inputs(tool_call_id,arguments_json,arguments_digest,created_at)
		VALUES(?,?,?,?)`, toolCallID, string(canonical), hex.EncodeToString(digest[:]), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return attempt.ToolGrant{}, err
	}
	var grantID int64
	err = conn.QueryRowContext(ctx, `
		SELECT id FROM attempt_connection_grants
		WHERE attempt_id=? AND purpose=? AND connection_id=? AND connection_revision_id=? AND credential_generation_id=?`,
		attemptID, QueryToolPurpose, source.ConnectionID, frozenRevisionID, generationID).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		insert, insertErr := conn.ExecContext(ctx, `
			INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,
				credential_generation_id,created_by_tool_call_id,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			attemptID, QueryToolPurpose, source.ConnectionID, frozenRevisionID, generationID, toolCallID, time.Now().UTC().Format(time.RFC3339Nano))
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
	return attempt.ToolGrant{GrantID: grantID, ConnectionRevisionID: frozenRevisionID, CredentialGenerationID: generationID, Purpose: QueryToolPurpose}, nil
}

// ResolveQueryGrant authorizes one proposed thanos_query call inside the
// Tool Call persistence transaction. Authority derives solely from the
// attempt's frozen metrics_source items (ADR-0004): one per admin-enabled
// metrics connection at creation. Historical declarations never grant new
// authority — a published declaration cannot scope or widen a new attempt.
// The model names the source explicitly (sourceRef) whenever the frozen
// list is ambiguous; zero or several candidates without a name is a
// recoverable preflight result, never a silent first-pick.
func ResolveQueryGrant(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64) (attempt.ToolResolution, error) {
	var resourceRef, sourceRef, query sql.NullString
	if err := conn.QueryRowContext(ctx, `
		SELECT json_extract(arguments_json, '$.resourceRef'),
		       json_extract(arguments_json, '$.sourceRef'),
		       json_extract(arguments_json, '$.query')
		FROM tool_calls WHERE id=? AND attempt_id=?`, toolCallID, attemptID).
		Scan(&resourceRef, &sourceRef, &query); err != nil {
		return attempt.ToolResolution{}, err
	}
	if !query.Valid || query.String == "" {
		return attempt.ToolResolution{}, fmt.Errorf("%w: proposed query is empty", ErrThanosUnavailable)
	}
	// Defense in depth: even a raw low-level proposal (frozen catalog drift,
	// historical catalogs) must meet the same v3 shape the offered catalog
	// enforces — the removed resourceRef vocabulary can never route again.
	if resourceRef.Valid && resourceRef.String != "" {
		return sourcePreflight(PreflightTargetNotFound, "resourceRef 已从 thanos_query 移除；请改用 sourceRef 指明指标接入。"), nil
	}
	return resolveSourceQueryGrant(ctx, conn, attemptID, toolCallID, sourceRef.String, query.String)
}

// resolveSourceQueryGrant authorizes the only thanos_query path: the
// admin-enabled integrations frozen as metrics_source items are the
// read-only scope.
func resolveSourceQueryGrant(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64, sourceRef, query string) (attempt.ToolResolution, error) {
	sources, err := frozenDeclarationSourceItems(ctx, conn, attemptID, "metrics_source")
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	if sourceRef != "" {
		var matched []frozenSource
		for _, source := range sources {
			if source.Name == sourceRef {
				matched = append(matched, source)
			}
		}
		if len(matched) == 0 {
			if len(sources) == 0 {
				return sourcePreflight(PreflightNoMapping, "本次分析没有已授权的指标接入；请管理员先启用 Thanos/Prometheus 接入。"), nil
			}
			return sourcePreflight(PreflightTargetNotFound, "未找到该指标接入，可用来源："+namesOf(sources)+"。"), nil
		}
		sources = matched
	} else {
		switch len(sources) {
		case 0:
			return sourcePreflight(PreflightNoMapping, "本次分析没有已授权的指标接入；请管理员先启用 Thanos/Prometheus 接入。"), nil
		case 1:
		default:
			return sourcePreflight(PreflightTargetAmbiguous, "存在多个已授权的指标接入，请用 sourceRef 明确指定："+namesOf(sources)+"。"), nil
		}
	}
	selected := sources[0]
	// The grant must close onto the exact frozen revision while that
	// revision is still the enabled current pair; anything else is a
	// recoverable routing miss (a fresh analysis re-freezes sources).
	revisionID, generationID, enabled, err := currentConnectionPair(ctx, conn, selected.ConnectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt.ToolResolution{}, fmt.Errorf("%w: source connection disappeared", ErrThanosUnavailable)
	}
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	if !enabled || revisionID != selected.RevisionID {
		return sourcePreflight(PreflightNoMapping, fmt.Sprintf("指标接入 %q 已停用或已轮换；请管理员重新启用后发起新的分析。", selected.Name)), nil
	}
	grant, err := freezeSourceExecution(ctx, conn, attemptID, toolCallID, map[string]string{"query": query, "sourceRef": selected.Name}, selected, generationID, selected.RevisionID)
	if err != nil {
		return attempt.ToolResolution{}, err
	}
	return attempt.ToolResolution{Grants: []attempt.ToolGrant{grant}}, nil
}

// ResolveConfigGrantForConnection freezes one exact Prometheus-compatible
// connection for a deterministic Config Verification or Inspection attempt.
// The declaration locator is mandatory: historical attempts retain their
// already-created immutable grants rather than re-resolving a global default.
// conn is the execution.Executor surface, so grant freezing composes both
// inside plain caller connections and inside the shared execution runner's
// guarded transaction (ADR-0006).
func ResolveConfigGrantForConnection(ctx context.Context, conn execution.Executor, attemptID, requiredConnectionID int64) (attempt.ToolGrant, error) {
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
func ValidateConfigGrantForExecution(ctx context.Context, conn execution.Executor, attemptID int64) error {
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
	switch {
	case enabled != 1 || revalidation != 0:
		return fmt.Errorf("%w: connection disabled or pending revalidation", ErrGrantNotCurrent)
	case !currentRevisionID.Valid || !currentGenID.Valid ||
		currentRevisionID.Int64 != grantRevisionID || currentGenID.Int64 != grantGenerationID:
		return fmt.Errorf("%w: frozen revision/generation pair no longer current", ErrGrantNotCurrent)
	case bindingRevision != rootBinding:
		return fmt.Errorf("%w: credential root binding drifted", ErrGrantNotCurrent)
	}
	return nil
}

// ValidateGrantForExecution re-checks the frozen binding before a pending
// thanos_query tool call may begin executing (DATA-CONN-002: the execution
// authorization transaction re-reads the connection state; a disable,
// rotation or root rebind committed first refuses execution).
func ValidateGrantForExecution(ctx context.Context, conn execution.Executor, attemptID, toolCallID int64) error {
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
