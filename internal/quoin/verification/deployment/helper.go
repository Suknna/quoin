package deployment

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gen "github.com/Suknna/quoin/internal/gen/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// HelperRequest renders the immutable offline helper request for one
// invocation. The document is a pure projection of frozen rows — generatedAt
// is the manifest start, so re-reading yields byte-identical output and the
// same requestDigest (HTTP-VERIFY-003).
func (service *Service) HelperRequest(ctx context.Context, invocationID int64) ([]byte, string, error) {
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		return nil, "", err
	}
	items, err := service.loadHelperItems(ctx, invocationID)
	if err != nil {
		return nil, "", err
	}
	request := map[string]any{
		"schemaVersion":          1,
		"documentType":           "helper_request",
		"invocationId":           fmt.Sprint(manifest.id),
		"manifestDigest":         manifest.manifestDigest,
		"itemSetDigest":          manifest.itemSetDigest,
		"releaseSubjectDigest":   manifest.releaseSubjectDigest,
		"catalogDigest":          manifest.catalogDigest,
		"resultProfileDigest":    manifest.resultProfileDigest,
		"deploymentConfigDigest": manifest.deploymentConfigDigest,
		"publicOriginDigest":     manifest.publicOriginDigest,
		"backend":                service.binding.Backend,
		"architecture":           service.binding.Architecture,
		"generatedAt":            manifest.startedAt,
		"deadlineAt":             manifest.deadline.Format(time.RFC3339Nano),
		"items":                  items,
	}
	body, err := yaml.Marshal(request)
	if err != nil {
		return nil, "", err
	}
	return body, sha256Hex(body), nil
}

type helperItemView struct {
	ItemID       string         `yaml:"itemId"`
	ScenarioID   string         `yaml:"scenarioId"`
	CellID       string         `yaml:"cellId"`
	InputDigest  string         `yaml:"inputDigest"`
	TypedLocator map[string]any `yaml:"typedLocator"`
}

