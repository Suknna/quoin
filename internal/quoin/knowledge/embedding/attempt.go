// attempt.go: embedding attempt creation (rebuild batches and query
// embeds) and the deterministic input rebuild used at dispatch time.
package embedding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// Input is the frozen embedding_v1 wire shape. A rebuild attempt carries
// `inputs` (one per pending knowledge version); a query attempt carries
// `query` only. Each text travels with its SHA-256 so the result commit can
// re-verify the provider answer against the frozen input.
type Input struct {
	SchemaKind    string        `json:"schemaKind"`
	AttemptID     int64         `json:"attemptId"`
	GenerationID  int64         `json:"generationId"`
	Mode          string        `json:"mode"` // rebuild | query
	ModelContract ModelContract `json:"modelContract"`
	Inputs        []TextInput   `json:"inputs,omitempty"`
	Query         *TextInput    `json:"query,omitempty"`
}

// ModelContract is the frozen embedding model contract (no context/output
// budgets: embeddings carry none, per the model_calls CHECK).
type ModelContract struct {
	EmbeddingModelID string `json:"embeddingModelId"`
	VectorDim        int64  `json:"vectorDim"`
}

// TextInput is one text to embed with its digest.
type TextInput struct {
	VersionID int64  `json:"versionId,omitempty"` // 0 for the query text
	Text      string `json:"text"`
	Digest    string `json:"digest"`
}

// frozenItem is one persisted input-lineage row of an embedding attempt.
type frozenItem struct {
	versionID int64
	digest    string
}

// VersionText is the canonical embedded text of one knowledge version.
func VersionText(title, body string) string {
	return title + "\n" + body
}

// TextDigest is the canonical SHA-256 of one embedded text.
func TextDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// insertRebuildAttempt persists one Queued embedding attempt whose batch is
// every pending row of the generation, freezing the grant over the
// provider's qualified probe result.
func (service *Service) insertRebuildAttempt(ctx context.Context, conn *sql.Conn, generationID int64, selected provider) (int64, error) {
	attemptID, err := service.insertAttempt(ctx, conn, generationID, selected, "rebuild")
	if err != nil {
		return 0, err
	}
	input := Input{
		SchemaKind: InputSchemaKind, AttemptID: attemptID, GenerationID: generationID, Mode: "rebuild",
		ModelContract: ModelContract{EmbeddingModelID: selected.EmbeddingModel, VectorDim: selected.VectorDim},
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT v.id, v.title, v.body FROM embeddings e
		JOIN knowledge_versions v ON v.id=e.knowledge_version_id
		WHERE e.embedding_generation_id=? AND e.state='pending'
		ORDER BY v.id`, generationID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var items []frozenItem
	for rows.Next() {
		var versionID int64
		var title, body string
		if err := rows.Scan(&versionID, &title, &body); err != nil {
			return 0, err
		}
		text := VersionText(title, body)
		input.Inputs = append(input.Inputs, TextInput{VersionID: versionID, Text: text, Digest: TextDigest(text)})
		items = append(items, frozenItem{versionID: versionID, digest: TextDigest(text)})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := service.writeSnapshot(ctx, conn, attemptID, input, items); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// CreateQueryAttempt persists one Queued query-embedding attempt against the
// serving generation. It fails with ErrBusy when the generation scope
// already holds an active attempt.
func (service *Service) CreateQueryAttempt(ctx context.Context, query string) (int64, GenerationView, error) {
	generation, ok, err := service.CurrentGeneration(ctx)
	if err != nil || !ok {
		return 0, generation, err
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return 0, generation, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, generation, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	selected, ok, err := service.selectProvider(ctx, conn)
	if err != nil || !ok {
		if err == nil {
			err = ErrNoProvider
		}
		return 0, generation, err
	}
	// The generation must still be the serving one (a switch may commit
	// between the read above and this transaction).
	var serving int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM embedding_generations WHERE state='current'`).Scan(&serving); err != nil {
		return 0, generation, err
	}
	if serving != generation.ID {
		return 0, generation, ErrBusy
	}
	attemptID, err := service.insertAttempt(ctx, conn, generation.ID, selected, "query")
	if err != nil {
		return 0, generation, err
	}
	text := TextInput{Text: query, Digest: TextDigest(query)}
	input := Input{
		SchemaKind: InputSchemaKind, AttemptID: attemptID, GenerationID: generation.ID, Mode: "query",
		ModelContract: ModelContract{EmbeddingModelID: selected.EmbeddingModel, VectorDim: selected.VectorDim},
		Query:         &text,
	}
	if err := service.writeSnapshot(ctx, conn, attemptID, input, nil); err != nil {
		return 0, generation, err
	}
	// The originator registers BEFORE the commit publishes the attempt: a
	// concurrent sweep running between the commit and the caller's own
	// registration must never converge this live request as an orphan.
	service.mu.Lock()
	service.queryTexts[attemptID] = query
	service.mu.Unlock()
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		service.mu.Lock()
		delete(service.queryTexts, attemptID)
		service.mu.Unlock()
		return 0, generation, err
	}
	committed = true
	return attemptID, generation, nil
}

