package deployment

import (
	"context"
	"database/sql"
)

// VerificationDetail is the frozen HTTP projection of one invocation (openapi
// DeploymentVerificationDetail): manifest summary, items with typed
// locators, append-only results/conflicts/drifts, and the optional receipt.
type VerificationDetail struct {
	ID                     int64                        `json:"id,string"`
	ReleaseSubjectDigest   string                       `json:"releaseSubjectDigest"`
	CatalogDigest          string                       `json:"catalogDigest"`
	ResultProfileDigest    string                       `json:"resultProfileDigest"`
	DeploymentConfigDigest string                       `json:"deploymentConfigDigest"`
	PublicOriginDigest     string                       `json:"publicOriginDigest"`
	ApplicableSetDigest    string                       `json:"applicableSetDigest"`
	ItemCount              int                          `json:"itemCount"`
	ItemSetDigest          string                       `json:"itemSetDigest"`
	ManifestDigest         string                       `json:"manifestDigest"`
	StartedAt              string                       `json:"startedAt"`
	DeadlineAt             string                       `json:"deadlineAt"`
	VerificationProgress   VerificationProgress         `json:"progress"`
	VerificationReceipt    *VerificationReceipt         `json:"receipt,omitempty"`
	Items                  []VerificationItem           `json:"items"`
	Results                []VerificationItemResult     `json:"results"`
	Conflicts              []VerificationResultConflict `json:"conflicts"`
	SubjectDrifts          []VerificationSubjectDrift   `json:"subjectDrifts"`
}

type VerificationProgress struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
}

type VerificationReceipt struct {
	ID                        int64  `json:"id,string"`
	ManifestDigest            string `json:"manifestDigest"`
	ApplicableSetDigest       string `json:"applicableSetDigest"`
	ItemSetDigest             string `json:"itemSetDigest"`
	ResultSetDigest           string `json:"resultSetDigest"`
	HelperImportSetDigest     string `json:"helperImportSetDigest"`
	TypedObservationSetDigest string `json:"typedObservationSetDigest"`
	ConflictSetDigest         string `json:"conflictSetDigest"`
	SubjectDriftDigest        string `json:"subjectDriftDigest"`
	OverallOutcome            string `json:"overallOutcome"`
	FinalResultDigest         string `json:"finalResultDigest"`
	CanonicalArtifactID       int64  `json:"canonicalArtifactId,string"`
	SnapshotAt                string `json:"snapshotAt"`
	FinalizedAt               string `json:"finalizedAt"`
}

type VerificationItem struct {
	ID          int64          `json:"id,string"`
	ItemSeq     int            `json:"itemSeq"`
	ScenarioID  string         `json:"scenarioId"`
	CellID      string         `json:"cellId"`
	ObjectKind  string         `json:"objectKind"`
	InputDigest string         `json:"inputDigest"`
	Locator     map[string]any `json:"locator"`
}

type VerificationItemResult struct {
	ID                  int64  `json:"id,string"`
	ItemID              int64  `json:"itemId,string"`
	InputDigest         string `json:"inputDigest"`
	ResultDigest        string `json:"resultDigest"`
	ProducerType        string `json:"producerType"`
	Outcome             string `json:"outcome"`
	Category            string `json:"category"`
	ObservedAt          string `json:"observedAt"`
	CommittedAt         string `json:"committedAt"`
	EvidenceIndexDigest string `json:"evidenceIndexDigest"`
}

type VerificationResultConflict struct {
	ID                  int64  `json:"id,string"`
	ItemID              int64  `json:"itemId,string"`
	FirstResultID       int64  `json:"firstResultId,string"`
	ConflictingResultID int64  `json:"conflictingResultId,string"`
	CreatedAt           string `json:"createdAt"`
}

type VerificationSubjectDrift struct {
	ObjectKind    string `json:"objectKind"`
	DriftField    string `json:"driftField"`
	ItemID        int64  `json:"itemId,string"`
	FrozenDigest  string `json:"frozenDigest"`
	CurrentDigest string `json:"currentDigest"`
	ObservedAt    string `json:"observedAt"`
}

