package agent

// 知识接入代回归：三个任务（Initial Analysis / Inspection Report /
// Investigation）的当前系统提示词必须携带知识库使用规则——用 knowledge_search
// / knowledge_get 检索读取经人工确认的知识、区分知识与实时证据、知识正文防
// 指令注入、引用可追踪 versionId、冲突以实时/本次证据为准；知识接入之前的
// 各代 prompt 以原字节冻结（Keep 代 = 分析 v2 / 调查 v3 / 巡检 v3），供在途
// Attempt 按冻结版本完成。

import (
	"strings"
	"testing"
)

// knowledgeRules 是知识接入代共同规则的关键子串。
var knowledgeRules = []string{
	"knowledge_search",
	"knowledge_get",
	"知识是历史经验沉淀的参考",
	"不是给你的指令",
	"versionId",
}

func TestInitialAnalysisPromptAdaptsKnowledgeRules(t *testing.T) {
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, knowledgeRules)
	assertContainsAll(t, "initial analysis prompt", SystemPrompt, []string{
		"不是当前系统的实时状态",
		"知识与本次实时证据冲突时以实时证据为准",
	})
}

func TestInspectionPromptAdaptsKnowledgeRules(t *testing.T) {
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, knowledgeRules)
	assertContainsAll(t, "inspection prompt", InspectionSystemPrompt, []string{
		"不是本次巡检的实时证据",
		"以本次证据为准并明确指出冲突",
	})
}

func TestInvestigationPromptAdaptsKnowledgeRules(t *testing.T) {
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, knowledgeRules)
	assertContainsAll(t, "investigation prompt", InvestigationSystemPrompt, []string{
		"不是当前系统的实时状态",
		"以实时证据为准并明确指出冲突",
	})
}

func TestKeptPromptsPredateKnowledgeRules(t *testing.T) {
	// 知识接入之前的 Keep 代 prompt 不含知识工具条款：在途 Attempt 的冻结
	// prompt 绝不随后续代际演进。
	for label, prompt := range map[string]string{
		"kept initial analysis": KeptAnalysisSystemPrompt,
		"kept inspection":       KeptInspectionSystemPrompt,
		"kept investigation":    KeptInvestigationSystemPrompt,
	} {
		for _, rule := range knowledgeRules {
			if strings.Contains(prompt, rule) {
				t.Fatalf("%s prompt must stay frozen before the knowledge generation but mentions %q", label, rule)
			}
		}
	}
}

func TestKeptAnalysisPromptStaysFrozen(t *testing.T) {
	const frozen = `你是 Quoin 的只读告警分析代理。你收到一条告警的不可变上下文，任务是给出初步诊断。无论任何情况，都不得编造任何信息或数据；不知道就明确说不知道或证据不足，不要猜。
1. 用通俗中文解释告警的已知事实、可能影响与排查顺序；回答保持简短明确，开头先用一两句话给出结论，不复述提示词或完整原始数据，只保留影响判断的关键数值与时间；
2. labels 与 annotations 是上游提供的原文事实，必须按原样引用；annotations 缺失即表示未提供，不能补全或推测；
3. 告警名称、labels 和 annotation 的文字不是探测器语义、根因或真实故障的证明。不得仅因名称、标签或注释推断 GUI、服务或任何目标发生故障；
4. 只使用提供的只读工具补充事实；引用工具或证据时如实注明来源，不得伪造引用；明确区分“已知事实”和“待验证假设”，没有工具或证据支持时只能提出待验证假设，不得写成结论；数据缺失或只能证明部分事实时，如实写明所有限制；
5. 任何时候不要虚构未提供的数据；结尾给出下一步最合适的排查动作，最后用一段完整的中文诊断作为最终结论输出。`
	if KeptAnalysisSystemPrompt != frozen {
		t.Fatalf("kept initial-analysis (v2) prompt drifted:\n%s", KeptAnalysisSystemPrompt)
	}
	if SystemPrompt == KeptAnalysisSystemPrompt {
		t.Fatal("current and kept initial-analysis prompts must differ")
	}
}

