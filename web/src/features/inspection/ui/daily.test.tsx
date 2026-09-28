// 每日报告 UI 行为：配置编辑校验、缺口如实呈现、AI 总结缺席明示、补跑/重分析流转。
// API 全部 mock（走 vi.hoisted），不依赖真实后端。
import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const dailyApi = vi.hoisted(() => ({
	listDailyReportConfigs: vi.fn(),
	getDailyReportConfig: vi.fn(),
	createDailyReportConfig: vi.fn(),
	updateDailyReportConfig: vi.fn(),
	listDailyReports: vi.fn(),
	getDailyReport: vi.fn(),
	getDailyReportVersion: vi.fn(),
	listDailyReportAnalyses: vi.fn(),
	getDailyReportAnalysis: vi.fn(),
	backfillDailyReport: vi.fn(),
	rerunDailyReport: vi.fn(),
}));
const planApi = vi.hoisted(() => ({
	listInspectionPlans: vi.fn(),
	listInspectionRuns: vi.fn(),
}));
vi.mock("@/features/inspection/daily", async (original) => ({
	...(await original<typeof import("@/features/inspection/daily")>()),
	...dailyApi,
}));
vi.mock("@/features/inspection/api", async (original) => ({
	...(await original<typeof import("@/features/inspection/api")>()),
	...planApi,
}));

import { DailyConfigEditor } from "./DailyConfigEditor";
import { DailyReportDetailView } from "./DailyReportDetail";
import { DailyReportsOverview } from "./DailyReports";
import { useInspectionsModule } from "./index";

/** 与 index.test.tsx 同一模式：hook 在组件体内调用，渲染返回的视图内容。 */
function DailyModuleView({ route }: { route: string }) {
	const view = useInspectionsModule(baseProps(route));
	return <>{view.content}</>;
}

function baseProps(route: string) {
	return {
		user: {
			id: "u", username: "u", displayName: "U", role: "admin" as const,
			passwordChangeRequired: false, authRevision: 1, enabled: true,
			initialized: true, lastLoginAt: null, rowVersion: 1,
		},
		route,
		navigate: vi.fn(),
		suspended: false,
		openEvidence: vi.fn(),
	};
}

const config = {
	configKey: "ops-daily", displayName: "运维每日报告", enabled: true,
	timezone: "Asia/Shanghai", triggerTime: "08:00", planKeys: ["prom-up"],
	rowVersion: 1, createdAt: "2026-09-27T00:00:00Z", updatedAt: "2026-09-27T00:00:00Z",
};
const sealedSummary = {
	id: "5", configKey: "ops-daily", localDate: "2026-09-27", timezone: "Asia/Shanghai",
	windowStartUtc: "2026-09-26T16:00:00Z", windowEndUtc: "2026-09-27T16:00:00Z",
	triggerKind: "schedule" as const, state: "Sealed" as const,
	sealedAt: "2026-09-28T02:00:00Z", latestVersion: 1, createdAt: "2026-09-28T00:00:00Z",
};
const sealedDetail = {
	...sealedSummary,
	configRowVersion: 1, cutoffAt: "2026-09-28T02:00:00Z",
	analysisAttemptState: "Pending" as const,
	contributions: [
		{ planKey: "prom-up", displayName: "Prom 连通巡检", connectionName: "lab-prometheus", enabled: true, sourceEnabled: true },
		{ planKey: "legacy", enabled: false, sourceEnabled: false, missing: true },
	],
	versions: [{ version: 1, createdAt: "2026-09-28T02:00:00Z" }],
	latest: {
		schemaKind: "inspection_daily_report_v1", configKey: "ops-daily",
		localDate: "2026-09-27", timezone: "Asia/Shanghai",
		windowStartUtc: "2026-09-26T16:00:00Z", windowEndUtc: "2026-09-27T16:00:00Z",
		sealedAt: "2026-09-28T02:00:00Z",
		sources: [
			{ planKey: "prom-up", displayName: "Prom 连通巡检", connectionName: "lab-prometheus", enabled: true, sourceEnabled: true, status: "ok" as const, checks: [{ runId: 7, evidenceId: 33, checkKey: "promql_check", status: "ok", observedAt: "2026-09-27T09:30:00Z", measurement: { resultType: "vector", series: 1, samples: 1, lastValue: "1" } }] },
			{ planKey: "legacy", enabled: false, sourceEnabled: false, missing: true, status: "gap" as const, gapReasons: ["plan_missing"] },
		],
		totals: { checksOk: 1, checksGap: 0, checksError: 0, sourcesGap: 1 },
	},
};

