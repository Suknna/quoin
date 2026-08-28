package businesssystem

// Config Verification Journey result adjudication (T23, CFG-JOURNEY-003,
// RUNTIME-TASK-012): the sealed browser_journey_result_v1 is re-validated
// against the frozen schema, the operation's frozen catalog binding and the
// catalog's own output schema before Quoin creates the primary structured
// Evidence and commits the single browser_journey_results INSERT; the SQL
// trigger derives the check result and closes the operation and Attempt in
// the same statement.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Suknna/quoin/internal/gen/contracts"
	quoinconfig "github.com/Suknna/quoin/internal/quoin/config"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	browserExecutionSchemaOnce sync.Once
	browserExecutionSchema     *jsonschema.Schema
	browserExecutionSchemaErr  error
)

func loadBrowserExecutionSchema() (*jsonschema.Schema, error) {
	browserExecutionSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(contracts.BrowserExecutionSchema)))
		if err != nil {
			browserExecutionSchemaErr = fmt.Errorf("parse frozen browser execution schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseRegexpEngine(ecmaRegexpAdapter)
		resource := "https://github.com/Suknna/quoin/schemas/browser-execution.schema.json"
		if err := compiler.AddResource(resource, document); err != nil {
			browserExecutionSchemaErr = fmt.Errorf("register frozen browser execution schema: %w", err)
			return
		}
		browserExecutionSchema, browserExecutionSchemaErr = compiler.Compile(resource)
	})
	return browserExecutionSchema, browserExecutionSchemaErr
}

// ecmaRegexpAdapter translates the frozen schema's ECMAScript Unicode escapes
// into RE2 form; the schema stays the sole authority.
func ecmaRegexpAdapter(pattern string) (jsonschema.Regexp, error) {
	translated := regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).ReplaceAllStringFunc(pattern, func(token string) string {
		return `\x` + token[len(token)-2:]
	})
	return regexp.Compile(translated)
}

// validateJourneyVerificationShape checks one canonical input or result
// document against the frozen browser execution contract.
func validateJourneyVerificationShape(canonical []byte) error {
	schema, err := loadBrowserExecutionSchema()
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return err
	}
	return nil
}

// journeyResult is the typed projection of the sealed browser_journey_result_v1.
type journeyResult struct {
	SchemaKind      string                `json:"schemaKind"`
	AttemptID       int64                 `json:"attemptId"`
	OperationID     *int64                `json:"operationId"`
	Outcome         string                `json:"outcome"`
	ProbeResults    []journeyProbeFact    `json:"probeResults"`
	Evidence        []journeyEvidenceFact `json:"evidence"`
	TraceArtifactID *int64                `json:"traceArtifactId"`
	TraceIntegrity  *string               `json:"traceIntegrity"`
	GapCode         *string               `json:"gapCode"`
	OriginalGapCode *string               `json:"originalGapCode"`
	TerminalReason  *string               `json:"terminalReason"`
	ErrorDetail     *string               `json:"errorDetail"`
}

type journeyProbeFact struct {
	Phase          string            `json:"phase"`
	Result         string            `json:"result"`
	JourneyID      string            `json:"journeyId"`
	JourneyVersion int64             `json:"journeyVersion"`
	Catalog        journeyCatalogRef `json:"catalog"`
	ReasonCode     *string           `json:"reasonCode"`
	ObservedAt     string            `json:"observedAt"`
}

type journeyEvidenceFact struct {
	Kind       string         `json:"kind"`
	Primary    bool           `json:"primary"`
	ObservedAt string         `json:"observedAt"`
	Content    map[string]any `json:"content"`
	ArtifactID *int64         `json:"artifactId"`
}