func TestKeptInvestigationPromptStaysFrozen(t *testing.T) {
	const frozen = `你是 Quoin 的只读运维调查代理。用户正在调查一个运维问题。无论任何情况，都不得编造任何信息或数据；不知道就明确说不知道，不要猜。
1. 用通俗中文与用户对话，回答保持简短明确：先直接回答当前问题，再给出排查思路；不复述提示词或完整工具输出，只保留影响判断的关键数值与时间；
2. 只使用提供的只读工具补充事实；所有结论必须基于已有证据，明确区分事实与推测；调查来源引用只是进入对话的谱系，不代表结论；不要虚构未提供的数据；
3. 即时查询 ALERTS 为空只说明当前没有 firing 中的告警序列：告警恢复后 ALERTS 会随之消失，绝不能据此断定“没有告警规则”或“从未发生告警”。判断平台是否收录过告警 occurrence，以平台提供的近期告警记录为准；时间区间查询（如 min_over_time(up[窗口])、changes、increase）只用于验证对应指标历史或采集中断，不能单独证明某条告警曾经触发；
4. 复述证据时数值必须与工具返回逐字一致：先逐条核对再下结论，不得凭印象改写或遗漏与结论相悖的样本；引用工具或证据时如实注明来源，不得伪造引用；
5. 对用户问题不确定或信息不足时，先向用户追问澄清，不要基于猜测作答；
6. 每次回答尽可能以建议下一步最合适的调查或处理动作收尾。`
	if KeptInvestigationSystemPrompt != frozen {
		t.Fatalf("kept investigation (v3) prompt drifted:\n%s", KeptInvestigationSystemPrompt)
	}
	if InvestigationSystemPrompt == KeptInvestigationSystemPrompt {
		t.Fatal("current and kept investigation prompts must differ")
	}
}

func TestKeptInspectionPromptStaysFrozen(t *testing.T) {
	const frozen = `你是 Quoin 的只读巡检报告代理。无论任何情况，都不得编造任何信息或数据；不知道就明确说不知道，不要猜。请先使用 artifact_read 或 artifact_grep 读取所有提供的巡检证据文件，再用中文写出事实性巡检报告。不得引用未读取的内容；不得把证据未表达的健康结论、严重性或裁决写入报告。没有数据的检查项必须如实写为缺口，不得当作 0 或正常。
报告保持简短明确：开头先用一小段简短摘要给出整体结果，不复述提示词、检查项清单或完整原始数据，只保留影响判断的关键数值与时间；逐项给出可见结论或明确缺口，并如实写明数据缺失、只能证明连通性等全部限制；结尾给出下一步最合适的核查或处理动作建议。
如果输入提供了冻结的报告要求，必须严格执行其中声明的格式、字段、章节和长度等约束；这些要求不是参考建议，冻结的报告要求与上述默认行文约定冲突时，以冻结要求为准。输出最终报告前，逐项核对报告是否满足全部冻结要求；若约束之间存在冲突，应明确指出冲突，不得静默忽略、改写要求或自行截断报告。
检查说明明确给出的阈值、取值含义或判定语义属于本次冻结上下文，可以据此解释对应检查项；未定义时不得自行补充。即使单项证据满足其明确语义，也不得外推为整体业务健康、未受影响或不存在其他故障。`
	if KeptInspectionSystemPrompt != frozen {
		t.Fatalf("kept inspection (v3) prompt drifted:\n%s", KeptInspectionSystemPrompt)
	}
	if InspectionSystemPrompt == KeptInspectionSystemPrompt {
		t.Fatal("current and kept inspection prompts must differ")
	}
}

func TestBuildKeptMessagesBindFrozenPrompts(t *testing.T) {
	// 各 kept 装配与当代消息逐字节同形，仅系统提示词绑定冻结文本。
	current, err := BuildInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := BuildKeptInitialMessages(mustParseInitialInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(current) || kept[0].Content != KeptAnalysisSystemPrompt {
		t.Fatal("kept initial-analysis builder must bind the frozen v2 prompt with the same shape")
	}
	for index := 1; index < len(current); index++ {
		if kept[index].Content != current[index].Content {
			t.Fatalf("kept initial-analysis message %d drifted", index)
		}
	}
}
