package kubernetes

import (
	"encoding/json"
	"errors"

	"github.com/Suknna/quoin/internal/quoin/evidence"
)

// EvidenceFor derives bounded, deterministic facts from a successful
// kubernetes_read result. Raw per-mapping output stays solely in the sealed
// tool result or its Artifact; it is never copied into Evidence.
func EvidenceFor(argumentsJSON, payloadJSON []byte, artifactID int64) (evidence.Projection, error) {
	var payload struct {
		Success     bool              `json:"success"`
		Operation   string            `json:"operation"`
		ObservedAt  string            `json:"observedAt"`
		Results     []json.RawMessage `json:"results"`
		ErrorCode   string            `json:"errorCode"`
		ErrorDetail string            `json:"errorDetail"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return evidence.Projection{}, err
	}
	if payload.Operation == "" || payload.ObservedAt == "" || len(payload.Results) == 0 {
		return evidence.Projection{}, errors.New("kubernetes result lacks observation facts")
	}
	partial := payload.ErrorCode == "partial_failure"
	if partial && artifactID == 0 {
		return evidence.Projection{}, errors.New("only artifact-backed Kubernetes partial failures produce evidence")
	}
	var successful, failed int
	for _, result := range payload.Results {
		var item struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(result, &item); err != nil {
			return evidence.Projection{}, errors.New("kubernetes result item is invalid")
		}
		if item.Success {
			successful++
		} else {
			failed++
		}
	}
	if partial && (successful == 0 || failed == 0) {
		return evidence.Projection{}, errors.New("partial Kubernetes evidence requires successful and failed mappings")
	}
	body, err := json.Marshal(map[string]any{"operation": payload.Operation, "mappingCount": len(payload.Results), "successfulMappings": successful, "failedMappings": failed})
	if err != nil {
		return evidence.Projection{}, err
	}
	projection := evidence.Projection{ParamsJSON: argumentsJSON, ObservedAt: payload.ObservedAt, Integrity: "complete"}
	if partial {
		projection.Integrity = "incomplete"
		projection.ErrorsJSON, err = json.Marshal(map[string]any{"code": payload.ErrorCode, "detail": payload.ErrorDetail, "failedMappings": failed})
		if err != nil {
			return evidence.Projection{}, err
		}
	}
	if artifactID > 0 {
		projection.ArtifactID = artifactID
	} else {
		projection.ResultJSON = body
	}
	return projection, nil
}