// CommitJourneyProposal adjudicates one Lintel Journey result: schema, frozen
// binding, catalog output revalidation, Evidence creation and the single
// ledger INSERT land in one transaction whose triggers close the operation
// and the Attempt.
func (service *Service) CommitJourneyProposal(ctx context.Context, attemptID int64, bootID string, epoch uint64, raw []byte) error {
	if err := validateJourneyVerificationShape(raw); err != nil {
		return fmt.Errorf("journey result violates the frozen contract: %w", err)
	}
	var result journeyResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("journey result is unparseable: %w", err)
	}
	if result.SchemaKind != "browser_journey_result_v1" || result.AttemptID != attemptID || result.OperationID == nil {
		return fmt.Errorf("journey result identity envelope does not match the attempt")
	}
	// Reconstruct the sealed dispatch input before accepting a result. It binds
	// the Attempt snapshot to the operation ID plus the Journey parameters,
	// catalog and probe references; a changed durable row cannot be committed
	// merely because the result still names a valid operation.
	frozenInput, err := service.rebuildJourneyVerificationInput(ctx, attemptID)
	if err != nil {
		return fmt.Errorf("rebuild frozen journey input: %w", err)
	}
	var frozen journeyVerificationInput
	if err := json.Unmarshal(frozenInput, &frozen); err != nil || frozen.OperationID == nil || *frozen.OperationID != *result.OperationID {
		return fmt.Errorf("journey result does not match the frozen operation input")
	}
	conn, err := service.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var runID int64
	var planKey, checkKey, attemptState string
	var attemptBoot string
	var attemptEpoch uint64
	err = conn.QueryRowContext(ctx, `
		SELECT a.scope_id,a.plan_key,a.check_key,a.state,COALESCE(a.boot_id,''),COALESCE(a.connection_epoch,0)
		FROM execution_attempts a
		WHERE a.id=? AND a.scope_type='config_verification_run' AND a.attempt_type='inspection_collection' AND a.runtime_slot='lintel'`, attemptID).
		Scan(&runID, &planKey, &checkKey, &attemptState, &attemptBoot, &attemptEpoch)
	if err != nil {
		return err
	}
	var operationState string
	var operationJourneyID string
	var operationJourneyVersion int64
	var operationDigest, operationVersion string
	var probeRevision int64
	var expectedProbeID string
	var expectedProbeVersion int64
	err = conn.QueryRowContext(ctx, `
		SELECT o.state,o.journey_id,o.journey_version,o.journey_catalog_digest,o.journey_catalog_version,o.identity_revision_id
		FROM browser_operations o WHERE o.id=? AND o.owner_attempt_id=? AND o.kind='journey'`,
		*result.OperationID, attemptID).Scan(&operationState, &operationJourneyID, &operationJourneyVersion, &operationDigest, &operationVersion, &probeRevision)
	if err != nil {
		return fmt.Errorf("journey result does not bind a journey operation of this attempt: %w", err)
	}
	// Idempotent replay: the same immutable digest rebuilds the same ack; a
	// different digest loses to the operation's unique ledger key.
	digest := sha256Sum(raw)
	var existingDigest []byte
	var existingRun int64
	lookupErr := conn.QueryRowContext(ctx, `SELECT result_digest,attempt_id FROM browser_journey_results WHERE operation_id=?`, *result.OperationID).Scan(&existingDigest, &existingRun)
	if lookupErr == nil {
		if string(existingDigest) == string(digest) && existingRun == attemptID && attemptState == "Succeeded" {
			if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
				return err
			}
			committed = true
			return nil
		}
		return fmt.Errorf("journey result replay digest conflicts")
	}
	if lookupErr != sql.ErrNoRows {
		return lookupErr
	}
	if attemptState != "Running" || attemptBoot != bootID || epoch < attemptEpoch {
		return fmt.Errorf("journey result arrived for a non-running or stale-stream attempt")
	}
	if operationState != "Running" {
		return fmt.Errorf("journey result arrived for a non-running operation")
	}
	if operationJourneyID == "" || operationJourneyVersion < 1 || operationDigest == "" || operationVersion == "" {
		return fmt.Errorf("journey operation lost its frozen catalog binding")
	}
	if err := conn.QueryRowContext(ctx, `SELECT probe_journey_id,probe_journey_version FROM browser_identity_revisions WHERE id=?`, probeRevision).Scan(&expectedProbeID, &expectedProbeVersion); err != nil {
		return fmt.Errorf("journey operation lost its frozen authentication probe binding: %w", err)
	}
	// The probes and the primary content must close against the operation's
	// frozen catalog facts (CFG-JOURNEY-003).
	document, _, _, err := quoinconfig.JourneyCatalog()
	if err != nil {
		return err
	}
	entry, _ := document["journeys"].(map[string]any)[operationJourneyID].(map[string]any)
	if entry == nil {
		return fmt.Errorf("journey %q disappeared from the embedded catalog", operationJourneyID)
	}
	if version, _ := entry["version"].(float64); int64(version) != operationJourneyVersion {
		return fmt.Errorf("embedded catalog no longer carries the operation's frozen journey version")
	}
	for _, probe := range result.ProbeResults {
		if probe.JourneyID != expectedProbeID || probe.JourneyVersion != expectedProbeVersion ||
			probe.Catalog.Digest != operationDigest || probe.Catalog.Version != operationVersion {
			return fmt.Errorf("journey probe does not match the operation's frozen authentication binding")
		}
	}
	if result.Outcome == "success" {
		if len(result.Evidence) != 1 || result.Evidence[0].Kind != "structured" || !result.Evidence[0].Primary || result.Evidence[0].ArtifactID != nil || len(result.Evidence[0].Content) == 0 {
			return fmt.Errorf("journey success must carry exactly one primary structured Evidence proposal")
		}
		if err := validateAgainstInlineSchema(entry["output_schema"], "journey:"+operationJourneyID+":output", result.Evidence[0].Content); err != nil {
			return fmt.Errorf("journey typed output violates the catalog output schema: %w", err)
		}
		hasAdmission, hasCompletion := false, false
		for _, probe := range result.ProbeResults {
			switch {
			case probe.Phase == "admission" && probe.Result == "Authenticated":
				hasAdmission = true
			case probe.Phase == "completion" && probe.Result == "Authenticated":
				hasCompletion = true
			}
		}
		if !hasAdmission || !hasCompletion {
			return fmt.Errorf("journey success requires authenticated admission and completion probes")
		}
	} else if result.Outcome != "gap" || result.GapCode == nil || result.TerminalReason == nil || result.ErrorDetail == nil || len(result.Evidence) != 0 {
		return fmt.Errorf("journey gap result is malformed")
	}
	now := service.nowText()
	for index, probe := range result.ProbeResults {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO browser_probe_results(operation_id,probe_seq,phase,identity_revision_id,journey_id,journey_version,journey_catalog_digest,journey_catalog_version,result,reason_code,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			*result.OperationID, index+1, probe.Phase, probeRevision, probe.JourneyID, probe.JourneyVersion, probe.Catalog.Digest, probe.Catalog.Version, probe.Result, probe.ReasonCode, probe.ObservedAt); err != nil {
			return err
		}
	}
	// The operation's trace columns must precede the ledger statement: the
	// frozen table CHECK rejects a Failed journey operation whose terminal
	// reason is journey_failed without its mandatory trace artifact.
	if result.TraceArtifactID != nil {
		integrity := "incomplete"
		if result.TraceIntegrity != nil {
			integrity = *result.TraceIntegrity
		}
		if _, err := conn.ExecContext(ctx, `UPDATE browser_operations SET trace_artifact_id=?,trace_integrity=?,row_version=row_version+1 WHERE id=? AND state='Running'`, *result.TraceArtifactID, integrity, *result.OperationID); err != nil {
			return err
		}
	}
	var primaryEvidenceID any
	if result.Outcome == "success" {
		params, _ := json.Marshal(map[string]string{"plan_key": planKey, "check_key": checkKey})
		insert, err := conn.ExecContext(ctx, `
			INSERT INTO evidence(attempt_id,target_type,target_id,params_json,observed_at,result_json,integrity,created_at)
			VALUES(?,'config_verification_run',?,?,?,?,?,?)`,
			attemptID, runID, string(params), result.Evidence[0].ObservedAt, marshalContent(result.Evidence[0].Content), "complete", now)
		if err != nil {
			return err
		}
		id, err := insert.LastInsertId()
		if err != nil {
			return err
		}
		primaryEvidenceID = id
	}
	var nullableGap, nullableOriginalGap, nullableTerminal, nullableDetail any
	if result.GapCode != nil {
		nullableGap = *result.GapCode
	}
	if result.OriginalGapCode != nil {
		nullableOriginalGap = *result.OriginalGapCode
	}
	if result.TerminalReason != nil {
		nullableTerminal = *result.TerminalReason
	}
	if result.ErrorDetail != nil {
		nullableDetail = *result.ErrorDetail
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO browser_journey_results(operation_id,attempt_id,result_digest,outcome,primary_evidence_id,gap_code,original_gap_code,terminal_reason,error_detail,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		*result.OperationID, attemptID, digest, result.Outcome, primaryEvidenceID, nullableGap, nullableOriginalGap, nullableTerminal, nullableDetail, now); err != nil {
		return err
	}
	if err := convergeVerificationRunOn(ctx, conn, runID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func sha256Sum(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

func marshalContent(content map[string]any) string {
	encoded, err := json.Marshal(content)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// validateAgainstInlineSchema compiles one draft 2020-12 subschema from the
// embedded catalog and validates a decoded value against it.
func validateAgainstInlineSchema(rawSchema any, name string, value any) error {
	encoded, err := json.Marshal(rawSchema)
	if err != nil {
		return fmt.Errorf("catalog subschema %s cannot be encoded: %w", name, err)
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	resource := "https://github.com/Suknna/quoin/schemas/inline/" + name
	if err := compiler.AddResource(resource, document); err != nil {
		return err
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("catalog subschema %s does not compile: %w", name, err)
	}
	return compiled.Validate(value)
}
