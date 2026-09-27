import { useCursorPages } from '@/hooks/use-cursor-pages';
import {
	CircleCheck,
	CircleHelp,
	CircleX,
	Funnel,
	ListTree,
	LoaderCircle,
	ShieldX,
} from "lucide-react";
import { type ReactNode, useEffect, useState } from "react";
import { newClientCommandId, WorkbenchApiError } from "@/api/workbench";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
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
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Popover,
	PopoverContent,
	PopoverTrigger,
} from "@/components/ui/popover";
import { Separator } from "@/components/ui/separator";
import { TableCell, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { DataTable } from "@/components/workbench/DataTable";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { CursorPagination } from "@/components/workbench/CursorPagination";
import { PropertyList } from "@/components/workbench/PropertyList";
import {
	type AuditEvent,
	type AuditEventFilter,
	type AuditOutcome,
	type AuditSettings,
	type AuditSettingsPreview,
	actionLabel,
	actorLabels,
	domainRefTypeLabel,
	formatTimestamp,
	getAuditSettings,
	listAuditEvents,
	localInputToTimestamp,
	MIN_RETENTION_MONTHS,
	outcomeLabels,
	phaseLabel,
	previewAuditSettings,
	updateAuditSettings,
} from "@/features/audit/api";

interface FilterDraft {
	correlationId: string;
	action: string;
	actorType: string;
	outcome: string;
	since: string;
	until: string;
}

const emptyDraft: FilterDraft = {
	correlationId: "",
	action: "",
	actorType: "",
	outcome: "",
	since: "",
	until: "",
};

/** Blank inputs mean "no criterion"; the server must never see empty-string filters. */
function draftToFilter(draft: FilterDraft): AuditEventFilter {
	return {
		correlationId: draft.correlationId.trim() || undefined,
		actorType: (draft.actorType || undefined) as AuditEventFilter["actorType"],
		action: draft.action.trim() || undefined,
		outcome: (draft.outcome || undefined) as AuditEventFilter["outcome"],
		since: localInputToTimestamp(draft.since),
		until: localInputToTimestamp(draft.until),
	};
}

/**
 * Excel-style column filter: a funnel button on the table header that opens
 * the column's criterion, highlighted while its criterion is applied.
 */
function ColumnFilter({
	label,
	active,
	children,
}: {
	label: string;
	active: boolean;
	children: (close: () => void) => ReactNode;
}) {
	const [open, setOpen] = useState(false);
	return (
		<Popover open={open} onOpenChange={setOpen}>
			<PopoverTrigger asChild>
				<button
					type="button"
					aria-label={label}
					className={`inline-flex size-5 items-center justify-center rounded hover:bg-accent ${
						active ? "text-primary" : "text-muted-foreground"
					}`}
				>
					<Funnel className="size-3.5" aria-hidden="true" />
				</button>
			</PopoverTrigger>
			<PopoverContent align="start" className="w-64">
				{children(() => setOpen(false))}
			</PopoverContent>
		</Popover>
	);
}

/** Single-value option list for enum columns (结果 / 主体类型): one click applies, mirroring Excel's value checklist. */
function OptionFilter({
	options,
	current,
	onPick,
}: {
	options: readonly { value: string; label: string }[];
	current: string;
	onPick: (value: string) => void;
}) {
	return (
		<div className="flex flex-col gap-1">
			{options.map((option) => (
				<Button
					key={option.value || "all"}
					type="button"
					size="sm"
					variant={current === option.value ? "secondary" : "ghost"}
					className="justify-start"
					onClick={() => onPick(option.value)}
				>
					{option.label}
				</Button>
			))}
		</div>
	);
}

/** Free-text criterion popover (操作 / 关联 ID); Enter applies, 清除 resets the column. */
function TextFilter({
	id,
	label,
	placeholder,
	initial,
	onApply,
}: {
	id: string;
	label: string;
	placeholder?: string;
	initial: string;
	onApply: (value: string) => void;
}) {
	const [value, setValue] = useState(initial);
	return (
		<form
			className="flex flex-col gap-2"
			onSubmit={(event) => {
				event.preventDefault();
				onApply(value);
			}}
		>
			<Field>
				<FieldLabel htmlFor={id}>{label}</FieldLabel>
				<Input
					id={id}
					placeholder={placeholder}
					value={value}
					onChange={(event) => setValue(event.target.value)}
				/>
			</Field>
			<div className="flex gap-2">
				<Button type="submit" size="sm">
					应用
				</Button>
				<Button
					type="button"
					size="sm"
					variant="ghost"
					onClick={() => onApply("")}
				>
					清除
				</Button>
			</div>
		</form>
	);
}

/** 时间列范围筛选：开始/结束两个 datetime-local，应用或清除后关闭。 */
function TimeFilter({
	initialSince,
	initialUntil,
	onApply,
}: {
	initialSince: string;
	initialUntil: string;
	onApply: (since: string, until: string) => void;
}) {
	const [since, setSince] = useState(initialSince);
	const [until, setUntil] = useState(initialUntil);
	return (
		<form
			className="flex flex-col gap-2"
			onSubmit={(event) => {
				event.preventDefault();
				onApply(since, until);
			}}
		>
			<Field>
				<FieldLabel htmlFor="audit-filter-since">开始时间</FieldLabel>
				<Input
					id="audit-filter-since"
					type="datetime-local"
					value={since}
					onChange={(event) => setSince(event.target.value)}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="audit-filter-until">结束时间</FieldLabel>
				<Input
					id="audit-filter-until"
					type="datetime-local"
					value={until}
					onChange={(event) => setUntil(event.target.value)}
				/>
			</Field>
			<div className="flex gap-2">
				<Button type="submit" size="sm">
					应用
				</Button>
				<Button
					type="button"
					size="sm"
					variant="ghost"
					onClick={() => onApply("", "")}
				>
					清除
				</Button>
			</div>
		</form>
	);
}

const outcomeVariant = (outcome: AuditOutcome) =>
	outcome === "success"
		? "default"
		: outcome === "failure"
			? "destructive"
			: outcome === "rejected"
				? "outline"
				: "secondary";

const outcomeIcons = {
	success: CircleCheck,
	failure: CircleX,
	rejected: ShieldX,
	unknown: CircleHelp,
} as const;

function OutcomeBadge({ outcome }: { outcome: AuditOutcome }) {
	const Icon = outcomeIcons[outcome];
	return (
		<Badge variant={outcomeVariant(outcome)}>
			<Icon data-icon="inline-start" aria-hidden="true" />
			{outcomeLabels[outcome]}
		</Badge>
	);
}

/**
 * Outcome counts over the currently loaded window. The audit API is a
 * paged list without server-side aggregation, so the strip is honestly
 * labelled as covering the loaded rows rather than the whole ledger.
 */
const outcomeTones: Record<AuditOutcome, string> = {
	success: "text-emerald-600 dark:text-emerald-400",
	failure: "text-destructive",
	rejected: "text-amber-600 dark:text-amber-400",
	unknown: "text-muted-foreground",
};

function OutcomeStats({ items }: { items: AuditEvent[] }) {
	const counts: Record<AuditOutcome, number> = {
		success: 0,
		failure: 0,
		rejected: 0,
		unknown: 0,
	};
	for (const item of items) counts[item.outcome] += 1;
	return (
		<div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
			<span className="text-muted-foreground">
				当前页 {items.length.toLocaleString("zh-CN")} 条
			</span>
			{(Object.keys(outcomeLabels) as AuditOutcome[]).map((outcome) => {
				const Icon = outcomeIcons[outcome];
				return (
					<span key={outcome} className="inline-flex items-center gap-1">
						<Icon className={`size-4 ${outcomeTones[outcome]}`} aria-hidden="true" />
						<span className="font-medium tabular-nums">
							{counts[outcome].toLocaleString("zh-CN")}
						</span>
						<span className="text-xs text-muted-foreground">
							{outcomeLabels[outcome]}
						</span>
					</span>
				);
			})}
		</div>
	);
}

/** 统一审计入口（docs/audit-design.md §6）：仅管理员，按时间/主体/动作/结果/关联筛选。 */
export function AuditPage({ suspended }: { suspended: boolean }) {
	const [applied, setApplied] = useState<AuditEventFilter>({});
	const [draft, setDraft] = useState<FilterDraft>(emptyDraft);
	const [settings, setSettings] = useState<AuditSettings>();
	const [settingsError, setSettingsError] = useState("");
	const [viewing, setViewing] = useState<{
		event: AuditEvent;
		tab: "detail" | "correlation";
	}>();
	const list = useCursorPages<AuditEvent>(
		(cursor) => listAuditEvents(applied, cursor),
		{
			suspended,
			resetKey: JSON.stringify(applied),
			fallbackError: "暂时无法完成操作，请重试。",
		},
	);
	const { items, error, loading } = list;

	useEffect(() => {
		let cancelled = false;
		getAuditSettings()
			.then((value) => {
				if (!cancelled) setSettings(value);
			})
			.catch((reason: unknown) => {
				if (!cancelled)
					setSettingsError(messageOf(reason, "暂时无法完成操作，请重试。"));
			});
		return () => {
			cancelled = true;
		};
	}, []);

	/** Column filters commit immediately: patch the draft and apply in one step. */
	function applyDraftPatch(patch: Partial<FilterDraft>) {
		const next = { ...draft, ...patch };
		setDraft(next);
		setApplied(draftToFilter(next));
	}

	return (
		<section className="flex flex-col gap-4">
			<div className="flex flex-col gap-1">
				<h2 className="text-xl font-semibold">审计日志</h2>
				<p className="text-sm text-muted-foreground">
					谁在何时访问或变更了什么资源、结果如何。
				</p>
			</div>
			{settingsError && (
				<Alert variant="destructive">
					<AlertDescription>保留设置读取失败：{settingsError}</AlertDescription>
				</Alert>
			)}
			{settings && (
				<RetentionBar
					settings={settings}
					suspended={suspended}
					onSaved={setSettings}
				/>
			)}
			{!loading && !error && <OutcomeStats items={items} />}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{!(error && items.length === 0) && (
				<>
					<DataTable
						columns={[
							{
								label: (
									<span className="flex items-center gap-1">
										结果
										<ColumnFilter label="筛选结果" active={Boolean(applied.outcome)}>
											{(close) => (
												<OptionFilter
													current={applied.outcome ?? ""}
													options={[
														{ value: "", label: "全部" },
														{ value: "success", label: outcomeLabels.success },
														{ value: "failure", label: outcomeLabels.failure },
														{ value: "rejected", label: outcomeLabels.rejected },
														{ value: "unknown", label: outcomeLabels.unknown },
													]}
													onPick={(value) => {
														applyDraftPatch({ outcome: value });
														close();
													}}
												/>
											)}
										</ColumnFilter>
									</span>
								),
								className: "w-24",
							},
							{
								label: (
									<span className="flex items-center gap-1">
										时间
										<ColumnFilter
											label="筛选时间"
											active={Boolean(applied.since || applied.until)}
										>
											{(close) => (
												<TimeFilter
													initialSince={draft.since}
													initialUntil={draft.until}
													onApply={(since, until) => {
														applyDraftPatch({ since, until });
														close();
													}}
												/>
											)}
										</ColumnFilter>
									</span>
								),
								className: "w-40",
							},
							{
								label: (
									<span className="flex items-center gap-1">
										操作
										<ColumnFilter label="筛选操作" active={Boolean(applied.action)}>
											{(close) => (
												<TextFilter
													id="audit-filter-action"
													label="操作"
													placeholder="如 user.create"
													initial={draft.action}
													onApply={(value) => {
														applyDraftPatch({ action: value });
														close();
													}}
												/>
											)}
										</ColumnFilter>
									</span>
								),
							},
							{
								label: (
									<span className="flex items-center gap-1">
										主体 → 对象
										<ColumnFilter
											label="筛选主体"
											active={Boolean(applied.actorType)}
										>
											{(close) => (
												<OptionFilter
													current={applied.actorType ?? ""}
													options={[
														{ value: "", label: "全部" },
														{ value: "user", label: actorLabels.user },
														{ value: "service", label: actorLabels.service },
														{ value: "system", label: actorLabels.system },
													]}
													onPick={(value) => {
														applyDraftPatch({ actorType: value });
														close();
													}}
												/>
											)}
										</ColumnFilter>
									</span>
								),
								className: "w-56",
							},
							{ label: "阶段", className: "w-20" },
							{
								label: (
									<span className="flex items-center gap-1">
										关联
										<ColumnFilter
											label="筛选关联"
											active={Boolean(applied.correlationId)}
										>
											{(close) => (
												<TextFilter
													id="audit-filter-correlation"
													label="关联 ID"
													initial={draft.correlationId}
													onApply={(value) => {
														applyDraftPatch({ correlationId: value });
														close();
													}}
												/>
											)}
										</ColumnFilter>
									</span>
								),
								className: "w-16",
							},
						]}
						loading={loading}
						loadingLabel="正在读取审计事件"
						emptyTitle="没有匹配的审计事件"
						emptyDescription="当前筛选没有可显示的记录；请调整时间范围或其他条件。"
					>
						{items.map((event) => {
							const label = actionLabel(event.action);
							return (
								// Row accents answer the audit question at a glance:
								// failures tint red, rejections amber, and local
								// emergency logins keep the strong amber accent
								// ("who entered through the IdP-outage channel",
								// ADR-0010).
								<TableRow
									key={event.id}
									className={
										event.action.includes("login.local")
											? "cursor-pointer bg-amber-500/10"
											: event.outcome === "failure"
												? "cursor-pointer bg-destructive/5"
												: event.outcome === "rejected"
													? "cursor-pointer bg-amber-500/5"
													: "cursor-pointer"
									}
									onClick={() => setViewing({ event, tab: "detail" })}
								>
									<TableCell>
										<OutcomeBadge outcome={event.outcome} />
									</TableCell>
									<TableCell className="text-xs tabular-nums text-muted-foreground">
										{formatTimestamp(event.createdAt)}
									</TableCell>
									<TableCell>
										<div className="font-medium">
											{label}
											{event.action.includes("login.local") && (
												<span className="ml-1.5 rounded bg-amber-500/20 px-1.5 py-0.5 text-xs text-amber-700 dark:text-amber-400">
													应急
												</span>
											)}
										</div>
										{label !== event.action && (
											<div className="font-mono text-xs text-muted-foreground">
												{event.action}
											</div>
										)}
									</TableCell>
									<TableCell>
										{actorLabels[event.actorType]} · {event.actorId}
										{event.domainRefType && (
											<span className="text-muted-foreground">
												{" "}→ {domainRefTypeLabel(event.domainRefType)} ·{" "}
												{event.domainRefId ?? "—"}
											</span>
										)}
									</TableCell>
									<TableCell className="text-muted-foreground">
										{phaseLabel(event.phase)}
									</TableCell>
									<TableCell>
										{event.correlationId && (
											<Button
												size="icon-sm"
												variant="ghost"
												aria-label="查看关联"
												onClick={(click) => {
													click.stopPropagation();
													setViewing({ event, tab: "correlation" });
												}}
											>
												<ListTree aria-hidden="true" />
											</Button>
										)}
									</TableCell>
								</TableRow>
							);
						})}
				</DataTable>
				<CursorPagination
					page={list.page}
					hasPrev={list.hasPrev}
					hasNext={list.hasNext}
					loading={list.navigating}
					onPrev={list.goPrev}
					onNext={list.goNext}
				/>
			</>
			)}
			{viewing && (
				<EventDetails
					key={viewing.event.id}
					view={viewing}
					onClose={() => setViewing(undefined)}
				/>
			)}
		</section>
	);
}

const chronological = (a: AuditEvent, b: AuditEvent) =>
	a.createdAt.localeCompare(b.createdAt);

const monoValue = (value: string) => (
	<span className="font-mono text-xs">{value}</span>
);

/**
 * 单条事件的完整事实抽屉。详情与关联链路同在一个抽屉内以 Tab 切换（与告警
 * 详情的 DetailSheet + line Tabs 同一形态），不再跳出到独立对话框。
 */
function EventDetails({
	view,
	onClose,
}: {
	view: { event: AuditEvent; tab: "detail" | "correlation" };
	onClose: () => void;
}) {
	const { event } = view;
	const [tab, setTab] = useState(view.tab);
	const detailContent = (
		<div className="flex flex-col gap-6">
			<section className="flex flex-col gap-3">
				<h3 className="text-sm font-medium">概要</h3>
				<PropertyList
					layout="grid-2"
					entries={[
						{ label: "结果", value: <OutcomeBadge outcome={event.outcome} /> },
						{ label: "阶段", value: phaseLabel(event.phase) },
						{ label: "时间", value: formatTimestamp(event.createdAt) },
						{
							label: "主体",
							value: `${actorLabels[event.actorType]} · ${event.actorId}`,
						},
						{
							label: "访问对象",
							value: event.domainRefType
								? `${domainRefTypeLabel(event.domainRefType)} · ${event.domainRefId ?? "—"}`
								: "—",
						},
					]}
				/>
			</section>
			<Separator />
			<section className="flex flex-col gap-3">
				<h3 className="text-sm font-medium">标识</h3>
				<PropertyList
					layout="grid-2"
					entries={[
						{ label: "事件 ID", value: monoValue(event.id) },
						{
							label: "关联 ID",
							value: event.correlationId
								? monoValue(event.correlationId)
								: "—（历史无关联）",
						},
						{
							label: "请求 ID",
							value: event.requestId ? monoValue(event.requestId) : "—",
						},
						{
							label: "幂等命令 ID",
							value: event.clientCommandId
								? monoValue(event.clientCommandId)
								: "—",
						},
						{
							label: "任务 ID",
							value: event.taskId ? monoValue(event.taskId) : "—",
						},
						{
							label: "尝试 ID",
							value: event.attemptId ? monoValue(event.attemptId) : "—",
						},
					]}
				/>
			</section>
			{event.action.includes("login.local") && (
				<Alert>
					<AlertDescription>
						该事件来自本地应急登录通道（IdP 不可用时的例外入口），请结合上下
						文确认其必要性。
					</AlertDescription>
				</Alert>
			)}
		</div>
	);
	return (
		<DetailSheet
			open
			onClose={onClose}
			title={actionLabel(event.action)}
			description={<span className="font-mono text-xs">{event.action}</span>}
			size="narrow"
		>
			{event.correlationId ? (
				<Tabs
					value={tab}
					onValueChange={(value) =>
						setTab(value as "detail" | "correlation")
					}
					className="min-h-0 flex-1 gap-0"
				>
					<div className="shrink-0 border-b px-4">
						<TabsList variant="line" className="h-11">
							<TabsTrigger value="detail">详情</TabsTrigger>
							<TabsTrigger value="correlation">关联链路</TabsTrigger>
						</TabsList>
					</div>
					<TabsContent value="detail" className="min-h-0 overflow-y-auto p-4">
						{detailContent}
					</TabsContent>
					<TabsContent
						value="correlation"
						className="min-h-0 overflow-y-auto p-4"
					>
						<CorrelationTimeline correlationId={event.correlationId} />
					</TabsContent>
				</Tabs>
			) : (
				<div className="flex-1 overflow-y-auto p-4">{detailContent}</div>
			)}
		</DetailSheet>
	);
}

/** 关联链路复用列表接口按 correlationId 过滤，按时间正序呈现发起、准入、执行尝试与结果。 */
function CorrelationTimeline({ correlationId }: { correlationId: string }) {
	const list = useCursorPages<AuditEvent>(
		(cursor) =>
			listAuditEvents({ correlationId }, cursor).then((page) => ({
				items: [...(page.items ?? [])].sort(chronological),
				nextCursor: page.nextCursor,
			})),
		{
			resetKey: correlationId,
			fallbackError: "暂时无法完成操作，请重试。",
		},
	);
	const { items: events, error, loading } = list;

	return (
		<section className="flex flex-col gap-3" aria-label="关联链路">
			<p className="text-xs text-muted-foreground">
				关联 <span className="font-mono">{correlationId}</span> · 按时间正序
			</p>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{loading ? (
				<DetailSkeleton
					label="正在读取关联事件"
					rows={["line", "line", "line"]}
				/>
			) : events.length === 0 ? (
				<Empty>
					<EmptyHeader>
						<EmptyTitle>关联事件不可用</EmptyTitle>
						<EmptyDescription>
							该关联的事件可能已过保留期被清理，或当前账户无权查看。
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			) : (
				<ol className="flex flex-col">
					{events.map((event, index) => (
						<li key={event.id} className="flex flex-col">
							{index > 0 && <Separator />}
							<div className="flex flex-col gap-1 py-3">
								<div className="flex flex-wrap items-center gap-2">
									<Badge variant="outline">{phaseLabel(event.phase)}</Badge>
									<span className="text-sm font-medium">
										{actionLabel(event.action)}
									</span>
									<OutcomeBadge outcome={event.outcome} />
								</div>
								{actionLabel(event.action) !== event.action && (
									<div className="font-mono text-xs text-muted-foreground">
										{event.action}
									</div>
								)}
								<div className="text-xs text-muted-foreground">
									{formatTimestamp(event.createdAt)} ·{" "}
									{actorLabels[event.actorType]} {event.actorId}
									{event.domainRefType
										? ` · ${domainRefTypeLabel(event.domainRefType)} ${event.domainRefId ?? ""}`
										: ""}
								</div>
								{(event.clientCommandId ||
									event.taskId ||
									event.attemptId) && (
									<div className="text-xs text-muted-foreground">
										{[
											event.clientCommandId &&
												`命令 ${event.clientCommandId}`,
											event.taskId && `任务 ${event.taskId}`,
											event.attemptId && `尝试 ${event.attemptId}`,
										]
											.filter(Boolean)
											.join(" · ")}
									</div>
								)}
							</div>
						</li>
					))}
				</ol>
			)}
			<CursorPagination
				page={list.page}
				hasPrev={list.hasPrev}
				hasNext={list.hasNext}
				loading={list.navigating}
				onPrev={list.goPrev}
				onNext={list.goNext}
			/>
		</section>
	);
}


function RetentionBar({
	settings,
	suspended,
	onSaved,
}: {
	settings: AuditSettings;
	suspended: boolean;
	onSaved: (settings: AuditSettings) => void;
}) {
	const [editing, setEditing] = useState(false);
	const cleanup = settings.cleanup;
	return (
		<>
			<div className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border px-3 py-2 text-sm">
				<span className="font-medium">
					保留 {settings.retentionMonths} 个月
				</span>
				<span className="text-xs text-muted-foreground">
					保留期内不可修改或删除 · 最短{" "}
					{settings.minRetentionMonths || MIN_RETENTION_MONTHS} 个月
					{cleanup?.lastRunAt &&
						` · 上次清理 ${formatTimestamp(cleanup.lastRunAt)}`}
					{cleanup?.lastSuccessDeletedCount != null &&
						`（已清 ${cleanup.lastSuccessDeletedCount.toLocaleString("zh-CN")} 条）`}
				</span>
				<Button
					className="ml-auto"
					variant="outline"
					size="sm"
					disabled={suspended}
					onClick={() => setEditing(true)}
				>
					修改保留期
				</Button>
			</div>
			{cleanup?.lastFailureAt && (
				<Alert variant="destructive">
					<AlertDescription>
						上次清理失败（{formatTimestamp(cleanup.lastFailureAt)}），错误码：
						{cleanup.lastErrorCode ?? "unknown"}
						。数据已保留；在恢复之前请勿假定更早事件的链路完整。
					</AlertDescription>
				</Alert>
			)}
			{editing && (
				<RetentionDialog
					settings={settings}
					suspended={suspended}
					onSaved={onSaved}
					onClose={() => setEditing(false)}
				/>
			)}
		</>
	);
}

/** 缩短保留期必须先预览影响并确认（docs/audit-design.md §7）；服务端仍是最终裁决。 */
function RetentionDialog({
	settings,
	suspended,
	onSaved,
	onClose,
}: {
	settings: AuditSettings;
	suspended: boolean;
	onSaved: (settings: AuditSettings) => void;
	onClose: () => void;
}) {
	const minMonths = settings.minRetentionMonths || MIN_RETENTION_MONTHS;
	const [months, setMonths] = useState(String(settings.retentionMonths));
	const [preview, setPreview] = useState<AuditSettingsPreview>();
	const [error, setError] = useState("");
	const [previewing, setPreviewing] = useState(false);
	const [saving, setSaving] = useState(false);
	const parsed = Number.parseInt(months, 10);
	const valid = Number.isInteger(parsed) && parsed >= minMonths;
	const previewMatches = preview?.retentionMonths === parsed;

	async function runPreview() {
		if (!valid) return;
		setPreview(undefined);
		setError("");
		setPreviewing(true);
		try {
			setPreview(await previewAuditSettings(parsed));
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setPreviewing(false);
		}
	}

	async function save() {
		if (!previewMatches) return;
		setError("");
		setSaving(true);
		try {
			const updated = await updateAuditSettings(
				parsed,
				settings.rowVersion,
				newClientCommandId(),
			);
			notify.success("已保存");
			onSaved(updated);
			onClose();
		} catch (reason) {
			setError(
				reason instanceof WorkbenchApiError && reason.status === 409
					? "审计设置已被其他管理员修改，请刷新后重试。"
					: messageOf(reason, "暂时无法完成操作，请重试。"),
			);
		} finally {
			setSaving(false);
		}
	}

	return (
		<Dialog
			open
			onOpenChange={(open) => {
				if (!open && !saving && !previewing) onClose();
			}}
		>
			<DialogContent>
				<DialogHeader>
					<DialogTitle>修改审计保留期</DialogTitle>
					<DialogDescription>
						先预览影响，确认后由后台执行清理。缩短保留期无法恢复已删除的事件。
					</DialogDescription>
				</DialogHeader>
				<div className="flex flex-col gap-4">
					<Field>
						<FieldLabel htmlFor="audit-retention-months">
							新保留期（自然月）
						</FieldLabel>
						<Input
							id="audit-retention-months"
							type="number"
							min={minMonths}
							step={1}
							value={months}
							disabled={suspended || previewing || saving}
							onChange={(event) => {
								setMonths(event.target.value);
								setPreview(undefined);
							}}
						/>
						<FieldDescription>
							不低于 {minMonths} 个自然月。当前：{settings.retentionMonths}{" "}
							个自然月。
						</FieldDescription>
					</Field>
					{error && (
						<Alert variant="destructive">
							<AlertDescription>{error}</AlertDescription>
						</Alert>
					)}
					{preview && previewMatches && (
						<div className="flex flex-col gap-2 rounded border p-3 text-sm">
							{preview.shortening ? (
								<Alert variant="destructive">
									<AlertDescription>
										正在缩短保留期：更早的事件将变为可清理状态，确认后由后台分批清理且不可恢复。
									</AlertDescription>
								</Alert>
							) : (
								<Alert>
									<AlertDescription>
										延长保留期不影响已按原策略清理的事件。
									</AlertDescription>
								</Alert>
							)}
							<div>
								清理截止点（此前记录的事件到期）：
								{formatTimestamp(preview.cutoffAt)}
							</div>
							<div>
								预计到期事件：
								{preview.estimatedExpirableEvents.toLocaleString("zh-CN")} 条
							</div>
							<div>
								预计受影响关联：
								{preview.estimatedExpirableCorrelations.toLocaleString("zh-CN")}{" "}
								个
							</div>
						</div>
					)}
				</div>
				<DialogFooter>
					<Button
						variant="outline"
						disabled={!valid || previewing || saving}
						onClick={() => void runPreview()}
					>
						{previewing ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								预览中…
							</>
						) : (
							"预览影响"
						)}
					</Button>
					<Button
						disabled={!previewMatches || previewing || saving}
						onClick={() => void save()}
					>
						{saving ? (
							<>
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
									aria-hidden="true"
								/>
								保存中…
							</>
						) : (
							"确认调整"
						)}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}
