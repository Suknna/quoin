package analysis

// Deterministic input rebuild (ARCH-CONTEXT-006): the snapshot row stores
// only the schema kind and digest; the canonical bytes are rebuilt on
// demand from the durable item references (the occurrence context plus the
// attempt's frozen chat contract) and must reproduce the frozen digest
// exactly before any dispatch.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
)

// queryer is the minimal query surface the rebuild needs (a pool handle
// outside transactions, the transaction connection inside them — SQLite is
// single-writer and a nested pool fetch would deadlock).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RebuildInput rebuilds the canonical initial_analysis_v1 input for one
// attempt from its occurrence reference and the attempt's frozen chat
// contract. The dispatch path verifies the digest against the snapshot
// row, so any drift here fails dispatch instead of silently diverging.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var occurrenceID int64
	var probeResultID int64
	err := service.db.QueryRowContext(ctx, `
		SELECT i.occurrence_id, g.qualified_probe_result_id
		FROM attempt_input_items i
		JOIN attempt_input_snapshots s ON s.id=i.snapshot_id
		JOIN attempt_connection_grants g ON g.attempt_id=s.attempt_id AND g.purpose='chat_model'
		WHERE s.attempt_id=? AND i.occurrence_id IS NOT NULL`, attemptID).Scan(&occurrenceID, &probeResultID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d occurrence reference missing: %w", attemptID, err)
	}
	return service.rebuildFor(ctx, service.db, occurrenceID, probeResultID)
}

// rebuildFor renders the canonical input from one occurrence and one
// qualified chat contract (used by create, retry and dispatch rebuilds).
func (service *Service) rebuildFor(ctx context.Context, queries queryer, occurrenceID, probeResultID int64) ([]byte, error) {
	var input Input
	var labelsJSON string
	var resolvedAt sql.NullString
	err := queries.QueryRowContext(ctx, `
		SELECT state, first_seen_at, last_state_change_at, resolved_at, labels_canonical
		FROM alert_occurrences WHERE id=?`, occurrenceID).
		Scan(&input.Occurrence.State, &input.Occurrence.FirstSeenAt, &input.Occurrence.LastStateChange,
			&resolvedAt, &labelsJSON)
	if err != nil {
		return nil, err
	}
	input.Occurrence.ID = strconv.FormatInt(occurrenceID, 10)
	if resolvedAt.Valid {
		input.Occurrence.ResolvedAt = &resolvedAt.String
	}
	if err := json.Unmarshal([]byte(labelsJSON), &input.Occurrence.Labels); err != nil {
		return nil, err
	}
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}
