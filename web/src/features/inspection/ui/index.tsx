/* eslint-disable react-refresh/only-export-components -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

import { parseRoute } from "@/lib/parse-route";
import { ChevronDown, LoaderCircle } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { parse as parseYaml } from "yaml";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf } from "@/app/shared";
import { AiContent, EvidenceLinks } from "@/components/ai/AiContent";
import { RawPayload, StructuredData } from "@/components/ai/StructuredData";
import { parseStructured } from "@/components/ai/structured";
import {
	Accordion,
	AccordionContent,
	AccordionItem,
	AccordionTrigger,
} from "@/components/ui/accordion";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Collapsible,
	CollapsibleContent,
	CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import {
	appendFeedback,
	type FeedbackTimeline,
	type FeedbackValue,
	feedbackValueLabels,
	fetchFeedback,
} from "@/features/feedback/api";
import {
	cancelInspectionRun,
	createInspectionPlan,
	createInspectionRun,
	formatInspectionTime,
	getInspectionPlan,
	getInspectionReport,
	getInspectionRun,
	type InspectionPlan,
	type InspectionPlanInput,
	type InspectionPlanScope,
	type InspectionReportDetail,
	type InspectionRunDetail,
	type InspectionRunSummary,
	inspectionActive,
	inspectionGapReasonText,
	inspectionScheduleText,
	inspectionScopeKindText,
	inspectionScopeText,
	inspectionStateText,
	listInspectionPlans,
	listInspectionReports,
	listInspectionRuns,
	reanalyzeInspectionRun,
	rerunInspection,
	updateInspectionPlan,
} from "@/features/inspection/api";

const terminal = new Set([
	"Completed",
	"CompletedWithGaps",
	"Failed",
	"Cancelled",
	"Interrupted",
	"SkippedOverlap",
]);
const statusBadgeClass = (state: string) =>
	inspectionActive(state as never)
		? "border-warning/50 text-warning"
		: state.startsWith("Completed")
			? "border-success/50 text-success"
			: undefined;
/** The module owns its query state: /inspections/runs/:run and ?connectionName= hint. */
function parts(route: string) {
	const { pathname, searchParams } = parseRoute(route);
	return { path: pathname, query: searchParams };
}
function runRoute(runId: string) {
	return `/inspections/runs/${encodeURIComponent(runId)}`;
}

/** Renders immutable report Markdown without accepting raw HTML from model output. */
export function ReportBody({
	content,
	evidenceIds,
	openEvidence,
}: {
	content: string;
	evidenceIds: string[];
	openEvidence: (id: string) => void;
}) {
	return (
		<AiContent
			content={content}
			evidenceIds={evidenceIds}
			openEvidence={openEvidence}
		/>
	);
}

/** JSON-shaped content is detected mechanically; anything else keeps the Markdown reading path. */
function looksStructured(content: string): boolean {
	const trimmed = content.trim();
	return trimmed.startsWith("{") || trimmed.startsWith("[");
}

/**
 * 报告正文分区：完整 JSON 报告按真实字段结构化展示（未知字段原样保留，不生成摘要），
 * JSON 形状但解析失败的原文安全保留，普通 Markdown 与历史自由文本走共享阅读器。
 * 有界容器内长报告可滚动，原文始终折叠可查。
 */
function ReportPresentation({
	report,
	openEvidence,
}: {
	report: InspectionReportDetail;
	openEvidence: (id: string) => void;
}) {
	const structured = parseStructured(report.content);
	return (
		<div
			data-testid="report-content"
			className="max-h-[28rem] overflow-y-auto rounded-md border p-4"
		>
			{structured.ok ? (
				<div className="flex flex-col gap-3">
					<p className="text-xs text-muted-foreground">
						此报告为完整 JSON，以下按真实字段展示，不生成额外摘要。
					</p>
					<StructuredData value={structured.value} raw={report.content} />
				</div>
			) : looksStructured(report.content) ? (
				<div className="flex flex-col gap-3">
					<p className="text-xs text-muted-foreground">
						此报告不是完整 JSON，以下原样保留全部原文。
					</p>
					<RawPayload text={report.content} label="报告原文" />
				</div>
			) : (
				<ReportBody
					content={report.content}
					evidenceIds={report.evidenceIds}
					openEvidence={openEvidence}
				/>
			)}
		</div>
	);
}

/** 证据往返会按路由重建 Run 页面：生成详情展开状态按不可变报告 ID 保留，返回后阅读状态不丢。 */
const generationDetailsOpenByReport = new Map<string, boolean>();

/** 生成详情按需展开：本版本要求与运行事件不占据结论首屏。 */
function GenerationDetails({
	report,
	detail,
}: {
	report: InspectionReportDetail;
	detail: InspectionRunDetail;
}) {
	const [open, setOpen] = useState(
		() => generationDetailsOpenByReport.get(report.id) ?? false,
	);
	return (
		<Collapsible
			open={open}
			onOpenChange={(value) => {
				generationDetailsOpenByReport.set(report.id, value);
				setOpen(value);
			}}
			className="rounded-md border"
		>
			<CollapsibleTrigger asChild>
				<Button variant="ghost" className="group w-full justify-between">
					生成详情
					<ChevronDown
						className="transition-transform group-data-[state=open]:rotate-180"
						aria-hidden="true"
					/>
				</Button>
			</CollapsibleTrigger>
			<CollapsibleContent>
				<dl className="grid gap-2 border-t px-3 py-2 text-sm">
					<div>
						<dt className="text-muted-foreground">本次报告要求</dt>
						<dd className="whitespace-pre-wrap break-words">
							{report.reportInstructions || "未设置"}
						</dd>
					</div>
					<div>
						<dt className="text-muted-foreground">触发</dt>
						<dd>{detail.triggerKind}</dd>
					</div>
					<div>
						<dt className="text-muted-foreground">创建</dt>
						<dd>{formatInspectionTime(detail.createdAt)}</dd>
					</div>
					<div>
						<dt className="text-muted-foreground">分析</dt>
						<dd>{detail.latestAnalysis?.state ?? "尚未开始"}</dd>
					</div>
					<div>
						<dt className="text-muted-foreground">证据集摘要</dt>
						<dd className="break-all font-mono text-xs">
							{report.evidenceDigest}
						</dd>
					</div>
					<div>
						<dt className="text-muted-foreground">报告 ID</dt>
						<dd className="break-all font-mono text-xs">{report.id}</dd>
					</div>
				</dl>
			</CollapsibleContent>
		</Collapsible>
	);
}

