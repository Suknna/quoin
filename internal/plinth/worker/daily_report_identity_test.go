package worker

// 日报总结代的准入回归（ADR-0014）：只有
// (inspection_daily_analysis_v1, inspection-daily-analysis-v1) 组合被 admit，
// 绑定日报总结冻结 prompt 与专用结果 schema；巡检 Run 分析身份不得借用日报
// 模式，日报身份也不得借用巡检模式。

import (
	"crypto/sha256"
	"strings"
	"testing"

	workerv1 "github.com/Suknna/quoin/internal/gen/proto/plinth/worker/v1"

	"github.com/Suknna/quoin/internal/plinth/agent"
)

func TestVerifyStartAdmitsDailyReportGeneration(t *testing.T) {
	canonical := []byte(`{"schemaKind":"inspection_daily_analysis_v1","attemptId":1,"dailyReportId":2,"configKey":"core-daily","localDate":"2026-09-27","reportVersion":1,"timezone":"UTC","windowStartUtc":"2026-09-26T00:00:00Z","windowEndUtc":"2026-09-27T00:00:00Z","modelContract":{"modelId":"m"},"sources":[],"totals":{"checksOk":0,"checksGap":0,"checksError":0,"sourcesGap":0}}`)
	sum := sha256.Sum256(canonical)
	build := func(schemaKind, agentVersion string) *workerv1.StartAttempt {
		return &workerv1.StartAttempt{
			SchemaKind: schemaKind, AgentVersion: agentVersion,
			CanonicalJson: canonical, ContentDigest: sum[:],
		}
	}

	mode, err := verifyStart(build("inspection_daily_analysis_v1", InspectionDailyAnalysisAgentVersion))
	if err != nil {
		t.Fatalf("daily report generation must be executable: %v", err)
	}
	if mode.agentVersion != InspectionDailyAnalysisAgentVersion || mode.prompt != agent.DailyReportSystemPrompt {
		t.Fatalf("daily mode = %s/%q", mode.agentVersion, mode.prompt)
	}
	if mode.outputSchemaKind != InspectionDailyOutputSchemaKind {
		t.Fatalf("daily output schema = %q, want %q", mode.outputSchemaKind, InspectionDailyOutputSchemaKind)
	}
	rendered, err := mode.buildMessages(canonical)
	if err != nil {
		t.Fatalf("daily mode must render its bounded XML prompt: %v", err)
	}
	if !strings.Contains(rendered[1].Content, `tool="daily_report_get"`) {
		t.Fatalf("rendered prompt missing the frozen tool call: %q", rendered[1].Content)
	}

	// 任何其他身份组合一律拒绝：日报模式不吸收旧/新巡检身份，巡检模式也不
	// 接受日报身份。
	for _, mismatch := range []struct{ schemaKind, agentVersion string }{
		{"inspection_daily_analysis_v1", InspectionAnalysisAgentVersion},
		{"inspection_daily_analysis_v1", WorkerAgentVersion},
		{"inspection_analysis_v1", InspectionDailyAnalysisAgentVersion},
	} {
		if _, err := verifyStart(build(mismatch.schemaKind, mismatch.agentVersion)); err == nil {
			t.Fatalf("combination %s/%s must be rejected", mismatch.schemaKind, mismatch.agentVersion)
		}
	}
}
