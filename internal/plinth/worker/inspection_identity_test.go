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

func TestVerifyStartAdmitsBothInspectionGenerations(t *testing.T) {
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
	// 新身份：当前 prompt 与结构化清单渲染。
	current, err := verifyStart(build(InspectionAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("current inspection generation must be executable: %v", err)
	}
	if current.agentVersion != InspectionAnalysisAgentVersion || current.prompt != agent.InspectionSystemPrompt {
		t.Fatalf("current mode = %s/%q", current.agentVersion, current.prompt)
	}
	// prompt digest 随实际 renderer：两代 digest 互异。
	legacyDigest := sha256.Sum256([]byte(legacy.prompt))
	currentDigest := sha256.Sum256([]byte(current.prompt))
	if hex.EncodeToString(legacyDigest[:]) == hex.EncodeToString(currentDigest[:]) {
		t.Fatal("the two generations must not share a prompt digest")
	}
	// 未知身份拒绝：不存在第三个巡检组合。
	if _, err := verifyStart(build("inspection-analysis-v9")); err == nil {
		t.Fatal("unknown inspection agent version must reject")
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
