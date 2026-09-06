package manifest

// The categorized evidence contract: every validation category binds one
// signed bundle whose statement is PASSED, covers the category's frozen
// catalog cells and binds the release subject — directly through the
// inventory digest, transitively through the qualification manifest the
// suites projected from that inventory, or, for the contract gate,
// through the source revision. The delegation allowance exists for the
// local acceptance mechanism proof only (see Inputs).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Suknna/quoin/internal/release/signing"
	"github.com/Suknna/quoin/internal/release/subjects"
	"github.com/Suknna/quoin/internal/release/supplychain"
)

// VerifyEvidence proves every closed category carries one verified PASSED
// bundle bound to the release subject and covering its frozen cells, and
// returns the one-way evidence bindings. The bundle digest of the binding
// covers the bundle bytes only: the companion travels beside the bundle
// and is bound through the bundle's signature. The full form additionally
// accepts the transitive qualification-manifest binding and the
// source-revision contract-gate binding.
func VerifyEvidence(evidence map[string]EvidenceInput, subjectDigest string, inventory *subjects.Inventory, sourceSubject *SourceSubject, trust supplychain.Trust) (map[string]PassedEvidence, error) {
	return VerifyEvidenceWithDelegation(evidence, subjectDigest, inventory, sourceSubject, nil, trust)
}

// VerifyEvidenceWithDelegation is the full form with the
// local-acceptance delegation allowance (see
// Inputs.AllowDelegatedCategories); the production gate passes the same
// allowance finalize used so re-verification matches construction.
func VerifyEvidenceWithDelegation(evidence map[string]EvidenceInput, subjectDigest string, inventory *subjects.Inventory, sourceSubject *SourceSubject, delegated map[string]bool, trust supplychain.Trust) (map[string]PassedEvidence, error) {
	for _, category := range Categories {
		if _, ok := evidence[category]; !ok {
			return nil, fmt.Errorf("validation category %q has no evidence bundle", category)
		}
	}
	if len(evidence) != len(Categories) {
		return nil, fmt.Errorf("evidence set has %d bundles, want the closed %d", len(evidence), len(Categories))
	}
	for category, input := range evidence {
		expected := subjectDigestOf(input, subjectDigest)
		if category == "contracts" && sourceSubject != nil {
			expected = sourceSubject.Digest
		}
		if _, err := supplychain.VerifyBundleWithPayload(input.Bundle, input.Payload, "sha256:"+expected, trust); err != nil {
			return nil, fmt.Errorf("category %s: %w", category, err)
		}
		payload, _, err := signing.BundlePayload(input.Bundle, input.Payload)
		if err != nil {
			return nil, fmt.Errorf("category %s: %w", category, err)
		}
		if err := checkCategory(category, payload, input, subjectDigest, inventory, sourceSubject, delegated); err != nil {
			return nil, fmt.Errorf("category %s: %w", category, err)
		}
	}
	validation := map[string]PassedEvidence{}
	for category, input := range evidence {
		digest := sha256.Sum256(input.Bundle)
		validation[category] = PassedEvidence{Status: "passed", EvidenceSHA256: hex.EncodeToString(digest[:])}
	}
	return validation, nil
}

// subjectDigestOf returns the digest the bundle's own statement subject
// must carry: the inventory digest directly, or — for a qualification
// statement bound to its manifest — the qualification manifest's digest.
func subjectDigestOf(input EvidenceInput, inventoryDigest string) string {
	if len(input.QualificationManifest) == 0 {
		return inventoryDigest
	}
	sum := sha256.Sum256(input.QualificationManifest)
	return hex.EncodeToString(sum[:])
}

