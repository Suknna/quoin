import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
	listInspectionPlans: vi.fn(),
	createInspectionPlan: vi.fn(),
	updateInspectionPlan: vi.fn(),
	getInspectionPlan: vi.fn(),
	listInspectionRuns: vi.fn(),
	createInspectionRun: vi.fn(),
	getInspectionRun: vi.fn(),
	listInspectionReports: vi.fn(),
	getInspectionReport: vi.fn(),
	cancelInspectionRun: vi.fn(),
	reanalyzeInspectionRun: vi.fn(),
	rerunInspection: vi.fn(),
}));
const feedback = vi.hoisted(() => ({
	appendFeedback: vi.fn(),
	fetchFeedback: vi.fn(),
}));
const resources = vi.hoisted(() => ({
	listConnections: vi.fn(),
	listIntegrationPlugins: vi.fn(),
	listBusinessViews: vi.fn(),
}));
vi.mock("@/features/inspection/api", async (original) => ({
	...(await original<typeof import("@/features/inspection/api")>()),
	...api,
}));
vi.mock("@/features/feedback/api", async (original) => ({
	...(await original<typeof import("@/features/feedback/api")>()),
	...feedback,
}));
vi.mock("@/api/workbench", async (original) => {
	const base = await original<typeof import("@/api/workbench")>();
	return {
		...base,
		workbenchApi: {
			...base.workbenchApi,
			listConnections: resources.listConnections,
		},
	};
});
vi.mock("@/features/integrations/api", async (original) => ({
	...(await original<typeof import("@/features/integrations/api")>()),
	listIntegrationPlugins: resources.listIntegrationPlugins,
}));
vi.mock("@/features/systems/api", async (original) => ({
	...(await original<typeof import("@/features/systems/api")>()),
	listBusinessViews: resources.listBusinessViews,
}));

import { WorkspaceShell } from "@/app/WorkspaceShell";
import { ReportBody, RunDetail, useInspectionsModule } from "./index";

function baseProps(route: string) {
	return {
		user: {
			id: "u",
			username: "u",
			displayName: "U",
			role: "admin" as const,
			passwordChangeRequired: false,
			authRevision: 1,
			enabled: true,
			initialized: true,
			lastLoginAt: null,
			rowVersion: 1,
		},
		route,
		navigate: vi.fn(),
		suspended: false,
		openEvidence: vi.fn(),
	};
}
const props = baseProps("/inspections");

/** Standalone plans scope an integration directly; no BusinessSystem declaration is involved. */
const integrationPlan = {
	planKey: "prom-up",
	displayName: "Prometheus 连通巡检",
	enabled: true,
	connectionName: "lab-prometheus",
	pluginId: "prometheus",
	templateId: "prometheus-up",
	templateVersion: null,
	params: { expression: "up" },
	scope: { kind: "integration" as const },
	cron: "*/5 * * * *",
	timezone: "Asia/Shanghai",
	rowVersion: 3,
	createdAt: "2026-09-10T08:00:00Z",
	updatedAt: "2026-09-10T08:00:00Z",
};
const objectsPlan = {
	...integrationPlan,
	planKey: "pod-check",
	displayName: "指定对象巡检",
	enabled: true,
	cron: null,
	scope: {
		kind: "objects" as const,
		objects: [{ objectType: "kubernetes_pod", identityKey: "demo/api" }],
	},
	rowVersion: 5,
};
const businessViewPlan = {
	...integrationPlan,
	planKey: "bv-payments",
	displayName: "支付核心视图巡检",
	enabled: true,
	cron: null,
	scope: { kind: "businessView" as const, businessViewKey: "payments-core" },
	rowVersion: 7,
};
const runSummary = {
	id: "1",
	planKey: "prom-up",
	connectionName: "lab-prometheus",
	businessSystemKey: "legacy-mall",
	state: "Completed" as const,
	rowVersion: 1,
	triggerKind: "schedule" as const,
	createdAt: "2026-09-10T08:00:00Z",
};

const labConnection = {
	name: "lab-prometheus",
	type: "prometheus",
	enabled: true,
	revalidationRequired: false,
	rowVersion: 1,
	config: {},
};
const prometheusPlugin = {
	id: "prometheus",
	displayName: "Prometheus",
	description: "指标采集插件",
	enabled: true,
	version: "1",
	capabilities: ["inspection_templates"],
};
const paymentsView = {
	viewKey: "payments-core",
	displayName: "支付核心",
	description: "",
	scope: { labelConditions: {} },
	rowVersion: 1,
	createdAt: "2026-09-10T08:00:00Z",
	updatedAt: "2026-09-10T08:00:00Z",
};

function InspectionView({
	route = "/inspections",
	navigate,
}: {
	route?: string;
	navigate?: (route: string) => void;
}) {
	const moduleProps = { ...props, route, navigate: navigate ?? props.navigate };
	const view = useInspectionsModule(moduleProps);
	return (
		<>
			{view.list}
			{view.actions}
			{view.content}
		</>
	);
}