beforeEach(() => {
	dailyApi.listDailyReportConfigs.mockResolvedValue([config]);
	dailyApi.listDailyReports.mockResolvedValue([sealedSummary]);
	dailyApi.getDailyReport.mockResolvedValue(sealedDetail);
	dailyApi.getDailyReportVersion.mockResolvedValue(JSON.stringify(sealedDetail.latest));
	dailyApi.listDailyReportAnalyses.mockResolvedValue([]);
	dailyApi.createDailyReportConfig.mockResolvedValue(config);
	dailyApi.updateDailyReportConfig.mockResolvedValue(config);
	dailyApi.backfillDailyReport.mockResolvedValue({ ...sealedSummary, state: "Collecting", sealedAt: undefined });
	dailyApi.rerunDailyReport.mockResolvedValue(sealedSummary);
	dailyApi.getDailyReportConfig.mockResolvedValue(config);
	planApi.listInspectionPlans.mockResolvedValue([
		{
			planKey: "prom-up", displayName: "Prom 连通巡检", enabled: true,
			connectionName: "lab-prometheus", pluginId: "prometheus", templateId: "prometheus-up",
			templateVersion: null, params: {}, scope: { kind: "integration" },
			cron: "*/5 * * * *", timezone: "Asia/Shanghai", rowVersion: 1,
			createdAt: "2026-09-27T00:00:00Z", updatedAt: "2026-09-27T00:00:00Z",
		},
	]);
	planApi.listInspectionRuns.mockResolvedValue({ items: [] });
});

afterEach(() => {
	cleanup();
	vi.clearAllMocks();
});

describe("DailyReportsOverview", () => {
	it("renders configs and frozen reports with honest state badges", async () => {
		const navigate = vi.fn();
		render(<DailyReportsOverview suspended={false} navigate={navigate} />);
		expect(await screen.findByText("运维每日报告")).toBeInTheDocument();
		expect(screen.getByText(/每天 08:00/)).toBeInTheDocument();
		expect(screen.getByText(/2026-09-27 · 运维每日报告/)).toBeInTheDocument();
		expect(screen.getByText("已封存")).toBeInTheDocument();
		expect(screen.getByText(/封存于/)).toBeInTheDocument();
	});

	it("navigates to the report detail on row select", async () => {
		const navigate = vi.fn();
		render(<DailyReportsOverview suspended={false} navigate={navigate} />);
		fireEvent.click(await screen.findByText(/2026-09-27 · 运维每日报告/));
		await waitFor(() =>
			expect(navigate).toHaveBeenCalledWith("/inspections/daily/ops-daily/2026-09-27"),
		);
	});

	it("guards the backfill dialog on config + date before enabling submission", async () => {
		render(<DailyReportsOverview suspended={false} navigate={vi.fn()} />);
		// 等配置列表装载完成，按钮才可用（装载中禁用）。
		await screen.findByText("运维每日报告");
		const trigger = await screen.findByRole("button", { name: "补跑日报" });
		await waitFor(() => expect(trigger).toBeEnabled());
		fireEvent.click(trigger);
		const submit = await screen.findByRole("button", { name: "发起补跑" });
		expect(submit).toBeDisabled();
	});
});

describe("DailyReportDetailView", () => {
	it("presents gaps as gaps and marks the absent AI summary explicitly", async () => {
		const openEvidence = vi.fn();
		render(
			<DailyReportDetailView
				configKey="ops-daily"
				localDate="2026-09-27"
				suspended={false}
				navigate={vi.fn()}
				openEvidence={openEvidence}
			/>,
		);
		expect(await screen.findByText("本报告存在缺口")).toBeInTheDocument();
		expect(screen.getAllByText("有缺口").length).toBeGreaterThan(0);
		expect(screen.getAllByText("计划不存在").length).toBeGreaterThan(0);
		expect(screen.getByText(/暂无 AI 总结/)).toBeInTheDocument();
		expect(screen.getByText(/分析任务待创建/)).toBeInTheDocument();
		expect(screen.getAllByText("采证完整").length).toBeGreaterThan(0);
		expect(screen.getByText(/末值 1/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "证据 #33" }));
		expect(openEvidence).toHaveBeenCalledWith("33");
	});

	it("shows a failed Agent attempt without changing the sealed facts", async () => {
		dailyApi.getDailyReport.mockResolvedValue({ ...sealedDetail, analysisAttemptState: "Failed" });
		render(<DailyReportDetailView configKey="ops-daily" localDate="2026-09-27" suspended={false} navigate={vi.fn()} />);
		expect(await screen.findByText(/分析失败；可重新生成版本/)).toBeInTheDocument();
		expect(screen.getByText("本报告存在缺口")).toBeInTheDocument();
	});

	it("shows the Agent's versioned summary separately from the immutable facts", async () => {
		dailyApi.listDailyReportAnalyses.mockResolvedValue([{ analysisVersion: 2, id: "19", attemptState: "Succeeded", reportVersion: 1, modelId: "model-a", createdAt: "2026-09-28T03:00:00Z" }]);
		dailyApi.getDailyReportAnalysis.mockResolvedValue({ analysisVersion: 2, id: "19", attemptState: "Succeeded", reportVersion: 1, modelId: "model-a", createdAt: "2026-09-28T03:00:00Z", content: "建议人工核对缺口（Run 7）" });
		render(<DailyReportDetailView configKey="ops-daily" localDate="2026-09-27" suspended={false} navigate={vi.fn()} />);
		expect(await screen.findByText("建议人工核对缺口（Run 7）")).toBeInTheDocument();
		expect(screen.getByText("本报告存在缺口")).toBeInTheDocument();
		expect(screen.getByText(/AI 总结是分析意见/)).toBeInTheDocument();
		expect(dailyApi.getDailyReportAnalysis).toHaveBeenCalledWith("ops-daily", "2026-09-27", 2);
		expect(screen.getByText("分析 2 · 事实 1")).toBeInTheDocument();
	});

	it("opens the raw immutable version content on select", async () => {
		render(
			<DailyReportDetailView
				configKey="ops-daily"
				localDate="2026-09-27"
				suspended={false}
				navigate={vi.fn()}
			/>,
		);
		fireEvent.click(await screen.findByText("版本 1"));
		await waitFor(() => expect(dailyApi.getDailyReportVersion).toHaveBeenCalledWith("ops-daily", "2026-09-27", 1));
		expect(await screen.findByText(/inspection_daily_report_v1/)).toBeInTheDocument();
	});

	it("keeps a collecting report conclusion-free and blocks rerun until sealing", async () => {
		dailyApi.getDailyReport.mockResolvedValue({
			...sealedDetail,
			state: "Collecting",
			sealedAt: undefined,
			latestVersion: 0,
			versions: [],
			latest: undefined,
		});
		render(
			<DailyReportDetailView
				configKey="ops-daily"
				localDate="2026-09-28"
				suspended={false}
				navigate={vi.fn()}
			/>,
		);
		expect(await screen.findByText("正在采集中")).toBeInTheDocument();
		expect(screen.getByText("尚未封存")).toBeInTheDocument();
		expect(screen.queryByText("封存事实")).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: "重新分析" })).toBeDisabled();
		expect(dailyApi.rerunDailyReport).not.toHaveBeenCalled();
	});
});

