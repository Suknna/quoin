import {
	AlertTriangle,
	BookPlus,
	CheckCircle2,
	ChevronDown,
	LoaderCircle,
} from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import { messageOf, notify } from "@/app/shared";
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
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { PropertyList } from "@/components/workbench/PropertyList";
import { FeedbackPanel } from "@/features/feedback/ui";
import { api as knowledgeApi } from "@/features/knowledge/api";
import { organizeIntoKnowledge } from "@/features/knowledge/organize";
import {
	cancelInspectionRun,
	formatInspectionTime,
	getInspectionPlan,
	getInspectionReport,
	getInspectionRun,
	type InspectionReportDetail,
	type InspectionRunDetail,
	inspectionActive,
	inspectionGapReasonText,
	inspectionStateText,
	listInspectionReports,
	reanalyzeInspectionRun,
	rerunInspection,
	statusBadgeClass,
} from "@/features/inspection/api";

const terminal = new Set([
	"Completed",
	"CompletedWithGaps",
	"Failed",
	"Cancelled",
	"Interrupted",
	"SkippedOverlap",
]);

const analysisStateText: Record<string, string> = {
	Queued: "排队中",
	Assigned: "分配中",
	Running: "分析中",
	Cancelling: "停止中",
	Succeeded: "已完成",
	Failed: "失败",
	Cancelled: "已取消",
	Interrupted: "已中断",
};

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
				<div className="border-t px-3 py-2">
					<PropertyList
						entries={[
							{
								label: "本次报告要求",
								value: report.reportInstructions || "未设置",
							},
							{ label: "触发", value: detail.triggerKind },
							{ label: "创建", value: formatInspectionTime(detail.createdAt) },
							{
								label: "分析",
								value: detail.latestAnalysis?.state ?? "尚未开始",
							},
							{
								label: "证据集摘要",
								value: (
									<span className="break-all font-mono text-xs">
										{report.evidenceDigest}
									</span>
								),
							},
							{
								label: "报告 ID",
								value: <span className="break-all font-mono">{report.id}</span>,
							},
						]}
					/>
				</div>
			</CollapsibleContent>
		</Collapsible>
	);
}

/** 检查结论行：只由真实 Run 状态与检查项统计推导，不猜测健康度。 */
function conclusionOf(detail: InspectionRunDetail): string {
	const checks = detail.checks;
	const ok = checks.filter((check) => check.status === "ok").length;
	const gapCount = checks.length - ok;
	switch (detail.state) {
		case "Queued":
			return "排队等待采证";
		case "Running":
			return "采证进行中";
		case "Failed":
			return "采证失败，未生成报告";
		case "Cancelled":
			return "已在采证阶段取消";
		case "Interrupted":
			return "采证被中断";
		case "SkippedOverlap":
			return "上次执行未结束时跳过本次调度";
		default:
			if (!checks.length) return "采证完成，等待报告";
			return gapCount
				? `${ok} 项通过 · ${gapCount} 项有缺口`
				: `${ok} 项检查全部通过`;
	}
}

function CheckIcon({ status }: { status: string }) {
	if (status === "ok")
		return (
			<CheckCircle2
				className="size-4 shrink-0 text-success"
				aria-hidden="true"
			/>
		);
	if (status === "cancelling")
		return (
			<LoaderCircle
				className="size-4 shrink-0 animate-spin text-muted-foreground"
				aria-hidden="true"
			/>
		);
	return (
		<AlertTriangle
			className="size-4 shrink-0 text-warning"
			aria-hidden="true"
		/>
	);
}

/**
 * 结果摘要卡：首屏只回答“这次巡检结果如何”——状态、检查结论、缺口告警与逐项清单。
 * 全部来自 Run 的真实字段；报告正文与生成资料在其后，需要时才展开。
 */
