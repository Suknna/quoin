package inspection

// The generic Attempt authority configured to reconstruct the frozen
// inspection inputs from their immutable projections (RUNTIME-TASK-011):
// run_check PromQL children rebuild inspection_promql_execution_v1,
// report analyses rebuild inspection_analysis_v1.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

// Attempts exposes the generic attempt service for dispatch and fencing.
func (s *Service) Attempts() *attempt.Service {
	attempts := attempt.NewService(s.db)
	attempts.SnapshotRebuilder = s.rebuildAttemptInput
	return attempts
}

// QueuedPromQLAttempts returns supervisor-only run_check collection work:
// historical declaration PromQL children plus ADR-0004 plan-run plugin
// collection children (both supervisor-executed, both config_thanos_query
// granted).
func (s *Service) QueuedPromQLAttempts(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.attempt_type='inspection_collection' AND a.scope_type='run_check'
		  AND a.state='Queued' AND r.state='Running' AND c.kind='promql'
		UNION
		SELECT a.id FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id AND r.plan_id IS NOT NULL
		JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
		WHERE a.attempt_type='inspection_collection' AND a.scope_type='run_check'
		  AND a.state='Queued' AND r.state='Running'
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// QueuedAnalysisAttempts returns Queued report analyses whose Run has closed.
func (s *Service) QueuedAnalysisAttempts(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		WHERE a.attempt_type='inspection_analysis' AND a.scope_type='run'
		  AND a.state='Queued' AND r.state IN ('Completed','CompletedWithGaps')
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rebuildAttemptInput dispatches to the schema-kind rebuilder and verifies the
// rebuilt canonical bytes against the frozen digest.
func (s *Service) rebuildAttemptInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var schemaKind string
	if err := s.db.QueryRowContext(ctx, `SELECT schema_kind FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&schemaKind); err != nil {
		return nil, err
	}
	var canonical []byte
	var err error
	switch schemaKind {
	case "inspection_plugin_execution_v1":
		canonical, err = s.rebuildPluginInput(ctx, attemptID)
	case "inspection_promql_execution_v1":
		canonical, err = s.rebuildPromQLInput(ctx, attemptID)
	case "inspection_collection_v1":
		canonical, err = s.rebuildJourneyInput(ctx, attemptID)
	case "inspection_analysis_v1":
		canonical, err = s.rebuildAnalysisInput(ctx, attemptID)
	default:
		return nil, fmt.Errorf("attempt %d has no inspection rebuilder for %s", attemptID, schemaKind)
	}
	if err != nil {
		return nil, err
	}
	var frozen string
	if err := s.db.QueryRowContext(ctx, `SELECT content_digest FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&frozen); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != frozen {
		return nil, fmt.Errorf("inspection input digest no longer matches frozen snapshot")
	}
	return canonical, nil
}

func (s *Service) rebuildPromQLInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID int64
	var checkKey, mode, expression string
	var rangeSeconds, stepSeconds, grant sql.NullInt64
	var evidenceAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT a.scope_id, a.check_key, c.query_mode, c.expression, c.range_seconds, c.step_seconds,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query'), r.evidence_at
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check' AND c.kind='promql'`, attemptID).
		Scan(&runID, &checkKey, &mode, &expression, &rangeSeconds, &stepSeconds, &grant, &evidenceAt)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen config_thanos_query grant", attemptID)
	}
	var rangePointer, stepPointer *int64
	if rangeSeconds.Valid {
		rangePointer = &rangeSeconds.Int64
	}
	if stepSeconds.Valid {
		stepPointer = &stepSeconds.Int64
	}
	return json.Marshal(map[string]any{
		"schemaKind": "inspection_promql_execution_v1", "attemptId": attemptID, "inspectionRunId": runID,
		"checkKey": checkKey, "evidenceAt": evidenceAt,
		"query":   map[string]any{"mode": mode, "expression": expression, "rangeSeconds": rangePointer, "stepSeconds": stepPointer},
		"grantId": grant.Int64,
	})
}

