package worker

// 各任务新旧代渲染的准入回归：仅明确的 (schema_kind, agent_version) 组合被
// admit；旧组合绑定各自冻结代的 prompt，新组合绑定当前 prompt；其余组合一律
// 拒绝。Keep 提示词迁入后巡检共四代、调查三代、初步分析两代；knowledge 抽取
// 显式固定在原共享身份上，不随 analysis prompt 代际升级。

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"

	"github.com/Suknna/quoin/internal/plinth/agent"
)

func TestVerifyStartAdmitsInspectionGenerations(t *testing.T) {
	canonical := []byte(`{"schemaKind":"inspection_analysis_v1","attemptId":1,"inspectionRunId":1,"artifactIds":[2],"evidenceIds":[3],"modelContract":{"modelId":"m"}}`)
	sum := sha256.Sum256(canonical)

	build := func(agentVersion string) *workerv1.StartAttempt {
		return &workerv1.StartAttempt{
			SchemaKind:    "inspection_analysis_v1",
			AgentVersion:  agentVersion,
			CanonicalJson: canonical,
			ContentDigest: sum[:],
			ArtifactRefs:  []*workerv1.WorkerArtifactRef{{ArtifactId: 2, Role: "report_source", MediaType: "application/json", SizeBytes: 2}},
		}
	}

	// 旧共享身份：上一代冻结 prompt（原始消息形状）。
	legacy, err := verifyStart(build(LegacyInitialAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("legacy inspection generation must stay executable: %v", err)
	}
	if legacy.agentVersion != LegacyInitialAnalysisAgentVersion || legacy.prompt != agent.LegacyInspectionSystemPrompt {
		t.Fatalf("legacy mode = %s/%q", legacy.agentVersion, legacy.prompt)
	}
	// 第一代专属身份：report-compliance 之前的冻结 prompt。
	initial, err := verifyStart(build(PreviousInspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("initial inspection generation must stay executable: %v", err)
	}
	if initial.prompt != agent.PreviousInspectionSystemPrompt {
		t.Fatalf("initial mode = %s/%q", initial.agentVersion, initial.prompt)
	}
	// report-compliance 代：冻结的上一代 prompt 与结构化清单渲染。
	compliance, err := verifyStart(build(ReportComplianceInspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("report-compliance inspection generation must stay executable: %v", err)
	}
	if compliance.prompt != agent.ReportComplianceInspectionSystemPrompt {
		t.Fatalf("report-compliance mode = %s/%q", compliance.agentVersion, compliance.prompt)
	}
	// 当前代：Keep 适配后的 prompt 与结构化清单渲染。
	current, err := verifyStart(build(InspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("current inspection generation must be executable: %v", err)
	}
	if current.agentVersion != InspectionAnalysisAgentVersion || current.prompt != agent.InspectionSystemPrompt {
		t.Fatalf("current mode = %s/%q", current.agentVersion, current.prompt)
	}
	// 四代 prompt digest 按实际 renderer 区分。
	digests := map[string]string{}
	for name, mode := range map[string]attemptMode{
		"legacy":           legacy,
		"initial":          initial,
		"reportCompliance": compliance,
		"current":          current,
	} {
		sum := sha256.Sum256([]byte(mode.prompt))
		digest := hex.EncodeToString(sum[:])
		for other, otherDigest := range digests {
			if otherDigest == digest {
				t.Fatalf("inspection generations %s and %s share prompt digest", name, other)
			}
		}
		digests[name] = digest
	}
	// 未知身份拒绝：不存在第五个巡检组合。
	if _, err := verifyStart(build("inspection-analysis-v9")); err == nil {
		t.Fatal("unknown inspection agent version must reject")
	}
}

func TestVerifyStartAdmitsInvestigationGenerations(t *testing.T) {
	canonical := []byte(`{"messages":[{"role":"user","content":"检查告警"}],"sources":[],"modelContract":{"modelId":"m"}}`)
	sum := sha256.Sum256(canonical)
	build := func(version string) *workerv1.StartAttempt {
		return &workerv1.StartAttempt{SchemaKind: "investigation_v1", AgentVersion: version, CanonicalJson: canonical, ContentDigest: sum[:]}
	}
	legacy, err := verifyStart(build(LegacyInvestigationAgentVersion))
	if err != nil {
		t.Fatalf("legacy investigation generation must stay executable: %v", err)
	}
	if legacy.prompt != agent.LegacyInvestigationSystemPrompt {
		t.Fatalf("legacy prompt drifted: %q", legacy.prompt)
	}
	previous, err := verifyStart(build(PreviousInvestigationAgentVersion))
	if err != nil {
		t.Fatalf("previous investigation generation must stay executable: %v", err)
	}
	if previous.prompt != agent.PreviousInvestigationSystemPrompt {
		t.Fatalf("previous prompt drifted: %q", previous.prompt)
	}
	current, err := verifyStart(build(WorkerInvestigationAgentVersion))
	if err != nil {
		t.Fatalf("current investigation generation must be executable: %v", err)
	}
	if current.prompt != agent.InvestigationSystemPrompt {
		t.Fatalf("current prompt drifted: %q", current.prompt)
	}
	if legacy.prompt == previous.prompt || previous.prompt == current.prompt || legacy.prompt == current.prompt {
		t.Fatal("investigation generations must keep distinct prompt bytes")
	}
	if _, err := verifyStart(build("investigation-v9")); err == nil {
		t.Fatal("unknown investigation agent version must reject")
	}
}

func TestVerifyStartAdmitsInitialAnalysisGenerations(t *testing.T) {
	canonical := []byte(`{"schemaKind":"initial_analysis_v1","occurrence":{"id":"1","labels":{}},"integrations":[{"kind":"metrics","name":"thanos-prod"}],"modelContract":{"modelId":"m"}}`)
	sum := sha256.Sum256(canonical)
	build := func(version string) *workerv1.StartAttempt {
		return &workerv1.StartAttempt{SchemaKind: "initial_analysis_v1", AgentVersion: version, CanonicalJson: canonical, ContentDigest: sum[:]}
	}
	current, err := verifyStart(build(WorkerAgentVersion))
	if err != nil {
		t.Fatalf("current initial-analysis generation must be executable: %v", err)
	}
	if current.prompt != agent.SystemPrompt {
		t.Fatalf("current mode = %s/%q", current.agentVersion, current.prompt)
	}
	previous, err := verifyStart(build(LegacyInitialAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("previous initial-analysis generation must stay executable: %v", err)
	}
	if previous.prompt != agent.PreviousAnalysisSystemPrompt {
		t.Fatalf("previous mode = %s/%q", previous.agentVersion, previous.prompt)
	}
	if current.prompt == previous.prompt {
		t.Fatal("initial-analysis generations must keep distinct prompt bytes")
	}
	if _, err := verifyStart(build("initial-analysis-v9")); err == nil {
		t.Fatal("unknown initial-analysis agent version must reject")
	}
}

func TestVerifyStartPinsKnowledgeToOriginalSharedIdentity(t *testing.T) {
	// knowledge 抽取的 prompt 从未随 analysis 代际演进：它显式固定在原共享
	// 身份 initial-analysis-v1 上，不升入新 analysis 输出风格。
	canonical := []byte(`{"schemaKind":"knowledge_extraction_v1","attemptId":1,"batchId":1,"sourceMaterialId":1,"text":"原文","modelContract":{"modelId":"m"}}`)
	sum := sha256.Sum256(canonical)
	build := func(version string) *workerv1.StartAttempt {
		return &workerv1.StartAttempt{SchemaKind: "knowledge_extraction_v1", AgentVersion: version, CanonicalJson: canonical, ContentDigest: sum[:]}
	}
	admitted, err := verifyStart(build(KnowledgeExtractionAgentVersion))
	if err != nil {
		t.Fatalf("knowledge extraction must stay executable on its frozen identity: %v", err)
	}
	if admitted.prompt != agent.KnowledgeExtractionSystemPrompt {
		t.Fatalf("knowledge mode = %s/%q", admitted.agentVersion, admitted.prompt)
	}
	if _, err := verifyStart(build(WorkerAgentVersion)); err == nil {
		t.Fatal("knowledge extraction must not admit the new analysis generation")
	}
}

func TestOtherModesKeepStrictSingleIdentity(t *testing.T) {
	initial := []byte(`{"schemaKind":"initial_analysis_v1"}`)
	sum := sha256.Sum256(initial)
	// initial_analysis 携带 investigation 身份：必须拒绝。
	start := &workerv1.StartAttempt{
		SchemaKind:    "initial_analysis_v1",
		AgentVersion:  WorkerInvestigationAgentVersion,
		CanonicalJson: initial,
		ContentDigest: sum[:],
	}
	if _, err := verifyStart(start); err == nil {
		t.Fatal("cross-generation initial analysis must keep rejecting")
	}
	// inspection 模式的输入喂给 investigation 身份同样拒绝。
	inspectionBody := []byte(`{"schemaKind":"inspection_analysis_v1","attemptId":1,"inspectionRunId":1,"artifactIds":[2],"evidenceIds":[3],"modelContract":{"modelId":"m"}}`)
	inspectionSum := sha256.Sum256(inspectionBody)
	misfiled := &workerv1.StartAttempt{
		SchemaKind:    "inspection_analysis_v1",
		AgentVersion:  "investigation-v1",
		CanonicalJson: inspectionBody,
		ContentDigest: inspectionSum[:],
	}
	if _, err := verifyStart(misfiled); err == nil {
		t.Fatal("investigation identity must not render inspection input")
	}
}
