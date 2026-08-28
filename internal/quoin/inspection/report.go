// Immutable Inspection Report closure (CFG-INSPECTRUN-002, RUNTIME-TASK-013):
// once a Run's collection has closed, Quoin creates exactly one
// inspection_analysis attempt whose frozen input carries the structured
// preallocated Report version, the Run locator, every check result and every
// complete Evidence. The model's ResultProposal is re-adjudicated against the
// frozen facts and canonical digests, and the single ledger INSERT lets the
// frozen SQL closure create the immutable Report atomically.
package inspection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Suknna/quoin/internal/quoin/attempt"
)

const reportInputKind = "inspection_analysis_v1"
const reportResultKind = "inspection_report_result_v1"

type reportModelContract struct {
	ModelID             string `json:"modelId"`
	ContextBudgetTokens int64  `json:"contextBudgetTokens"`
	MaxOutputTokens     int64  `json:"maxOutputTokens"`
}

type reportInput struct {
	SchemaKind         string              `json:"schemaKind"`
	AttemptID          int64               `json:"attemptId"`
	InspectionRunID    int64               `json:"inspectionRunId"`
	ReportVersion      int64               `json:"reportVersion"`
	ConfigVersionID    int64               `json:"configVersionId"`
	PlanKey            string              `json:"planKey"`
	EvidenceIDs        []int64             `json:"evidenceIds"`
	ArtifactIDs        []int64             `json:"artifactIds"`
	KnowledgeVersionID []int64             `json:"knowledgeVersionIds"`
	ModelContract      reportModelContract `json:"modelContract"`
}

// modelProviderSelection mirrors the single enabled model provider resolution
// owned by internal/quoin/analysis (DATA-CONN-003): one enabled provider with
// a current explicit qualification.
type modelProviderSelection struct {
	ConnectionID  int64
	RevisionID    int64
	CredentialGen int64
	ProbeResultID int64
	ChatModelID   string
	ContextBudget int64
	MaxOutput     int64
}

var ErrModelProviderMissing = errors.New("no enabled qualified model provider")

func selectReportModelProvider(ctx context.Context, conn *sql.Conn) (modelProviderSelection, error) {
	var selected modelProviderSelection
	var qualificationRowVersion, connectionRowVersion int64
	var probeOutcome string
	err := conn.QueryRowContext(ctx, `
		SELECT c.id, c.current_revision_id, c.current_credential_generation_id,
		       q.probe_result_id, q.enabled_row_version, c.row_version, p.outcome
		FROM connections c
		JOIN connection_enable_qualifications q ON q.connection_id=c.id
		JOIN connection_probe_results p ON p.id=q.probe_result_id
		WHERE c.type='model_provider' AND c.enabled=1 AND c.revalidation_required=0
		ORDER BY q.id DESC LIMIT 1`).
		Scan(&selected.ConnectionID, &selected.RevisionID, &selected.CredentialGen,
			&selected.ProbeResultID, &qualificationRowVersion, &connectionRowVersion, &probeOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return selected, ErrModelProviderMissing
	}
	if err != nil {
		return selected, err
	}
	if qualificationRowVersion != connectionRowVersion || probeOutcome != "passed" {
		return selected, ErrModelProviderMissing
	}
	var streaming, nativeToolCalling bool
	if err = conn.QueryRowContext(ctx, `
		SELECT chat_model_id, context_budget_tokens, max_output_tokens, streaming_supported, native_tool_calling_supported
		FROM model_provider_connection_probe_results WHERE probe_result_id=?`, selected.ProbeResultID).
		Scan(&selected.ChatModelID, &selected.ContextBudget, &selected.MaxOutput, &streaming, &nativeToolCalling); err != nil {
		return selected, err
	}
	if selected.ChatModelID == "" || !nativeToolCalling {
		return selected, ErrModelProviderMissing
	}
	return selected, nil
}