// rebuildPluginInput deterministically reconstructs a plan-run plugin
// collection input from the run's frozen binding, its run-owned check row and
// the frozen metrics grant.
func (s *Service) rebuildPluginInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID int64
	var checkKey, pluginID, templateID, templateVersion, paramsJSON, evidenceAt, frozenScopeJSON string
	var grant sql.NullInt64
	var targetJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT a.scope_id, a.check_key, c.plugin_id, c.template_id, c.template_version, c.params_json, c.target_json,
		       r.evidence_at, r.frozen_scope_json,
		       (SELECT id FROM attempt_connection_grants WHERE attempt_id=a.id AND purpose='config_thanos_query')
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id AND r.plan_id IS NOT NULL
		JOIN inspection_run_checks c ON c.run_id=r.id AND c.check_key=a.check_key
		WHERE a.id=? AND a.attempt_type='inspection_collection' AND a.scope_type='run_check'`, attemptID).
		Scan(&runID, &checkKey, &pluginID, &templateID, &templateVersion, &paramsJSON, &targetJSON, &evidenceAt, &frozenScopeJSON, &grant)
	if err != nil {
		return nil, err
	}
	if !grant.Valid {
		return nil, fmt.Errorf("attempt %d has no frozen config_thanos_query grant", attemptID)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		return nil, err
	}
	// wire 作用域类型来自 Run 冻结范围；缺失视为 legacy integration——独立计划
	// 的冻结范围恒带 kind，business_view 缺 labelConditions 会被 collector 拒绝。
	frozen := map[string]any{}
	_ = json.Unmarshal([]byte(frozenScopeJSON), &frozen)
	wireKind, _ := frozen["kind"].(string)
	input := pluginCollectionInput{
		SchemaKind: pluginExecutionSchemaKind, AttemptID: attemptID, InspectionRunID: runID,
		CheckKey: checkKey, PluginID: pluginID, TemplateID: templateID, TemplateVersion: templateVersion,
		Params: params, EvidenceAt: evidenceAt, GrantID: grant.Int64, ScopeKind: wireKind,
	}
	if targetJSON.Valid {
		var target planObjectRef
		if err := json.Unmarshal([]byte(targetJSON.String), &target); err != nil {
			return nil, err
		}
		input.Target = &target
	}
	return json.Marshal(input)
}

func (s *Service) rebuildJourneyInput(ctx context.Context, attemptID int64) ([]byte, error) {
	var runID, opID, identityID, revisionID, profileID, generation, journeyVersion, probeVersion int64
	var planKey, checkKey, journeyID, params, catalogDigest, catalogVersion, startURL, probeID, probeParams string
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id,o.id,o.identity_id,o.identity_revision_id,o.profile_generation_id,g.generation,
		 r.plan_key,a.check_key,o.journey_id,COALESCE(c.journey_params_json,'{}'),o.journey_version,o.journey_catalog_digest,o.journey_catalog_version,
		 ir.start_url,ir.probe_journey_id,ir.probe_journey_version,COALESCE(ir.probe_params_json,'{}')
		FROM execution_attempts a JOIN inspection_runs r ON r.id=a.scope_id
		JOIN browser_operations o ON o.owner_attempt_id=a.id AND o.kind='journey'
		JOIN browser_profile_generations g ON g.id=o.profile_generation_id
		JOIN browser_identity_revisions ir ON ir.id=o.identity_revision_id
		JOIN config_plans p ON p.config_version_id=r.config_version_id AND p.plan_key=r.plan_key
		JOIN config_checks c ON c.plan_id=p.id AND c.check_key=a.check_key AND c.kind='browser'
		WHERE a.id=? AND a.scope_type='run_check'`, attemptID).Scan(&runID, &opID, &identityID, &revisionID, &profileID, &generation, &planKey, &checkKey, &journeyID, &params, &journeyVersion, &catalogDigest, &catalogVersion, &startURL, &probeID, &probeVersion, &probeParams)
	if err != nil {
		return nil, err
	}
	binding := func(id string, v int64, p string) map[string]any {
		return map[string]any{"id": id, "version": v, "params": json.RawMessage(p), "catalog": map[string]any{"digest": catalogDigest, "version": catalogVersion}}
	}
	return json.Marshal(map[string]any{"schemaKind": "inspection_collection_v1", "attemptId": attemptID, "operationId": opID, "identity": map[string]any{"identityId": identityID, "identityRevisionId": revisionID, "profileGenerationId": profileID, "profileGeneration": generation, "startUrl": startURL}, "journey": binding(journeyID, journeyVersion, params), "authenticationProbe": binding(probeID, probeVersion, probeParams), "planKey": planKey, "checkKey": checkKey})
}