beforeEach(() => {
	Element.prototype.scrollIntoView = vi.fn();
	api.listInspectionPlans.mockResolvedValue([]);
	api.listInspectionRuns.mockResolvedValue({ items: [] });
	resources.listConnections.mockResolvedValue([labConnection]);
	resources.listIntegrationPlugins.mockResolvedValue([prometheusPlugin]);
	resources.listBusinessViews.mockResolvedValue([paymentsView]);
});
afterEach(() => {
	cleanup();
	vi.clearAllMocks();
	vi.unstubAllGlobals();
});

describe("inspection report body", () => {
	it("renders Markdown headings, tables, and frozen evidence links", () => {
		const openEvidence = vi.fn();
		render(
			<ReportBody
				content={
					"# 巡检结论\n\n| 检查 | 状态 |\n| --- | --- |\n| 连通性 | 正常 |\n\n结论见 #e-1。"
				}
				evidenceIds={["e-1"]}
				openEvidence={openEvidence}
			/>,
		);
		expect(
			screen.getByRole("heading", { name: "巡检结论" }),
		).toBeInTheDocument();
		expect(screen.getByRole("table")).toHaveTextContent("连通性");
		fireEvent.click(screen.getByRole("button", { name: "#e-1" }));
		expect(openEvidence).toHaveBeenCalledWith("e-1");
	});
});