// insertAttempt creates the Queued execution attempt plus the frozen
// embedding grant (agent_version stays NULL: RUNTIME-AGENT-010).
func (service *Service) insertAttempt(ctx context.Context, conn *sql.Conn, generationID int64, selected provider, mode string) (int64, error) {
	_ = mode
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,created_at)
		VALUES('embedding','embedding_generation',?,'Queued',?,?)`,
		generationID, attempt.ReleaseVersion(), service.nowText())
	if err != nil {
		return 0, busyOrErr(err)
	}
	attemptID, err := insert.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?,'embedding',?,?,?,?,?)`,
		attemptID, selected.ConnectionID, selected.RevisionID, selected.CredentialGen, selected.ProbeResultID, service.nowText()); err != nil {
		return 0, err
	}
	return attemptID, nil
}

// busyOrErr maps the active-scope unique violation onto ErrBusy.
func busyOrErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "ux_execution_attempt_active_scope") {
		return ErrBusy
	}
	return err
}

// writeSnapshot freezes the canonical input (digest in
// attempt_input_snapshots) and the per-version lineage items.
func (service *Service) writeSnapshot(ctx context.Context, conn *sql.Conn, attemptID int64, input Input, items []frozenItem) error {
	canonical, err := json.Marshal(input)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	snapshot, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,created_at)
		VALUES(?,?,?,?,?)`, attemptID, InputSchemaKind, RendererVersion, hex.EncodeToString(sum[:]), service.nowText())
	if err != nil {
		return err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return err
	}
	for index, item := range items {
		// One locator per lineage item (schema CHECK): the version id. The
		// generation binding lives in the frozen snapshot itself.
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,knowledge_version_id)
			VALUES(?,?,'knowledge_version',?,?)`,
			snapshotID, int64(index+1), item.digest, item.versionID); err != nil {
			return err
		}
	}
	if input.Query != nil {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,embedding_generation_id)
			VALUES(?,?,'user',?,?)`,
			snapshotID, int64(len(items)+1), input.Query.Digest, input.GenerationID); err != nil {
			return err
		}
	}
	return nil
}

// RebuildInput reconstructs the frozen embedding_v1 bytes of one attempt
// from durable records only; dispatch verifies the digest (DATA-ATTEMPT-003).
// A query attempt's text is not durable domain state — it is rebuilt from
// the in-memory registry of its live HTTP originator.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var scopeID int64
	if err := service.db.QueryRowContext(ctx, `
		SELECT a.scope_id FROM execution_attempts a
		WHERE a.id=? AND a.attempt_type='embedding' AND a.scope_type='embedding_generation'`, attemptID).
		Scan(&scopeID); err != nil {
		return nil, err
	}
	modelID, vectorDim, err := service.lookupContract(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	input := Input{
		SchemaKind: InputSchemaKind, AttemptID: attemptID, GenerationID: scopeID,
		ModelContract: ModelContract{EmbeddingModelID: modelID, VectorDim: vectorDim},
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT i.knowledge_version_id, i.source_digest FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.knowledge_version_id IS NOT NULL
		ORDER BY i.item_seq`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var frozenItems []frozenItem
	for rows.Next() {
		var item frozenItem
		if err := rows.Scan(&item.versionID, &item.digest); err != nil {
			return nil, err
		}
		frozenItems = append(frozenItems, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(frozenItems) > 0 {
		input.Mode = "rebuild"
		versionRows, err := service.db.QueryContext(ctx, `
			SELECT id, title, body FROM knowledge_versions WHERE id IN (`+placeholders(len(frozenItems))+`)`, idsOf(frozenItems)...)
		if err != nil {
			return nil, err
		}
		defer versionRows.Close()
		texts := map[int64]string{}
		for versionRows.Next() {
			var id int64
			var title, body string
			if err := versionRows.Scan(&id, &title, &body); err != nil {
				return nil, err
			}
			texts[id] = VersionText(title, body)
		}
		if err := versionRows.Err(); err != nil {
			return nil, err
		}
		// Item order is the frozen lineage order.
		for _, item := range frozenItems {
			text, ok := texts[item.versionID]
			if !ok {
				return nil, fmt.Errorf("embedding attempt %d lineage references missing version %d", attemptID, item.versionID)
			}
			input.Inputs = append(input.Inputs, TextInput{VersionID: item.versionID, Text: text, Digest: item.digest})
		}
		return json.Marshal(input)
	}
	// Query attempt: the frozen query digest is in the single user-role item.
	var queryDigest string
	if err := service.db.QueryRowContext(ctx, `
		SELECT i.source_digest FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		WHERE s.attempt_id=? AND i.item_role='user'`, attemptID).Scan(&queryDigest); err != nil {
		return nil, err
	}
	input.Mode = "query"
	// The query text is not durable domain state: it lives only in the
	// in-memory registry of the originating HTTP request. Dispatch for a
	// query attempt can therefore only be replayed from that request's
	// original bytes, so the rebuild reproduces the digest-bearing envelope
	// with the registry-held text.
	text, ok := service.queryTextFor(attemptID)
	if !ok {
		return nil, fmt.Errorf("embedding query attempt %d has no live originator", attemptID)
	}
	input.Query = &TextInput{Text: text, Digest: queryDigest}
	return json.Marshal(input)
}

// lookupContract resolves the frozen embedding contract of the attempt's
// grant (model id and dimension from the qualified probe result).
func (service *Service) lookupContract(ctx context.Context, attemptID int64) (string, int64, error) {
	var modelID sql.NullString
	var dim sql.NullInt64
	err := service.db.QueryRowContext(ctx, `
		SELECT e.embedding_model_id, e.embedding_vector_dim
		FROM attempt_connection_grants g
		JOIN model_provider_connection_probe_results e ON e.probe_result_id=g.qualified_probe_result_id
		WHERE g.attempt_id=? AND g.purpose='embedding'`, attemptID).Scan(&modelID, &dim)
	if err != nil {
		return "", 0, err
	}
	if !modelID.Valid || !dim.Valid {
		return "", 0, fmt.Errorf("embedding attempt %d grant lacks a qualified embedding contract", attemptID)
	}
	return modelID.String, dim.Int64, nil
}

func placeholders(count int) string {
	result := ""
	for index := 0; index < count; index++ {
		if index > 0 {
			result += ","
		}
		result += "?"
	}
	return result
}

func idsOf(items []frozenItem) []any {
	ids := make([]any, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.versionID)
	}
	return ids
}
