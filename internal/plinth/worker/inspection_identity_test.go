package worker

// 巡检分析新旧两代渲染的准入回归：仅 (inspection_analysis_v1,
// inspection-analysis-v1) 与 (inspection_analysis_v1, initial-analysis-v1)
// 两个明确组合被 admit；旧组合绑定上一代冻结 prompt，新组合绑定当前 prompt；
// 其余模式不获得 legacy 别名。

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
	legacy, err := verifyStart(build(WorkerAgentVersion))
	if err != nil {
		t.Fatalf("legacy inspection generation must stay executable: %v", err)
	}
	if legacy.agentVersion != WorkerAgentVersion || legacy.prompt != agent.LegacyInspectionSystemPrompt {
		t.Fatalf("legacy mode = %s/%q", legacy.agentVersion, legacy.prompt)
	}
	previous, err := verifyStart(build(PreviousInspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("previous inspection generation must stay executable: %v", err)
	}
	if previous.prompt != agent.PreviousInspectionSystemPrompt {
		t.Fatalf("previous mode = %s/%q", previous.agentVersion, previous.prompt)
	}
	// 新身份：当前 prompt 与结构化清单渲染。
	current, err := verifyStart(build(InspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("current inspection generation must be executable: %v", err)
	}
	if current.agentVersion != InspectionAnalysisAgentVersion || current.prompt != agent.InspectionSystemPrompt {
		t.Fatalf("current mode = %s/%q", current.agentVersion, current.prompt)
	}
	// 三代 prompt digest 按实际 renderer 区分。
	legacyDigest := sha256.Sum256([]byte(legacy.prompt))
	previousDigest := sha256.Sum256([]byte(previous.prompt))
	currentDigest := sha256.Sum256([]byte(current.prompt))
	if hex.EncodeToString(legacyDigest[:]) == hex.EncodeToString(previousDigest[:]) ||
		hex.EncodeToString(previousDigest[:]) == hex.EncodeToString(currentDigest[:]) ||
		hex.EncodeToString(legacyDigest[:]) == hex.EncodeToString(currentDigest[:]) {
		t.Fatal("inspection generations must not share prompt digests")
	}
	// 未知身份拒绝：不存在第三个巡检组合。
	if _, err := verifyStart(build("inspection-analysis-v9")); err == nil {
		t.Fatal("unknown inspection agent version must reject")
	}
}

func TestVerifyStartAdmitsBothInvestigationGenerations(t *testing.T) {
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
	current, err := verifyStart(build(WorkerInvestigationAgentVersion))
	if err != nil {
		t.Fatalf("current investigation generation must be executable: %v", err)
	}
	if current.prompt != agent.InvestigationSystemPrompt {
		t.Fatalf("current prompt drifted: %q", current.prompt)
	}
	if legacy.prompt == current.prompt {
		t.Fatal("legacy and current investigation prompts must differ")
	}
	if _, err := verifyStart(build("investigation-v9")); err == nil {
		t.Fatal("unknown investigation agent version must reject")
	}
}

func TestOtherModesKeepStrictSingleIdentity(t *testing.T) {
	initial := []byte(`{"schemaKind":"initial_analysis_v1"}`)
	sum := sha256.Sum256(initial)
	// initial_analysis 携带 investigation 身份：必须拒绝（无 legacy 别名）。
	start := &workerv1.StartAttempt{
		SchemaKind:    "initial_analysis_v1",
		AgentVersion:  WorkerInvestigationAgentVersion,
		CanonicalJson: initial,
		ContentDigest: sum[:],
	}
	if _, err := verifyStart(start); err == nil {
		t.Fatal("cross-generation initial analysis must keep rejecting")
	}
	// inspection 模式的输入喂给 initial 模式身份同样拒绝。
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
