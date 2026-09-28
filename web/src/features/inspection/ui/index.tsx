import { CursorPagination } from "@/components/workbench/CursorPagination";
/* eslint-disable react-refresh/only-export-components -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

import { useCursorPages } from '@/hooks/use-cursor-pages';
import { LoaderCircle } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf, notify } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import { Separator } from "@/components/ui/separator";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { PropertyList } from "@/components/workbench/PropertyList";
import {
	createInspectionRun,
	formatInspectionTime,
	type InspectionPlan,
	type InspectionRunSummary,
	inspectionActive,
	inspectionScheduleText,
	inspectionScopeText,
	inspectionStateText,
	listInspectionPlans,
	listInspectionRuns,
	statusBadgeClass,
} from "@/features/inspection/api";
import { parseRoute } from "@/lib/parse-route";

export { PlanEditor, type PlanEditorPrefill } from "./PlanEditor";
export { ReportBody, RunDetail } from "./RunDetail";

import { PlanEditor, type PlanEditorPrefill } from "./PlanEditor";
import { RunDetail } from "./RunDetail";
import { DailyConfigEditor } from "./DailyConfigEditor";
import { DailyReportDetailView } from "./DailyReportDetail";
import { DailyReportsOverview } from "./DailyReports";

/** The module owns its query state: /inspections/runs/:run and /inspections/plans/(new|:key/edit). */
function parts(route: string) {
	const { pathname, searchParams } = parseRoute(route);
	return { path: pathname, query: searchParams };
}
function runRoute(runId: string, path = "/inspections") {
	// 详情是右侧抽屉，由 query 标志驱动：所在页面（概览或计划详情）保持在抽屉下方挂载。
	return `${path}?run=${encodeURIComponent(runId)}`;
}
function newPlanRoute(prefill?: PlanEditorPrefill) {
	const query = new URLSearchParams();
	if (prefill?.connectionName)
		query.set("connectionName", prefill.connectionName);
	if (prefill?.businessViewKey)
		query.set("businessViewKey", prefill.businessViewKey);
	const suffix = query.size ? `?${query.toString()}` : "";
	return `/inspections/plans/new${suffix}`;
}

/** Lightweight chooser that starts a Run from a real enabled plan. */
function RunChooserDialog({
	onOpenChange,
	plans,
	planKey,
	onPlanChange,
	suspended,
	busy,
	error,
	onStart,
}: {
	onOpenChange: (open: boolean) => void;
	plans: InspectionPlan[];
	planKey: string;
	onPlanChange: (planKey: string) => void;
	suspended: boolean;
	busy: boolean;
	error: string;
	onStart: () => void;
}) {
	const enabled = plans.filter((item) => item.enabled);
	const selected = enabled.find((item) => item.planKey === planKey);
	return (
		<DialogContent>
			<DialogHeader>
				<DialogTitle>运行巡检</DialogTitle>
				<DialogDescription>
					选择巡检计划立即采证；范围与接入在 Run
					创建时冻结，同一计划同时最多一个执行中的 Run。
				</DialogDescription>
			</DialogHeader>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{enabled.length ? (
				<FieldGroup>
					<Field>
						<FieldLabel htmlFor="run-plan">巡检计划</FieldLabel>
						<Select
							value={planKey}
							onValueChange={onPlanChange}
							disabled={busy || suspended}
						>
							<SelectTrigger id="run-plan" aria-label="巡检计划选择">
								<SelectValue placeholder="选择计划" />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									{enabled.map((item) => (
										<SelectItem key={item.planKey} value={item.planKey}>
											{item.displayName} · {item.connectionName}
										</SelectItem>
									))}
								</SelectGroup>
							</SelectContent>
						</Select>
						{selected && (
							<FieldDescription>
								接入 {selected.connectionName} ·{" "}
								{inspectionScopeText(selected.scope)} ·{" "}
								{inspectionScheduleText(selected)}
							</FieldDescription>
						)}
					</Field>
				</FieldGroup>
			) : (
				<Alert>
					<AlertTitle>还没有可运行的巡检计划</AlertTitle>
					<AlertDescription>
						先创建并启用一个巡检计划，再从这里立即采证。
					</AlertDescription>
				</Alert>
			)}
			<DialogFooter>
				<Button
					variant="outline"
					onClick={() => onOpenChange(false)}
					disabled={busy}
				>
					取消
				</Button>
				<Button
					onClick={onStart}
					disabled={!planKey || !enabled.length || busy || suspended}
				>
					{busy ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							启动中…
						</>
					) : (
						"开始巡检"
					)}
				</Button>
			</DialogFooter>
		</DialogContent>
	);
}

