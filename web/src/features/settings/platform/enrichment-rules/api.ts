// 富化规则管理 API（ADR-0012）：管理员维护的"命中即叠加 outputs"声明式配置。
// 规则 key 退役不复用（enabled=0 停用，行永不删除）；更新携带
// expectedRowVersion 并发前提，与业务视图管理同一命令模式。

import { newClientCommandId, request } from "@/api/workbench";

export interface EnrichmentRule {
	ruleKey: string
	displayName: string
	description: string
	enabled: boolean
	labelConditions: Record<string, string>
	alertSourceKeys: string[]
	outputs: Record<string, string>
	priority: number
	rowVersion: number
	createdAt: string
	updatedAt: string
}

export interface EnrichmentRuleInput {
	ruleKey: string
	displayName: string
	description: string
	enabled: boolean
	labelConditions: Record<string, string>
	alertSourceKeys: string[]
	outputs: Record<string, string>
	priority: number
}

export async function listEnrichmentRules(): Promise<EnrichmentRule[]> {
	const page = await request<{ items?: EnrichmentRule[] }>(
		"/api/v1/enrichment-rules",
	)
	return page.items ?? []
}

export function createEnrichmentRule(
	input: EnrichmentRuleInput,
): Promise<EnrichmentRule> {
	return request<EnrichmentRule>("/api/v1/enrichment-rules", {
		method: "POST",
		body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
	})
}

export function updateEnrichmentRule(
	ruleKey: string,
	input: Omit<EnrichmentRuleInput, "ruleKey">,
	expectedRowVersion: number,
): Promise<EnrichmentRule> {
	return request<EnrichmentRule>(
		`/api/v1/enrichment-rules/${encodeURIComponent(ruleKey)}`,
		{
			method: "PUT",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				...input,
				expectedRowVersion,
			}),
		},
	)
}

/** labelConditions 的行编辑形态：每行 "key=value"。 */
export function parseConditionLines(
	lines: string,
): { conditions: Record<string, string>; error?: string } {
	const conditions: Record<string, string> = {}
	for (const line of lines.split("\n")) {
		const trimmed = line.trim()
		if (!trimmed) continue
		const separator = trimmed.indexOf("=")
		if (separator <= 0 || separator >= trimmed.length - 1) {
			return { conditions, error: "标签条件每行必须是 key=value。" }
		}
		conditions[trimmed.slice(0, separator)] = trimmed.slice(separator + 1)
	}
	return { conditions }
}

/** outputs 的行编辑形态：每行 "key=value"。 */
export function parseOutputLines(
	lines: string,
): { outputs: Record<string, string>; error?: string } {
	const outputs: Record<string, string> = {}
	for (const line of lines.split("\n")) {
		const trimmed = line.trim()
		if (!trimmed) continue
		const separator = trimmed.indexOf("=")
		if (separator <= 0 || separator >= trimmed.length - 1) {
			return { outputs, error: "富化字段每行必须是 key=value。" }
		}
		outputs[trimmed.slice(0, separator)] = trimmed.slice(separator + 1)
	}
	return { outputs }
}
