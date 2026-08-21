package worker

// Model input lineage items (ARCH-CONTEXT-006): the worker declares the
// ordered sources of every chat request; Quoin persists them as
// model_call_input_items and re-checks each source. Digests travel as raw
// 32-byte SHA-256 values.

import (
	"crypto/sha256"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"
	runtimev1 "github.com/Suknna/quoin/internal/gen/proto/runtime/v1"
	"github.com/Suknna/quoin/internal/plinth/agent"
)

// inputItem is the worker-side lineage item.
type inputItem struct {
	seq    uint32
	kind   string
	itemID int64
	digest []byte
	role   string
}

// initialInputItems builds the first request lineage: the fixed system
// contract, the fixed tool schema and the frozen attempt input snapshot
// (the snapshot row id is resolved by Quoin, which owns the row).
func initialInputItems(start *workerv1.StartAttempt) []inputItem {
	return []inputItem{
		{seq: 1, kind: "system_contract", digest: digestOf(agent.SystemPrompt), role: "system"},
		{seq: 2, kind: "tool_schema", digest: start.GetToolSchemaDigest(), role: "system"},
		{seq: 3, kind: "snapshot", digest: start.GetContentDigest(), role: "system"},
	}
}

// priorCallItem references the completed assistant response of the
// previous logical call.
func priorCallItem(modelCallID int64, responseDigest []byte) inputItem {
	return inputItem{kind: "prior_call", itemID: modelCallID, digest: responseDigest, role: "assistant"}
}

// toolResultItem references one committed tool result.
func toolResultItem(toolCallID int64, resultDigest []byte) inputItem {
	return inputItem{kind: "tool_call", itemID: toolCallID, digest: resultDigest, role: "tool"}
}

// inputItemsDigest derives the wire input_digest: the concatenation of the
// ordered 32-byte digests (runtime.proto BeginModelCall).
func inputItemsDigest(items []inputItem) []byte {
	hash := sha256.New()
	for _, item := range items {
		hash.Write(item.digest)
	}
	return hash.Sum(nil)
}

// wireInputItems maps the lineage to the wire shape with sequence numbers
// assigned in order.
func wireInputItems(items []inputItem) []*workerv1.WorkerModelInputItem {
	var wire []*workerv1.WorkerModelInputItem
	for index, item := range items {
		wire = append(wire, &workerv1.WorkerModelInputItem{
			Sequence: uint32(index + 1), ItemKind: itemKindOf(item.kind),
			ItemId: item.itemID, ContentDigest: item.digest, Role: roleOf(item.role),
		})
	}
	return wire
}

func itemKindOf(kind string) runtimev1.ModelInputItemKind {
	switch kind {
	case "snapshot":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_ATTEMPT_INPUT_SNAPSHOT
	case "message":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_INVESTIGATION_MESSAGE
	case "prior_call":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_PRIOR_MODEL_CALL
	case "tool_call":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_TOOL_CALL
	case "evidence":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_EVIDENCE
	case "artifact":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_ARTIFACT
	case "knowledge":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_KNOWLEDGE_VERSION
	case "system_contract":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_SYSTEM_CONTRACT
	case "tool_schema":
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_TOOL_SCHEMA
	default:
		return runtimev1.ModelInputItemKind_MODEL_INPUT_ITEM_KIND_UNSPECIFIED
	}
}

func roleOf(role string) runtimev1.ModelInputRole {
	switch role {
	case "system":
		return runtimev1.ModelInputRole_MODEL_INPUT_ROLE_SYSTEM
	case "user":
		return runtimev1.ModelInputRole_MODEL_INPUT_ROLE_USER
	case "assistant":
		return runtimev1.ModelInputRole_MODEL_INPUT_ROLE_ASSISTANT
	case "tool":
		return runtimev1.ModelInputRole_MODEL_INPUT_ROLE_TOOL
	default:
		return runtimev1.ModelInputRole_MODEL_INPUT_ROLE_UNSPECIFIED
	}
}

func digestOf(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