// startReportAnalysisOn creates the collection-following analysis attempt
// inside the caller's transaction. It is a no-op unless the Run has closed
// and no analysis attempt exists yet.
func (s *Service) startReportAnalysisOn(ctx context.Context, conn *sql.Conn, runID int64, now string) error {
	var runState string
	if err := conn.QueryRowContext(ctx, `SELECT state FROM inspection_runs WHERE id=?`, runID).Scan(&runState); err != nil {
		return err
	}
	if runState != "Completed" && runState != "CompletedWithGaps" {
		return nil
	}
	var existing int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM execution_attempts WHERE attempt_type='inspection_analysis' AND scope_type='run' AND scope_id=?`, runID).
		Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return nil
	}
	provider, err := selectReportModelProvider(ctx, conn)
	if err != nil {
		// A closed collection without a usable model keeps its state; the
		// reconciliation loop retries the analysis creation (RUNTIME-TASK-013
		// failure semantics: no placeholder report).
		return nil
	}
	var configVersionID int64
	var planKey string
	var contractID int64
	if err = conn.QueryRowContext(ctx, `
		SELECT config_version_id, plan_key, label_contract_version_id FROM inspection_runs WHERE id=?`, runID).
		Scan(&configVersionID, &planKey, &contractID); err != nil {
		return err
	}
	var reportVersion int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM inspection_reports WHERE run_id=?`, runID).Scan(&reportVersion); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT x.id, x.evidence_id FROM inspection_check_results x
		WHERE x.run_id=? ORDER BY x.check_key`, runID)
	if err != nil {
		return err
	}
	type settledCheck struct {
		resultID int64
		evidence sql.NullInt64
	}
	checks := []settledCheck{}
	for rows.Next() {
		var check settledCheck
		if err = rows.Scan(&check.resultID, &check.evidence); err != nil {
			rows.Close()
			return err
		}
		checks = append(checks, check)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	evidenceIDs := []int64{}
	for _, check := range checks {
		if check.evidence.Valid {
			evidenceIDs = append(evidenceIDs, check.evidence.Int64)
		}
	}
	insert, err := conn.ExecContext(ctx, `
		INSERT INTO execution_attempts(attempt_type,scope_type,scope_id,state,quoin_release_version,agent_version,created_at)
		VALUES('inspection_analysis','run',?,'Queued',?,?,?)`, runID, attempt.ReleaseVersion(), attempt.AgentVersion, now)
	if err != nil {
		return err
	}
	analysisID, err := insert.LastInsertId()
	if err != nil {
		return err
	}
	input := reportInput{
		SchemaKind: reportInputKind, AttemptID: analysisID, InspectionRunID: runID,
		ReportVersion: int64(reportVersion + 1), ConfigVersionID: configVersionID, PlanKey: planKey,
		EvidenceIDs: evidenceIDs, ArtifactIDs: []int64{}, KnowledgeVersionID: []int64{},
		ModelContract: reportModelContract{ModelID: provider.ChatModelID, ContextBudgetTokens: provider.ContextBudget, MaxOutputTokens: provider.MaxOutput},
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	snapshot, err := conn.ExecContext(ctx, `
		INSERT INTO attempt_input_snapshots(attempt_id,schema_kind,renderer_version,content_digest,inspection_report_version,created_at)
		VALUES(?,?,?,?,?,?)`, analysisID, reportInputKind, "v1", hex.EncodeToString(digest[:]), input.ReportVersion, now)
	if err != nil {
		return err
	}
	snapshotID, err := snapshot.LastInsertId()
	if err != nil {
		return err
	}
	runDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-run:%d", runID)))
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_run_id)
		VALUES(?,1,'inspection_run',?,?)`, snapshotID, hex.EncodeToString(runDigest[:]), runID); err != nil {
		return err
	}
	itemSeq := 1
	for _, check := range checks {
		itemSeq++
		checkDigest := sha256.Sum256([]byte(fmt.Sprintf("inspection-check-result:%d", check.resultID)))
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,inspection_check_result_id)
			VALUES(?,?,'inspection_check_result',?,?)`, snapshotID, itemSeq, hex.EncodeToString(checkDigest[:]), check.resultID); err != nil {
			return err
		}
	}
	for _, evidenceID := range evidenceIDs {
		itemSeq++
		evidenceDigest := sha256.Sum256([]byte(fmt.Sprintf("evidence:%d", evidenceID)))
		if _, err = conn.ExecContext(ctx, `
			INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,evidence_id)
			VALUES(?,?,'inspection_evidence',?,?)`, snapshotID, itemSeq, hex.EncodeToString(evidenceDigest[:]), evidenceID); err != nil {
			return err
		}
	}
	versionDigest := sha256.Sum256([]byte(fmt.Sprintf("business-system-config-version:%d", configVersionID)))
	itemSeq++
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,business_system_config_version_id)
		VALUES(?,?,'config_version',?,?)`, snapshotID, itemSeq, hex.EncodeToString(versionDigest[:]), configVersionID); err != nil {
		return err
	}
	contractDigest := sha256.Sum256([]byte(fmt.Sprintf("label-contract-version:%d", contractID)))
	itemSeq++
	_, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_input_items(snapshot_id,item_seq,item_role,source_digest,label_contract_version_id)
		VALUES(?,?,'label_contract',?,?)`, snapshotID, itemSeq, hex.EncodeToString(contractDigest[:]), contractID)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO attempt_connection_grants(attempt_id,purpose,connection_id,connection_revision_id,credential_generation_id,qualified_probe_result_id,created_at)
		VALUES(?, 'chat_model', ?,?,?,?,?)`,
		analysisID, provider.ConnectionID, provider.RevisionID, provider.CredentialGen, provider.ProbeResultID, now)
	return err
}

type reportProposal struct {
	SchemaKind          string  `json:"schemaKind"`
	AttemptID           int64   `json:"attemptId"`
	InspectionRunID     int64   `json:"inspectionRunId"`
	ModelCallID         int64   `json:"modelCallId"`
	Outcome             string  `json:"outcome"`
	Content             string  `json:"content"`
	EvidenceIDs         []int64 `json:"evidenceIds"`
	ArtifactIDs         []int64 `json:"artifactIds"`
	KnowledgeVersionIDs []int64 `json:"knowledgeVersionIds"`
	ResultDigest        string  `json:"resultDigest"`
	EvidenceDigest      string  `json:"evidenceDigest"`
	PromptDigest        string  `json:"promptDigest"`
}