/**
 * Preselect rule for the chooser. A business-view hint restricts candidates to
 * business-view plans of that key (optionally same connection) so a wider
 * integration-scoped plan can never be executed for a business-view entry.
 */
function preferredPlan(
	plans: InspectionPlan[],
	businessViewHint: string,
	connectionHint: string,
): string {
	const enabled = plans.filter((item) => item.enabled);
	if (businessViewHint) {
		const bvMatch = enabled.find(
			(item) =>
				item.scope.kind === "businessView" &&
				item.scope.businessViewKey === businessViewHint &&
				(!connectionHint || item.connectionName === connectionHint),
		);
		return bvMatch?.planKey ?? "";
	}
	const hinted = connectionHint
		? enabled.find((item) => item.connectionName === connectionHint)
		: undefined;
	return hinted?.planKey ?? enabled[0]?.planKey ?? "";
}

export function useInspectionsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const { path, query } = parts(props.route);
	const runId = query.get("run") || undefined;
	const creatingPlan = path === "/inspections/plans/new";
	const editMatch = path.match(/^\/inspections\/plans\/([^/]+)\/edit$/);
	const editPlanKey = editMatch ? decodeURIComponent(editMatch[1]) : undefined;
	const detailMatch = path.match(/^\/inspections\/plans\/([^/]+)$/);
	const detailPlanKey = detailMatch
		? decodeURIComponent(detailMatch[1])
		: undefined;
	// 每日报告（ADR-0014）：跨来源日报面，挂在巡检模块下；/daily 分支先于
	// 计划路由，localDate 严格 YYYY-MM-DD，不会与 /edit 混淆。
	const dailyOverview = path === "/inspections/daily";
	const dailyCreating = path === "/inspections/daily/new";
	const dailyEditMatch = path.match(/^\/inspections\/daily\/([^/]+)\/edit$/);
	const dailyEditKey = dailyEditMatch
		? decodeURIComponent(dailyEditMatch[1])
		: undefined;
	const dailyReportMatch = path.match(
		/^\/inspections\/daily\/([^/]+)\/(\d{4}-\d{2}-\d{2})$/,
	);
	const dailyReportKey = dailyReportMatch
		? {
				configKey: decodeURIComponent(dailyReportMatch[1]),
				localDate: dailyReportMatch[2],
			}
		: undefined;
	const dailySection =
		dailyOverview || dailyCreating || Boolean(dailyEditKey) || Boolean(dailyReportKey);
	const connectionHint = query.get("connectionName") ?? "";
	const businessViewHint = query.get("businessViewKey") ?? "";
	// 预填始终来自 URL 提示：概览页操作按钮与 deep-link 升级都把它带给编辑器路由。
	const hintPrefill = useMemo<PlanEditorPrefill>(
		() => ({
			connectionName: connectionHint || undefined,
			businessViewKey: businessViewHint || undefined,
		}),
		[connectionHint, businessViewHint],
	);
	const editorPrefill: PlanEditorPrefill | undefined =
		creatingPlan || editPlanKey ? hintPrefill : undefined;
	const [plans, setPlans] = useState<InspectionPlan[]>([]);
	const [latestRuns, setLatestRuns] = useState<
		Record<string, InspectionRunSummary>
	>({});
	const [loaded, setLoaded] = useState(false);
	const [chooserOpen, setChooserOpen] = useState(false);
	const [chooserPlan, setChooserPlan] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [hintApplied, setHintApplied] = useState(false);
	// Plans and the latest-run projection are independent reads: each settles and
	// updates the UI on its own, so one hung endpoint can never starve the other.
	const load = useCallback(async () => {
		const plansRead = listInspectionPlans().then(
			(items) => {
				setPlans(items);
				setLoaded(true);
			},
			(reason: unknown) => setError(messageOf(reason, "无法读取巡检计划。")),
		);
		// 概览只取第一页 Run 投影“最近一次结论”；更早的历史归档在计划详情页分页呈现。
		const latestRead = listInspectionRuns({ limit: 100 }).then(
			(page) => setLatestRuns(latestByPlan(page.items)),
			(reason: unknown) => setError(messageOf(reason, "无法读取巡检记录。")),
		);
		// The wrapped reads never reject; awaiting them keeps save-reload callers in step.
		await Promise.allSettled([plansRead, latestRead]);
	}, []);
	// biome-ignore lint/correctness/useExhaustiveDependencies: path 是有意依赖——从编辑器或 Run 页返回时必须重新拉取计划与最近结论。
	useEffect(() => {
		// 每日报告路由不消费计划/Run 投影，跳过拉取以免无谓请求与无关错误。
		if (dailySection) return;
		const timer = window.setTimeout(() => void load(), 0);
		return () => clearTimeout(timer);
	}, [load, path, dailySection]);
	// 概览有执行中的 Run 时轮询最近结论；计划详情页对自己的历史归档单独轮询。
	const hasActiveRun = Object.values(latestRuns).some((run) =>
		inspectionActive(run.state),
	);
	useEffect(() => {
		if (props.suspended || !hasActiveRun) return;
		const timer = window.setTimeout(() => void load(), 2000);
		return () => clearTimeout(timer);
	}, [load, props.suspended, hasActiveRun]);
	// Deep links act once. A business-view hint only ever matches business-view
	// scoped plans — a same-connection integration plan would widen the range, so
	// without a match the prefilled editor route is opened instead of any run.
	useEffect(() => {
		if (
			hintApplied ||
			!loaded ||
			runId ||
			creatingPlan ||
			editPlanKey ||
			(!businessViewHint && !connectionHint)
		)
			return;
		setHintApplied(true);
		if (businessViewHint) {
			const match = plans.find(
				(item) =>
					item.enabled &&
					item.scope.kind === "businessView" &&
					item.scope.businessViewKey === businessViewHint &&
					(!connectionHint || item.connectionName === connectionHint),
			);
			if (match) {
				setChooserPlan(match.planKey);
				setChooserOpen(true);
			} else props.navigate(newPlanRoute(hintPrefill));
			return;
		}
		const match = plans.find(
			(item) => item.enabled && item.connectionName === connectionHint,
		);
		if (match) {
			setChooserPlan(match.planKey);
			setChooserOpen(true);
		}
		// Connection-only entries keep the overview visible; its hint alert links to a
		// prefilled editor. Only business-view entries escalate straight to the editor.
	}, [
		businessViewHint,
		connectionHint,
		hintPrefill,
		hintApplied,
		loaded,
		plans,
		props,
		runId,
		creatingPlan,
		editPlanKey,
	]);
	function openChooser() {
		setChooserPlan((current) =>
			current && plans.some((item) => item.planKey === current && item.enabled)
				? current
				: preferredPlan(plans, businessViewHint, connectionHint),
		);
		setChooserOpen(true);
	}
	/** Starts a Run for one plan and deep-links the created run. */
	async function startPlan(planKey: string) {
		if (!planKey || props.suspended) return;
		setBusy(true);
		setError("");
		try {
			const run = await createInspectionRun(planKey);
			notify.success("已开始巡检");
			setChooserOpen(false);
			await load();
			props.navigate(runRoute(run.id, path));
		} catch (reason) {
			// 失败原因保留内联展示：ui/index.test.tsx 断言该文案同时出现在 chooser 与 overview，不迁移到全局 toast。
			setError(messageOf(reason, "无法创建巡检。"));
		} finally {
			setBusy(false);
		}
	}
	const planName = (planKey: string) =>
		plans.find((item) => item.planKey === planKey)?.displayName ?? planKey;
	const hintHasEnabledPlan = plans.some(
		(item) => item.enabled && item.connectionName === connectionHint,
	);
	const overview = (
		<div className="space-y-4">
			{error && (
				<Alert variant="destructive">
					<AlertTitle>读取失败</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{!businessViewHint &&
				connectionHint &&
				loaded &&
				(hintHasEnabledPlan ? (
					<p role="status" className="text-sm text-muted-foreground">
						已为接入 {connectionHint} 预选巡检计划，可在“运行巡检”中直接启动。
					</p>
				) : (
					<Alert>
						<AlertTitle>
							接入 {connectionHint} 还没有可立即运行的巡检计划。
						</AlertTitle>
						<AlertDescription>
							为该接入创建并启用一个计划后，即可立即采证并生成报告。
						</AlertDescription>
						<Button
							size="sm"
							variant="outline"
							className="mt-2"
							disabled={props.suspended}
							onClick={() => props.navigate(newPlanRoute(hintPrefill))}
						>
							为该接入新建计划
						</Button>
					</Alert>
				))}
			<section className="space-y-3">
				<div className="flex flex-wrap items-start justify-between gap-2">
					<div className="space-y-1">
						<h2 className="text-xl font-semibold">巡检计划</h2>
						<p className="text-sm text-muted-foreground">
							计划绑定接入与模板，按范围定期或手动采证；点击计划查看最近结论与历史归档。
						</p>
					</div>
					<div className="flex gap-2">
						<Button
							variant="outline"
							size="sm"
							disabled={props.suspended}
							onClick={() => props.navigate("/inspections/daily")}
						>
							每日报告
						</Button>
						<Button
							variant="outline"
							size="sm"
							disabled={props.suspended}
							onClick={openChooser}
						>
							运行巡检
						</Button>
						<Button
							size="sm"
							disabled={props.suspended}
							onClick={() => props.navigate(newPlanRoute(hintPrefill))}
						>
							新建计划
						</Button>
					</div>
				</div>
				<EntityList
					items={plans.map((item) => {
						const latest = latestRuns[item.planKey];
						return {
							id: item.planKey,
							title: item.displayName,
							subtitle: [
								item.planKey,
								item.connectionName,
								inspectionScopeText(item.scope),
								inspectionScheduleText(item),
							].join(" · "),
							badge: {
								text: item.enabled ? "已启用" : "已停用",
								variant: item.enabled
									? ("secondary" as const)
									: ("outline" as const),
							},
							// 最近一次结论：最新 Run 的状态与完成时间；从未运行如实标注。
							time: latest
								? `最近 ${inspectionStateText[latest.state]} · ${formatInspectionTime(latest.createdAt)}`
								: "从未运行",
							plan: item,
						};
					})}
					columns={["title", "subtitle", "status", "time", "actions"]}
					onSelect={(item) =>
						props.navigate(
							`/inspections/plans/${encodeURIComponent(item.plan.planKey)}`,
						)
					}
					renderActions={(item) => (
						<Button
							variant="outline"
							size="sm"
							aria-label={`运行 ${item.plan.displayName}`}
							disabled={!item.plan.enabled || busy || props.suspended}
							onClick={() => void startPlan(item.plan.planKey)}
						>
							运行
						</Button>
					)}
					loading={!loaded}
					loadingLabel="正在读取巡检计划"
					emptyTitle="还没有巡检计划"
					emptyDescription="使用标题栏的“新建计划”为接入创建第一个巡检计划。"
				/>
			</section>
		</div>
	);
	// The shell renders view.actions in both desktop and mobile headers. Plain
	// trigger buttons are safe to duplicate; stateful Dialogs must mount exactly
	// once, so they live here in the single-mount content slot. Otherwise two
	// stacked modal dialogs aria-hide each other and every control loses its
	// accessible name.
	const chooserDialog = (
		<Dialog open={chooserOpen} onOpenChange={setChooserOpen}>
			<RunChooserDialog
				onOpenChange={setChooserOpen}
				plans={plans}
				planKey={chooserPlan}
				onPlanChange={setChooserPlan}
				suspended={props.suspended}
				busy={busy}
				error={error}
				onStart={() => void startPlan(chooserPlan)}
			/>
		</Dialog>
	);
	const runSummary = runId
		? Object.values(latestRuns).find((run) => run.id === runId)
		: undefined;
	const runSheet = runId && (
		<DetailSheet
			open
			onClose={() => props.navigate(path)}
			title={
				runSummary
					? `${planName(runSummary.planKey)} · Run ${runSummary.id}`
					: `Run ${runId}`
			}
			description="报告版本不可修改。"
		>
			<div className="min-h-0 flex-1 overflow-y-auto">
				<div className="space-y-6 p-4 sm:p-6">
					<RunDetail
						runId={runId}
						props={props}
						onOpenRun={(id) => props.navigate(runRoute(id, path))}
					/>
				</div>
			</div>
		</DetailSheet>
	);
	// 每日报告路由：独立的面板与面包屑；不渲染计划概览的 chooser/run 抽屉。
	if (dailySection) {
		const reportKey = dailyReportKey;
		return {
			title: dailyOverview
				? "每日报告"
				: dailyCreating
					? "新建每日报告配置"
					: dailyEditKey
						? "编辑每日报告配置"
						: reportKey
							? `${reportKey.configKey} · ${reportKey.localDate}`
							: "每日报告",
			crumbs: [
				{ label: "巡检", to: "/inspections" },
				...(dailyOverview
					? [{ label: "每日报告" }]
					: [{ label: "每日报告", to: "/inspections/daily" }]),
				...(dailyCreating
					? [{ label: "新建配置" }]
					: dailyEditKey
						? [{ label: "编辑配置" }]
						: reportKey
							? [{ label: reportKey.localDate }]
							: []),
		],
		list: null,
		actions: undefined,
		content: dailyOverview ? (
			<DailyReportsOverview
				suspended={props.suspended}
				navigate={props.navigate}
			/>
		) : dailyCreating ? (
			<DailyConfigEditor suspended={props.suspended} navigate={props.navigate} />
		) : dailyEditKey ? (
			<DailyConfigEditor
				suspended={props.suspended}
				navigate={props.navigate}
				editKey={dailyEditKey}
			/>
		) : reportKey ? (
			<DailyReportDetailView
				suspended={props.suspended}
				navigate={props.navigate}
				configKey={reportKey.configKey}
				localDate={reportKey.localDate}
			/>
		) : null,
	};
	}
	return {
		title: creatingPlan
			? "新建巡检计划"
			: editPlanKey
				? "编辑巡检计划"
				: detailPlanKey
					? planName(detailPlanKey)
					: "巡检",
		crumbs: creatingPlan
			? [{ label: "巡检", to: "/inspections" }, { label: "新建巡检计划" }]
			: editPlanKey
				? [
						{ label: "巡检", to: "/inspections" },
						{
							label: planName(editPlanKey),
							to: `/inspections/plans/${encodeURIComponent(editPlanKey)}`,
						},
						{ label: "编辑" },
					]
				: detailPlanKey
					? [
							{ label: "巡检", to: "/inspections" },
							{ label: planName(detailPlanKey) },
						]
					: undefined,
		// 记录列表并入右侧概览页：第二栏只保留共享的运维模块导航，与告警列表一致。
		list: null,
		// 概览操作内联在页面头行（与用户管理/模型提供方同一模式），不走 shell
		// actions——概览页因此与其它页面一样没有顶部 sticky 标头。
		actions: undefined,
		content: (
			<>
				{chooserDialog}
				{runSheet}
				{creatingPlan || editPlanKey ? (
					<PlanEditor
						suspended={props.suspended}
						navigate={props.navigate}
						editKey={editPlanKey}
						prefill={creatingPlan ? editorPrefill : undefined}
					/>
				) : detailPlanKey ? (
					<PlanDetail
						plan={plans.find((item) => item.planKey === detailPlanKey)}
						plansLoaded={loaded}
						props={props}
						busy={busy}
						onRun={(planKey) => void startPlan(planKey)}
					/>
				) : (
					overview
				)}
			</>
		),
	};
}

/** 概览行的“最近一次结论”：按创建时间倒序取每个计划的最新一条 Run。 */
function latestByPlan(runs: InspectionRunSummary[]) {
	const latest: Record<string, InspectionRunSummary> = {};
	for (const run of [...runs].sort((a, b) =>
		b.createdAt.localeCompare(a.createdAt),
	)) {
		if (!latest[run.planKey]) latest[run.planKey] = run;
	}
	return latest;
}

/**
 * 计划详情：配置概要 + 操作（运行/编辑）+ 该计划的历史归档（分页 Run 列表）。
 * 记录是计划的下一级，不再与计划并列堆在概览页。
 */
function PlanDetail({
	plan,
	plansLoaded,
	props,
	busy,
	onRun,
}: {
	plan: InspectionPlan | undefined;
	plansLoaded: boolean;
	props: WorkspaceModuleProps;
	busy: boolean;
	onRun: (planKey: string) => void;
}) {
	const planKey = plan?.planKey ?? "";
	const history = useCursorPages<InspectionRunSummary>(
		(cursor) =>
			planKey
				? listInspectionRuns({ planKey, cursor })
				: Promise.resolve({ items: [] }),
		{
			suspended: props.suspended,
			resetKey: planKey,
			fallbackError: "无法读取巡检记录。",
		},
	);
	const hasActive = history.items.some((run) => inspectionActive(run.state));
	const { refresh: refreshHistory } = history;
	useEffect(() => {
		if (props.suspended || !hasActive) return;
		const timer = window.setTimeout(() => void refreshHistory(), 2000);
		return () => window.clearTimeout(timer);
	}, [props.suspended, hasActive, refreshHistory]);
	if (plansLoaded && !plan)
		return (
			<Empty>
				<EmptyHeader>
					<EmptyTitle>计划不存在</EmptyTitle>
					<EmptyDescription>它可能已被删除，或链接有误。</EmptyDescription>
				</EmptyHeader>
				<Button
					variant="outline"
					size="sm"
					onClick={() => props.navigate("/inspections")}
				>
					返回巡检概览
				</Button>
			</Empty>
		);
	if (!plan) return <DetailSkeleton label="正在读取巡检计划" />;
	return (
		<div className="space-y-6">
			<section className="space-y-3">
				<div className="flex flex-wrap items-center gap-2">
					<h2 className="text-xl font-semibold">{plan.displayName}</h2>
					<Badge variant={plan.enabled ? "secondary" : "outline"}>
						{plan.enabled ? "已启用" : "已停用"}
					</Badge>
					<div className="ml-auto flex gap-2">
						<Button
							variant="outline"
							size="sm"
							disabled={props.suspended}
							onClick={() =>
								props.navigate(
									`/inspections/plans/${encodeURIComponent(plan.planKey)}/edit`,
								)
							}
						>
							编辑
						</Button>
						<Button
							size="sm"
							disabled={!plan.enabled || busy || props.suspended}
							onClick={() => onRun(plan.planKey)}
						>
							运行
						</Button>
					</div>
				</div>
				<PropertyList
					layout="grid-2"
					entries={[
						{
							label: "计划 Key",
							value: <span className="font-mono text-xs">{plan.planKey}</span>,
						},
						{ label: "接入", value: plan.connectionName },
						{
							label: "插件 · 模板",
							value: `${plan.pluginId} · ${plan.templateId}${plan.templateVersion ? `@${plan.templateVersion}` : ""}`,
						},
						{ label: "范围", value: inspectionScopeText(plan.scope) },
						{ label: "调度", value: inspectionScheduleText(plan) },
						{ label: "更新时间", value: formatInspectionTime(plan.updatedAt) },
					]}
				/>
			</section>
			<Separator />
			<section className="space-y-3">
				<h3 className="text-sm font-medium">历史归档</h3>
				<p className="text-sm text-muted-foreground">
					每次运行生成一份不可修改的报告。
				</p>
				<EntityList
					items={history.items.map((run) => ({
						id: run.id,
						title: `Run ${run.id}`,
						subtitle: [run.triggerKind === "manual" ? "手动" : "定时"]
							.filter(Boolean)
							.join(" · "),
						badge: {
							text: inspectionStateText[run.state],
							variant: "outline" as const,
							className: statusBadgeClass(run.state),
						},
						time: formatInspectionTime(run.createdAt),
						run,
					}))}
					columns={["title", "subtitle", "status", "time"]}
					onSelect={(item) =>
						props.navigate(
							runRoute(
								item.run.id,
								`/inspections/plans/${encodeURIComponent(plan.planKey)}`,
							),
						)
					}
					loading={history.loading}
					loadingLabel="正在读取巡检记录"
					emptyTitle="没有巡检记录"
				/>
				{history.error && (
					<Alert variant="destructive">
						<AlertDescription>{history.error}</AlertDescription>
					</Alert>
				)}
				<CursorPagination
					page={history.page}
					hasPrev={history.hasPrev}
					hasNext={history.hasNext}
					loading={history.navigating}
					onPrev={history.goPrev}
					onNext={history.goNext}
				/>
			</section>
		</div>
	);
}
