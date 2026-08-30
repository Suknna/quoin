package agent

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// KnowledgeExtractionSystemPrompt constrains the model to a closed proposal
// shape. Quoin independently validates the JSON and remains the sole writer
// of candidate and version state.
const KnowledgeExtractionSystemPrompt = `你是 Quoin 知识提炼助手。根据用户粘贴的原文，提炼可复用的运维知识候选。
只输出一个 JSON 对象，不要 Markdown 或解释：
{"items":[{"title":"简明标题","body":"可操作、准确的知识正文","scope":{}}]}
items 至少一项。只陈述原文能支持的内容；不确定时不要臆测。`

// KnowledgeExtractionInput is the frozen source-material snapshot.
type KnowledgeExtractionInput struct {
	SchemaKind       string `json:"schemaKind"`
	AttemptID        int64  `json:"attemptId"`
	BatchID          int64  `json:"batchId"`
	Generation       int64  `json:"generation"`
	SourceMaterialID int64  `json:"sourceMaterialId"`
	Text             string `json:"text"`
	ModelContract    struct {
		ModelID string `json:"modelId"`
	} `json:"modelContract"`
}

func ParseKnowledgeExtractionInput(canonical []byte) (KnowledgeExtractionInput, error) {
	var input KnowledgeExtractionInput
	if err := json.Unmarshal(canonical, &input); err != nil {
		return input, fmt.Errorf("knowledge_extraction_v1 input unparseable: %w", err)
	}
	if input.SchemaKind != "knowledge_extraction_v1" || input.AttemptID < 1 || input.BatchID < 1 || input.SourceMaterialID < 1 || input.Text == "" || input.ModelContract.ModelID == "" {
		return input, fmt.Errorf("knowledge_extraction_v1 input missing required source or model context")
	}
	return input, nil
}

func BuildKnowledgeExtractionMessages(input KnowledgeExtractionInput) ([]*schema.Message, error) {
	return []*schema.Message{schema.SystemMessage(KnowledgeExtractionSystemPrompt), schema.UserMessage(input.Text)}, nil
}
