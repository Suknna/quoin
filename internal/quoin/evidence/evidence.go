// Package evidence owns the Evidence authority on the Quoin side
// (DATA-EVIDENCE-001): plinth-tool Evidence rows are committed inside the
// owning Tool Call's terminal transaction (the model never proposes
// Evidence), and every read path projects the frozen EvidenceDetail
// shape. The deterministic tool-specific projection (params, observation
// time, body) is owned by each fixed tool's package and registered here;
// this package owns the row fences and the read projections.
package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrNotFound reports an unknown evidence locator.
var ErrNotFound = errors.New("evidence not found")

// ErrEvidenceDenied reports an evidence write that lost a fence (attempt
// or tool call no longer in the required state).
var ErrEvidenceDenied = errors.New("evidence write denied")

// Projection is the deterministic evidence facts one fixed tool derives
// from its sealed result. Exactly one body position must be set: the
// inline result JSON or the committed Artifact (DATA-EVIDENCE-001).
type Projection struct {
	ParamsJSON   []byte
	ObservedAt   string
	Integrity    string
	ResultJSON   []byte // exclusive with ArtifactID
	ArtifactID   int64
	WarningsJSON []byte
	ErrorsJSON   []byte
}

// Projector derives the deterministic projection of one succeeded
// observation tool from the frozen tool arguments, the sealed payload and
// the committed result artifact.
type Projector func(argumentsJSON, payloadJSON []byte, artifactID int64) (Projection, error)

// Service is the evidence authority.
type Service struct {
	db        *sql.DB
	now       func() time.Time
	projector map[string]Projector
}

// NewService builds the evidence service on the product database.
func NewService(db *sql.DB) *Service {
	return &Service{
		db:        db,
		now:       func() time.Time { return time.Now().UTC() },
		projector: map[string]Projector{},
	}
}

// RegisterProjector wires one fixed tool's deterministic projection
// (wired once at application startup; the catalog is fixed per release).
func (service *Service) RegisterProjector(toolName string, projector Projector) {
	service.projector[toolName] = projector
}

// DB exposes the product database for read-only routing queries.
func (service *Service) DB() *sql.DB { return service.db }