func (service *Service) loadHelperItems(ctx context.Context, invocationID int64) ([]helperItemView, error) {
	detail, err := service.Load(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	items := make([]helperItemView, 0, len(detail.Items))
	for _, item := range detail.Items {
		locator := map[string]any{}
		for key, value := range item.Locator {
			locator[key] = value
		}
		switch item.ObjectKind {
		case "deployment":
			locator["kind"] = "deployment"
		case "connection":
			locator["kind"] = "connection"
		case "config":
			locator["kind"] = "config"
		case "browser_identity":
			locator["kind"] = "browser_identity"
		case "ui_observation":
			locator["kind"] = "ui_observation"
		}
		items = append(items, helperItemView{
			ItemID: fmt.Sprint(item.ID), ScenarioID: item.ScenarioID, CellID: item.CellID,
			InputDigest: item.InputDigest, TypedLocator: locator,
		})
	}
	return items, nil
}

// helperReport is the typed projection of the frozen helperReport schema.
type helperReport struct {
	SchemaVersion          int                `yaml:"schemaVersion"`
	DocumentType           string             `yaml:"documentType"`
	InvocationID           string             `yaml:"invocationId"`
	ManifestDigest         string             `yaml:"manifestDigest"`
	ItemSetDigest          string             `yaml:"itemSetDigest"`
	ReleaseSubjectDigest   string             `yaml:"releaseSubjectDigest"`
	CatalogDigest          string             `yaml:"catalogDigest"`
	ResultProfileDigest    string             `yaml:"resultProfileDigest"`
	DeploymentConfigDigest string             `yaml:"deploymentConfigDigest"`
	PublicOriginDigest     string             `yaml:"publicOriginDigest"`
	Backend                string             `yaml:"backend"`
	Architecture           string             `yaml:"architecture"`
	HelperRequestDigest    string             `yaml:"helperRequestDigest"`
	StartedAt              string             `yaml:"startedAt"`
	FinishedAt             string             `yaml:"finishedAt"`
	Items                  []helperReportItem `yaml:"items"`
}

type helperReportItem struct {
	ItemID         string             `yaml:"itemId"`
	ScenarioID     string             `yaml:"scenarioId"`
	CellID         string             `yaml:"cellId"`
	InputDigest    string             `yaml:"inputDigest"`
	ResultDigest   string             `yaml:"resultDigest"`
	Outcome        string             `yaml:"outcome"`
	Category       string             `yaml:"category"`
	StartedAt      string             `yaml:"startedAt"`
	FinishedAt     string             `yaml:"finishedAt"`
	ArgvSanitized  []string           `yaml:"argvSanitized"`
	ExitCode       int                `yaml:"exitCode"`
	Assertions     []helperAssertion  `yaml:"assertions"`
	Attachments    []helperAttachment `yaml:"attachments"`
	CleanupOutcome string             `yaml:"cleanupOutcome"`
}

type helperAssertion struct {
	ID       string `yaml:"id"`
	Expected any    `yaml:"expected"`
	Actual   any    `yaml:"actual"`
	Result   string `yaml:"result"`
}

type helperAttachment struct {
	Kind      string `yaml:"kind"`
	SHA256    string `yaml:"sha256"`
	SizeBytes int    `yaml:"sizeBytes"`
	MediaType string `yaml:"mediaType,omitempty"`
}

// ImportHelperReport validates and imports one offline helper report. The
// same reportDigest is idempotent; a different report against the same items
// forms immutable verifier conflicts; late reports after the receipt are
// rejected (HTTP-VERIFY-004, VERIFY-DA-003/006).
func (service *Service) ImportHelperReport(ctx context.Context, invocationID int64, reportYAML []byte) (*VerificationDetail, bool, error) {
	var report helperReport
	if err := yaml.Unmarshal(reportYAML, &report); err != nil {
		return nil, false, service.fail(errInvalid, "parse helper report: %v", err)
	}
	if err := validateHelperReport(reportYAML); err != nil {
		return nil, false, service.fail(errInvalid, "helper report violates the frozen exchange schema: %v", err)
	}
	manifest, err := service.loadManifest(ctx, invocationID)
	if err != nil {
		return nil, false, err
	}
	if report.InvocationID != fmt.Sprint(manifest.id) || report.ManifestDigest != manifest.manifestDigest {
		return nil, false, service.fail(errInvalid, "helper report does not bind this invocation manifest")
	}
	if _, requestDigest, err := service.HelperRequest(ctx, invocationID); err != nil {
		return nil, false, err
	} else if report.HelperRequestDigest != requestDigest {
		return nil, false, service.fail(errInvalid, "helper report does not close over the exported helper request digest")
	}
	if report.ItemSetDigest != manifest.itemSetDigest || report.ReleaseSubjectDigest != manifest.releaseSubjectDigest ||
		report.CatalogDigest != manifest.catalogDigest || report.ResultProfileDigest != manifest.resultProfileDigest ||
		report.DeploymentConfigDigest != manifest.deploymentConfigDigest || report.PublicOriginDigest != manifest.publicOriginDigest {
		return nil, false, service.fail(errInvalid, "helper report identity digests do not match the frozen manifest")
	}
	if service.binding == nil || report.Backend != service.binding.Backend || report.Architecture != service.binding.Architecture {
		return nil, false, service.fail(errInvalid, "helper report platform does not match this deployment")
	}
	seenItems := map[string]bool{}
	for _, item := range report.Items {
		if seenItems[item.ItemID] {
			return nil, false, service.fail(errInvalid, "helper report duplicates item %s", item.ItemID)
		}
		seenItems[item.ItemID] = true
	}
	reportDigest := sha256Hex(reportYAML)
	if service.artifacts == nil {
		return nil, false, service.fail(errUnavailable, "artifact store is not wired")
	}
	// Durable report artifact first: the writer transaction binds its exact
	// bytes; staging inside the writer would self-deadlock the store.
	receivedAt := service.now().UTC()
	artifactID, blobDigest, err := service.artifacts.CommitVerificationArtifact(ctx, "verification_attachment", "application/yaml", "long_term", invocationID, reportYAML, receivedAt)
	if err != nil {
		return nil, false, fmt.Errorf("stage helper report: %w", err)
	}
	if blobDigest != reportDigest {
		return nil, false, service.fail(errState, "helper report artifact digest mismatch")
	}
	created := false
	err = service.withConn(ctx, func(conn *sql.Conn) error {
		var receipt int64
		err := conn.QueryRowContext(ctx, `SELECT id FROM verification_finalization_receipts WHERE invocation_id=?`, invocationID).Scan(&receipt)
		if err == nil {
			return service.fail(errConflict, "verification invocation is already finalized; late helper reports are rejected")
		}
		if err != sql.ErrNoRows {
			return err
		}
		var existing int64
		err = conn.QueryRowContext(ctx, `SELECT id FROM verification_helper_imports WHERE invocation_id=? AND report_digest=?`, invocationID, reportDigest).Scan(&existing)
		if err == nil {
			return nil // idempotent re-import of the identical report
		}
		if err != sql.ErrNoRows {
			return err
		}
		now := service.now().UTC()
		if now.After(manifest.deadline) {
			return service.fail(errConflict, "invocation missed its fixed deadline; late reports are rejected")
		}
		// Frozen item closure for every reported item.
		frozen := map[string]int64{}
		rows, err := conn.QueryContext(ctx, `SELECT id,input_digest FROM verification_invocation_items WHERE invocation_id=?`, invocationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var inputDigest string
			if err := rows.Scan(&id, &inputDigest); err != nil {
				rows.Close()
				return err
			}
			frozen[fmt.Sprint(id)] = id
			if item, ok := findByInput(report.Items, inputDigest); ok && item.ItemID != fmt.Sprint(id) {
				rows.Close()
				return service.fail(errInvalid, "helper report input digest does not bind its declared item")
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, item := range report.Items {
			id, known := frozen[item.ItemID]
			if !known {
				return service.fail(errInvalid, "helper report references unknown item %s", item.ItemID)
			}
			var frozenInput string
			if err := conn.QueryRowContext(ctx, `SELECT input_digest FROM verification_invocation_items WHERE id=?`, id).Scan(&frozenInput); err != nil {
				return err
			}
			if frozenInput != item.InputDigest {
				return service.fail(errInvalid, "helper report input digest does not match frozen item %s", item.ItemID)
			}
		}
		helperStarted, err := time.Parse(time.RFC3339Nano, report.StartedAt)
		if err != nil {
			return service.fail(errInvalid, "helper report startedAt is not a valid timestamp")
		}
		helperFinished, err := time.Parse(time.RFC3339Nano, report.FinishedAt)
		if err != nil {
			return service.fail(errInvalid, "helper report finishedAt is not a valid timestamp")
		}
		if helperFinished.Before(helperStarted) {
			return service.fail(errInvalid, "helper report finishedAt precedes startedAt")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO verification_helper_imports(invocation_id,request_digest,report_digest,helper_reported_started_at,helper_reported_finished_at,received_at,artifact_id)
			VALUES(?,?,?,?,?,?,?)`, invocationID, manifest.canonicalInputDigest, reportDigest,
			helperStarted.UTC().Format(time.RFC3339Nano), helperFinished.UTC().Format(time.RFC3339Nano),
			receivedAt.Format(time.RFC3339Nano), artifactID); err != nil {
			return translateInsert(err, "helper import")
		}
		for _, item := range report.Items {
			id := frozen[item.ItemID]
			evidenceDigest, err := canonicalDigest(map[string]any{
				"resultDigest": item.ResultDigest, "assertions": item.Assertions,
				"attachments": item.Attachments, "cleanupOutcome": item.CleanupOutcome, "exitCode": item.ExitCode,
			})
			if err != nil {
				return err
			}
			observed, err := time.Parse(time.RFC3339Nano, item.FinishedAt)
			if err != nil {
				return service.fail(errInvalid, "helper report item finishedAt is not a valid timestamp")
			}
			_, err = conn.ExecContext(ctx, `INSERT INTO verification_item_results(item_id,input_digest,result_digest,producer_type,outcome,category,observed_at,committed_at,evidence_index_digest)
				VALUES(?,?,?,?,?,?,?,?,?)`, id, item.InputDigest, item.ResultDigest, "deployment_helper", item.Outcome, item.Category,
				observed.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), evidenceDigest)
			if err != nil {
				return translateInsert(err, "helper report result")
			}
			if item.Category == "subject_drift" {
				// The receipt closure demands a matching immutable drift marker
				// for a subject_drift result. Quoin freezes the manifest-side
				// digest and binds the helper's observed identity through the
				// result digest: the marker is the durable, auditable form of
				// the helper's drift assertion.
				frozen := manifest.deploymentConfigDigest
				kind := "deployment"
				if item.ResultDigest == "" {
					continue
				}
				if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO verification_subject_drifts(invocation_id,object_kind,drift_field,item_id,frozen_digest,current_digest,observed_at)
					VALUES(?,?,?,?,?,?,?)`, invocationID, kind, "deployment_config_digest", id, sha256Text(frozen), sha256Text(item.ResultDigest), observed.UTC().Format(time.RFC3339Nano)); err != nil {
					return translateInsert(err, "helper drift marker")
				}
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	_ = service.observeDrifts(ctx, invocationID)
	detail, err := service.Load(ctx, invocationID)
	if err != nil {
		return nil, false, err
	}
	return detail, created, nil
}

func findByInput(items []helperReportItem, inputDigest string) (helperReportItem, bool) {
	for _, item := range items {
		if item.InputDigest == inputDigest {
			return item, true
		}
	}
	return helperReportItem{}, false
}

// validateHelperReport enforces the frozen deployment-verification schema,
// including the deterministic itemId uniqueness the schema declares through
// x-quoin-unique-by (array order and deep equality are not substitutes).
func validateHelperReport(reportYAML []byte) error {
	var decoded any
	if err := yaml.Unmarshal(reportYAML, &decoded); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(strings.NewReader(string(gen.DeploymentVerificationSchema)))
	if err != nil {
		return fmt.Errorf("load frozen schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	const resource = "https://raw.githubusercontent.com/Suknna/quoin/main/docs/specs/quoin-v1/contracts/schemas/deployment-verification.schema.json"
	if err := compiler.AddResource(resource, schemaDocument); err != nil {
		return err
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("compile frozen schema: %w", err)
	}
	if err := schema.Validate(document); err != nil {
		return err
	}
	var report helperReport
	if err := yaml.Unmarshal(reportYAML, &report); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, item := range report.Items {
		if seen[item.ItemID] {
			return fmt.Errorf("duplicate report item %s", item.ItemID)
		}
		seen[item.ItemID] = true
	}
	return nil
}
