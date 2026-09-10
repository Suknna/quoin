package analysis

// Deterministic input rebuild (ARCH-CONTEXT-006): the snapshot row stores
// only the schema kind and digest; the canonical bytes are rebuilt on
// demand from durable occurrence, business configuration, Label Contract, and
// chat-contract references and must reproduce the frozen digest
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
// attempt from its occurrence reference, published business/Label Contract
// references, and the frozen chat contract. The dispatch path verifies the digest against the snapshot
// row, so any drift here fails dispatch instead of silently diverging.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var occurrenceID, probeResultID, configVersionID, contractVersionID int64
	err := service.db.QueryRowContext(ctx, `
		SELECT occurrence.occurrence_id, grant.qualified_probe_result_id,
		       config.business_system_config_version_id, contract.label_contract_version_id
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items occurrence ON occurrence.snapshot_id=snapshot.id AND occurrence.occurrence_id IS NOT NULL
		JOIN attempt_input_items config ON config.snapshot_id=snapshot.id AND config.business_system_config_version_id IS NOT NULL
		JOIN attempt_input_items contract ON contract.snapshot_id=snapshot.id AND contract.label_contract_version_id IS NOT NULL
		JOIN attempt_connection_grants grant ON grant.attempt_id=snapshot.attempt_id AND grant.purpose='chat_model'
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&occurrenceID, &probeResultID, &configVersionID, &contractVersionID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d immutable business context missing: %w", attemptID, err)
	}
	return service.rebuildFor(ctx, service.db, occurrenceID, probeResultID, configVersionID, contractVersionID)
}

// rebuildFor renders the canonical input from immutable occurrence,
// business-configuration, Label Contract, and chat-provider references.
func (service *Service) rebuildFor(ctx context.Context, queries queryer, occurrenceID, probeResultID, configVersionID, contractVersionID int64) ([]byte, error) {
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
		SELECT config.system_key, json_extract(contract.contract_json, '$.label_contract.business_system_label')
		FROM business_system_config_versions config
		JOIN label_contracts contract ON contract.id=config.label_contract_version_id
		WHERE config.id=? AND contract.id=?`, configVersionID, contractVersionID).
		Scan(&input.BusinessContext.SystemKey, &input.BusinessContext.BusinessSystemLabel); err != nil {
		return nil, err
	}
	input.BusinessContext.ConfigVersionID = strconv.FormatInt(configVersionID, 10)
	input.BusinessContext.LabelContractVersionID = strconv.FormatInt(contractVersionID, 10)
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}