// WriteForToolCall commits the deterministic Evidence for one succeeded
// observation tool inside the caller's transaction, while the Tool Call is
// still running (the frozen trg_evidence_attempt_tool_closure demands the
// running state at the Evidence INSERT; the terminal state advances in the
// same transaction afterwards — ARCH-TOOL-003, DATA-EVIDENCE-001).
func (service *Service) WriteForToolCall(ctx context.Context, conn *sql.Conn, attemptID, toolCallID, artifactID int64, payloadJSON []byte, toolName string) ([]int64, error) {
	var attemptState, scopeType string
	var scopeID int64
	if err := conn.QueryRowContext(ctx, `
		SELECT a.state, a.scope_type, a.scope_id FROM execution_attempts a WHERE a.id=?`,
		attemptID).Scan(&attemptState, &scopeType, &scopeID); err != nil {
		return nil, err
	}
	// Agent attempt scopes whose tools may seal deterministic Evidence
	// (the frozen trg_evidence_attempt_tool_closure is scope-agnostic: it
	// binds a Running attempt to its running tool call; the scope maps onto
	// the evidence target type).
	var targetType string
	switch scopeType {
	case "analysis":
		targetType = "initial_analysis"
	case "investigation":
		targetType = "investigation"
	default:
		return nil, fmt.Errorf("%w: attempt %d is %s/%s", ErrEvidenceDenied, attemptID, attemptState, scopeType)
	}
	if attemptState != "Running" {
		return nil, fmt.Errorf("%w: attempt %d is %s/%s", ErrEvidenceDenied, attemptID, attemptState, scopeType)
	}
	var callAttempt int64
	var callStatus string
	var argumentsJSON string
	if err := conn.QueryRowContext(ctx, `
		SELECT attempt_id,status,arguments_json FROM tool_calls WHERE id=?`,
		toolCallID).Scan(&callAttempt, &callStatus, &argumentsJSON); err != nil {
		return nil, err
	}
	if callAttempt != attemptID {
		return nil, fmt.Errorf("%w: tool call %d belongs to attempt %d", ErrEvidenceDenied, toolCallID, callAttempt)
	}
	if callStatus != "running" {
		return nil, fmt.Errorf("%w: tool call %d is %s", ErrEvidenceDenied, toolCallID, callStatus)
	}
	projector := service.projector[toolName]
	if projector == nil {
		return nil, fmt.Errorf("%w: tool %s has no evidence projector", ErrEvidenceDenied, toolName)
	}
	projection, err := projector([]byte(argumentsJSON), payloadJSON, artifactID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEvidenceDenied, err)
	}
	if projection.Integrity != "complete" && projection.Integrity != "incomplete" {
		return nil, fmt.Errorf("%w: invalid evidence integrity %q", ErrEvidenceDenied, projection.Integrity)
	}
	if projection.ObservedAt == "" || len(projection.ParamsJSON) == 0 {
		return nil, fmt.Errorf("%w: evidence projection lacks params or observed time", ErrEvidenceDenied)
	}
	var resultJSON, warningsJSON, errorsJSON sql.NullString
	var evidenceArtifactID sql.NullInt64
	switch {
	case len(projection.ResultJSON) > 0 && projection.ArtifactID == 0:
		if !jsonValid(projection.ResultJSON) {
			return nil, fmt.Errorf("%w: evidence result body is not valid JSON", ErrEvidenceDenied)
		}
		resultJSON = sql.NullString{String: string(projection.ResultJSON), Valid: true}
	case projection.ArtifactID > 0 && len(projection.ResultJSON) == 0:
		var ownerType string
		var ownerID int64
		var bodyExpired int
		if err := conn.QueryRowContext(ctx, `
			SELECT owner_type,owner_id,body_expired FROM artifacts WHERE id=?`,
			projection.ArtifactID).Scan(&ownerType, &ownerID, &bodyExpired); err != nil {
			return nil, fmt.Errorf("%w: evidence artifact %d unknown: %v", ErrEvidenceDenied, projection.ArtifactID, err)
		}
		if ownerType != "tool_call" || ownerID != toolCallID || bodyExpired != 0 {
			return nil, fmt.Errorf("%w: artifact %d is not a live tool_result of tool call %d", ErrEvidenceDenied, projection.ArtifactID, toolCallID)
		}
		evidenceArtifactID = sql.NullInt64{Int64: projection.ArtifactID, Valid: true}
	default:
		return nil, fmt.Errorf("%w: evidence body must be exactly one of inline JSON or artifact", ErrEvidenceDenied)
	}
	if len(projection.WarningsJSON) > 0 {
		if !jsonValid(projection.WarningsJSON) {
			return nil, fmt.Errorf("%w: evidence warnings are not valid JSON", ErrEvidenceDenied)
		}
		warningsJSON = sql.NullString{String: string(projection.WarningsJSON), Valid: true}
	}
	if len(projection.ErrorsJSON) > 0 {
		if !jsonValid(projection.ErrorsJSON) {
			return nil, fmt.Errorf("%w: evidence errors are not valid JSON", ErrEvidenceDenied)
		}
		errorsJSON = sql.NullString{String: string(projection.ErrorsJSON), Valid: true}
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO evidence(attempt_id,tool_call_id,target_type,target_id,params_json,observed_at,
			result_json,artifact_id,warnings_json,errors_json,integrity,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, toolCallID, targetType, scopeID, string(projection.ParamsJSON), projection.ObservedAt,
		resultJSON, evidenceArtifactID, warningsJSON, errorsJSON, projection.Integrity,
		service.now().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	evidenceID, err := insert.LastInsertId()
	if err != nil {
		return nil, err
	}
	return []int64{evidenceID}, nil
}

// View is the frozen EvidenceDetail read projection.
type View struct {
	ID          string `json:"id"`
	TargetType  string `json:"targetType"`
	TargetID    string `json:"targetId"`
	Params      any    `json:"params"`
	ObservedAt  string `json:"observedAt"`
	Integrity   string `json:"integrity"`
	Warnings    any    `json:"warnings,omitempty"`
	Errors      any    `json:"errors,omitempty"`
	Producer    any    `json:"producer"`
	Connections []Conn `json:"connections"`
	Body        any    `json:"body"`
	CreatedAt   string `json:"createdAt"`
}

// Conn is one non-secret logical connection name of the evidence binding.
type Conn struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// Get returns the frozen detail projection of one immutable Evidence row.
func (service *Service) Get(ctx context.Context, evidenceID int64) (View, error) {
	var detail View
	var targetID int64
	var paramsJSON, observedAt, integrity, createdAt string
	var attemptID, toolCallID sql.NullInt64
	var resultJSON, warningsJSON, errorsJSON sql.NullString
	var artifactID sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT id,target_type,target_id,params_json,observed_at,integrity,created_at,
		       attempt_id,tool_call_id,result_json,artifact_id,warnings_json,errors_json
		FROM evidence WHERE id=?`, evidenceID).
		Scan(&evidenceID, &detail.TargetType, &targetID, &paramsJSON, &observedAt, &integrity, &createdAt,
			&attemptID, &toolCallID, &resultJSON, &artifactID, &warningsJSON, &errorsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	detail.ID = strconv.FormatInt(evidenceID, 10)
	detail.TargetID = strconv.FormatInt(targetID, 10)
	detail.ObservedAt = observedAt
	detail.Integrity = integrity
	detail.CreatedAt = createdAt
	detail.Connections = []Conn{}
	detail.Params = parseJSON(paramsJSON)
	if warningsJSON.Valid {
		detail.Warnings = parseJSON(warningsJSON.String)
	}
	if errorsJSON.Valid {
		detail.Errors = parseJSON(errorsJSON.String)
	}
	switch {
	case attemptID.Valid && toolCallID.Valid:
		var toolName, toolVersion string
		if err := service.db.QueryRowContext(ctx, `SELECT tool_name,tool_version FROM tool_calls WHERE id=?`, toolCallID.Int64).Scan(&toolName, &toolVersion); err != nil {
			return View{}, err
		}
		detail.Producer = map[string]any{
			"kind":        "plinth_tool",
			"attemptId":   strconv.FormatInt(attemptID.Int64, 10),
			"toolCallId":  strconv.FormatInt(toolCallID.Int64, 10),
			"toolName":    toolName,
			"toolVersion": toolVersion,
		}
		rows, err := service.db.QueryContext(ctx, `
			SELECT c.name, c.type
			FROM tool_call_connection_grants tcg
			JOIN attempt_connection_grants ag ON ag.id = tcg.connection_grant_id
			JOIN connections c ON c.id = ag.connection_id
			WHERE tcg.tool_call_id = ? ORDER BY tcg.ordinal`, toolCallID.Int64)
		if err != nil {
			return View{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var conn Conn
			if err := rows.Scan(&conn.Key, &conn.Type); err != nil {
				return View{}, err
			}
			detail.Connections = append(detail.Connections, conn)
		}
		if err := rows.Err(); err != nil {
			return View{}, err
		}
	case attemptID.Valid:
		detail.Producer = map[string]any{"kind": "lintel_browser", "attemptId": strconv.FormatInt(attemptID.Int64, 10)}
	default:
		detail.Producer = map[string]any{"kind": "quoin_local"}
	}
	switch {
	case resultJSON.Valid:
		detail.Body = map[string]any{"kind": "inline_json", "value": parseJSON(resultJSON.String)}
	case artifactID.Valid:
		detail.Body = map[string]any{"kind": "artifact", "artifact": artifactSummary(service.db, artifactID.Int64)}
	}
	return detail, nil
}

// artifactSummary projects the frozen ArtifactSummary of one artifact row.
func artifactSummary(db *sql.DB, artifactID int64) map[string]any {
	var kind, mediaType, retentionKind, ownerType, sha256Hex string
	var sensitive, bodyExpired int
	var ownerID, sizeBytes int64
	var expiresAt, createdAt sql.NullString
	err := db.QueryRow(`SELECT a.kind,a.media_type,a.sensitive,a.retention_kind,a.owner_type,a.owner_id,
		b.size_bytes,b.sha256,a.body_expired,a.expires_at,a.created_at
		FROM artifacts a JOIN artifact_blobs b ON b.id=a.blob_id WHERE a.id=?`, artifactID).
		Scan(&kind, &mediaType, &sensitive, &retentionKind, &ownerType, &ownerID,
			&sizeBytes, &sha256Hex, &bodyExpired, &expiresAt, &createdAt)
	if err != nil {
		return nil
	}
	summary := map[string]any{
		"id": strconv.FormatInt(artifactID, 10), "kind": kind, "sensitive": sensitive == 1,
		"retentionKind": retentionKind, "ownerType": ownerType, "ownerId": strconv.FormatInt(ownerID, 10),
		"sizeBytes": sizeBytes, "sha256": sha256Hex, "bodyExpired": bodyExpired == 1,
		"createdAt": createdAt.String,
	}
	if expiresAt.Valid {
		summary["expiresAt"] = expiresAt.String
	}
	return summary
}

func parseJSON(body string) any {
	var value any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		return nil
	}
	return value
}

func jsonValid(body []byte) bool {
	var value any
	return json.Unmarshal(body, &value) == nil
}