function ResultSummary({
	detail,
	report,
	reportPending,
	openEvidence,
}: {
	detail: InspectionRunDetail;
	report?: InspectionReportDetail;
	reportPending: boolean;
	openEvidence: (id: string) => void;
}) {
	const gapChecks = detail.checks.filter(
		(check) => check.status === "error" || check.status === "gap",
	);
	const analysis = detail.latestAnalysis;
	return (
		<section
			aria-label="巡检结果摘要"
			className="space-y-3 rounded-lg border p-4"
		>
			<div className="flex flex-wrap items-center gap-3">
				<Badge
					variant="outline"
					className={`px-2.5 py-1 text-sm ${statusBadgeClass(detail.state) ?? ""}`}
				>
					{inspectionStateText[detail.state]}
				</Badge>
				<p className="text-lg font-semibold">{conclusionOf(detail)}</p>
			</div>
			<PropertyList
				layout="inline"
				className="text-muted-foreground"
				entries={[
					{
						label: "采证冻结于",
						value: formatInspectionTime(detail.evidenceAt),
					},
					{
						label: "触发",
						value: detail.triggerKind === "manual" ? "手动" : "定时",
					},
					{
						label: "报告",
						value: report
							? `v${report.version} · ${formatInspectionTime(report.createdAt)}`
							: reportPending
								? "读取中…"
								: "尚无",
					},
					{
						label: "分析",
						value: analysis
							? `${analysisStateText[analysis.state] ?? analysis.state}${
									analysis.terminationReason
										? `（${analysis.terminationReason}）`
										: ""
								}`
							: "尚未开始",
					},
				]}
			/>
			{gapChecks.length > 0 && (
				<Alert variant="destructive">
					<AlertTitle>
						{gapChecks.length} 项检查未通过，报告可能不完整
					</AlertTitle>
					<AlertDescription>
						<ul className="list-disc space-y-0.5 pl-4">
							{gapChecks.map((check) => (
								<li key={check.checkKey}>
									{check.checkKey}：
									{check.status === "gap" || check.status === "error"
										? (inspectionGapReasonText[check.gapReason] ??
											check.gapReason)
										: check.status}
								</li>
							))}
						</ul>
					</AlertDescription>
				</Alert>
			)}
			{detail.checks.length > 0 && (
				<ul className="flex flex-col gap-1.5">
					{detail.checks.map((check) => (
						<li
							key={check.checkKey}
							className="flex items-center gap-2 text-sm"
						>
							<CheckIcon status={check.status} />
							<span className="font-medium">{check.checkKey}</span>
							<span className="text-muted-foreground">
								{check.status === "ok" ? (
									<Button
										variant="link"
										className="h-auto p-0 align-baseline"
										onClick={() => openEvidence(check.evidenceId)}
									>
										#{check.evidenceId}
									</Button>
								) : check.status === "cancelling" ? (
									<span className="flex items-center gap-1.5">
										<LoaderCircle
											className="size-3.5 animate-spin"
											aria-hidden="true"
										/>
										停止中…
									</span>
								) : (
									(inspectionGapReasonText[check.gapReason] ?? check.gapReason)
								)}
							</span>
						</li>
					))}
				</ul>
			)}
		</section>
	);
}


export function RunDetail({
	runId,
	props,
	onOpenRun,
}: {
	runId: string;
	props: WorkspaceModuleProps;
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
			if (kind === "cancel") {
				await cancelInspectionRun(detail.id, detail.rowVersion);
				notify.success("已取消巡检");
			} else if (kind === "analyze") {
				await reanalyzeInspectionRun(
					detail.id,
					customInstructions ? instructions : undefined,
				);
				setAnalyzeOpen(false);
				notify.success("已开始分析");
			} else {
				const nextRun = await rerunInspection(detail.id);
				notify.success("已开始重新采证");
				onOpenRun(nextRun.id);
				return;
			}
			await load();
		} catch (e) {
			notify.error(e, "操作未完成。");
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
			{/* 抽屉头部（DetailSheet）负责标题；这里保留来源与版本约束说明。 */}
			<p className="text-sm text-muted-foreground">
				{detail.planKey} · 来源接入 {detail.connectionName ?? "—"} ·
				报告版本不可修改。
			</p>
			{error && (
				<Alert variant="destructive">
					<AlertTitle>操作失败</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{/* 结论优先：摘要卡只回答“结果如何”，报告正文与资料区依次在后。 */}
			<ResultSummary
				detail={detail}
				report={report}
				reportPending={reportPending}
				openEvidence={props.openEvidence}
			/>
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
							disabled={props.suspended}
							onClick={() =>
								void organizeIntoKnowledge(
									() =>
										knowledgeApi.createReportCandidate(
											runId,
											report.version,
										),
									props.navigate,
								)
							}
						>
							<BookPlus data-icon="inline-start" />
							整理为知识
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
					<Separator />
					<FeedbackPanel
						target={{ type: "inspection_report", id: report.id }}
						suspended={props.suspended}
					/>
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
			{frozen && (
				<section className="space-y-2" aria-labelledby="inspection-facts-title">
					<h2 id="inspection-facts-title" className="text-sm font-medium">
						Run 冻结配置
					</h2>
					<p className="text-xs text-muted-foreground">
						Run 创建时从计划复制，之后的计划修改不改写本 Run。
					</p>
					<Accordion type="multiple" defaultValue={["frozen"]}>
						<AccordionItem value="frozen">
							<AccordionTrigger>冻结的分析配置（Run 创建时）</AccordionTrigger>
							<AccordionContent>
								<PropertyList
									layout="grid-2"
									entries={frozenRows.map(([label, value]) => ({
										label,
										value: value || "—",
									}))}
								/>
								<p className="text-xs text-muted-foreground">
									重新分析默认沿用这里的初始报告要求。
								</p>
							</AccordionContent>
						</AccordionItem>
					</Accordion>
				</section>
			)}
		</div>
	);
}
