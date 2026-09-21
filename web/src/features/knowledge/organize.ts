// 各诊断面共用的“整理为知识”落地逻辑:create-or-return 后按候选状态导航——
// 待确认进入候选编辑层,已确认直接打开知识;被标记不采纳的来源给出明确原因。

import { notify } from "@/app/shared";
import {
	type CandidateSummary,
	CommandConflictError,
} from "@/features/knowledge/api";

export async function organizeIntoKnowledge(
	create: () => Promise<CandidateSummary>,
	navigate: (route: string) => void,
): Promise<void> {
	try {
		const candidate = await create();
		if (candidate.state === "Confirmed" && candidate.confirmedKnowledgeId) {
			notify.success("该内容已沉淀为知识");
			navigate(`/knowledge/items/${candidate.confirmedKnowledgeId}`);
			return;
		}
		notify.success("已生成知识候选,确认后进入知识库");
		navigate(`/knowledge/candidates/${candidate.id}`);
	} catch (reason) {
		// 创建端点的 409 只有 active_conflict 一种:来源被标记过不采纳。
		// CommandConflictError 的通用文案不适用于此,直接给出原因。
		if (reason instanceof CommandConflictError) {
			notify.warning("该来源已被标记为不采纳,不能整理为知识。");
			return;
		}
		notify.error(reason, "暂时无法完成操作,请重试。");
	}
}