// checkCategory enforces the per-category evidence contract: test-result
// categories must be PASSED statements of the right layer covering the
// frozen cells; the two report categories must be passed typed reports.
// The subject binding is dual: the release inventory digest directly, or —
// for qualification statements — the digest of the qualification-manifest
// companion whose images block must equal the inventory's subjects.
// Delegated categories may additionally carry a signed delegation report.
func checkCategory(category string, payload []byte, input EvidenceInput, subjectDigest string, inventory *subjects.Inventory, sourceSubject *SourceSubject, delegated map[string]bool) error {
	switch category {
	case "contracts":
		statement, err := signing.DecodeTestResult(payload)
		if err != nil {
			return err
		}
		if statement.PredicateType != TestResultPredicate {
			return fmt.Errorf("predicateType %q is not a Test Result", statement.PredicateType)
		}
		if statement.Layer != layerContractGate {
			return fmt.Errorf("layer %q, want %s", statement.Layer, layerContractGate)
		}
		if sourceSubject != nil {
			if statement.SubjectDigest != sourceSubject.Digest {
				return fmt.Errorf("subject digest does not bind this source revision")
			}
		} else if statement.SubjectDigest != subjectDigest {
			return fmt.Errorf("subject %q does not bind this release", statement.SubjectDigest)
		}
		if statement.Result != "PASSED" || len(statement.PassedTests) == 0 {
			return fmt.Errorf("contract gate result %q over %d passed tests is not a passing invocation", statement.Result, len(statement.PassedTests))
		}
	case "offline_import":
		report, err := decodeReportStatement(payload, KindOfflineImport)
		if err != nil {
			return err
		}
		if report.Result != "passed" {
			return fmt.Errorf("offline import result %q", report.Result)
		}
		if input.QualificationManifest != nil {
			return fmt.Errorf("the offline import report binds the release inventory directly")
		}
	case "supply_chain":
		report, err := decodeReportStatement(payload, KindSupplyChainGate)
		if err != nil {
			return err
		}
		if report.Result != "passed" {
			return fmt.Errorf("supply chain gate result %q", report.Result)
		}
		if input.QualificationManifest != nil {
			return fmt.Errorf("the supply chain report binds the release inventory directly")
		}
	default:
		if delegated[category] {
			if report, err := decodeReportStatement(payload, KindDelegation); err == nil && report.Result == "delegated" {
				if input.QualificationManifest != nil {
					return fmt.Errorf("the delegation report binds the release inventory directly")
				}
				return nil
			}
		}
		statement, err := signing.DecodeTestResult(payload)
		if err != nil {
			return err
		}
		if statement.PredicateType != TestResultPredicate {
			return fmt.Errorf("predicateType %q is not a Test Result", statement.PredicateType)
		}
		if statement.Layer != layerRelease {
			return fmt.Errorf("layer %q, want %s", statement.Layer, layerRelease)
		}
		if statement.SubjectDigest != subjectDigest {
			if len(input.QualificationManifest) == 0 || inventory == nil {
				return fmt.Errorf("subject %q does not bind this release", statement.SubjectDigest)
			}
			if err := qualificationImagesEqual(input.QualificationManifest, inventory); err != nil {
				return fmt.Errorf("transitive subject binding: %w", err)
			}
		}
		if statement.Result != "PASSED" {
			return fmt.Errorf("result %q is not PASSED", statement.Result)
		}
		passed := map[string]bool{}
		for _, test := range statement.PassedTests {
			passed[test] = true
		}
		for _, required := range requiredCells[category] {
			if !passed[required] {
				return fmt.Errorf("passed tests do not cover required cell %s", required)
			}
		}
	}
	return nil
}

// qualificationImagesEqual proves the qualification-manifest companion
// projects exactly the release inventory's image subjects: every index
// digest and both platform digests equal. Repositories are transport
// locations — CI mirrors the release subjects into an invocation-local
// registry before deployment — so only the digests, the subject identity,
// are compared.
func qualificationImagesEqual(companion []byte, inventory *subjects.Inventory) error {
	var shape struct {
		Images map[string]struct {
			Repository  string            `json:"repository"`
			IndexDigest string            `json:"index_digest"`
			Platforms   map[string]string `json:"platforms"`
		} `json:"images"`
	}
	if err := json.Unmarshal(companion, &shape); err != nil {
		return fmt.Errorf("companion is not a qualification manifest: %w", err)
	}
	if len(shape.Images) != len(subjects.Components) {
		return fmt.Errorf("companion carries %d image subjects, want %d", len(shape.Images), len(subjects.Components))
	}
	for _, component := range subjects.Components {
		image, ok := shape.Images[component]
		if !ok {
			return fmt.Errorf("companion image %s missing", component)
		}
		subject := inventory.Images[component]
		if image.IndexDigest != subject.IndexDigest {
			return fmt.Errorf("%s index digest drift: companion %s inventory %s",
				component, image.IndexDigest, subject.IndexDigest)
		}
		for _, platform := range subjects.Platforms {
			if image.Platforms[platform] != subject.Platforms[platform] {
				return fmt.Errorf("%s %s platform digest drift", component, platform)
			}
		}
	}
	return nil
}

func decodeReportStatement(payload []byte, kind string) (signing.ReportStatement, error) {
	var shape struct {
		PredicateType string `json:"predicateType"`
	}
	if err := json.Unmarshal(payload, &shape); err != nil {
		return signing.ReportStatement{}, err
	}
	if shape.PredicateType != ReportPredicate {
		return signing.ReportStatement{}, fmt.Errorf("predicateType %q is not a release report", shape.PredicateType)
	}
	report, err := signing.DecodeReport(payload)
	if err != nil {
		return signing.ReportStatement{}, err
	}
	if report.Kind != kind {
		return signing.ReportStatement{}, fmt.Errorf("report kind %q, want %q", report.Kind, kind)
	}
	return report, nil
}