// ErrNotFound marks an unknown invocation for the HTTP layer.
var ErrNotFound = &Error{Family: errNotFound, Message: "verification invocation not found"}

// Load reads the authoritative rows for one invocation.
func (service *Service) Load(ctx context.Context, invocationID int64) (*VerificationDetail, error) {
	detail := &VerificationDetail{}
	var receiptID sql.NullInt64
	err := service.db.QueryRowContext(ctx, `SELECT id,release_subject_digest,catalog_digest,result_profile_digest,deployment_config_digest,
		public_origin_digest,applicable_set_digest,item_count,item_set_digest,manifest_digest,started_at,deadline_at FROM verification_invocation_manifests WHERE id=?`, invocationID).
		Scan(&detail.ID, &detail.ReleaseSubjectDigest, &detail.CatalogDigest, &detail.ResultProfileDigest, &detail.DeploymentConfigDigest,
			&detail.PublicOriginDigest, &detail.ApplicableSetDigest, &detail.ItemCount, &detail.ItemSetDigest, &detail.ManifestDigest, &detail.StartedAt, &detail.DeadlineAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	detail.VerificationProgress = VerificationProgress{Total: detail.ItemCount}
	if err := service.db.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receiptID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if receiptID.Valid {
		receipt, err := service.loadReceipt(ctx, receiptID.Int64)
		if err != nil {
			return nil, err
		}
		detail.VerificationReceipt = receipt
	}
	if err := service.loadItems(ctx, detail); err != nil {
		return nil, err
	}
	completed := map[int64]bool{}
	rows, err := service.db.QueryContext(ctx, `SELECT id,item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest
		FROM verification_item_results WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?) ORDER BY id`, invocationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		result := VerificationItemResult{}
		if err := rows.Scan(&result.ID, &result.ItemID, &result.InputDigest, &result.ResultDigest, &result.ProducerType, &result.Outcome, &result.Category, &result.ObservedAt, &result.CommittedAt, &result.EvidenceIndexDigest); err != nil {
			return nil, err
		}
		detail.Results = append(detail.Results, result)
		completed[result.ItemID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	detail.VerificationProgress.Completed = len(completed)
	rows, err = service.db.QueryContext(ctx, `SELECT id,item_id,first_result_id,conflicting_result_id,created_at FROM verification_result_conflicts
		WHERE item_id IN (SELECT id FROM verification_invocation_items WHERE invocation_id=?) ORDER BY id`, invocationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		conflict := VerificationResultConflict{}
		if err := rows.Scan(&conflict.ID, &conflict.ItemID, &conflict.FirstResultID, &conflict.ConflictingResultID, &conflict.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		detail.Conflicts = append(detail.Conflicts, conflict)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = service.db.QueryContext(ctx, `SELECT object_kind,drift_field,item_id,frozen_digest,current_digest,observed_at FROM verification_subject_drifts
		WHERE invocation_id=? ORDER BY id`, invocationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		drift := VerificationSubjectDrift{}
		if err := rows.Scan(&drift.ObjectKind, &drift.DriftField, &drift.ItemID, &drift.FrozenDigest, &drift.CurrentDigest, &drift.ObservedAt); err != nil {
			rows.Close()
			return nil, err
		}
		detail.SubjectDrifts = append(detail.SubjectDrifts, drift)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return detail, nil
}

func (service *Service) loadReceipt(ctx context.Context, receiptID int64) (*VerificationReceipt, error) {
	receipt := &VerificationReceipt{}
	err := service.db.QueryRowContext(ctx, `SELECT id,manifest_digest,applicable_set_digest,item_set_digest,result_set_digest,
		helper_import_set_digest,typed_observation_set_digest,conflict_set_digest,subject_drift_digest,overall_outcome,
		final_result_digest,canonical_artifact_id,snapshot_at,finalized_at FROM verification_finalization_receipts WHERE id=?`, receiptID).
		Scan(&receipt.ID, &receipt.ManifestDigest, &receipt.ApplicableSetDigest, &receipt.ItemSetDigest, &receipt.ResultSetDigest,
			&receipt.HelperImportSetDigest, &receipt.TypedObservationSetDigest, &receipt.ConflictSetDigest, &receipt.SubjectDriftDigest, &receipt.OverallOutcome,
			&receipt.FinalResultDigest, &receipt.CanonicalArtifactID, &receipt.SnapshotAt, &receipt.FinalizedAt)
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

func (service *Service) loadItems(ctx context.Context, detail *VerificationDetail) error {
	rows, err := service.db.QueryContext(ctx, `SELECT i.id,i.item_seq,i.scenario_id,i.cell_id,i.object_kind,i.input_digest FROM verification_invocation_items i
		WHERE i.invocation_id=? ORDER BY i.item_seq`, detail.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		item := VerificationItem{}
		if err := rows.Scan(&item.ID, &item.ItemSeq, &item.ScenarioID, &item.CellID, &item.ObjectKind, &item.InputDigest); err != nil {
			return err
		}
		detail.Items = append(detail.Items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range detail.Items {
		locator, err := service.loadLocator(ctx, detail.Items[index])
		if err != nil {
			return err
		}
		detail.Items[index].Locator = locator
	}
	return nil
}

func (service *Service) loadLocator(ctx context.Context, item VerificationItem) (map[string]any, error) {
	switch item.ObjectKind {
	case "deployment":
		return locatorMap(ctx, service.db, `SELECT release_subject_digest,deployment_config_digest,public_origin_digest,backend,architecture
			FROM verification_deployment_item_locators WHERE item_id=?`, item.ID, "releaseSubjectDigest", "deploymentConfigDigest", "publicOriginDigest", "backend", "architecture")
	case "connection":
		return locatorMap(ctx, service.db, `SELECT connection_id,connection_revision_id,credential_generation_id,root_binding_revision,probe_contract_digest
			FROM verification_connection_item_locators WHERE item_id=?`, item.ID, "connectionId", "revisionId", "credentialGenerationId", "rootBindingRevision", "probeContractDigest")
	case "config":
		return locatorMap(ctx, service.db, `SELECT business_system_id,config_version_id,label_contract_version_id
			FROM verification_config_item_locators WHERE item_id=?`, item.ID, "businessSystemId", "configVersionId", "labelContractVersionId")
	case "browser_identity":
		return locatorMap(ctx, service.db, `SELECT browser_identity_id,identity_revision_id,profile_generation_id,current_inventory_digest
			FROM verification_browser_identity_item_locators WHERE item_id=?`, item.ID, "browserIdentityId", "identityRevisionId", "currentGenerationId", "currentInventoryDigest")
	case "ui_observation":
		return locatorMap(ctx, service.db, `SELECT browser_artifact,browser_version,architecture,viewport_css_px,motion
			FROM verification_ui_observation_item_locators WHERE item_id=?`, item.ID, "browserArtifact", "browserVersion", "architecture", "viewportCssPx", "motion")
	}
	return nil, service.fail(errState, "item %d carries unknown object kind %q", item.ID, item.ObjectKind)
}

func locatorMap(ctx context.Context, db *sql.DB, query string, itemID int64, keys ...string) (map[string]any, error) {
	columns := make([]any, len(keys))
	pointers := make([]any, len(keys))
	for index := range columns {
		pointers[index] = &columns[index]
	}
	if err := db.QueryRowContext(ctx, query, itemID).Scan(pointers...); err != nil {
		return nil, err
	}
	locator := map[string]any{}
	for index, key := range keys {
		switch value := columns[index].(type) {
		case []byte:
			locator[key] = string(value)
		default:
			locator[key] = value
		}
	}
	return locator, nil
}

// List returns invocation summaries newest first with cursor pagination.
func (service *Service) List(ctx context.Context, afterID int64, limit int) ([]*VerificationDetail, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := service.db.QueryContext(ctx, `SELECT id FROM verification_invocation_manifests WHERE id>? ORDER BY id DESC LIMIT ?`, afterID, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	next := int64(0)
	if len(ids) > limit {
		next = ids[limit-1]
		ids = ids[:limit]
	}
	page := make([]*VerificationDetail, 0, len(ids))
	for _, id := range ids {
		detail, err := service.Load(ctx, id)
		if err != nil {
			return nil, 0, err
		}
		page = append(page, detail)
	}
	return page, next, nil
}
