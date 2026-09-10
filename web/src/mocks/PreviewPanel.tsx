import { useState } from "react";

/**
 * Scenarios are intentionally declarative: the panel only selects a state and
 * delegates all fixture mutation to the mock runtime. This keeps credentials
 * out of the browser UI and lets a change remount the application cleanly.
 */
// The panel is dev-only and deliberately exports its scenario contract for bootstrap typing.
// eslint-disable-next-line react-refresh/only-export-components
export const previewScenarios = [
	{ id: "admin", label: "管理员", description: "已登录的管理员工作台" },
	{ id: "operator", label: "操作员", description: "受限权限的已登录工作台" },
	{ id: "login", label: "登录", description: "未登录状态" },
	{ id: "first-password", label: "首次改密", description: "需要设置首次密码" },
	{ id: "expired", label: "会话过期", description: "已登录后会话失效" },
	{ id: "unavailable", label: "服务不可用", description: "认证服务不可用" },
	{ id: "maintenance", label: "维护中", description: "维护窗口中的工作台" },
	{ id: "platform-one", label: "平台单项", description: "关于页单项维护清单预览" },
	{ id: "platform-boundary", label: "平台边界", description: "关于页 50 项与超长事实预览" },
	{ id: "empty", label: "空数据", description: "无业务记录的工作台" },
	{ id: "slow", label: "慢响应", description: "延迟返回的模拟接口" },
	{ id: "conflict", label: "并发冲突", description: "写入返回冲突" },
] as const;

export type PreviewScenario = (typeof previewScenarios)[number]["id"];

export interface PreviewPanelProps {
	activeScenario: PreviewScenario;
	onScenarioChange: (scenario: PreviewScenario) => void | Promise<void>;
	onReset: () => void | Promise<void>;
}

/** Dev-only control surface. Its caller is responsible for excluding it from production. */
export function PreviewPanel({ activeScenario, onScenarioChange, onReset }: PreviewPanelProps) {
	const [busy, setBusy] = useState(false);
	const [expanded, setExpanded] = useState(false);

	async function run(action: () => void | Promise<void>) {
		setBusy(true);
		try {
			await action();
		} finally {
			setBusy(false);
		}
	}

	return (
		<aside aria-label="模拟数据开发预览" className="fixed bottom-3 right-3 z-[100] text-sm text-slate-900 dark:text-slate-100">
			<button
				type="button"
				aria-expanded={expanded}
				aria-controls="mock-preview-controls"
				className="rounded-full border border-amber-300 bg-amber-50 px-3 py-2 font-medium shadow-md dark:border-amber-600 dark:bg-slate-900"
				onClick={() => setExpanded((value) => !value)}
			>
				开发预览 · 模拟数据
			</button>
			{expanded && <div id="mock-preview-controls" className="absolute bottom-11 right-0 w-72 rounded-lg border border-amber-300 bg-amber-50 p-4 shadow-lg dark:border-amber-600 dark:bg-slate-900">
				<div className="mb-3 flex items-center justify-between gap-2"><strong>本地模拟场景</strong><span className="rounded bg-amber-200 px-2 py-0.5 text-xs font-medium dark:bg-amber-800">本地</span></div>
				<label className="grid gap-1.5" htmlFor="mock-scenario"><span className="font-medium">场景</span><select id="mock-scenario" value={activeScenario} disabled={busy} className="rounded border border-slate-300 bg-white px-2 py-1.5 dark:border-slate-600 dark:bg-slate-800" onChange={(event) => void run(() => onScenarioChange(event.target.value as PreviewScenario))}>{previewScenarios.map((scenario) => <option key={scenario.id} value={scenario.id}>{scenario.label} — {scenario.description}</option>)}</select></label>
				<p className="mt-2 text-xs text-slate-600 dark:text-slate-300">切换场景会重置模拟状态并重新挂载应用。</p>
				<button type="button" disabled={busy} className="mt-3 rounded bg-slate-900 px-3 py-1.5 font-medium text-white disabled:opacity-50 dark:bg-slate-100 dark:text-slate-900" onClick={() => void run(onReset)}>{busy ? "正在重置…" : "重置模拟数据"}</button>
			</div>}
		</aside>
	);
}
