// Package modelprovider batch embedding executor (T29): the supervisor's
// direct Embedding work mode. One attempt embeds its frozen input batch
// through the provider's /v1/embeddings endpoint with exactly one
// Begin/CompleteModelCall ledger pair, then proposes the closed typed
// result. No worker, no agent, no ReAct loop (RUNTIME-AGENT-010).
package modelprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	sharedops "github.com/Suknna/quoin/internal/ops"
)

// BatchInput is the frozen embedding_v1 attempt input.
type BatchInput struct {
	SchemaKind    string        `json:"schemaKind"`
	AttemptID     int64         `json:"attemptId"`
	GenerationID  int64         `json:"generationId"`
	Mode          string        `json:"mode"`
	ModelContract ModelContract `json:"modelContract"`
	Inputs        []TextInput   `json:"inputs,omitempty"`
	Query         *TextInput    `json:"query,omitempty"`
}

// ModelContract is the frozen embedding contract.
type ModelContract struct {
	EmbeddingModelID string `json:"embeddingModelId"`
	VectorDim        int64  `json:"vectorDim"`
}

// TextInput is one frozen text with its digest.
type TextInput struct {
	VersionID int64  `json:"versionId,omitempty"`
	Text      string `json:"text"`
	Digest    string `json:"digest"`
}

// BatchOutcome is the closed result payload (schema kind selected by mode).
type BatchOutcome struct {
	SchemaKind   string         `json:"schemaKind"`
	AttemptID    int64          `json:"attemptId"`
	GenerationID int64          `json:"generationId"`
	ModelID      string         `json:"modelId"`
	Vectors      []ResultVector `json:"vectors,omitempty"`
	Digest       string         `json:"digest,omitempty"`
	Values       []float32      `json:"values,omitempty"`
}

// ResultVector is one proposed version vector.
type ResultVector struct {
	VersionID int64     `json:"versionId"`
	Digest    string    `json:"digest"`
	Values    []float32 `json:"values"`
}

// ResultSchemaKind is the rebuild result payload kind.
const ResultSchemaKind = "embedding_generation_result_v1"

// QuerySchemaKind is the query result payload kind.
const QuerySchemaKind = "embedding_query_result_v1"

// ParseBatchInput decodes and validates the frozen input envelope.
func ParseBatchInput(raw []byte, attemptID int64) (BatchInput, error) {
	var input BatchInput
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("embedding input is not valid closed JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return input, fmt.Errorf("embedding input carries trailing data")
	}
	if input.SchemaKind != "embedding_v1" || input.AttemptID != attemptID || input.ModelContract.EmbeddingModelID == "" {
		return input, fmt.Errorf("embedding input identity envelope invalid")
	}
	if input.Mode == "rebuild" && len(input.Inputs) == 0 {
		return input, fmt.Errorf("embedding rebuild input carries no texts")
	}
	if input.Mode == "query" && input.Query == nil {
		return input, fmt.Errorf("embedding query input carries no query text")
	}
	return input, nil
}

