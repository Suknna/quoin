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
	"errors"
	"fmt"
	"strconv"

	"github.com/Suknna/quoin/internal/quoin/config"
)

// queryer is the minimal query surface the rebuild needs (a pool handle
// outside transactions, the transaction connection inside them — SQLite is
// single-writer and a nested pool fetch would deadlock).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// populateOccurrenceAnnotations selects the first accepted observation as the
// immutable annotation source. Both admission and dispatch call this shared
// projection so their canonical JSON stays byte-identical; later repeats or
// resolution observations cannot alter a frozen analysis input.
func populateOccurrenceAnnotations(ctx context.Context, queries queryer, occurrenceID int64, occurrence *OccurrenceContext) error {
	var annotationsJSON sql.NullString
	err := queries.QueryRowContext(ctx, `
		SELECT json_extract(delivery.body, '$.alerts[' || item.item_index || '].annotations')
		FROM alert_observations observation
		JOIN alert_delivery_items item ON item.id=observation.delivery_item_id
		JOIN alert_deliveries delivery ON delivery.id=observation.delivery_id
		WHERE observation.occurrence_id=?
		ORDER BY observation.committed_at ASC, observation.id ASC
		LIMIT 1`, occurrenceID).Scan(&annotationsJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !annotationsJSON.Valid || annotationsJSON.String == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(annotationsJSON.String), &occurrence.Annotations); err != nil {
		return fmt.Errorf("decode immutable alert observation annotations: %w", err)
	}
	return nil
}

// RebuildInput rebuilds the canonical initial_analysis_v1 input for one
// attempt from its occurrence reference, published business/Label Contract
// references, and the frozen chat contract. The dispatch path verifies the digest against the snapshot
// row, so any drift here fails dispatch instead of silently diverging.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var occurrenceID, probeResultID, configVersionID int64
	var rendererVersion string
	err := service.db.QueryRowContext(ctx, `
		SELECT snapshot.renderer_version, occurrence.occurrence_id, grant.qualified_probe_result_id,
		       config.business_system_config_version_id
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items occurrence ON occurrence.snapshot_id=snapshot.id AND occurrence.occurrence_id IS NOT NULL
		JOIN attempt_input_items config ON config.snapshot_id=snapshot.id AND config.business_system_config_version_id IS NOT NULL
		JOIN attempt_connection_grants grant ON grant.attempt_id=snapshot.attempt_id AND grant.purpose='chat_model'
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&rendererVersion, &occurrenceID, &probeResultID, &configVersionID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d immutable business context missing: %w", attemptID, err)
	}
	if rendererVersion == RendererVersion {
		return service.rebuildFor(ctx, service.db, occurrenceID, probeResultID, configVersionID, true)
	}
	return service.rebuildLegacyInput(ctx, attemptID, occurrenceID, probeResultID, configVersionID, rendererVersion != "initial-analysis-renderer-v1")
}

// rebuildFor renders the current declaration-backed input from immutable
// occurrence, config-version and chat-provider references.
func (service *Service) rebuildFor(ctx context.Context, queries queryer, occurrenceID, probeResultID, configVersionID int64, includeAnnotations bool) ([]byte, error) {
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
	if includeAnnotations {
		if err := populateOccurrenceAnnotations(ctx, queries, occurrenceID, &input.Occurrence); err != nil {
			return nil, err
		}
	}
	var declarationJSON string
	if err := queries.QueryRowContext(ctx, `SELECT declaration_json FROM business_system_config_versions WHERE id=?`, configVersionID).Scan(&declarationJSON); err != nil {
		return nil, err
	}
	var declaration config.BusinessSystemDocument
	if err := json.Unmarshal([]byte(declarationJSON), &declaration); err != nil {
		return nil, fmt.Errorf("decode frozen business declaration: %w", err)
	}
	if declaration.SystemKey == "" || len(declaration.Resources) == 0 {
		return nil, errors.New("frozen business declaration has no resource scope")
	}
	input.BusinessContext.SystemKey = declaration.SystemKey
	input.BusinessContext.ConfigVersionID = strconv.FormatInt(configVersionID, 10)
	input.BusinessContext.Resources = append([]config.ResourceProjection(nil), declaration.Resources...)
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}

// rebuildLegacyInput preserves historical renderer bytes only. It deliberately
// retains the old Label Contract lineage, which is never consulted by new work.
func (service *Service) rebuildLegacyInput(ctx context.Context, attemptID, occurrenceID, probeResultID, configVersionID int64, includeAnnotations bool) ([]byte, error) {
	var contractVersionID int64
	if err := service.db.QueryRowContext(ctx, `
		SELECT label_contract_version_id FROM attempt_input_items
		WHERE snapshot_id=(SELECT id FROM attempt_input_snapshots WHERE attempt_id=?)
		  AND label_contract_version_id IS NOT NULL`, attemptID).Scan(&contractVersionID); err != nil {
		return nil, fmt.Errorf("attempt %d historical Label Contract lineage missing: %w", attemptID, err)
	}
	var input Input
	var labelsJSON string
	var resolvedAt sql.NullString
	if err := service.db.QueryRowContext(ctx, `SELECT state,first_seen_at,last_state_change_at,resolved_at,labels_canonical FROM alert_occurrences WHERE id=?`, occurrenceID).
		Scan(&input.Occurrence.State, &input.Occurrence.FirstSeenAt, &input.Occurrence.LastStateChange, &resolvedAt, &labelsJSON); err != nil {
		return nil, err
	}
	input.Occurrence.ID = strconv.FormatInt(occurrenceID, 10)
	if resolvedAt.Valid {
		input.Occurrence.ResolvedAt = &resolvedAt.String
	}
	if err := json.Unmarshal([]byte(labelsJSON), &input.Occurrence.Labels); err != nil {
		return nil, err
	}
	if includeAnnotations {
		if err := populateOccurrenceAnnotations(ctx, service.db, occurrenceID, &input.Occurrence); err != nil {
			return nil, err
		}
	}
	if err := service.db.QueryRowContext(ctx, `
		SELECT config.system_key,json_extract(contract.contract_json,'$.label_contract.business_system_label')
		FROM business_system_config_versions config JOIN label_contracts contract ON contract.id=config.label_contract_version_id
		WHERE config.id=? AND contract.id=?`, configVersionID, contractVersionID).
		Scan(&input.BusinessContext.SystemKey, &input.BusinessContext.BusinessSystemLabel); err != nil {
		return nil, err
	}
	input.BusinessContext.ConfigVersionID = strconv.FormatInt(configVersionID, 10)
	input.BusinessContext.LabelContractVersionID = strconv.FormatInt(contractVersionID, 10)
	if err := service.db.QueryRowContext(ctx, `SELECT chat_model_id,context_budget_tokens,max_output_tokens FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}
