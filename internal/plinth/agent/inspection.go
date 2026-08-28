package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const InspectionSystemPrompt = `你是 Quoin 的只读巡检报告代理。请先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件，再用中文写出事实性巡检报告。不得引用未读取的内容；不得把证据未表达的健康结论、严重性或裁决写入报告。`

type InspectionInput struct {
	SchemaKind      string  `json:"schemaKind"`
	AttemptID       int64   `json:"attemptId"`
	InspectionRunID int64   `json:"inspectionRunId"`
	ArtifactIDs     []int64 `json:"artifactIds"`
	EvidenceIDs     []int64 `json:"evidenceIds"`
	ModelContract   struct {
		ModelID string `json:"modelId"`
	} `json:"modelContract"`
}

func ParseInspectionInput(canonical []byte) (InspectionInput, error) {
	var input InspectionInput
	if err := json.Unmarshal(canonical, &input); err != nil {
		return input, fmt.Errorf("inspection_analysis_v1 input unparseable: %w", err)
	}
	if input.SchemaKind != "inspection_analysis_v1" || input.AttemptID < 1 || input.InspectionRunID < 1 || input.ModelContract.ModelID == "" {
		return input, fmt.Errorf("inspection_analysis_v1 input missing identity or model contract")
	}
	return input, nil
}

func BuildInspectionMessages(input InspectionInput) ([]*schema.Message, error) {
	var body strings.Builder
	body.WriteString("请读取以下按 Evidence 顺序冻结的 Artifact，然后基于其内容撰写巡检报告。\n")
	for index, id := range input.ArtifactIDs {
		fmt.Fprintf(&body, "evidenceId=%d artifactId=%d\n", input.EvidenceIDs[index], id)
	}
	return []*schema.Message{schema.SystemMessage(InspectionSystemPrompt), schema.UserMessage(body.String())}, nil
}