// RunBatch executes one embedding attempt: the frozen batch against the
// provider with one ledger pair, returning the canonical result payload and
// its digest. A provider failure returns the error; the caller proposes the
// failed outcome.
func RunBatch(ctx context.Context, config Config, apiKey string, input BatchInput, ledger Ledger) ([]byte, [32]byte, error) {
	texts := make([]string, 0, len(input.Inputs)+1)
	digests := make([]string, 0, len(input.Inputs)+1)
	if input.Mode == "query" {
		texts = append(texts, input.Query.Text)
		digests = append(digests, input.Query.Digest)
	} else {
		for _, item := range input.Inputs {
			texts = append(texts, item.Text)
			digests = append(digests, item.Digest)
		}
	}
	requestBody, _ := json.Marshal(map[string]any{"model": input.ModelContract.EmbeddingModelID, "input": texts})
	rendered := sha256.Sum256(requestBody)
	whole := sha256.Sum256([]byte(fmt.Sprintf("embedding-batch:%d:%s", input.AttemptID, input.ModelContract.EmbeddingModelID)))
	callID, _, ok := ledger.Begin(ctx, 1, runtimev1.ModelOperation_MODEL_OPERATION_EMBEDDING,
		input.ModelContract.EmbeddingModelID,
		hex.EncodeToString(whole[:]), hex.EncodeToString(rendered[:]), 0, 0)
	if !ok {
		return nil, [32]byte{}, fmt.Errorf("embedding model call ledger rejected the begin")
	}
	result, vectors, err := postEmbeddings(ctx, config, apiKey, input.ModelContract.EmbeddingModelID, texts)
	if err != nil {
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
				AttemptId: attemptOf(ctx), ModelCallId: callID,
				Outcome:       runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
				FailureReason: failureReasonFor(err),
			})
		}
		return nil, [32]byte{}, err
	}
	if len(vectors) != len(texts) {
		if ok {
			ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
				AttemptId: attemptOf(ctx), ModelCallId: callID,
				Outcome:       runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
				FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE,
			})
		}
		return nil, [32]byte{}, fmt.Errorf("provider returned %d vectors for %d inputs", len(vectors), len(texts))
	}
	wire := make([]*runtimev1.EmbeddingVector, 0, len(vectors))
	outcome := BatchOutcome{
		AttemptID: input.AttemptID, GenerationID: input.GenerationID,
		ModelID:    input.ModelContract.EmbeddingModelID,
		SchemaKind: map[bool]string{true: QuerySchemaKind, false: ResultSchemaKind}[input.Mode == "query"],
	}
	digestBytes, _ := hex.DecodeString(digests[0])
	for index, vector := range vectors {
		if input.ModelContract.VectorDim > 0 && int64(len(vector)) != input.ModelContract.VectorDim {
			if ok {
				ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
					AttemptId: attemptOf(ctx), ModelCallId: callID,
					Outcome:       runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_FAILED,
					FailureReason: runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE,
				})
			}
			return nil, [32]byte{}, fmt.Errorf("provider vector dimension %d does not match frozen contract %d", len(vector), input.ModelContract.VectorDim)
		}
		itemDigest, _ := hex.DecodeString(digests[index])
		wire = append(wire, &runtimev1.EmbeddingVector{InputIndex: uint32(index), SourceDigest: itemDigest, Values: toFloat32(vector)})
		if input.Mode == "query" {
			outcome.Digest = digests[index]
			outcome.Values = toFloat32(vector)
		} else {
			outcome.Vectors = append(outcome.Vectors, ResultVector{VersionID: input.Inputs[index].VersionID, Digest: digests[index], Values: toFloat32(vector)})
		}
	}
	if ok {
		ledger.Complete(ctx, callID, &runtimev1.CompleteModelCall{
			AttemptId: attemptOf(ctx), ModelCallId: callID,
			Outcome:           runtimev1.ModelCallOutcome_MODEL_CALL_OUTCOME_SUCCEEDED,
			ProviderRequestId: result.RequestID,
			InputTokens:       uint64(result.Usage.Input), TotalTokens: uint64(result.Usage.Total),
			ResponseDigest: digestOf(fmt.Sprintf("embedding-batch:%d", input.AttemptID)), ResponseComplete: true,
			EmbeddingVectors: wire,
		})
	}
	_ = digestBytes
	canonical, marshalErr := json.Marshal(outcome)
	if marshalErr != nil {
		return nil, [32]byte{}, marshalErr
	}
	sum := sha256.Sum256(canonical)
	sharedops.LogEvent("plinth", "info", "embedding.batch_embedded", fmt.Sprintf("attempt=%d mode=%s vectors=%d", input.AttemptID, input.Mode, len(vectors)))
	return canonical, sum, nil
}

// postEmbeddings performs the raw provider embeddings request.
func postEmbeddings(ctx context.Context, config Config, apiKey, modelID string, texts []string) (ActionResult, [][]float64, error) {
	probe := newClient(config, apiKey)
	return RunEmbeddingBatch(ctx, probe, modelID, texts)
}

func failureReasonFor(err error) runtimev1.ModelCallFailureReason {
	message := err.Error()
	if strings.Contains(message, "HTTP 429") {
		return runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_RATE_LIMITED
	}
	if strings.Contains(message, "请求失败") {
		return runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_TRANSPORT_ERROR
	}
	return runtimev1.ModelCallFailureReason_MODEL_CALL_FAILURE_REASON_INVALID_RESPONSE
}
