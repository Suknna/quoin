package observation

import (
	"fmt"

	"github.com/Suknna/quoin/internal/verification/evidence"
)

// FormSchema is the stable kind of the generated observation form/wizard
// document (VERIFY-OBSERVATION-001).
const FormSchema = "quoin-ui-observation-form-v1"

// TypedField is one closed typed field of a form: the catalog assertion ID,
// its kind and the frozen expectation. Observations may only answer these
// fields with the closed result vocabulary — free text never replaces a
// typed observation.
const (
	FieldResultPassed = "passed"
	FieldResultFailed = "failed"
)

// Form is the deterministic observation wizard of one cell, generated from
// the catalog and bound to the invocation, subject digest, browser
// artifact and observer session before any value is recorded.
type Form struct {
	Schema                string       `json:"schema"`
	Tag                   string       `json:"tag"`
	InvocationID          string       `json:"invocation_id"`
	ScenarioID            string       `json:"scenario_id"`
	CellID                string       `json:"cell_id"`
	ReleaseSubjectDigest  string       `json:"release_subject_digest"`
	PublicOriginDigest    string       `json:"public_origin_digest"`
	BrowserSubject        string       `json:"browser_subject"`
	Architecture          string       `json:"architecture"`
	BrowserArtifactSHA256 string       `json:"browser_artifact_sha256"`
	BrowserBuild          string       `json:"browser_build"`
	ViewportCSSPx         int          `json:"viewport_css_px"`
	MotionMode            string       `json:"motion_mode"`
	Fields                []TypedField `json:"fields"`
}

// TypedField mirrors one catalog assertion as a closed form field.
type TypedField struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Expected any    `json:"expected"`
}

// FormBinding carries the invocation-wide inputs every generated form is
// bound to. BrowserSubjects is keyed by cell ID.
type FormBinding struct {
	Tag                  string
	InvocationID         string
	ReleaseSubjectDigest string
	PublicOriginDigest   string
	BrowserSubjects      map[string]BrowserSubject
}

// Forms generates the full observation matrix forms in canonical cell
// order. Every cell must have a browser subject carrying the frozen
// artifact digest; the observed build string may still be empty on hosts
// that cannot execute the cell's architecture (the native runner
// completes it) — the recorded observation itself always requires the
// observed build, so a form alone never proves an observation.
func Forms(matrix *Matrix, binding FormBinding) ([]Form, error) {
	if binding.InvocationID == "" || binding.ReleaseSubjectDigest == "" {
		return nil, fmt.Errorf("forms bind invocation and release subject digest; both are required")
	}
	forms := make([]Form, 0, len(matrix.Cells))
	for _, cell := range matrix.Cells {
		subject, ok := binding.BrowserSubjects[cell.ID]
		if !ok {
			return nil, fmt.Errorf("cell %s has no browser subject binding", cell.ID)
		}
		if subject.SHA256 == "" {
			return nil, fmt.Errorf("cell %s browser subject carries no frozen artifact digest", cell.ID)
		}
		form := Form{
			Schema:                FormSchema,
			Tag:                   binding.Tag,
			InvocationID:          binding.InvocationID,
			ScenarioID:            matrix.ScenarioID,
			CellID:                cell.ID,
			ReleaseSubjectDigest:  binding.ReleaseSubjectDigest,
			PublicOriginDigest:    binding.PublicOriginDigest,
			BrowserSubject:        cell.Key.BrowserSubject,
			Architecture:          cell.Key.Architecture,
			BrowserArtifactSHA256: subject.SHA256,
			BrowserBuild:          subject.Build,
			ViewportCSSPx:         cell.Key.ViewportCSSPx,
			MotionMode:            cell.Key.MotionMode,
		}
		for _, assertion := range cell.Assertions {
			form.Fields = append(form.Fields, TypedField{ID: assertion.ID, Kind: assertion.Kind, Expected: assertion.Expected})
		}
		forms = append(forms, form)
	}
	return forms, nil
}

// FormDigest is the content digest of a form excluding nothing: forms are
// immutable once generated, so the whole document identifies the wizard a
// given observation was recorded against.
func FormDigest(form Form) string {
	return evidence.Digest([]byte(evidence.CanonicalJSON(form)))
}

// ObservationDigest identifies one recorded observation by content; the
// ledger uses it for idempotent replay and conflict detection.
func ObservationDigest(observation CellObservation) string {
	return evidence.Digest([]byte(evidence.CanonicalJSON(observation)))
}