describe("inspection run result summary", () => {
	const detail = {
		id: "6",
		planKey: "prom-up",
		connectionName: "lab-prometheus",
		state: "Completed" as const,
		rowVersion: 1,
		triggerKind: "manual" as const,
		createdAt: "2026-09-10T10:00:00Z",
		reportCount: 1,
		analysisActive: false,
		checks: [{ checkKey: "up", status: "ok" as const, evidenceId: "e-1" }],
	};

	it("leads with an honest result summary: state, conclusion, and per-check outcome", async () => {
		api.getInspectionRun.mockResolvedValue({ ...detail });
		api.listInspectionReports.mockResolvedValue([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		api.getInspectionReport.mockResolvedValue({
			id: "42",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content: "# 报告",
			createdAt: "2026-09-10T10:01:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		const summary = await screen.findByLabelText("巡检结果摘要");
		expect(within(summary).getByText("已完成")).toBeInTheDocument();
		expect(within(summary).getByText("1 项检查全部通过")).toBeInTheDocument();
		expect(
			within(summary).getByRole("button", { name: "#e-1" }),
		).toBeInTheDocument();
		// 摘要卡先于报告正文，首屏即回答“结果如何”。
		const reportBody = screen.getByTestId("report-content");
		expect(
			Boolean(
				summary.compareDocumentPosition(reportBody) &
					Node.DOCUMENT_POSITION_FOLLOWING,
			),
		).toBe(true);
	});

	it("surfaces gap checks as a prominent warning with the gap reason", async () => {
		api.getInspectionRun.mockResolvedValue({
			...detail,
			state: "CompletedWithGaps" as const,
			checks: [
				{ checkKey: "up", status: "ok" as const, evidenceId: "e-1" },
				{
					checkKey: "latency",
					status: "gap" as const,
					gapReason: "no_data" as const,
				},
			],
		});
		api.listInspectionReports.mockResolvedValue([]);
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		const summary = await screen.findByLabelText("巡检结果摘要");
		expect(
			within(summary).getByText("1 项通过 · 1 项有缺口"),
		).toBeInTheDocument();
		expect(
			within(summary).getByText(/1 项检查未通过，报告可能不完整/),
		).toBeInTheDocument();
		expect(within(summary).getAllByText(/无数据/).length).toBeGreaterThan(0);
	});
});

describe("inspection report feedback", () => {
	const detail = {
		id: "6",
		planKey: "prom-up",
		connectionName: "lab-prometheus",
		state: "Completed" as const,
		rowVersion: 1,
		triggerKind: "manual" as const,
		createdAt: "2026-09-10T10:00:00Z",
		reportCount: 1,
		analysisActive: false,
		checks: [],
	};
	function renderRunDetail(onOpenRun = vi.fn()) {
		api.getInspectionRun.mockResolvedValue({ ...detail });
		api.listInspectionReports.mockResolvedValue([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		api.getInspectionReport.mockResolvedValue({
			id: "42",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content: "# 报告\n\n".repeat(200),
			createdAt: "2026-09-10T10:01:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={onOpenRun}
			/>,
		);
	}

	it("keeps long report content in a bounded native scroll container", async () => {
		renderRunDetail();
		expect(await screen.findByTestId("report-content")).toHaveClass(
			"max-h-[28rem]",
			"overflow-y-auto",
		);
		expect(screen.getByRole("button", { name: "已采纳" })).toBeInTheDocument();
	});

	it("uses the immutable report ID rather than a run/version composite", async () => {
		renderRunDetail();
		await waitFor(() =>
			expect(feedback.fetchFeedback).toHaveBeenCalledWith({
				type: "inspection_report",
				id: "42",
			}),
		);
	});

	it("reuses frozen evidence for reanalysis and deep-links the recollection run", async () => {
		const onOpenRun = vi.fn();
		renderRunDetail(onOpenRun);
		api.reanalyzeInspectionRun.mockResolvedValue({
			id: "att-1",
			type: "inspection_analysis",
			state: "Queued",
			rowVersion: 1,
			createdAt: "2026-09-10T10:02:00Z",
		});
		api.rerunInspection.mockResolvedValue({ ...detail, id: "run-10" });
		fireEvent.click(
			await screen.findByRole("button", { name: "重新分析现有证据" }),
		);
		// 弹框确认后才提交；未编辑时仅本次覆盖缺省（沿用冻结要求）。
		fireEvent.click(await screen.findByRole("button", { name: "开始分析" }));
		await waitFor(() =>
			expect(api.reanalyzeInspectionRun).toHaveBeenCalledWith("6", undefined),
		);
		fireEvent.click(
			await screen.findByRole("button", { name: "重新采证（新 Run）" }),
		);
		await waitFor(() => expect(onOpenRun).toHaveBeenCalledWith("run-10"));
	});

	it("prefills the reanalysis dialog with the frozen requirement and sends edited text as a this-run override", async () => {
		const frozenDetail = {
			...detail,
			frozenConfig: {
				displayName: "Prometheus 连通巡检",
				checkDescription: "连通性检查",
				metricUnit: "1=在线",
				reportInstructions: "冻结的初始要求",
			},
		};
		api.getInspectionRun.mockResolvedValue(frozenDetail);
		api.listInspectionReports.mockResolvedValue([]);
		api.reanalyzeInspectionRun.mockResolvedValue({
			id: "att-2",
			type: "inspection_analysis",
			state: "Queued",
			rowVersion: 1,
			createdAt: "2026-09-10T10:02:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: "重新分析现有证据" }),
		);
		const dialog = await screen.findByRole("dialog", {
			name: "重新分析现有证据",
		});
		// 默认展示 Run 冻结的初始报告要求（只读，继承模式）。
		const textarea = within(dialog).getByLabelText("本次报告要求");
		expect(textarea).toHaveValue("冻结的初始要求");
		expect(textarea).toBeDisabled();
		// 打开“仅本次自定义”后可编辑，提交按原文作为仅本次覆盖。
		fireEvent.click(within(dialog).getByLabelText("仅本次自定义报告要求"));
		expect(within(dialog).getByLabelText("本次报告要求")).toBeEnabled();
		fireEvent.change(within(dialog).getByLabelText("本次报告要求"), {
			target: { value: "仅本次：只看异常" },
		});
		fireEvent.click(within(dialog).getByRole("button", { name: "开始分析" }));
		await waitFor(() =>
			expect(api.reanalyzeInspectionRun).toHaveBeenCalledWith(
				"6",
				"仅本次：只看异常",
			),
		);
	});

	it("distinguishes inherit from an explicit empty-clear in the reanalysis dialog", async () => {
		const frozenDetail = {
			...detail,
			frozenConfig: {
				displayName: "旧名",
				reportInstructions: "冻结的初始要求",
			},
		};
		api.getInspectionRun.mockResolvedValue(frozenDetail);
		api.listInspectionReports.mockResolvedValue([]);
		api.reanalyzeInspectionRun.mockResolvedValue({
			id: "att-3",
			type: "inspection_analysis",
			state: "Queued",
			rowVersion: 1,
			createdAt: "2026-09-10T10:02:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: "重新分析现有证据" }),
		);
		const dialog = await screen.findByRole("dialog", {
			name: "重新分析现有证据",
		});
		// 继承模式提交：请求不含 reportInstructions 字段。
		fireEvent.click(within(dialog).getByRole("button", { name: "开始分析" }));
		await waitFor(() =>
			expect(api.reanalyzeInspectionRun).toHaveBeenCalledWith("6", undefined),
		);
		// 自定义模式清空后提交：显式空串（本次无要求），不是继承。
		fireEvent.click(
			await screen.findByRole("button", { name: "重新分析现有证据" }),
		);
		const reopened = await screen.findByRole("dialog", {
			name: "重新分析现有证据",
		});
		fireEvent.click(within(reopened).getByLabelText("仅本次自定义报告要求"));
		fireEvent.change(within(reopened).getByLabelText("本次报告要求"), {
			target: { value: "" },
		});
		fireEvent.click(within(reopened).getByRole("button", { name: "开始分析" }));
		await waitFor(() =>
			expect(api.reanalyzeInspectionRun).toHaveBeenLastCalledWith("6", ""),
		);
	});

	it("loads the current plan requirement into the reanalysis dialog only on the explicit action", async () => {
		const frozenDetail = {
			...detail,
			frozenConfig: {
				displayName: "旧名",
				reportInstructions: "冻结的初始要求",
			},
		};
		api.getInspectionRun.mockResolvedValue(frozenDetail);
		api.listInspectionReports.mockResolvedValue([]);
		api.getInspectionPlan.mockResolvedValue({
			...integrationPlan,
			reportInstructions: "计划当前的最新要求",
		});
		api.reanalyzeInspectionRun.mockResolvedValue({
			id: "att-4",
			type: "inspection_analysis",
			state: "Queued",
			rowVersion: 1,
			createdAt: "2026-09-10T10:02:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		fireEvent.click(
			await screen.findByRole("button", { name: "重新分析现有证据" }),
		);
		const dialog = await screen.findByRole("dialog", {
			name: "重新分析现有证据",
		});
		expect(within(dialog).getByLabelText("本次报告要求")).toHaveValue(
			"冻结的初始要求",
		);
		// 继承模式下加载按钮不可用；计划读取只在显式开启自定义并点击后发生。
		expect(
			within(dialog).getByRole("button", { name: "加载当前计划要求" }),
		).toBeDisabled();
		fireEvent.click(within(dialog).getByLabelText("仅本次自定义报告要求"));
		fireEvent.click(
			within(dialog).getByRole("button", { name: "加载当前计划要求" }),
		);
		await waitFor(() =>
			expect(api.getInspectionPlan).toHaveBeenCalledWith("prom-up"),
		);
		expect(within(dialog).getByLabelText("本次报告要求")).toHaveValue(
			"计划当前的最新要求",
		);
	});

	it("shows each report version's effective instructions inside generation details", async () => {
		renderRunDetail();
		api.getInspectionReport.mockResolvedValue({
			id: "42",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content: "# 报告",
			createdAt: "2026-09-10T10:01:00Z",
			reportInstructions: "仅本次：只看异常",
		});
		expect(await screen.findByTestId("report-content")).toBeInTheDocument();
		// 结论优先：本版本要求不占据首屏，仅在生成详情展开后可读。
		expect(screen.queryByText("仅本次：只看异常")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "生成详情" }));
		expect(await screen.findByText("仅本次：只看异常")).toBeInTheDocument();
		expect(screen.getByText("本次报告要求")).toBeInTheDocument();
	});

	it("keeps generation details expanded across an evidence round trip remount", async () => {
		api.getInspectionRun.mockResolvedValue({ ...detail });
		api.listInspectionReports.mockResolvedValue([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		api.getInspectionReport.mockResolvedValue({
			id: "persist-1",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content: "# 报告",
			createdAt: "2026-09-10T10:01:00Z",
			reportInstructions: "仅本次：只看异常",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		const first = render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		await screen.findByTestId("report-content");
		fireEvent.click(screen.getByRole("button", { name: "生成详情" }));
		expect(await screen.findByText("仅本次：只看异常")).toBeInTheDocument();
		first.unmount();
		// 证据阅读层往返按路由重建 Run 页面，展开状态按报告 ID 保留。
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		await screen.findByTestId("report-content");
		expect(screen.getByRole("button", { name: "生成详情" })).toHaveAttribute(
			"aria-expanded",
			"true",
		);
		expect(screen.getByText("仅本次：只看异常")).toBeInTheDocument();
	});

	it("renders a complete JSON report as real fields while preserving the original text", async () => {
		const content = JSON.stringify({
			conclusion: "连通正常",
			unknown_field: { deep: 1 },
		});
		api.getInspectionRun.mockResolvedValue({ ...detail });
		api.listInspectionReports.mockResolvedValue([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		api.getInspectionReport.mockResolvedValue({
			id: "43",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content,
			createdAt: "2026-09-10T10:01:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		expect(await screen.findByText("conclusion")).toBeInTheDocument();
		expect(screen.getByText("连通正常")).toBeInTheDocument();
		// 未知字段保留，不猜测标签，也不生成额外摘要。
		expect(screen.getByText("unknown_field")).toBeInTheDocument();
		expect(screen.getByText(/按真实字段展示/)).toBeInTheDocument();
		// 原文折叠保留，展开即原文。
		fireEvent.click(screen.getByRole("button", { name: "原始 JSON" }));
		expect(await screen.findByText(content)).toBeInTheDocument();
	});

	it("falls back to the preserved original when JSON-like report content fails to parse", async () => {
		const content = '{"truncated';
		api.getInspectionRun.mockResolvedValue({ ...detail });
		api.listInspectionReports.mockResolvedValue([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		api.getInspectionReport.mockResolvedValue({
			id: "44",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content,
			createdAt: "2026-09-10T10:01:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		expect(await screen.findByText(/原样保留全部原文/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "报告原文" }));
		expect(await screen.findByText(content)).toBeInTheDocument();
	});

	it("shows a reading state instead of claiming no report while the report request is in flight", async () => {
		api.getInspectionRun.mockResolvedValue({ ...detail });
		let resolveReports: (reports: unknown[]) => void = () => {};
		api.listInspectionReports.mockReturnValue(
			new Promise((resolve) => {
				resolveReports = resolve;
			}),
		);
		api.getInspectionReport.mockResolvedValue({
			id: "45",
			runId: "6",
			version: 1,
			evidenceDigest: "digest",
			evidenceIds: [],
			modelId: "fixture-chat",
			content: "# 报告",
			createdAt: "2026-09-10T10:01:00Z",
		});
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		// 报告请求在途时只显示读取状态，不发出“尚无报告”的错误断言。
		expect(
			await screen.findByRole("status", { name: "正在读取报告" }),
		).toBeInTheDocument();
		expect(screen.queryByText("该 Run 尚无报告版本。")).not.toBeInTheDocument();
		resolveReports([
			{
				version: 1,
				modelId: "fixture-chat",
				createdAt: "2026-09-10T10:01:00Z",
			},
		]);
		expect(await screen.findByTestId("report-content")).toBeInTheDocument();
		expect(screen.queryByText("该 Run 尚无报告版本。")).not.toBeInTheDocument();
	});

	it("shows the run's frozen analysis config", async () => {
		const frozenDetail = {
			...detail,
			frozenConfig: {
				displayName: "Prometheus 连通巡检",
				checkDescription: "连通性检查",
				metricUnit: "1=在线",
				reportInstructions: "冻结的初始要求",
			},
		};
		api.getInspectionRun.mockResolvedValue(frozenDetail);
		api.listInspectionReports.mockResolvedValue([]);
		feedback.fetchFeedback.mockResolvedValue({ items: [] });
		render(
			<RunDetail
				runId="6"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		// 冻结配置默认展开（defaultValue 含 frozen 段）。
		expect(
			await screen.findByText("冻结的分析配置（Run 创建时）"),
		).toBeInTheDocument();
		expect(await screen.findByText("连通性检查")).toBeInTheDocument();
		expect(screen.getByText("1=在线")).toBeInTheDocument();
		expect(screen.getByText("冻结的初始要求")).toBeInTheDocument();
		expect(
			screen.getByText("Prometheus 连通巡检", { selector: "dd" }),
		).toBeInTheDocument();
	});

	it("disables cancellation once the run is terminal even while its analysis is active", async () => {
		api.getInspectionRun.mockResolvedValue({
			id: "run-9",
			planKey: "prom-up",
			connectionName: "lab-prometheus",
			state: "Completed",
			rowVersion: 2,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: true,
			checks: [],
		});
		const { unmount } = render(
			<RunDetail
				runId="run-9"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		// Terminal run: cancel stays visible (the analysis still shows here) but can no longer be fired.
		expect(await screen.findByRole("button", { name: "取消" })).toBeDisabled();
		unmount();
		api.getInspectionRun.mockResolvedValue({
			id: "run-9",
			planKey: "prom-up",
			connectionName: "lab-prometheus",
			state: "Running",
			rowVersion: 2,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		render(
			<RunDetail
				runId="run-9"
				props={props}
				onBack={vi.fn()}
				onOpenRun={vi.fn()}
			/>,
		);
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "取消" })).toBeEnabled(),
		);
	});
});

describe("plan workspace", () => {
	it("shows a loading state instead of a premature empty list while reads hang", async () => {
		// The reads never settle (backend starvation symptom): the UI must not
		// claim "还没有巡检计划" / "没有巡检记录" — a hang is not an authoritative empty range.
		let resolvePlans: (plans: unknown[]) => void = () => {};
		api.listInspectionPlans.mockReturnValue(
			new Promise((resolve) => {
				resolvePlans = resolve;
			}),
		);
		api.listInspectionRuns.mockReturnValue(new Promise(() => {}));
		render(<InspectionView />);
		expect(
			await screen.findByRole("status", { name: "正在读取巡检计划" }),
		).toBeInTheDocument();
		expect(screen.queryByText("还没有巡检计划")).not.toBeInTheDocument();
		expect(screen.queryByText("没有巡检记录")).not.toBeInTheDocument();
		resolvePlans([integrationPlan]);
		expect(await screen.findByText("Prometheus 连通巡检")).toBeInTheDocument();
		expect(screen.queryByText("还没有巡检计划")).not.toBeInTheDocument();
	});

	it("lists real plans and a single server page of runs without business systems", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		api.listInspectionRuns.mockResolvedValue({ items: [runSummary] });
		render(<InspectionView />);
		expect(await screen.findByText("Prometheus 连通巡检")).toBeInTheDocument();
		// One page request, no business-system precondition and no full cursor walk.
		expect(api.listInspectionRuns).toHaveBeenCalledTimes(1);
		expect(api.listInspectionRuns).toHaveBeenCalledWith({});
		expect(
			screen.getByRole("button", { name: /Prometheus 连通巡检 · Run 1/ }),
		).toHaveTextContent("lab-prometheus");
	});

	it("filters runs by a selected plan server-side", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		render(<InspectionView />);
		await screen.findByText("Prometheus 连通巡检");
		fireEvent.click(screen.getByRole("combobox", { name: "按计划筛选" }));
		fireEvent.click(
			await screen.findByRole("option", { name: "Prometheus 连通巡检" }),
		);
		await waitFor(() =>
			expect(api.listInspectionRuns).toHaveBeenLastCalledWith({
				planKey: "prom-up",
			}),
		);
	});

	it("opens the run workspace for deep-linked run routes", async () => {
		api.getInspectionRun.mockResolvedValue({
			id: "run-9",
			planKey: "prom-up",
			connectionName: "lab-prometheus",
			state: "Running",
			rowVersion: 1,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		render(<InspectionView route="/inspections/runs/run-9" />);
		expect(await screen.findByText(/Run run-9/)).toBeInTheDocument();
		expect(api.getInspectionRun).toHaveBeenCalledWith("run-9");
	});

	it("starts a run from a real plan and deep-links the created run", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		api.createInspectionRun.mockResolvedValue({
			id: "run-6",
			planKey: "prom-up",
			state: "Queued",
			rowVersion: 1,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		const navigate = vi.fn();
		render(<InspectionView navigate={navigate} />);
		await screen.findByText("Prometheus 连通巡检");
		fireEvent.click(screen.getByRole("button", { name: "运行巡检" }));
		fireEvent.click(await screen.findByRole("button", { name: "开始巡检" }));
		await waitFor(() =>
			expect(api.createInspectionRun).toHaveBeenCalledWith("prom-up"),
		);
		await waitFor(() =>
			expect(navigate).toHaveBeenCalledWith("/inspections/runs/run-6"),
		);
	});

	it("starts a run directly from a plan row", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		api.createInspectionRun.mockResolvedValue({
			id: "run-7",
			planKey: "prom-up",
			state: "Queued",
			rowVersion: 1,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		const navigate = vi.fn();
		render(<InspectionView navigate={navigate} />);
		await screen.findByText("Prometheus 连通巡检");
		fireEvent.click(
			screen.getByRole("button", { name: "运行 Prometheus 连通巡检" }),
		);
		await waitFor(() =>
			expect(api.createInspectionRun).toHaveBeenCalledWith("prom-up"),
		);
		await waitFor(() =>
			expect(navigate).toHaveBeenCalledWith("/inspections/runs/run-7"),
		);
	});

	it("preselects the linked connection's enabled plan when arriving from integration details", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan, objectsPlan]);
		api.createInspectionRun.mockResolvedValue({
			id: "run-7",
			planKey: "prom-up",
			state: "Queued",
			rowVersion: 1,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		render(
			<InspectionView route="/inspections?connectionName=lab-prometheus" />,
		);
		// The chooser opens by itself with the connection's plan preselected; keys are never synthesized.
		const chooser = await screen.findByRole("dialog", { name: "运行巡检" });
		expect(within(chooser).getByRole("combobox")).toHaveTextContent(
			"Prometheus 连通巡检",
		);
		fireEvent.click(within(chooser).getByRole("button", { name: "开始巡检" }));
		await waitFor(() =>
			expect(api.createInspectionRun).toHaveBeenCalledWith("prom-up"),
		);
	});

	it("runs the business-view scoped plan when arriving with a businessViewKey hint", async () => {
		api.listInspectionPlans.mockResolvedValue([
			integrationPlan,
			businessViewPlan,
		]);
		api.createInspectionRun.mockResolvedValue({
			id: "run-8",
			planKey: "bv-payments",
			state: "Queued",
			rowVersion: 1,
			triggerKind: "manual",
			createdAt: "2026-09-10T10:00:00Z",
			reportCount: 0,
			analysisActive: false,
			checks: [],
		});
		render(
			<InspectionView route="/inspections?businessViewKey=payments-core&connectionName=lab-prometheus" />,
		);
		// Only the matching business-view plan qualifies; the whole-integration plan of the same
		// connection must never be executed for a business-view entry.
		const chooser = await screen.findByRole("dialog", { name: "运行巡检" });
		expect(within(chooser).getByRole("combobox")).toHaveTextContent(
			"支付核心视图巡检",
		);
		fireEvent.click(within(chooser).getByRole("button", { name: "开始巡检" }));
		await waitFor(() =>
			expect(api.createInspectionRun).toHaveBeenCalledTimes(1),
		);
		expect(api.createInspectionRun).toHaveBeenCalledWith("bv-payments");
	});

	it("routes to a prefilled business-view plan editor instead of running a wider plan", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		const navigate = vi.fn();
		render(
			<InspectionView
				route="/inspections?businessViewKey=payments-core&connectionName=lab-prometheus"
				navigate={navigate}
			/>,
		);
		// No business-view plan exists: navigation targets the prefilled editor route; the
		// integration-scope plan of the same connection must not be started for this entry.
		await waitFor(() =>
			expect(navigate).toHaveBeenCalledWith(
				"/inspections/plans/new?connectionName=lab-prometheus&businessViewKey=payments-core",
			),
		);
		expect(api.createInspectionRun).not.toHaveBeenCalled();
	});

	it("offers a prefilled plan editor when the linked connection has no enabled plan", async () => {
		api.listInspectionPlans.mockResolvedValue([
			{ ...integrationPlan, enabled: false },
		]);
		const navigate = vi.fn();
		render(
			<InspectionView
				route="/inspections?connectionName=lab-prometheus"
				navigate={navigate}
			/>,
		);
		expect(
			await screen.findByText(/还没有可立即运行的巡检计划/),
		).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "为该接入新建计划" }));
		expect(navigate).toHaveBeenCalledWith(
			"/inspections/plans/new?connectionName=lab-prometheus",
		);
	});
});

describe("plan editor", () => {
	it("creates a plan with YAML params, integration scope, and analysis semantics", async () => {
		const navigate = vi.fn();
		api.createInspectionPlan.mockResolvedValue(integrationPlan);
		render(
			<InspectionView route="/inspections/plans/new" navigate={navigate} />,
		);
		fireEvent.change(await screen.findByLabelText("计划 Key"), {
			target: { value: "prom-up" },
		});
		fireEvent.change(screen.getByLabelText("显示名称"), {
			target: { value: "Prometheus 连通巡检" },
		});
		// 接入与插件从实时列表选择；选中接入后按接入类型自动预选插件。
		fireEvent.click(screen.getByRole("combobox", { name: "接入连接" }));
		fireEvent.click(
			await screen.findByRole("option", {
				name: /lab-prometheus（prometheus）/,
			}),
		);
		fireEvent.change(screen.getByLabelText("模板 ID"), {
			target: { value: "prometheus-up" },
		});
		fireEvent.change(screen.getByLabelText("采集参数（YAML）"), {
			target: { value: "expression: up\n" },
		});
		// 可选分析要求收在折叠区内，展开后填写。
		fireEvent.click(
			screen.getByRole("button", { name: /分析与报告要求（可选）/ }),
		);
		fireEvent.change(await screen.findByLabelText("检查说明"), {
			target: { value: "连通性检查" },
		});
		fireEvent.change(screen.getByLabelText("指标单位"), {
			target: { value: "1=在线" },
		});
		fireEvent.change(screen.getByLabelText("初始报告要求"), {
			target: { value: "逐检查项给出结论" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
		await waitFor(() =>
			expect(api.createInspectionPlan).toHaveBeenCalledWith({
				planKey: "prom-up",
				displayName: "Prometheus 连通巡检",
				enabled: true,
				connectionName: "lab-prometheus",
				pluginId: "prometheus",
				templateId: "prometheus-up",
				templateVersion: null,
				params: { expression: "up" },
				scope: { kind: "integration" },
				checkDescription: "连通性检查",
				metricUnit: "1=在线",
				reportInstructions: "逐检查项给出结论",
				cron: null,
				timezone: expect.any(String),
			}),
		);
		await waitFor(() => expect(navigate).toHaveBeenCalledWith("/inspections"));
	});

	it("prefills the new-plan editor from route query hints", async () => {
		render(
			<InspectionView route="/inspections/plans/new?connectionName=lab-prometheus&businessViewKey=payments-core" />,
		);
		const connection = await screen.findByRole("combobox", {
			name: "接入连接",
		});
		expect(connection).toHaveTextContent("lab-prometheus（prometheus）");
		expect(
			screen.getByRole("combobox", { name: "巡检范围" }),
		).toHaveTextContent("业务视图");
		expect(
			await screen.findByRole("combobox", { name: "业务视图" }),
		).toHaveTextContent("支付核心（payments-core）");
	});

	it("creates an objects-scoped plan from explicit object rows", async () => {
		api.createInspectionPlan.mockResolvedValue(objectsPlan);
		render(<InspectionView route="/inspections/plans/new" />);
		fireEvent.change(await screen.findByLabelText("计划 Key"), {
			target: { value: "pod-check" },
		});
		fireEvent.change(screen.getByLabelText("显示名称"), {
			target: { value: "指定对象巡检" },
		});
		fireEvent.click(screen.getByRole("combobox", { name: "接入连接" }));
		fireEvent.click(
			await screen.findByRole("option", {
				name: /lab-prometheus（prometheus）/,
			}),
		);
		fireEvent.change(screen.getByLabelText("模板 ID"), {
			target: { value: "pod-check" },
		});
		fireEvent.click(screen.getByRole("combobox", { name: "巡检范围" }));
		fireEvent.click(await screen.findByRole("option", { name: "指定对象" }));
		fireEvent.change(await screen.findByLabelText("对象类型"), {
			target: { value: "kubernetes_pod" },
		});
		fireEvent.change(screen.getByLabelText("对象身份键"), {
			target: { value: "demo/api" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
		await waitFor(() =>
			expect(api.createInspectionPlan).toHaveBeenCalledWith(
				expect.objectContaining({
					scope: {
						kind: "objects",
						objects: [
							{ objectType: "kubernetes_pod", identityKey: "demo/api" },
						],
					},
				}),
			),
		);
	});

	it("creates a business-view scoped plan from the view picker", async () => {
		api.createInspectionPlan.mockResolvedValue(businessViewPlan);
		render(<InspectionView route="/inspections/plans/new" />);
		fireEvent.change(await screen.findByLabelText("计划 Key"), {
			target: { value: "bv-check" },
		});
		fireEvent.change(screen.getByLabelText("显示名称"), {
			target: { value: "业务视图巡检" },
		});
		fireEvent.click(screen.getByRole("combobox", { name: "接入连接" }));
		fireEvent.click(
			await screen.findByRole("option", {
				name: /lab-prometheus（prometheus）/,
			}),
		);
		fireEvent.change(screen.getByLabelText("模板 ID"), {
			target: { value: "bv-check" },
		});
		fireEvent.click(screen.getByRole("combobox", { name: "巡检范围" }));
		fireEvent.click(await screen.findByRole("option", { name: "业务视图" }));
		fireEvent.click(await screen.findByRole("combobox", { name: "业务视图" }));
		fireEvent.click(
			await screen.findByRole("option", { name: /支付核心（payments-core）/ }),
		);
		fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
		await waitFor(() =>
			expect(api.createInspectionPlan).toHaveBeenCalledWith(
				expect.objectContaining({
					scope: { kind: "businessView", businessViewKey: "payments-core" },
				}),
			),
		);
	});

	it("updates an existing plan with its optimistic row version", async () => {
		api.getInspectionPlan.mockResolvedValue({
			...integrationPlan,
			checkDescription: "连通性检查",
			metricUnit: "1=在线",
			reportInstructions: "逐检查项给出结论",
		});
		api.updateInspectionPlan.mockResolvedValue({
			...integrationPlan,
			displayName: "改名巡检",
			rowVersion: 4,
		});
		render(<InspectionView route="/inspections/plans/prom-up/edit" />);
		fireEvent.click(
			await screen.findByRole("button", { name: /分析与报告要求（可选）/ }),
		);
		expect(await screen.findByLabelText("检查说明")).toHaveValue("连通性检查");
		expect(screen.getByLabelText("指标单位")).toHaveValue("1=在线");
		expect(screen.getByLabelText("初始报告要求")).toHaveValue(
			"逐检查项给出结论",
		);
		// 编辑模式下计划 Key 锁定。
		expect(screen.getByLabelText("计划 Key")).toBeDisabled();
		fireEvent.change(screen.getByLabelText("显示名称"), {
			target: { value: "改名巡检" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
		await waitFor(() =>
			expect(api.updateInspectionPlan).toHaveBeenCalledWith(
				"prom-up",
				expect.objectContaining({
					expectedRowVersion: 3,
					displayName: "改名巡检",
					scope: { kind: "integration" },
					params: { expression: "up" },
					cron: "*/5 * * * *",
				}),
			),
		);
	});

	it("keeps invalid plan input on the form with a readable reason", async () => {
		render(<InspectionView route="/inspections/plans/new" />);
		fireEvent.click(await screen.findByRole("button", { name: "保存计划" }));
		expect(
			await screen.findByText(
				/计划 Key、显示名称、接入连接、插件和模板 ID 必须填写/,
			),
		).toBeInTheDocument();
		expect(api.createInspectionPlan).not.toHaveBeenCalled();
	});

	it("falls back to a plain input when the connection list cannot be read", async () => {
		resources.listConnections.mockRejectedValue(new Error("boom"));
		render(<InspectionView route="/inspections/plans/new" />);
		fireEvent.change(await screen.findByLabelText("接入连接"), {
			target: { value: "lab-prometheus" },
		});
		expect(
			screen.getByRole("button", { name: "重试读取" }),
		).toBeInTheDocument();
	});

	it("keeps a retired connection visible as the current value", async () => {
		api.getInspectionPlan.mockResolvedValue({
			...integrationPlan,
			planKey: "old-plan",
			connectionName: "retired-prometheus",
		});
		render(<InspectionView route="/inspections/plans/old-plan/edit" />);
		const connection = await screen.findByRole("combobox", {
			name: "接入连接",
		});
		expect(connection).toHaveTextContent("retired-prometheus（当前值）");
	});
});

describe("workspace shell integration", () => {
	// Integration path: the real shell renders view.actions twice (desktop and mobile
	// headers). Stateful dialogs must live in the single-mount content slot, or two
	// stacked modal dialogs aria-hide each other and every button loses its name.
	function ShellHost({ route }: { route: string }) {
		const view = useInspectionsModule({ ...props, route });
		return (
			<WorkspaceShell
				user={props.user}
				route={route}
				view={view}
				navigate={props.navigate}
				onLogout={vi.fn()}
			/>
		);
	}

	beforeEach(() => {
		vi.stubGlobal(
			"matchMedia",
			vi.fn().mockReturnValue({
				matches: false,
				addEventListener: vi.fn(),
				removeEventListener: vi.fn(),
			}),
		);
		vi.stubGlobal(
			"ResizeObserver",
			class {
				observe() {}
				unobserve() {}
				disconnect() {}
			},
		);
	});

	it("mounts exactly one accessible run chooser although the shell renders header actions twice", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		render(<ShellHost route="/inspections?connectionName=lab-prometheus" />);
		const dialogs = await screen.findAllByRole("dialog", { name: "运行巡检" });
		expect(dialogs).toHaveLength(1);
		expect(screen.getAllByRole("button", { name: "开始巡检" })).toHaveLength(1);
	});

	it("routes to a single prefilled business-view editor through the shell", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		render(<ShellHost route="/inspections?businessViewKey=payments-core" />);
		// 无匹配计划时跳转编辑器路由（恰好一次），不弹出对话框、不启动任何 Run。
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/inspections/plans/new?businessViewKey=payments-core",
			),
		);
		expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
		expect(api.createInspectionRun).not.toHaveBeenCalled();
	});

	it("opens the chooser once from any duplicated header trigger", async () => {
		api.listInspectionPlans.mockResolvedValue([integrationPlan]);
		const { getAllByRole } = render(<ShellHost route="/inspections" />);
		await screen.findByText("Prometheus 连通巡检");
		fireEvent.click(getAllByRole("button", { name: "运行巡检" })[0]);
		const dialogs = await screen.findAllByRole("dialog", { name: "运行巡检" });
		expect(dialogs).toHaveLength(1);
		expect(screen.getAllByRole("button", { name: "开始巡检" })).toHaveLength(1);
	});
});
