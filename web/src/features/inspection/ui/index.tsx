import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
/* eslint-disable react-refresh/only-export-components -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

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
import { DetailSheet } from "@/components/workbench/DetailSheet";
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

/** The module owns its query state: /inspections/runs/:run and /inspections/plans/(new|:key/edit). */
function parts(route: string) {
	const { pathname, searchParams } = parseRoute(route);
	return { path: pathname, query: searchParams };
}
function runRoute(runId: string) {
	// 详情是右侧抽屉，由 query 标志驱动：列表页保持在抽屉下方挂载。
	return `/inspections?run=${encodeURIComponent(runId)}`;
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
	const [runs, setRuns] = useState<InspectionRunSummary[]>([]);
 const [nextCursor,setNextCursor]=useState<string>();
 const [loadingMore,setLoadingMore]=useState(false);
	const [planFilter, setPlanFilter] = useState("all");
	const [loaded, setLoaded] = useState(false);
	const [runsLoaded, setRunsLoaded] = useState(false);
	const [chooserOpen, setChooserOpen] = useState(false);
	const [chooserPlan, setChooserPlan] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [hintApplied, setHintApplied] = useState(false);
	// Runs and plans are independent reads: each settles and updates the UI on its
	// own, so one hung or failing endpoint can never starve the other, and each
	// loaded flag reflects its own read (a hung read must not masquerade as an
	// authoritative empty range).
	const load = useCallback(async () => {
		const filter = planFilter === "all" ? {} : { planKey: planFilter };
		const runsRead = listInspectionRuns(filter).then(
			(page) => {
				setRuns(page.items); setNextCursor(page.nextCursor);
				setRunsLoaded(true);
			},
			(reason: unknown) => setError(messageOf(reason, "无法读取巡检记录。")),
		);
		const plansRead = listInspectionPlans().then(
			(items) => {
				setPlans(items);
				setLoaded(true);
			},
			(reason: unknown) => setError(messageOf(reason, "无法读取巡检计划。")),
		);
		// The wrapped reads never reject; awaiting them keeps save-reload callers in step.
		await Promise.allSettled([runsRead, plansRead]);
	}, [planFilter]);
	// biome-ignore lint/correctness/useExhaustiveDependencies: path 是有意依赖——从编辑器或 Run 页返回概览时必须重新拉取计划与记录。
	useEffect(() => {
		const timer = window.setTimeout(() => void load(), 0);
		return () => clearTimeout(timer);
	}, [load, path]);
	useEffect(() => {
		if (props.suspended || !runs.some((run) => inspectionActive(run.state)))
			return;
		const timer = window.setTimeout(() => void load(), 2000);
		return () => clearTimeout(timer);
	}, [load, props.suspended, runs]);
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
			props.navigate(runRoute(run.id));
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
				<h2 className="text-xl font-semibold">巡检计划</h2>
				<p className="text-sm text-muted-foreground">
					计划绑定接入与模板，按范围定期或手动采证；每次运行生成一份不可修改的报告。
				</p>
				<EntityList
					items={plans.map((item) => ({
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
						time: formatInspectionTime(item.updatedAt),
						plan: item,
					}))}
					columns={["title", "subtitle", "status", "time", "actions"]}
					onSelect={(item) =>
						props.navigate(
							`/inspections/plans/${encodeURIComponent(item.plan.planKey)}/edit`,
						)
					}
					renderActions={(item) => (
						<div className="flex gap-2">
							<Button
								variant="outline"
								size="sm"
								aria-label={`编辑 ${item.plan.displayName}`}
								disabled={props.suspended}
								onClick={() =>
									props.navigate(
										`/inspections/plans/${encodeURIComponent(item.plan.planKey)}/edit`,
									)
								}
							>
								编辑
							</Button>
							<Button
								variant="outline"
								size="sm"
								aria-label={`运行 ${item.plan.displayName}`}
								disabled={!item.plan.enabled || busy || props.suspended}
								onClick={() => void startPlan(item.plan.planKey)}
							>
								运行
							</Button>
						</div>
					)}
					loading={!loaded}
					loadingLabel="正在读取巡检计划"
					emptyTitle="还没有巡检计划"
					emptyDescription="使用标题栏的“新建计划”为接入创建第一个巡检计划。"
				/>
			</section>
			<section className="space-y-3">
				<div className="flex flex-wrap items-center justify-between gap-2">
					<h2 className="text-xl font-semibold">巡检记录</h2>
					<Select value={planFilter} onValueChange={setPlanFilter}>
						<SelectTrigger aria-label="按计划筛选" className="w-40">
							<SelectValue placeholder="全部计划" />
						</SelectTrigger>
						<SelectContent>
							<SelectGroup>
								<SelectItem value="all">全部计划</SelectItem>
								{plans.map((item) => (
									<SelectItem key={item.planKey} value={item.planKey}>
										{item.displayName}
									</SelectItem>
								))}
							</SelectGroup>
						</SelectContent>
					</Select>
				</div>
				<p className="text-sm text-muted-foreground">
					每次运行生成一份不可修改的报告；点击记录查看详情。
				</p>
				<EntityList
					items={runs.map((run) => ({
						id: run.id,
						title: `${planName(run.planKey)} · Run ${run.id}`,
						subtitle: [run.connectionName, run.triggerKind === "manual" ? "手动" : "定时"]
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
					selectedId={runId}
					onSelect={(item) => props.navigate(runRoute(item.run.id))}
					loading={!runsLoaded}
					loadingLabel="正在读取巡检记录"
					emptyTitle="没有巡检记录"
				/>
 <LoadMoreButton hasMore={!!nextCursor} loading={loadingMore} onLoadMore={() => {
 setLoadingMore(true); void listInspectionRuns({cursor:nextCursor,...(planFilter === "all" ? {} : {planKey:planFilter})}).then(page => {setRuns(current => [...current,...page.items]);setNextCursor(page.nextCursor);}).catch(reason => setError(messageOf(reason,"无法读取巡检记录。"))).finally(() => setLoadingMore(false));
 }} />
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
	const onOverview = !runId && !creatingPlan && !editPlanKey;
	const actions = onOverview ? (
		<>
			<Button
				variant="outline"
				disabled={props.suspended}
				onClick={openChooser}
			>
				运行巡检
			</Button>
			<Button
				disabled={props.suspended}
				onClick={() => props.navigate(newPlanRoute(hintPrefill))}
			>
				新建计划
			</Button>
		</>
	) : undefined;
	const runSummary = runId ? runs.find((run) => run.id === runId) : undefined;
	const runSheet = runId && (
		<DetailSheet
			open
			onClose={() => props.navigate("/inspections")}
			title={
				runSummary
					? `${planName(runSummary.planKey)} · Run ${runSummary.id}`
					: `Run ${runId}`
			}
			description="报告版本不可修改；关闭抽屉返回巡检概览。"
		>
			<div className="min-h-0 flex-1 overflow-y-auto">
				<div className="space-y-6 p-4 sm:p-6">
					<RunDetail
						runId={runId}
						props={props}
						onOpenRun={(id) => props.navigate(runRoute(id))}
					/>
				</div>
			</div>
		</DetailSheet>
	);
	return {
		title: creatingPlan
			? "新建巡检计划"
			: editPlanKey
				? "编辑巡检计划"
				: "巡检",
		crumbs: creatingPlan
			? [{ label: "巡检", to: "/inspections" }, { label: "新建巡检计划" }]
			: editPlanKey
				? [
						{ label: "巡检", to: "/inspections" },
						{ label: `编辑 ${planName(editPlanKey)}` },
					]
				: undefined,
		// 记录列表并入右侧概览页：第二栏只保留共享的运维模块导航，与告警列表一致。
		list: null,
		actions,
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
				) : (
					overview
				)}
			</>
		),
	};
}