function Feedback({
	reportId,
	suspended,
}: {
	reportId: string;
	suspended: boolean;
}) {
	const [timeline, setTimeline] = useState<FeedbackTimeline>();
	const [note, setNote] = useState("");
	const [error, setError] = useState("");
	const [submitting, setSubmitting] = useState(false);
	useEffect(() => {
		if (!suspended)
			void fetchFeedback({ type: "inspection_report", id: reportId })
				.then(setTimeline)
				.catch((e) => setError(messageOf(e, "无法读取反馈。")));
	}, [reportId, suspended]);
	async function record(value: FeedbackValue) {
		if (suspended || submitting) return;
		setError("");
		setSubmitting(true);
		try {
			await appendFeedback(
				{ type: "inspection_report", id: reportId },
				value,
				note,
			);
			setNote("");
			setTimeline(
				await fetchFeedback({ type: "inspection_report", id: reportId }),
			);
		} catch (e) {
			setError(messageOf(e, "无法记录反馈。"));
		} finally {
			setSubmitting(false);
		}
	}
	return (
		<section className="space-y-3 border-t pt-4">
			<h3 className="font-medium">实际反馈</h3>
			<div className="flex flex-wrap gap-2">
				{(Object.keys(feedbackValueLabels) as FeedbackValue[]).map((value) => (
					<Button
						key={value}
						size="sm"
						variant="outline"
						disabled={suspended || submitting}
						onClick={() => void record(value)}
					>
						{feedbackValueLabels[value]}
					</Button>
				))}
				<Textarea
					aria-label="反馈备注"
					className="min-h-16"
					value={note}
					onChange={(e) => setNote(e.target.value)}
					disabled={suspended}
					placeholder="可选备注"
				/>
				{error && (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}
				{timeline?.items.length ? (
					<p className="text-xs text-muted-foreground">
						最近：{feedbackValueLabels[timeline.items[0].value]} ·{" "}
						{formatInspectionTime(timeline.items[0].createdAt)}
					</p>
				) : null}
			</div>
		</section>
	);
}

export function RunDetail({
	runId,
	props,
	onBack,
	onOpenRun,
}: {
	runId: string;
	props: WorkspaceModuleProps;
	onBack: () => void;
	onOpenRun: (id: string) => void;
}) {
	const [detail, setDetail] = useState<InspectionRunDetail>();
	const [report, setReport] = useState<InspectionReportDetail>();
	// 报告读取未完成前不得断言“尚无报告”：加载态只显示读取状态。
	const [reportPending, setReportPending] = useState(true);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const alive = useRef(true);
	// 重分析弹框三态：默认“沿用 Run 冻结要求”（inherit，请求不含该字段）；
	// 打开“仅本次自定义”后输入框可编辑，提交按原文上送（清空=本次显式无要求）。
	// “加载当前计划要求”是显式动作，只在自定义模式下可用。
	const [analyzeOpen, setAnalyzeOpen] = useState(false);
	const [instructions, setInstructions] = useState("");
	const [customInstructions, setCustomInstructions] = useState(false);
	const [loadNote, setLoadNote] = useState("");
	const load = useCallback(async () => {
		try {
			const current = await getInspectionRun(runId);
			if (!alive.current) return;
			setDetail(current);
			if (current.reportCount) {
				const reports = await listInspectionReports(runId);
				if (!alive.current) return;
				if (reports[0])
					setReport(await getInspectionReport(runId, reports[0].version));
				else setReport(undefined);
			} else setReport(undefined);
			if (alive.current) setReportPending(false);
		} catch (e) {
			if (alive.current) {
				setError(messageOf(e, "无法读取巡检 Run。"));
				setReportPending(false);
			}
		}
	}, [runId]);
	useEffect(() => {
		alive.current = true;
		setReport(undefined);
		setReportPending(true);
		void load();
		return () => {
			alive.current = false;
		};
	}, [load]);
	useEffect(() => {
		if (
			props.suspended ||
			!detail ||
			(terminal.has(detail.state) && !detail.analysisActive)
		)
			return;
		const timer = window.setTimeout(() => void load(), 2000);
		return () => clearTimeout(timer);
	}, [detail, load, props.suspended]);
	// Reanalysis reuses this Run's frozen Evidence; recollection always starts a distinct Run.
	async function action(kind: "cancel" | "analyze" | "rerun") {
		if (!detail || props.suspended) return;
		setBusy(true);
		setError("");
		try {
			if (kind === "cancel")
				await cancelInspectionRun(detail.id, detail.rowVersion);
			else if (kind === "analyze") {
				await reanalyzeInspectionRun(
					detail.id,
					customInstructions ? instructions : undefined,
				);
				setAnalyzeOpen(false);
			} else {
				onOpenRun((await rerunInspection(detail.id)).id);
				return;
			}
			await load();
		} catch (e) {
			setError(messageOf(e, "操作未完成。"));
			await load();
		} finally {
			setBusy(false);
		}
	}
	/** 显式把计划当前定义的初始报告要求加载进输入框（不静默改写）。 */
	async function loadCurrentPlanInstructions() {
		if (!detail) return;
		setLoadNote("");
		try {
			const plan = await getInspectionPlan(detail.planKey);
			setInstructions(plan.reportInstructions ?? "");
			setLoadNote(
				plan.reportInstructions
					? "已加载计划当前的报告要求。"
					: "计划当前没有报告要求。",
			);
		} catch (e) {
			setLoadNote(messageOf(e, "无法读取巡检计划。"));
		}
	}
	function openAnalyzeDialog() {
		setInstructions(detail?.frozenConfig?.reportInstructions ?? "");
		setCustomInstructions(false);
		setLoadNote("");
		setAnalyzeOpen(true);
	}
	if (!detail)
		return (
			<div className="p-6">
				<DetailSkeleton
					label="正在读取 Run"
					rows={["title", "line", "line", "line"]}
				/>
			</div>
		);
	const canAnalyze =
		detail.state === "Completed" || detail.state === "CompletedWithGaps";
	// Cancellation only applies while the Run itself is active; a terminal Run keeps
	// the button visible during an active analysis but it can no longer be fired.
	const runActive = inspectionActive(detail.state);
	const frozen = detail.frozenConfig;
	const frozenRows: Array<[string, string | undefined]> = frozen
		? [
				["名称", frozen.displayName ?? undefined],
				["检查说明", frozen.checkDescription ?? undefined],
				["单位", frozen.metricUnit ?? undefined],
				["初始报告要求", frozen.reportInstructions ?? undefined],
			]
		: [];
	const analyzeDialog = (
		<Dialog open={analyzeOpen} onOpenChange={setAnalyzeOpen}>
			<DialogContent className="sm:max-w-lg">
				<DialogHeader>
					<DialogTitle>重新分析现有证据</DialogTitle>
					<DialogDescription>
						复用本 Run 冻结的 Evidence 生成下一个报告版本。报告要求默认为本 Run
						冻结值；编辑仅对本次分析生效，不改变旧证据与旧报告。
					</DialogDescription>
				</DialogHeader>
				<FieldGroup>
					<Field orientation="horizontal">
						<Switch
							id="analyze-custom"
							checked={customInstructions}
							disabled={busy || props.suspended}
							onCheckedChange={(checked) => {
								setCustomInstructions(checked === true);
								setLoadNote("");
							}}
						/>
						<FieldLabel htmlFor="analyze-custom">
							仅本次自定义报告要求
						</FieldLabel>
						<FieldDescription>
							关闭时沿用本 Run
							冻结的初始报告要求；开启后可编辑，清空表示本次分析无附加要求（仅本次生效）。
						</FieldDescription>
					</Field>
					<Field>
						<FieldLabel htmlFor="analyze-instructions">本次报告要求</FieldLabel>
						<Textarea
							id="analyze-instructions"
							aria-label="本次报告要求"
							className="min-h-24"
							value={instructions}
							disabled={busy || props.suspended || !customInstructions}
							onChange={(event) => {
								setInstructions(event.target.value);
								setLoadNote("");
							}}
							placeholder="留空表示本次分析无附加报告要求"
						/>
						<FieldDescription>
							{customInstructions
								? "提交时按原文作为仅本次覆盖；清空即本次无要求。"
								: "当前沿用本 Run 冻结的初始报告要求。"}
						</FieldDescription>
					</Field>
					<div className="flex items-center justify-between gap-2">
						<Button
							type="button"
							variant="outline"
							size="sm"
							disabled={busy || props.suspended || !customInstructions}
							onClick={() => void loadCurrentPlanInstructions()}
						>
							加载当前计划要求
						</Button>
						{loadNote && (
							<span className="text-xs text-muted-foreground">{loadNote}</span>
						)}
					</div>
				</FieldGroup>
				<DialogFooter>
					<Button
						variant="outline"
						onClick={() => setAnalyzeOpen(false)}
						disabled={busy}
					>
						取消
					</Button>
					<Button
						onClick={() => void action("analyze")}
						disabled={busy || props.suspended}
					>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								提交中…
							</>
						) : (
							"开始分析"
						)}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
	return (
		<div className="space-y-6">
			{analyzeDialog}
			<Button variant="ghost" onClick={onBack}>
				返回巡检记录
			</Button>
			<header className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h2 className="text-xl font-semibold">
						{detail.planKey} · Run {detail.id}
					</h2>
					<p className="text-sm text-muted-foreground">
						来源接入 {detail.connectionName ?? "—"} · 采证冻结于{" "}
						{formatInspectionTime(detail.evidenceAt)}，报告版本不可修改。
					</p>
				</div>
				<Badge variant="outline" className={statusBadgeClass(detail.state)}>
					{inspectionStateText[detail.state]}
				</Badge>
			</header>
			{error && (
				<Alert variant="destructive">
					<AlertTitle>操作失败</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<div className="flex flex-wrap gap-2">
				{runActive || detail.analysisActive ? (
					<Button
						variant="outline"
						disabled={busy || props.suspended || !runActive}
						title={runActive ? undefined : "Run 已到终态，取消不再可用"}
						onClick={() => void action("cancel")}
					>
						取消
					</Button>
				) : null}
				{canAnalyze ? (
					<Button
						variant="outline"
						disabled={busy || props.suspended || detail.analysisActive}
						onClick={openAnalyzeDialog}
					>
						重新分析现有证据
					</Button>
				) : null}
				{terminal.has(detail.state) ? (
					<Button
						variant="outline"
						disabled={busy || props.suspended}
						onClick={() => void action("rerun")}
					>
						重新采证（新 Run）
					</Button>
				) : null}
			</div>
			{/* 结论优先的连续单页：报告正文在前；检查项、缺口与冻结配置保留为可展开的资料区，不拆 Tab。 */}
			{report ? (
				<section className="space-y-4">
					<div className="flex flex-wrap items-center justify-between gap-2">
						<div>
							<h3 className="font-semibold">报告 v{report.version}</h3>
							<p className="text-xs text-muted-foreground">
								模型 {report.modelId} · {formatInspectionTime(report.createdAt)}
							</p>
						</div>
						<Button
							variant="outline"
							onClick={() => props.navigate("/knowledge")}
						>
							在知识库中检索
						</Button>
					</div>
					<ReportPresentation
						report={report}
						openEvidence={props.openEvidence}
					/>
					<GenerationDetails report={report} detail={detail} />
					{report.evidenceIds.length > 0 && (
						<section className="flex flex-col gap-2">
							<h3 className="text-sm font-medium">证据引用</h3>
							<EvidenceLinks
								ids={report.evidenceIds}
								openEvidence={props.openEvidence}
							/>
						</section>
					)}
					<Feedback reportId={report.id} suspended={props.suspended} />
				</section>
			) : reportPending ? (
				<div className="space-y-3 rounded-md border p-4">
					<DetailSkeleton
						label="正在读取报告"
						rows={["title", "line", "line"]}
					/>
				</div>
			) : detail.analysisActive ? (
				<div
					className="space-y-3 rounded-md border p-4"
					role="status"
					aria-label="分析正在生成报告"
				>
					<p className="text-sm text-muted-foreground">分析正在生成报告。</p>
					<DetailSkeleton
						label="分析正在生成报告"
						rows={["line", "line", "line"]}
					/>
				</div>
			) : (
				<Alert>
					<AlertDescription>该 Run 尚无报告版本。</AlertDescription>
				</Alert>
			)}
			<Separator />
			<section className="space-y-2" aria-labelledby="inspection-facts-title">
				<h2 id="inspection-facts-title" className="text-sm font-medium">
					检查与运行资料
				</h2>
				<p className="text-xs text-muted-foreground">
					检查项、缺口与冻结配置按 Run 原样保留，可展开核查。
				</p>
				<Accordion type="multiple" defaultValue={["checks", "frozen"]}>
					<AccordionItem value="checks">
						<AccordionTrigger>检查项与缺口</AccordionTrigger>
						<AccordionContent>
							<Table>
								<TableHeader>
									<TableRow>
										<TableHead>检查</TableHead>
										<TableHead>状态</TableHead>
										<TableHead>证据 / 原因</TableHead>
									</TableRow>
								</TableHeader>
								<TableBody>
									{detail.checks.map((check) => (
										<TableRow key={check.checkKey}>
											<TableCell>{check.checkKey}</TableCell>
											<TableCell>{check.status}</TableCell>
											<TableCell>
												{check.status === "ok" ? (
													<Button
														variant="link"
														className="h-auto p-0"
														onClick={() => props.openEvidence(check.evidenceId)}
													>
														#{check.evidenceId}
													</Button>
												) : check.status === "cancelling" ? (
													<span className="flex items-center gap-1.5 text-muted-foreground">
														<LoaderCircle
															className="size-3.5 animate-spin"
															aria-hidden="true"
														/>
														停止中…
													</span>
												) : (
													(inspectionGapReasonText[check.gapReason] ??
													check.gapReason)
												)}
											</TableCell>
										</TableRow>
									))}
								</TableBody>
							</Table>
						</AccordionContent>
					</AccordionItem>
					{frozen && (
						<AccordionItem value="frozen">
							<AccordionTrigger>冻结的分析配置（Run 创建时）</AccordionTrigger>
							<AccordionContent>
								<dl className="grid gap-2 text-sm sm:grid-cols-2">
									{frozenRows.map(([label, value]) => (
										<div key={label}>
											<dt className="text-muted-foreground">{label}</dt>
											<dd>{value || "—"}</dd>
										</div>
									))}
								</dl>
								<p className="text-xs text-muted-foreground">
									计划后续修改不改写本 Run；重新分析默认沿用这里的初始报告要求。
								</p>
							</AccordionContent>
						</AccordionItem>
					)}
				</Accordion>
			</section>
		</div>
	);
}

/** Form projection of one plan; params stay as YAML text until save parses them. */
interface PlanFormState {
	planKey: string;
	displayName: string;
	enabled: boolean;
	connectionName: string;
	pluginId: string;
	templateId: string;
	templateVersion: string;
	paramsText: string;
	scopeKind: InspectionPlanScope["kind"];
	businessViewKey: string;
	objects: Array<{ objectType: string; identityKey: string }>;
	cron: string;
	timezone: string;
	/** 可选分析语义：Run 创建时冻结，计划修改不改写已存在 Run。 */
	checkDescription: string;
	metricUnit: string;
	reportInstructions: string;
}

/** The operator's own timezone is the least surprising default for scheduled plans. */
function defaultTimezone(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
	} catch {
		return "UTC";
	}
}

function emptyPlanForm(connectionName: string): PlanFormState {
	return {
		planKey: "",
		displayName: "",
		enabled: true,
		connectionName,
		pluginId: "",
		templateId: "",
		templateVersion: "",
		paramsText: "",
		scopeKind: "integration",
		businessViewKey: "",
		objects: [],
		cron: "",
		timezone: defaultTimezone(),
		checkDescription: "",
		metricUnit: "",
		reportInstructions: "",
	};
}

function planFormOf(plan: InspectionPlan): PlanFormState {
	return {
		planKey: plan.planKey,
		displayName: plan.displayName,
		enabled: plan.enabled,
		connectionName: plan.connectionName,
		pluginId: plan.pluginId,
		templateId: plan.templateId,
		templateVersion: plan.templateVersion ?? "",
		paramsText: Object.keys(plan.params).length
			? paramsToYamlText(plan.params)
			: "",
		scopeKind: plan.scope.kind,
		businessViewKey:
			plan.scope.kind === "businessView" ? plan.scope.businessViewKey : "",
		objects:
			plan.scope.kind === "objects"
				? plan.scope.objects.map((object) => ({ ...object }))
				: [],
		cron: plan.cron ?? "",
		timezone: plan.timezone,
		checkDescription: plan.checkDescription ?? "",
		metricUnit: plan.metricUnit ?? "",
		reportInstructions: plan.reportInstructions ?? "",
	};
}

/** Round-trips the stored params object into editable YAML text; the server owns the authority. */
function paramsToYamlText(params: Record<string, unknown>): string {
	return Object.entries(params)
		// String values are JSON-quoted too, so colons, newlines, or quotes in a
		// value cannot forge extra YAML keys; the server re-validates regardless.
		.map(([key, value]) => `${key}: ${JSON.stringify(value)}`)
		.join("\n");
}

/** Mechanical client-side parse only; the server re-validates the whole plan. */
function parseParamsText(
	text: string,
):
	| { ok: true; params: Record<string, unknown> }
	| { ok: false; message: string } {
	const trimmed = text.trim();
	if (!trimmed) return { ok: true, params: {} };
	try {
		const parsed: unknown = parseYaml(trimmed);
		if (parsed === null || parsed === undefined)
			return { ok: true, params: {} };
		if (typeof parsed !== "object" || Array.isArray(parsed))
			return { ok: false, message: "采集参数必须是 YAML 键值映射（键值对）。" };
		return { ok: true, params: parsed as Record<string, unknown> };
	} catch (reason) {
		return {
			ok: false,
			message: `采集参数 YAML 无法解析：${messageOf(reason, "请检查缩进与语法。")}`,
		};
	}
}

function scopeOfForm(
	form: PlanFormState,
): { ok: true; scope: InspectionPlanScope } | { ok: false; message: string } {
	if (form.scopeKind === "businessView") {
		const key = form.businessViewKey.trim();
		return key
			? { ok: true, scope: { kind: "businessView", businessViewKey: key } }
			: { ok: false, message: "业务视图 Key 必须填写。" };
	}
	if (form.scopeKind === "objects") {
		const objects = form.objects
			.map((object) => ({
				objectType: object.objectType.trim(),
				identityKey: object.identityKey.trim(),
			}))
			.filter((object) => object.objectType || object.identityKey);
		if (
			objects.some((object) => !object.objectType || !object.identityKey) ||
			!objects.length
		)
			return {
				ok: false,
				message: "每个对象都需要对象类型和身份键，至少一行。",
			};
		return { ok: true, scope: { kind: "objects", objects } };
	}
	return { ok: true, scope: { kind: "integration" } };
}

/**
 * Deep-link prefill for a fresh plan, derived from query hints. A business-view
 * hint pins the scope so the editor never starts from the wider integration scope.
 */
export interface PlanEditorPrefill {
	connectionName?: string;
	scopeKind?: InspectionPlanScope["kind"];
	businessViewKey?: string;
}

/** Prefill implied by the current URL hints, if any. */
function editorPrefillFromHints(
	businessViewHint: string,
	connectionHint: string,
): PlanEditorPrefill | undefined {
	if (businessViewHint)
		return {
			connectionName: connectionHint,
			scopeKind: "businessView",
			businessViewKey: businessViewHint,
		};
	return connectionHint ? { connectionName: connectionHint } : undefined;
}

/**
 * Create/edit dialog for one standalone plan. The key is fixed after creation;
 * updates carry the plan's rowVersion for optimistic concurrency.
 */
function PlanEditorDialog({
	open,
	plan,
	prefill,
	suspended,
	onOpenChange,
	onSaved,
}: {
	open: boolean;
	plan?: InspectionPlan;
	prefill?: PlanEditorPrefill;
	suspended: boolean;
	onOpenChange: (open: boolean) => void;
	onSaved: () => Promise<void>;
}) {
	const [form, setForm] = useState<PlanFormState>(() => emptyPlanForm(""));
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	// Re-derive the whole form per opening so editing always starts from authority.
	useEffect(() => {
		if (!open) return;
		setError("");
		if (plan) {
			setForm(planFormOf(plan));
			return;
		}
		const fresh = emptyPlanForm(prefill?.connectionName ?? "");
		setForm({
			...fresh,
			...(prefill?.scopeKind ? { scopeKind: prefill.scopeKind } : {}),
			...(prefill?.businessViewKey
				? { businessViewKey: prefill.businessViewKey }
				: {}),
			objects:
				prefill?.scopeKind === "objects"
					? [{ objectType: "", identityKey: "" }]
					: [],
		});
	}, [open, plan, prefill]);
	function update(patch: Partial<PlanFormState>) {
		setForm((current) => ({ ...current, ...patch }));
	}
	async function save() {
		if (suspended) return;
		if (
			!form.planKey.trim() ||
			!form.displayName.trim() ||
			!form.connectionName.trim() ||
			!form.pluginId.trim() ||
			!form.templateId.trim()
		) {
			setError("计划 Key、显示名称、接入连接名、插件 ID 和模板 ID 必须填写。");
			return;
		}
		const scope = scopeOfForm(form);
		if (!scope.ok) {
			setError(scope.message);
			return;
		}
		const params = parseParamsText(form.paramsText);
		if (!params.ok) {
			setError(params.message);
			return;
		}
		const payload: InspectionPlanInput = {
			planKey: form.planKey.trim(),
			displayName: form.displayName.trim(),
			enabled: form.enabled,
			connectionName: form.connectionName.trim(),
			pluginId: form.pluginId.trim(),
			templateId: form.templateId.trim(),
			templateVersion: form.templateVersion.trim() || null,
			params: params.params,
			scope: scope.scope,
			checkDescription: form.checkDescription.trim() || null,
			metricUnit: form.metricUnit.trim() || null,
			reportInstructions: form.reportInstructions.trim() || null,
			cron: form.cron.trim() || null,
			timezone: form.timezone.trim() || defaultTimezone(),
		};
		setBusy(true);
		setError("");
		try {
			if (plan)
				await updateInspectionPlan(plan.planKey, {
					...payload,
					expectedRowVersion: plan.rowVersion,
				});
			else await createInspectionPlan(payload);
			onOpenChange(false);
			await onSaved();
		} catch (reason) {
			setError(
				messageOf(reason, plan ? "无法更新巡检计划。" : "无法创建巡检计划。"),
			);
		} finally {
			setBusy(false);
		}
	}
	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-xl">
				<DialogHeader>
					<DialogTitle>{plan ? "编辑巡检计划" : "新建巡检计划"}</DialogTitle>
					<DialogDescription>
						计划绑定一个接入与模板；范围决定采证对象，可覆盖整个接入、一个业务视图或显式对象列表。
					</DialogDescription>
				</DialogHeader>
				{error && (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}
				<FieldGroup>
					<div className="grid gap-3 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="plan-key">计划 Key</FieldLabel>
							<Input
								id="plan-key"
								value={form.planKey}
								disabled={Boolean(plan) || busy || suspended}
								onChange={(event) => update({ planKey: event.target.value })}
								placeholder="如 prom-up"
							/>
							<FieldDescription>创建后不可修改。</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-display-name">显示名称</FieldLabel>
							<Input
								id="plan-display-name"
								value={form.displayName}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ displayName: event.target.value })
								}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-connection">接入连接名</FieldLabel>
							<Input
								id="plan-connection"
								value={form.connectionName}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ connectionName: event.target.value })
								}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-plugin">插件 ID</FieldLabel>
							<Input
								id="plan-plugin"
								value={form.pluginId}
								disabled={busy || suspended}
								onChange={(event) => update({ pluginId: event.target.value })}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-template">模板 ID</FieldLabel>
							<Input
								id="plan-template"
								value={form.templateId}
								disabled={busy || suspended}
								onChange={(event) => update({ templateId: event.target.value })}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-template-version">
								模板版本（可选）
							</FieldLabel>
							<Input
								id="plan-template-version"
								value={form.templateVersion}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ templateVersion: event.target.value })
								}
							/>
						</Field>
					</div>
					<Field>
						<FieldLabel htmlFor="plan-scope">巡检范围</FieldLabel>
						<Select
							value={form.scopeKind}
							onValueChange={(value) =>
								update({
									scopeKind: value as PlanFormState["scopeKind"],
									objects:
										value === "objects" && !form.objects.length
											? [{ objectType: "", identityKey: "" }]
											: form.objects,
								})
							}
							disabled={busy || suspended}
						>
							<SelectTrigger id="plan-scope" aria-label="巡检范围">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									{(
										Object.keys(inspectionScopeKindText) as Array<
											keyof typeof inspectionScopeKindText
										>
									).map((kind) => (
										<SelectItem key={kind} value={kind}>
											{inspectionScopeKindText[kind]}
										</SelectItem>
									))}
								</SelectGroup>
							</SelectContent>
						</Select>
						<FieldDescription>
							Run 创建时按当前范围展开并冻结目标，执行中不扩大。
						</FieldDescription>
					</Field>
					{form.scopeKind === "businessView" && (
						<Field>
							<FieldLabel htmlFor="plan-business-view">业务视图 Key</FieldLabel>
							<Input
								id="plan-business-view"
								value={form.businessViewKey}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ businessViewKey: event.target.value })
								}
							/>
						</Field>
					)}
					{form.scopeKind === "objects" && (
						<FieldGroup aria-label="对象列表">
							{form.objects.map((object, index) => (
								<div
									key={index}
									className="grid gap-2 md:grid-cols-[1fr_1fr_auto]"
								>
									<Input
										aria-label="对象类型"
										placeholder="对象类型，如 kubernetes_pod"
										value={object.objectType}
										disabled={busy || suspended}
										onChange={(event) =>
											update({
												objects: form.objects.map((item, itemIndex) =>
													itemIndex === index
														? { ...item, objectType: event.target.value }
														: item,
												),
											})
										}
									/>
									<Input
										aria-label="对象身份键"
										placeholder="身份键，如 demo/api"
										value={object.identityKey}
										disabled={busy || suspended}
										onChange={(event) =>
											update({
												objects: form.objects.map((item, itemIndex) =>
													itemIndex === index
														? { ...item, identityKey: event.target.value }
														: item,
												),
											})
										}
									/>
									<Button
										type="button"
										variant="outline"
										aria-label={`移除对象 ${index + 1}`}
										disabled={busy || suspended}
										onClick={() =>
											update({
												objects: form.objects.filter(
													(_, itemIndex) => itemIndex !== index,
												),
											})
										}
									>
										移除
									</Button>
								</div>
							))}
							<Button
								type="button"
								variant="outline"
								disabled={busy || suspended}
								onClick={() =>
									update({
										objects: [
											...form.objects,
											{ objectType: "", identityKey: "" },
										],
									})
								}
							>
								添加对象
							</Button>
						</FieldGroup>
					)}
					<div className="grid gap-3 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="plan-cron">调度 Cron（可选）</FieldLabel>
							<Input
								id="plan-cron"
								value={form.cron}
								disabled={busy || suspended}
								onChange={(event) => update({ cron: event.target.value })}
								placeholder="*/5 * * * *"
							/>
							<FieldDescription>
								标准五字段 cron；留空表示仅人工运行。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-timezone">时区</FieldLabel>
							<Input
								id="plan-timezone"
								value={form.timezone}
								disabled={busy || suspended}
								onChange={(event) => update({ timezone: event.target.value })}
							/>
						</Field>
					</div>
					<Field>
						<FieldLabel htmlFor="plan-params">采集参数（YAML）</FieldLabel>
						<Textarea
							id="plan-params"
							aria-label="采集参数（YAML）"
							className="min-h-24 font-mono text-xs"
							value={form.paramsText}
							disabled={busy || suspended}
							onChange={(event) => update({ paramsText: event.target.value })}
							placeholder={"expression: up"}
						/>
						<FieldDescription>
							模板参数以 YAML 键值对填写；服务端会按模板 schema 复核。
						</FieldDescription>
					</Field>
					<Field>
						<FieldLabel htmlFor="plan-check-description">
							检查说明（可选）
						</FieldLabel>
						<Textarea
							id="plan-check-description"
							aria-label="检查说明（可选）"
							className="min-h-16"
							value={form.checkDescription}
							disabled={busy || suspended}
							onChange={(event) =>
								update({ checkDescription: event.target.value })
							}
							placeholder="这项检查在观测什么、如何解读结果"
						/>
						<FieldDescription>
							随 Run 冻结并进入分析上下文；最长 2000 字。
						</FieldDescription>
					</Field>
					<div className="grid gap-3 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="plan-metric-unit">
								指标单位（可选）
							</FieldLabel>
							<Input
								id="plan-metric-unit"
								value={form.metricUnit}
								disabled={busy || suspended}
								onChange={(event) => update({ metricUnit: event.target.value })}
								placeholder="如 %、ms、个"
							/>
							<FieldDescription>
								结果数值的语义单位；最长 100 字。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-report-instructions">
								初始报告要求（可选）
							</FieldLabel>
							<Input
								id="plan-report-instructions"
								value={form.reportInstructions}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ reportInstructions: event.target.value })
								}
								placeholder="报告的默认侧重点"
							/>
							<FieldDescription>
								每次 Run 冻结后作为分析默认要求；重分析可仅本次覆盖；最长 4000
								字。
							</FieldDescription>
						</Field>
					</div>
					<Field orientation="horizontal">
						<FieldLabel htmlFor="plan-enabled">启用计划</FieldLabel>
						<Switch
							id="plan-enabled"
							checked={form.enabled}
							disabled={busy || suspended}
							onCheckedChange={(checked) =>
								update({ enabled: checked === true })
							}
						/>
					</Field>
				</FieldGroup>
				<DialogFooter>
					<Button
						variant="outline"
						onClick={() => onOpenChange(false)}
						disabled={busy}
					>
						取消
					</Button>
					<Button onClick={() => void save()} disabled={busy || suspended}>
						{busy ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								保存中…
							</>
						) : (
							"保存计划"
						)}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
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
	const runId = path.startsWith("/inspections/runs/")
		? decodeURIComponent(path.slice("/inspections/runs/".length)) || undefined
		: undefined;
	const connectionHint = query.get("connectionName") ?? "";
	const businessViewHint = query.get("businessViewKey") ?? "";
	const [plans, setPlans] = useState<InspectionPlan[]>([]);
	const [runs, setRuns] = useState<InspectionRunSummary[]>([]);
	const [planFilter, setPlanFilter] = useState("all");
	const [loaded, setLoaded] = useState(false);
	const [runsLoaded, setRunsLoaded] = useState(false);
	const [chooserOpen, setChooserOpen] = useState(false);
	const [chooserPlan, setChooserPlan] = useState("");
	const [editor, setEditor] = useState<{
		open: boolean;
		plan?: InspectionPlan;
		prefill?: PlanEditorPrefill;
	}>({ open: false });
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
				setRuns(page.items);
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
	useEffect(() => {
		const timer = window.setTimeout(() => void load(), 0);
		return () => clearTimeout(timer);
	}, [load]);
	useEffect(() => {
		if (props.suspended || !runs.some((run) => inspectionActive(run.state)))
			return;
		const timer = window.setTimeout(() => void load(), 2000);
		return () => clearTimeout(timer);
	}, [load, props.suspended, runs]);
	// Deep links preselect once. A business-view hint only ever matches business-view
	// scoped plans — a same-connection integration plan would widen the range, so
	// without a match the prefilled editor opens instead of any run.
	useEffect(() => {
		if (
			hintApplied ||
			!loaded ||
			runId ||
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
			} else
				setEditor({
					open: true,
					prefill: editorPrefillFromHints(businessViewHint, connectionHint),
				});
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
	}, [businessViewHint, connectionHint, hintApplied, loaded, plans, runId]);
	function openChooser() {
		setChooserPlan((current) =>
			current && plans.some((item) => item.planKey === current && item.enabled)
				? current
				: preferredPlan(plans, businessViewHint, connectionHint),
		);
		setChooserOpen(true);
	}
	async function start() {
		if (!chooserPlan || props.suspended) return;
		setBusy(true);
		setError("");
		try {
			const run = await createInspectionRun(chooserPlan);
			setChooserOpen(false);
			await load();
			props.navigate(runRoute(run.id));
		} catch (reason) {
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
	const newPlanPrefill = editorPrefillFromHints(
		businessViewHint,
		connectionHint,
	);
	const list = (
		<div className="flex h-full flex-col gap-3 p-3">
			<div className="font-medium">巡检记录</div>
			<Select value={planFilter} onValueChange={setPlanFilter}>
				<SelectTrigger aria-label="按计划筛选">
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
			<ScrollArea className="min-h-0 flex-1">
				{runs.map((run) => (
					<Button
						key={run.id}
						variant={runId === run.id ? "secondary" : "ghost"}
						className="mb-1 h-auto w-full justify-start whitespace-normal text-left"
						onClick={() => props.navigate(runRoute(run.id))}
					>
						<span className="block">
							<strong className="block">
								{planName(run.planKey)} · Run {run.id}
							</strong>
							<small className="block text-muted-foreground">
								{[
									run.connectionName,
									run.triggerKind === "manual" ? "手动" : "定时",
									formatInspectionTime(run.createdAt),
								]
									.filter(Boolean)
									.join(" · ")}
							</small>
							<Badge variant="outline" className={statusBadgeClass(run.state)}>
								{inspectionStateText[run.state]}
							</Badge>
						</span>
					</Button>
				))}
				{runsLoaded && !runs.length && (
					<p className="p-2 text-sm text-muted-foreground">没有巡检记录</p>
				)}
			</ScrollArea>
		</div>
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
							onClick={() => setEditor({ open: true, prefill: newPlanPrefill })}
						>
							为该接入新建计划
						</Button>
					</Alert>
				))}
			<section className="space-y-3">
				<h2 className="text-xl font-semibold">巡检计划</h2>
				<p className="text-sm text-muted-foreground">
					选择已发布系统的历史
					Run，或先创建计划并立即采证。计划绑定接入与模板，范围可为整个接入、业务视图或显式对象。
				</p>
				{!loaded ? (
					<DetailSkeleton label="正在读取巡检计划" rows={["line", "line", "line"]} />
				) : plans.length ? (
					<Table>
						<TableHeader>
							<TableRow>
								<TableHead>计划</TableHead>
								<TableHead>接入</TableHead>
								<TableHead>范围</TableHead>
								<TableHead>调度</TableHead>
								<TableHead>状态</TableHead>
								<TableHead>更新时间</TableHead>
								<TableHead>
									<span className="sr-only">操作</span>
								</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{plans.map((item) => (
								<TableRow key={item.planKey}>
									<TableCell>
										<span className="block font-medium">
											{item.displayName}
										</span>
										<small className="block text-muted-foreground">
											{item.planKey}
										</small>
									</TableCell>
									<TableCell>{item.connectionName}</TableCell>
									<TableCell>{inspectionScopeText(item.scope)}</TableCell>
									<TableCell>{inspectionScheduleText(item)}</TableCell>
									<TableCell>
										<Badge variant={item.enabled ? "secondary" : "outline"}>
											{item.enabled ? "已启用" : "已停用"}
										</Badge>
									</TableCell>
									<TableCell>{formatInspectionTime(item.updatedAt)}</TableCell>
									<TableCell>
										<Button
											variant="outline"
											size="sm"
											aria-label={`编辑 ${item.displayName}`}
											disabled={props.suspended}
											onClick={() => setEditor({ open: true, plan: item })}
										>
											编辑
										</Button>
									</TableCell>
								</TableRow>
							))}
						</TableBody>
					</Table>
				) : (
					<Empty>
						<EmptyHeader>
							<EmptyTitle>还没有巡检计划</EmptyTitle>
							<EmptyDescription>
								使用标题栏的“新建计划”为接入创建第一个巡检计划。
							</EmptyDescription>
						</EmptyHeader>
					</Empty>
				)}
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
				onStart={() => void start()}
			/>
		</Dialog>
	);
	const editorDialog = (
		<Dialog
			open={editor.open}
			onOpenChange={(open) => setEditor((current) => ({ ...current, open }))}
		>
			<PlanEditorDialog
				open={editor.open}
				plan={editor.plan}
				prefill={editor.plan ? undefined : editor.prefill}
				suspended={props.suspended}
				onOpenChange={(open) => setEditor((current) => ({ ...current, open }))}
				onSaved={load}
			/>
		</Dialog>
	);
	const actions = (
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
				onClick={() => setEditor({ open: true, prefill: newPlanPrefill })}
			>
				新建计划
			</Button>
		</>
	);
	return {
		title: runId ? "巡检 Run" : "巡检",
		list,
		actions,
		content: (
			<>
				{chooserDialog}
				{editorDialog}
				{runId ? (
					<RunDetail
						runId={runId}
						props={props}
						onBack={() => props.navigate("/inspections")}
						onOpenRun={(id) => props.navigate(runRoute(id))}
					/>
				) : (
					overview
				)}
			</>
		),
	};
}
