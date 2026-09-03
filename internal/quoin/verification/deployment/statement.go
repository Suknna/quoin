package deployment

import (
	"context"
	"fmt"
	"time"
)

// buildStatement assembles the canonical in-toto Test Result statement for
// the finalization bundle. It validates against the frozen
// verification-result schema before it can become the receipt digest.
func (service *Service) buildStatement(ctx context.Context, manifest *manifestRecord, verdicts []itemVerdict, digests *setDigests, outcome string, snapshot, finalized time.Time) (map[string]any, map[string]any, error) {
	if service.binding == nil {
		return nil, nil, service.ErrBindingUnavailable()
	}
	passed, warned, failed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	cellCounts := map[string]int{"passed": 0, "warned": 0, "failed": 0}
	for _, verdict := range verdicts {
		switch verdict.outcome {
		case "passed":
			passed[verdict.scenario] = true
		case "warned":
			warned[verdict.scenario] = true
		case "failed":
			failed[verdict.scenario] = true
		}
		cellCounts[verdict.outcome]++
	}
	evidence := map[string]any{
		"invocationId":       fmt.Sprint(manifest.id),
		"resultSetDigest":    digests.resultSet,
		"helperImportDigest": digests.helperImportSet,
		"observationDigest":  digests.observationSet,
		"conflictDigest":     digests.conflictSet,
		"driftDigest":        digests.driftSet,
		"items":              len(verdicts),
	}
	evidenceBody, err := canonicalJSON(evidence)
	if err != nil {
		return nil, nil, err
	}
	environmentMatrix, err := canonicalDigest([]map[string]any{{
		"backend": service.binding.Backend, "architecture": service.binding.Architecture,
		"releaseSubjectDigest": manifest.releaseSubjectDigest, "deploymentConfigDigest": manifest.deploymentConfigDigest,
	}})
	if err != nil {
		return nil, nil, err
	}
	finalResult, err := outcomeResult(outcome)
	if err != nil {
		return nil, nil, err
	}
	artifactLocator := fmt.Sprintf("verification-invocations/%d/bundle", manifest.id)
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": "https://in-toto.io/attestation/test-result/v0.1",
		"subject": []map[string]any{{
			"name": service.binding.ReleaseVersion,
			"digest": map[string]any{
				"sha256": manifest.releaseSubjectDigest,
			},
		}},
		"predicate": map[string]any{
			"result": finalResult,
			"configuration": []map[string]any{
				{"name": "verification-catalog.yaml", "digest": map[string]any{"sha256": CatalogDigest()}},
				{"name": "verification-result-profile.yaml", "digest": map[string]any{"sha256": ResultProfileDigest()}},
				{"name": "deployment-config", "digest": map[string]any{"sha256": manifest.deploymentConfigDigest}},
			},
			"passedTests": sortedSet(passed),
			"warnedTests": sortedSet(warned),
			"failedTests": sortedSet(failed),
			"quoin": map[string]any{
				"profileVersion": 1,
				"invocationId":   fmt.Sprint(manifest.id),
				"layer":          "deployment_acceptance",
				"startedAt":      manifest.startedAt,
				"finishedAt":     finalized.Format(time.RFC3339Nano),
				"environment": map[string]any{
					"matrixDigest": environmentMatrix,
					"cellCount":    1,
				},
				"evidenceIndex": map[string]any{
					"sha256":    sha256Hex(evidenceBody),
					"mediaType": "application/vnd.quoin.verification-evidence.v1+json",
					"sizeBytes": len(evidenceBody),
				},
				"observationSummary": map[string]any{
					"requiredCells": len(verdicts),
					"passedCells":   cellCounts["passed"],
					"warnedCells":   cellCounts["warned"],
					"failedCells":   cellCounts["failed"],
				},
				"deploymentAcceptance": map[string]any{
					"releaseSubjectDigest":   manifest.releaseSubjectDigest,
					"backend":                service.binding.Backend,
					"architecture":           service.binding.Architecture,
					"deploymentConfigDigest": manifest.deploymentConfigDigest,
					"publicOrigin":           service.publicOrigin,
					"helperReportDigest":     digests.helperImportSet,
					"observationWindow": map[string]any{
						"startedAt": manifest.startedAt,
						"endedAt":   finalized.Format(time.RFC3339Nano),
					},
					"snapshotAt":            snapshot.Format(time.RFC3339Nano),
					"finalizedAt":           finalized.Format(time.RFC3339Nano),
					"manifestDigest":        manifest.manifestDigest,
					"finalizationReceiptId": fmt.Sprint(manifest.id),
					"artifactLocator":       artifactLocator,
				},
			},
		},
	}
	return statement, evidence, nil
}

func outcomeResult(outcome string) (string, error) {
	switch outcome {
	case "passed":
		return "PASSED", nil
	case "warned":
		return "WARNED", nil
	case "failed":
		return "FAILED", nil
	}
	return "", fmt.Errorf("unknown suite outcome %q", outcome)
}

func sortedSet(values map[string]bool) []string {
	set := make([]string, 0, len(values))
	for value := range values {
		set = append(set, value)
	}
	for i := 1; i < len(set); i++ {
		for j := i; j > 0 && set[j] < set[j-1]; j-- {
			set[j], set[j-1] = set[j-1], set[j]
		}
	}
	return set
}
