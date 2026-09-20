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

	"github.com/Suknna/quoin/internal/quoin/audit"
	"github.com/Suknna/quoin/internal/quoin/execution"
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
	db *sql.DB
	// reader serves every pure read. It stays the zero execution.Reader —
	// fail closed on every query — until the composition layer wires the real
	// read-only reader via SetReader; the writer database is never a read
	// fallback.
	reader    execution.Reader
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

// SetReader installs the composition layer's real read-only reader for every
// pure read of this service. It reuses the execution runner's trusted gate —
// only an OpenReadOnly-produced execution.Reader is accepted, arbitrary
// handles are refused — and propagates its error. Until it is wired, every
// pure read fails closed; there is no writer fallback.
func (service *Service) SetReader(reader audit.Reader) error {
	gate := execution.NewRunner(service.db, nil, nil)
	if err := gate.SetReader(reader); err != nil {
		return err
	}
	service.reader = gate.Reader()
	return nil
}

// WriteForToolCall commits the deterministic Evidence for one succeeded
// observation tool inside the caller's transaction, while the Tool Call is
// still running (the frozen trg_evidence_attempt_tool_closure demands the
// running state at the Evidence INSERT; the terminal state advances in the
// same transaction afterwards — ARCH-TOOL-003, DATA-EVIDENCE-001).
func (service *Service) WriteForToolCall(ctx context.Context, conn execution.Executor, attemptID, toolCallID, artifactID int64, payloadJSON []byte, toolName string) ([]int64, error) {
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

// inspectionProducer resolves the frozen run-check declaration for Evidence
// committed without a Tool Call. PromQL collection is executed by Plinth with
// an Attempt-level metrics grant; their shared evidence table intentionally
// does not duplicate kind.
func (service *Service) inspectionProducer(ctx context.Context, attemptID int64) (map[string]any, []Conn, error) {
	// 独立计划 Run（ADR-0004）：插件采集由 Plinth supervisor 执行，连接来自
	// Run 冻结的接入授权；插件身份随 producer 事实一并冻结。
	var pluginID string
	pluginErr := service.reader.QueryRowContext(ctx, `
		SELECT c.plugin_id
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id AND r.plan_id IS NOT NULL
		JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).Scan(&pluginID)
	if pluginErr == nil {
		rows, grantErr := service.reader.QueryContext(ctx, `
			SELECT c.name,c.type
			FROM attempt_connection_grants ag
			JOIN connections c ON c.id=ag.connection_id
			WHERE ag.attempt_id=? AND ag.purpose='config_thanos_query'
			ORDER BY ag.id`, attemptID)
		if grantErr != nil {
			return nil, nil, grantErr
		}
		defer rows.Close()
		connections := []Conn{}
		for rows.Next() {
			var connection Conn
			if err := rows.Scan(&connection.Key, &connection.Type); err != nil {
				return nil, nil, err
			}
			connections = append(connections, connection)
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return map[string]any{"kind": "plinth_plugin", "pluginId": pluginID, "attemptId": strconv.FormatInt(attemptID, 10)}, connections, nil
	}
	if !errors.Is(pluginErr, sql.ErrNoRows) {
		return nil, nil, pluginErr
	}
	// PromQL 检查的身份来自 Run 冻结的检查行（inspection_run_checks,与
	// plugin 结果路径同一权威）,声明计划时代的 config_plans 谱系已随首发
	// 清理删除。
	var templateID string
	err := service.reader.QueryRowContext(ctx, `
		SELECT c.template_id
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).Scan(&templateID)
	if errors.Is(err, sql.ErrNoRows) || (templateID != "promql_instant" && templateID != "promql_range") {
		return map[string]any{"kind": "inspection_collection", "attemptId": strconv.FormatInt(attemptID, 10)}, []Conn{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := service.reader.QueryContext(ctx, `
		SELECT c.name,c.type
		FROM attempt_connection_grants ag
		JOIN connections c ON c.id=ag.connection_id
		WHERE ag.attempt_id=? AND ag.purpose='config_thanos_query'
		ORDER BY ag.id`, attemptID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	connections := []Conn{}
	for rows.Next() {
		var connection Conn
		if err := rows.Scan(&connection.Key, &connection.Type); err != nil {
			return nil, nil, err
		}
		connections = append(connections, connection)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"kind": "plinth_promql", "attemptId": strconv.FormatInt(attemptID, 10)}, connections, nil
}

// Get returns the frozen detail projection of one immutable Evidence row.
func (service *Service) Get(ctx context.Context, evidenceID int64) (View, error) {
	var detail View
	var targetID int64
	var paramsJSON, observedAt, integrity, createdAt string
	var attemptID, toolCallID sql.NullInt64
	var resultJSON, warningsJSON, errorsJSON sql.NullString
	var artifactID sql.NullInt64
	err := service.reader.QueryRowContext(ctx, `
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
		if err := service.reader.QueryRowContext(ctx, `SELECT tool_name,tool_version FROM tool_calls WHERE id=?`, toolCallID.Int64).Scan(&toolName, &toolVersion); err != nil {
			return View{}, err
		}
		detail.Producer = map[string]any{
			"kind":        "plinth_tool",
			"attemptId":   strconv.FormatInt(attemptID.Int64, 10),
			"toolCallId":  strconv.FormatInt(toolCallID.Int64, 10),
			"toolName":    toolName,
			"toolVersion": toolVersion,
		}
		rows, err := service.reader.QueryContext(ctx, `
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
		// A run_check evidence row without a Tool Call can be either a PromQL
		// result committed by Plinth or a Journey result committed by Lintel.
		// Resolve its frozen check kind instead of treating every such row as
		// browser evidence.
		producer, connections, err := service.inspectionProducer(ctx, attemptID.Int64)
		if err != nil {
			return View{}, err
		}
		detail.Producer = producer
		detail.Connections = connections
	default:
		detail.Producer = map[string]any{"kind": "quoin_local"}
	}
	switch {
	case resultJSON.Valid:
		detail.Body = map[string]any{"kind": "inline_json", "value": parseJSON(resultJSON.String)}
	case artifactID.Valid:
		detail.Body = map[string]any{"kind": "artifact", "artifact": artifactSummary(ctx, service.reader, artifactID.Int64)}
	}
	return detail, nil
}

// artifactSummary projects the frozen ArtifactSummary of one artifact row on
// the read-only reader (pure read; never the writer pool).
func artifactSummary(ctx context.Context, reader execution.Reader, artifactID int64) map[string]any {
	var kind, mediaType, retentionKind, ownerType, sha256Hex string
	var sensitive, bodyExpired int
	var ownerID, sizeBytes int64
	var expiresAt, createdAt sql.NullString
	err := reader.QueryRowContext(ctx, `SELECT a.kind,a.media_type,a.sensitive,a.retention_kind,a.owner_type,a.owner_id,
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
