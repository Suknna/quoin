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

	"github.com/Suknna/quoin/internal/quoin/attempt"
	"github.com/Suknna/quoin/internal/quoin/config"
)

// queryer is the minimal query surface the rebuild needs (a pool handle
// outside transactions, the transaction connection inside them — SQLite is
// single-writer and a nested pool fetch would deadlock).
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
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
// attempt from its occurrence reference, its frozen business/Label Contract
// references (declared attempts) or frozen integration lineage (source
// attempts, ADR-0004), and the frozen chat contract. The dispatch path
// verifies the digest against the snapshot row, so any drift here fails
// dispatch instead of silently diverging.
func (service *Service) RebuildInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var occurrenceID, probeResultID int64
	var rendererVersion string
	err := service.db.QueryRowContext(ctx, `
		SELECT snapshot.renderer_version, occurrence.occurrence_id, grant.qualified_probe_result_id
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items occurrence ON occurrence.snapshot_id=snapshot.id AND occurrence.occurrence_id IS NOT NULL
		JOIN attempt_connection_grants grant ON grant.attempt_id=snapshot.attempt_id AND grant.purpose='chat_model'
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&rendererVersion, &occurrenceID, &probeResultID)
	if err != nil {
		return nil, fmt.Errorf("attempt %d immutable input lineage missing: %w", attemptID, err)
	}
	// The frozen catalog document is part of the digested input; rebuilding
	// reads the stored column so an enablement change never drifts history.
	catalog, err := attempt.FrozenToolCatalogDoc(ctx, service.db, attemptID)
	if err != nil {
		return nil, err
	}
	// v4 makes the config lineage optional (source-level attempts); v3 and
	// older snapshots always carry one and keep their exact historical paths.
	configVersionID, err := frozenConfigVersion(ctx, service.db, attemptID)
	if err != nil {
		return nil, err
	}
	if rendererVersion == RendererVersion {
		return service.rebuildFor(ctx, service.db, attemptID, occurrenceID, probeResultID, configVersionID, true, catalog)
	}
	if rendererVersion == "initial-analysis-renderer-v3" {
		if configVersionID == 0 {
			return nil, fmt.Errorf("attempt %d renderer v3 snapshot lost its business config lineage", attemptID)
		}
		return service.rebuildFor(ctx, service.db, attemptID, occurrenceID, probeResultID, configVersionID, true, catalog)
	}
	if configVersionID == 0 {
		return nil, fmt.Errorf("attempt %d historical snapshot lost its business config lineage", attemptID)
	}
	return service.rebuildLegacyInput(ctx, attemptID, occurrenceID, probeResultID, configVersionID, rendererVersion != "initial-analysis-renderer-v1")
}

// frozenConfigVersion returns the config version frozen on the attempt's
// snapshot, or 0 for source-level attempts that froze integrations instead.
func frozenConfigVersion(ctx context.Context, queries queryer, attemptID int64) (int64, error) {
	var configVersionID int64
	err := queries.QueryRowContext(ctx, `
		SELECT item.business_system_config_version_id
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.business_system_config_version_id IS NOT NULL
		WHERE snapshot.attempt_id=?`, attemptID).Scan(&configVersionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return configVersionID, nil
}

// rebuildFor renders the current declaration-backed input from immutable
// occurrence, config-version and chat-provider references. configVersionID=0
// renders the ADR-0004 source-level shape: the integrations frozen as input
// items replace the business context.
func (service *Service) rebuildFor(ctx context.Context, queries queryer, attemptID, occurrenceID, probeResultID, configVersionID int64, includeAnnotations bool, toolCatalog *attempt.FrozenCatalog) ([]byte, error) {
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
	// v4 authority is always the frozen integrations; an attributed
	// occurrence additionally carries its declaration as descriptive context.
	integrations, err := frozenIntegrations(ctx, queries, attemptID)
	if err != nil {
		return nil, err
	}
	input.Integrations = integrations
	if configVersionID > 0 {
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
		input.BusinessContext = &BusinessContext{
			SystemKey:       declaration.SystemKey,
			ConfigVersionID: strconv.FormatInt(configVersionID, 10),
			Resources:       append([]config.ResourceProjection(nil), declaration.Resources...),
		}
	}
	input.ToolCatalog = toolCatalog
	if err := queries.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, probeResultID).
		Scan(&input.ModelContract.ModelID, &input.ModelContract.ContextBudgetTokens, &input.ModelContract.MaxOutputTokens); err != nil {
		return nil, err
	}
	return json.Marshal(input)
}

// frozenIntegrations reconstructs the frozen source-level authority from the
// attempt's item lineage: the exact connections and revisions frozen at
// creation, so later enablement churn cannot re-interpret the snapshot.
func frozenIntegrations(ctx context.Context, queries queryer, attemptID int64) ([]RenderedIntegration, error) {
	rows, err := queries.QueryContext(ctx, `
		SELECT CASE WHEN c.type='kubernetes' THEN 'kubernetes' ELSE 'metrics' END AS kind, c.name
		FROM attempt_input_snapshots snapshot
		JOIN attempt_input_items item ON item.snapshot_id=snapshot.id
			AND item.item_role IN ('metrics_source','kubernetes_source')
			AND item.connection_revision_id IS NOT NULL
		JOIN connection_revisions r ON r.id=item.connection_revision_id
		JOIN connections c ON c.id=r.connection_id
		WHERE snapshot.attempt_id=?
		ORDER BY c.name, kind`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var integrations []RenderedIntegration
	for rows.Next() {
		var integration RenderedIntegration
		if err := rows.Scan(&integration.Kind, &integration.Name); err != nil {
			return nil, err
		}
		integrations = append(integrations, integration)
	}
	return integrations, rows.Err()
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
	// Historical snapshots always carry a business context; the pointer stays
	// nil only for the v4 source-level shape.
	input.BusinessContext = &BusinessContext{}
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