describe("DailyConfigEditor", () => {
	it("requires the config key vocabulary before submitting", async () => {
		render(<DailyConfigEditor suspended={false} navigate={vi.fn()} />);
		await screen.findByText("参与计划");
		fireEvent.click(screen.getByRole("button", { name: "保存配置" }));
		expect(await screen.findByText(/配置标识必须以小写字母开头/)).toBeInTheDocument();
		expect(dailyApi.createDailyReportConfig).not.toHaveBeenCalled();
	});

	it("requires at least one participating plan", async () => {
		render(<DailyConfigEditor suspended={false} navigate={vi.fn()} />);
		await screen.findByText("参与计划");
		fireEvent.change(screen.getByLabelText("配置标识"), { target: { value: "ops-daily" } });
		fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "运维每日报告" } });
		fireEvent.click(screen.getByRole("button", { name: "保存配置" }));
		expect(await screen.findByText(/至少选择一个参与日报的巡检计划/)).toBeInTheDocument();
		expect(dailyApi.createDailyReportConfig).not.toHaveBeenCalled();
	});

	it("saves with the selected plan keys and returns to the hub", async () => {
		const navigate = vi.fn();
		render(<DailyConfigEditor suspended={false} navigate={navigate} />);
		await screen.findByText("参与计划");
		fireEvent.change(screen.getByLabelText("配置标识"), { target: { value: "ops-daily" } });
		fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "运维每日报告" } });
		fireEvent.click(screen.getByLabelText("选择计划 Prom 连通巡检"));
		fireEvent.change(screen.getByLabelText("人类期望输出（可选）"), { target: { value: "按来源列出证据与缺口" } });
		fireEvent.click(screen.getByRole("button", { name: "保存配置" }));
		await waitFor(() => expect(dailyApi.createDailyReportConfig).toHaveBeenCalled());
		const input = dailyApi.createDailyReportConfig.mock.calls[0][0];
		expect(input).toMatchObject({ configKey: "ops-daily", timezone: "Asia/Shanghai", planKeys: ["prom-up"], reportInstructions: "按来源列出证据与缺口" });
		expect(input.triggerTime).toMatch(/^\d{2}:\d{2}$/);
		await waitFor(() => expect(navigate).toHaveBeenCalledWith("/inspections/daily"));
	});

	it("prefills from the server projection in edit mode", async () => {
		render(<DailyConfigEditor suspended={false} navigate={vi.fn()} editKey="ops-daily" />);
		await waitFor(() => expect(screen.getByLabelText("配置标识")).toHaveValue("ops-daily"));
		expect(screen.getByLabelText("本地触发时间")).toHaveValue("08:00");
	});
});

describe("inspection module routing", () => {
	it("mounts the daily hub for /inspections/daily without the plan overview", async () => {
		render(<DailyModuleView route="/inspections/daily" />);
		expect(await screen.findByText("报告配置")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "新建计划" })).not.toBeInTheDocument();
	});

	it("mounts the report detail for /inspections/daily/:config/:date", async () => {
		render(<DailyModuleView route="/inspections/daily/ops-daily/2026-09-27" />);
		expect(await screen.findByText("本报告存在缺口")).toBeInTheDocument();
		expect(dailyApi.getDailyReport).toHaveBeenCalledWith("ops-daily", "2026-09-27");
	});
});