// CommitReportProposal re-adjudicates the model's typed ResultProposal against
// the frozen input and commits the single ledger row; the frozen SQL closure
// creates the immutable Report, its ordered references, and the Attempt's
// Succeeded terminal state in the same statement.
func (s *Service) CommitReportProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	var proposal reportProposal
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return fmt.Errorf("inspection report result is not valid JSON: %w", err)
	}
	if proposal.SchemaKind != reportResultKind || proposal.AttemptID != attemptID || proposal.InspectionRunID < 1 ||
		proposal.ModelCallID < 1 || proposal.Outcome != "success" || proposal.Content == "" {
		return fmt.Errorf("inspection report result has an invalid identity envelope")
	}
	evidenceJSON, err := canonicalIDArray(proposal.EvidenceIDs)
	if err != nil {
		return err
	}
	artifactJSON, err := canonicalIDArray(proposal.ArtifactIDs)
	if err != nil {
		return err
	}
	knowledgeJSON, err := canonicalIDArray(proposal.KnowledgeVersionIDs)
	if err != nil {
		return err
	}
	evidenceSum := sha256.Sum256([]byte(evidenceJSON))
	evidenceDigest := hex.EncodeToString(evidenceSum[:])
	if proposal.EvidenceDigest != evidenceDigest {
		return fmt.Errorf("inspection report evidence digest does not match its locators")
	}
	canonical := fmt.Sprintf("%s|%d|%d|%d|%s|%s|%s|%s|%s|%s|%s",
		reportResultKind, attemptID, proposal.InspectionRunID, proposal.ModelCallID, "success",
		proposal.Content, evidenceJSON, artifactJSON, knowledgeJSON, evidenceDigest, proposal.PromptDigest)
	resultSum := sha256.Sum256([]byte(canonical))
	resultDigest := hex.EncodeToString(resultSum[:])
	if proposal.ResultDigest != resultDigest {
		return fmt.Errorf("inspection report result digest does not match its canonical payload")
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var runID int64
	var state string
	if err = conn.QueryRowContext(ctx, `
		SELECT scope_id, state FROM execution_attempts
		WHERE id=? AND attempt_type='inspection_analysis' AND scope_type='run'`, attemptID).
		Scan(&runID, &state); err != nil {
		return err
	}
	if runID != proposal.InspectionRunID {
		return fmt.Errorf("inspection report result identity does not match attempt")
	}
	// Idempotent replay: an already-committed ledger row accepts only the
	// identical payload (its digest was derived from this exact body).
	var existingDigest []byte
	replayErr := conn.QueryRowContext(ctx, `SELECT result_digest FROM inspection_report_result_ledgers WHERE attempt_id=?`, attemptID).Scan(&existingDigest)
	if replayErr == nil {
		if hex.EncodeToString(existingDigest) == proposal.ResultDigest {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("inspection report replay digest conflicts")
	}
	if replayErr != sql.ErrNoRows {
		return replayErr
	}
	// The model call must be the attempt's own succeeded call with the exact
	// prompt provenance.
	var callState string
	var callPrompt string
	err = conn.QueryRowContext(ctx, `
		SELECT status, prompt_digest FROM model_calls WHERE id=? AND attempt_id=?`, proposal.ModelCallID, attemptID).
		Scan(&callState, &callPrompt)
	if err != nil {
		return fmt.Errorf("inspection report model call does not belong to the attempt: %w", err)
	}
	if callState != "succeeded" || callPrompt != proposal.PromptDigest {
		return fmt.Errorf("inspection report model call is not the succeeded prompt provenance")
	}
	// Boot/epoch fence: the ledger closure performs the terminal transition.
	var bound int
	if err = conn.QueryRowContext(ctx, `
		SELECT 1 FROM execution_attempts
		WHERE id=? AND state='Running' AND boot_id=? AND connection_epoch=?`, attemptID, bootID, epoch).Scan(&bound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt.ErrLateResult
		}
		return err
	}
	var reportVersion int64
	if err = conn.QueryRowContext(ctx, `
		SELECT inspection_report_version FROM attempt_input_snapshots WHERE attempt_id=?`, attemptID).Scan(&reportVersion); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `
		INSERT INTO inspection_report_result_ledgers(
			attempt_id, inspection_run_id, report_version, model_call_id, result_digest, evidence_digest,
			content, prompt_digest, evidence_ids_json, artifact_ids_json, knowledge_version_ids_json, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, runID, reportVersion, proposal.ModelCallID, resultSum[:], evidenceDigest,
		proposal.Content, proposal.PromptDigest, evidenceJSON, artifactJSON, knowledgeJSON, s.nowText()); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// canonicalIDArray renders the locator array exactly as the frozen SQL json()
// projection: minified integers, no spaces.
func canonicalIDArray(ids []int64) (string, error) {
	if ids == nil {
		ids = []int64{}
	}
	for _, id := range ids {
		if id < 1 {
			return "", fmt.Errorf("inspection report locator ids must be positive")
		}
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