func (s *Service) rebuildAnalysisInput(ctx context.Context, attemptID int64) ([]byte, error) {
	// 历史 Run 携带声明版本谱系；独立计划 Run（ADR-0004）的 config_version_id
	// 为 NULL，其模型上下文改由 Run 冻结的计划绑定重建。两条路径必须与各自
	// 冻结时的字节逐一致。
	var runID sql.NullInt64
	var configVersionID, planID, connectionID sql.NullInt64
	var planKey string
	var reportVersion int64
	err := s.db.QueryRowContext(ctx, `
		SELECT a.scope_id, r.config_version_id, r.plan_key, r.plan_id, r.connection_id, s.inspection_report_version
		FROM execution_attempts a
		JOIN inspection_runs r ON r.id=a.scope_id
		JOIN attempt_input_snapshots s ON s.attempt_id=a.id
		WHERE a.id=? AND a.attempt_type='inspection_analysis' AND a.scope_type='run'`, attemptID).
		Scan(&runID, &configVersionID, &planKey, &planID, &connectionID, &reportVersion)
	if err != nil {
		return nil, err
	}
	modelID, contextBudget, maxOutput, err := attempt.NewService(s.db).LookupChatContract(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT x.id, x.evidence_id FROM inspection_check_results x
		WHERE x.run_id=? ORDER BY x.check_key`, runID)
	if err != nil {
		return nil, err
	}
	// Release this cursor before fetching artifact grants below. Production has
	// one SQLite connection, so a second pool query while rows remains open
	// waits forever and prevents the completed PromQL Run's report dispatch.
	evidenceIDs := []int64{}
	type settledCheck struct {
		resultID int64
		evidence sql.NullInt64
	}
	checks := []settledCheck{}
	for rows.Next() {
		var check settledCheck
		if err := rows.Scan(&check.resultID, &check.evidence); err != nil {
			_ = rows.Close()
			return nil, err
		}
		checks = append(checks, check)
		if check.evidence.Valid {
			evidenceIDs = append(evidenceIDs, check.evidence.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	grantRows, err := s.db.QueryContext(ctx, `SELECT g.artifact_id FROM attempt_artifact_grants g JOIN attempt_input_snapshots s ON s.id=g.source_id JOIN attempt_input_items i ON i.snapshot_id=s.id AND i.artifact_id=g.artifact_id WHERE g.attempt_id=? AND g.source_kind='input_snapshot' AND s.attempt_id=? ORDER BY i.item_seq`, attemptID, attemptID)
	if err != nil {
		return nil, err
	}
	artifactIDs := []int64{}
	for grantRows.Next() {
		var id int64
		if err := grantRows.Scan(&id); err != nil {
			_ = grantRows.Close()
			return nil, err
		}
		artifactIDs = append(artifactIDs, id)
	}
	if err := grantRows.Err(); err != nil {
		_ = grantRows.Close()
		return nil, err
	}
	if err := grantRows.Close(); err != nil {
		return nil, err
	}
	input := reportInput{
		SchemaKind: reportInputKind, AttemptID: attemptID, InspectionRunID: runID.Int64,
		ReportVersion: reportVersion, ConfigVersionID: configVersionID.Int64, PlanKey: planKey,
		EvidenceIDs: evidenceIDs, ArtifactIDs: artifactIDs, KnowledgeVersionID: []int64{},
		ModelContract: reportModelContract{ModelID: modelID, ContextBudgetTokens: contextBudget, MaxOutputTokens: maxOutput},
	}
	if planID.Valid {
		// 计划 Run：与冻结时同一来源重建计划上下文（frozen_params_json /
		// frozen_scope_json 经相同 decode→marshal 路径，键序确定性一致）。
		if connectionID.Valid {
			var connectionName string
			if err := s.db.QueryRowContext(ctx, `SELECT name FROM connections WHERE id=?`, connectionID.Int64).Scan(&connectionName); err != nil {
				return nil, err
			}
			input.ConnectionName = connectionName
		}
		var paramsRaw, scopeRaw, templateID, templateVersion string
		if err := s.db.QueryRowContext(ctx, `
			SELECT frozen_params_json, frozen_scope_json, template_id, template_version FROM inspection_runs WHERE id=?`, runID.Int64).
			Scan(&paramsRaw, &scopeRaw, &templateID, &templateVersion); err != nil {
			return nil, err
		}
		params := map[string]any{}
		_ = json.Unmarshal([]byte(paramsRaw), &params)
		scope := map[string]any{}
		_ = json.Unmarshal([]byte(scopeRaw), &scope)
		input.TemplateID = templateID
		input.TemplateVersion = templateVersion
		input.Plan = &planReportContext{Key: planKey, Params: params, Scope: scope}
	}
	return json.Marshal(input)
}
