/* eslint-disable react-refresh/only-export-components -- Alert views colocate their route lifecycle. */

import { cn } from "cn";
import {
	Activity,
	AlertTriangle,
	Bot,
	CheckCircle2,
	ChevronDown,
	CircleDot,
	Clock3,
	FileText,
	RefreshCw,
} from "lucide-react";
import {
	Fragment,
	useCallback,
	useEffect,
	useMemo,
	useRef,
	useState,
} from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { useAlertEventStream } from "@/app/realtime/hooks";
import { AiContent, EvidenceLinks } from "@/components/ai/AiContent";
import { EntityList, type EntityListItem } from "@/components/EntityList";
import { FeatureUnderConstruction } from "@/components/FeatureUnderConstruction";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Collapsible,
	CollapsibleContent,
	CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyMedia,
	EmptyTitle,
} from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import {
	Item,
	ItemContent,
	ItemDescription,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import {
	Sheet,
	SheetContent,
	SheetDescription,
	SheetHeader,
	SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
	type AlertOccurrenceSummary,
	type AttributionDiagnostic,
	fetchAlerts,
	fetchObservations,
	fetchOccurrence,
	type ObservationSummary,
	type ViewAttributionDiagnostic,
} from "@/features/alerts/api";
import { useLiveAlerts } from "@/features/alerts/useLiveAlerts";
import {
	type AttemptSummary,
	analysisCommandId,
	createAnalysis,
	fetchAnalyses,
	fetchAnalysis,
	fetchAttempts,
	type InitialAnalysisDetail,
	isActive,
	reasonLabel,
	retryAnalysis,
	stateLabel,
} from "@/features/analysis/api";
import { listBusinessViews } from "@/features/systems/api";

const problem = (reason: unknown, fallback: string) =>
	reason instanceof Error ? reason.message : fallback;
const time = (value?: string) =>
	value ? new Date(value).toLocaleString() : "—";
const creating = new Map<string, Promise<InitialAnalysisDetail>>();
function parts(route: string) {
	const url = new URL(route, "https://workbench.invalid");
	return { path: url.pathname, query: url.searchParams };
}
function listRoute(view: "current" | "history", viewKey: string, id?: string) {
	const query = new URLSearchParams({ view });
	if (viewKey) query.set("viewKey", viewKey);
	if (id) query.set("id", id);
	return `/alerts/list?${query}`;
}

/** Alert center owns query state and composes reusable presentation primitives over real APIs. */
export function useAlertsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const { path, query } = parts(props.route);
	const postmortems = path === "/postmortems";
	const view = query.get("view") === "history" ? "history" : "current";
	// 归属过滤按业务视图（ADR-0008）；旧 system 参数保持兼容读取。
	const viewKey = query.get("viewKey") ?? "";
	const legacySystem = query.get("system") ?? "";
	const selectedId = query.get("id");
	useEffect(() => {
		if (path === "/alerts")
			props.navigate(listRoute(view, viewKey, selectedId ?? undefined));
	}, [path, props, selectedId, view, viewKey]);
	if (postmortems)
		return {
			title: "运维中心",
			list: null,
			content: (
				<section className="p-6">
					<FeatureUnderConstruction description="故障复盘能力尚未开放。" />
				</section>
			),
		};
	return {
		title: "运维中心",
		list: null,
		content: (
			<AlertList
				{...props}
				view={view}
				viewKey={viewKey}
				legacySystem={legacySystem}
				selectedId={selectedId}
			/>
		),
	};
}

function severityTone(value?: string) {
	switch (value?.toLowerCase()) {
		case "critical":
			return "bg-destructive";
		case "warning":
			return "bg-warning";
		case "info":
			return "bg-info";
		default:
			return "bg-muted-foreground";
	}
}

function relativeTime(value: string) {
	const seconds = Math.max(
		0,
		Math.floor((Date.now() - new Date(value).getTime()) / 1_000),
	);
	if (seconds < 60) return "刚刚";
	if (seconds < 3_600) return `${Math.floor(seconds / 60)} 分钟前`;
	if (seconds < 86_400) return `${Math.floor(seconds / 3_600)} 小时前`;
	return `${Math.floor(seconds / 86_400)} 天前`;
}

const attributionReasonLabels: Record<string, string> = {
	no_declaration_labels: "没有可用于归属的业务声明标签",
	source_mismatch: "交付告警源不在任何参与视图的声明范围内",
	label_mismatch: "告警标签不满足任何参与视图的标签条件",
	no_matching_declaration: "没有匹配的业务声明",
	no_matching_view: "没有匹配的业务视图",
	exactly_one_matching_view: "唯一匹配的业务视图",
	multiple_matching_views: "多个业务视图同时匹配",
	multiple_matching_declarations: "多个业务声明同时匹配",
};

/** Parse only immutable server evidence; malformed legacy payloads stay visible without inference. */
function diagnosticValues(value: string): string {
	try {
		const parsed: unknown = JSON.parse(value);
		return Array.isArray(parsed) &&
			parsed.every(
				(entry) => typeof entry === "string" || typeof entry === "number",
			)
			? parsed.map(String).join(", ") || "无"
			: value;
	} catch {
		return value || "无";
	}
}

function attributionReason(reasonJson: string): string {
	try {
		const parsed: unknown = JSON.parse(reasonJson);
		if (
			typeof parsed === "object" &&
			parsed !== null &&
			"code" in parsed &&
			typeof parsed.code === "string"
		)
			return attributionReasonLabels[parsed.code] ?? parsed.code;
	} catch {
		/* Preserve the recorded payload below when a legacy response is malformed. */
	}
	return reasonJson || "未提供";
}

function AttributionDiagnosticNotice({
	attribution,
}: {
	attribution: AttributionDiagnostic;
}) {
	if (attribution.status === "attributed") return null;
	const conflict = attribution.status === "conflict";
	return (
		<Alert
			variant={conflict ? "destructive" : "default"}
			aria-label={conflict ? "归属冲突诊断" : "未归属诊断"}
		>
			<AlertTriangle />
			<AlertTitle>{conflict ? "归属冲突" : "未归属"}</AlertTitle>
			<AlertDescription>
				<p>
					{conflict
						? "该告警首次接收时匹配多个业务声明，未归属到任一业务系统。"
						: "该告警首次接收时未匹配业务声明。"}
				</p>
				<dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
					<dt>原因</dt>
					<dd className="break-words font-mono">
						{attributionReason(attribution.reasonJson)}
					</dd>
					<dt>候选业务系统 ID</dt>
					<dd className="break-words font-mono">
						{diagnosticValues(attribution.candidateSystemIdsJson)}
					</dd>
					<dt>候选配置版本 ID</dt>
					<dd className="break-words font-mono">
						{diagnosticValues(attribution.candidateConfigVersionIdsJson)}
					</dd>
				</dl>
				<p>以上为告警首次接收时冻结的归属证据，不会按当前业务声明重新解释。</p>
			</AlertDescription>
		</Alert>
	);
}

/** 冻结的候选视图快照（ADR-0008）：唯一归属与多候选歧义都保留完整身份可追溯。 */
function viewCandidates(value: string): string {
	try {
		const parsed: unknown = JSON.parse(value);
		if (!Array.isArray(parsed)) return value;
		const keys = parsed
			.map((entry) =>
				entry &&
				typeof entry === "object" &&
				"viewKey" in entry &&
				typeof entry.viewKey === "string"
					? entry.viewKey
					: "",
			)
			.filter(Boolean);
		return keys.length ? keys.join(", ") : value;
	} catch {
		return value || "无";
	}
}

function ViewAttributionNotice({
	attribution,
}: {
	attribution: ViewAttributionDiagnostic;
}) {
	if (attribution.status === "attributed") return null;
	const ambiguous = attribution.status === "ambiguous";
	return (
		<Alert
			variant={ambiguous ? "destructive" : "default"}
			aria-label={ambiguous ? "归属歧义诊断" : "未归属诊断"}
		>
			<AlertTriangle />
			<AlertTitle>{ambiguous ? "归属歧义" : "未归属"}</AlertTitle>
			<AlertDescription>
				<p>
					{ambiguous
						? "该告警首次接收时匹配多个业务视图，未归属到任一视图；请调整视图的告警源或标签条件后，由新告警重新归属。"
						: "该告警首次接收时未匹配任何业务视图。"}
				</p>
				<dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
					<dt>原因</dt>
					<dd className="break-words font-mono">
						{attributionReason(attribution.reasonJson)}
					</dd>
					{ambiguous && (
						<Fragment>
							<dt>候选视图</dt>
							<dd className="break-words font-mono">
								{viewCandidates(attribution.candidatesJson)}
							</dd>
						</Fragment>
					)}
				</dl>
				<p>
					以上为告警首次接收时冻结的归属证据；历史记录不会按当前视图配置重新计算。
				</p>
			</AlertDescription>
		</Alert>
	);
}

function AlertList({
	view,
	viewKey,
	legacySystem,
	selectedId,
	navigate,
	suspended,
	openEvidence,
}: WorkspaceModuleProps & {
	view: "current" | "history";
	viewKey: string;
	legacySystem: string;
	selectedId: string | null;
}) {
	const [views, setViews] = useState<
		{ viewKey: string; displayName: string }[]
	>([]);
	const [query, setQuery] = useState("");
	// 归属过滤统一走业务视图；旧声明过滤只在 URL 显式携带时生效（兼容历史链接）。
	const filter = legacySystem ? legacySystem : viewKey;
	const filterKind = legacySystem ? "businessSystemKey" : "viewKey";
	// Counts are independently snapshotted so neither tab presents a misleading zero before load.
	const [counts, setCounts] = useState<{
		key: string;
		firing?: number;
		resolved?: number;
	}>({ key: "" });
	const stream = useAlertEventStream();
	const countScopeRef = useRef("");
	const countGenerationRef = useRef(0);
	const countScope = `${filter}\u0000${filterKind}\u0000${suspended}`;
	// Invalidate count reads while rendering the new route scope so a delayed
	// old filter response cannot replace the badges before effect cleanup.
	if (countScopeRef.current !== countScope) {
		countScopeRef.current = countScope;
		countGenerationRef.current += 1;
	}
	// The list is live by default; workbench suspension is the only local pause boundary.
	const live = useLiveAlerts(
		view === "history" ? "Resolved" : "Firing",
		filterKind === "viewKey"
			? { viewKey: filter }
			: { businessSystemKey: filter },
		!suspended,
	);
	const { setAtTop } = live;
	useEffect(() => {
		if (!suspended)
			listBusinessViews()
				.then((items) =>
					setViews(
						items.map((item) => ({
							viewKey: item.viewKey,
							displayName: item.displayName,
						})),
					),
				)
				.catch(() => undefined);
	}, [suspended]);
	useEffect(() => {
		// The workbench main area scrolls with the document, so this is the real
		// reading-position boundary used to buffer new firing occurrences.
		const updateAtTop = () => setAtTop(window.scrollY < 8);
		updateAtTop();
		window.addEventListener("scroll", updateAtTop, { passive: true });
		return () => window.removeEventListener("scroll", updateAtTop);
	}, [setAtTop]);
	const countKey = filter;
	const refreshCounts = useCallback(
		async (requestGeneration = countGenerationRef.current) => {
			if (suspended || requestGeneration !== countGenerationRef.current) return;
			try {
				// Counts always come from the two server projections; never derive them
				// from the visible list because it may intentionally buffer new rows.
				const [firing, resolved] = await Promise.all([
					fetchAlerts(
						"Firing",
						filterKind === "businessSystemKey" ? filter : "",
						filterKind === "viewKey" ? filter : "",
					),
					fetchAlerts(
						"Resolved",
						filterKind === "businessSystemKey" ? filter : "",
						filterKind === "viewKey" ? filter : "",
					),
				]);
				if (requestGeneration === countGenerationRef.current && !suspended)
					setCounts({
						key: filter,
						firing: firing.items.length,
						resolved: resolved.items.length,
					});
			} catch {
				/* Keep the most recent verified counts until the next snapshot. */
			}
		},
		[filter, filterKind, suspended],
	);
	useEffect(() => {
		if (!suspended) void refreshCounts();
	}, [refreshCounts, suspended]);
	useEffect(() => {
		let cancelled = false;
		const refresh = () => {
			const generation = countGenerationRef.current;
			if (!cancelled && !suspended) void refreshCounts(generation);
		};
		const unsubscribeChange = stream.onChange(refresh);
		const unsubscribeResync = stream.onResync(refresh);
		return () => {
			cancelled = true;
			unsubscribeChange();
			unsubscribeResync();
		};
	}, [refreshCounts, stream, suspended]);
	const filteredItems = useMemo(() => {
		const needle = query.trim().toLocaleLowerCase();
		if (!needle) return live.items;
		return live.items.filter((item) =>
			[
				item.labels.alertname,
				item.annotations?.summary,
				item.annotations?.description,
				...Object.values(item.labels),
			]
				.filter(Boolean)
				.join(" ")
				.toLocaleLowerCase()
				.includes(needle),
		);
	}, [live.items, query]);
	const emptyTitle = query
		? "没有匹配的告警"
		: view === "current"
			? "当前没有告警"
			: "没有历史告警";
	const emptyDescription = query
		? "已加载的告警中没有匹配此搜索条件的记录。"
		: view === "current"
			? "当前没有正在触发的告警。"
			: "尚未加载到已恢复的告警记录。";
	const listItems: EntityListItem[] = filteredItems.map((item) => {
		// 归属标签展示真实新归属；旧历史行退回旧声明诊断，仅作历史事实展示。
		const attributedView =
			item.source === "alertmanager" &&
			item.viewAttribution?.status === "attributed"
				? item.viewAttribution.viewName || item.viewAttribution.viewKey
				: undefined;
		const diagnosticLabel =
			item.source === "alertmanager"
				? item.viewAttribution?.status === "ambiguous"
					? "归属歧义"
					: item.viewAttribution?.status === "unattributed"
						? "未归属"
						: !item.viewAttribution && item.attribution?.status === "conflict"
							? "归属冲突"
							: !item.viewAttribution &&
									item.attribution?.status === "unattributed"
								? "未归属"
								: undefined
				: undefined;
		return {
			id: item.id,
			title: item.labels.alertname ?? item.id,
			subtitle:
				item.annotations?.summary ??
				item.annotations?.description ??
				attributedView ??
				item.businessSystemKey ??
				diagnosticLabel ??
				"未提供摘要",
			badge: {
				text:
					item.source === "platform"
						? `平台内部 · ${item.state}`
						: diagnosticLabel
							? `${diagnosticLabel} · ${item.state}`
							: item.state,
				variant: item.state === "Firing" ? "destructive" : "secondary",
			},
			media: (
				<span
					className={cn(
						"size-2 rounded-full",
						severityTone(item.labels.severity),
					)}
					title={
						item.labels.severity
							? `严重性：${item.labels.severity}`
							: "严重性：未知"
					}
				/>
			),
			time: (
				<time
					dateTime={item.lastStateChangeAt}
					title={time(item.lastStateChangeAt)}
				>
					{relativeTime(item.lastStateChangeAt)}
				</time>
			),
		};
	});
	const controls = (
		<div className="flex flex-wrap items-center justify-between gap-3">
			<Tabs
				value={view}
				onValueChange={(next) =>
					navigate(
						listRoute(
							next as "current" | "history",
							viewKey,
							selectedId ?? undefined,
						),
					)
				}
			>
				<TabsList>
					<TabsTrigger value="current">
						当前告警
						{counts.key === countKey && counts.firing !== undefined && (
							<Badge variant="secondary" className="ml-1 tabular-nums">
								{counts.firing}
							</Badge>
						)}
					</TabsTrigger>
					<TabsTrigger value="history">
						历史告警
						{counts.key === countKey && counts.resolved !== undefined && (
							<Badge variant="secondary" className="ml-1 tabular-nums">
								{counts.resolved}
							</Badge>
						)}
					</TabsTrigger>
				</TabsList>
			</Tabs>
			<div className="flex flex-1 flex-wrap justify-end gap-3">
				<Select
					value={viewKey || "__all__"}
					onValueChange={(next) =>
						navigate(
							listRoute(
								view,
								next === "__all__" ? "" : next,
								selectedId ?? undefined,
							),
						)
					}
				>
					<SelectTrigger className="w-full sm:w-52" aria-label="按业务视图过滤">
						<SelectValue placeholder="全部业务视图" />
					</SelectTrigger>
					<SelectContent>
						<SelectGroup>
							<SelectItem value="__all__">全部业务视图</SelectItem>
							{views.map((item) => (
								<SelectItem key={item.viewKey} value={item.viewKey}>
									{item.displayName}
								</SelectItem>
							))}
						</SelectGroup>
					</SelectContent>
				</Select>
				<Input
					value={query}
					onChange={(event) => setQuery(event.target.value)}
					placeholder="搜索已加载告警"
					className="w-full sm:max-w-xs"
					aria-label="搜索已加载告警"
				/>
			</div>
		</div>
	);
	return (
		<section className="mx-auto flex w-full max-w-6xl flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">
					{view === "current" ? "当前告警" : "告警历史"}
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					查看上游告警与平台内部组件故障的真实记录；归属来自业务视图的首次接收判断。
				</p>
			</div>
			{live.pendingNew > 0 && (
				<Button variant="secondary" size="sm" onClick={live.mergePending}>
					显示 {live.pendingNew} 条新告警
				</Button>
			)}
			<EntityList
				items={listItems}
				columns={["media", "title", "subtitle", "status", "time"]}
				selectedId={selectedId}
				onSelect={(item) => navigate(listRoute(view, viewKey, item.id))}
				loading={live.loading}
				error={live.error}
				onRetry={live.refresh}
				controls={controls}
				emptyTitle={emptyTitle}
				emptyDescription={emptyDescription}
			/>
			<AlertDetailSheet
				id={selectedId}
				onClose={() => navigate(listRoute(view, viewKey))}
				suspended={suspended}
				openEvidence={openEvidence}
			/>
		</section>
	);
}

function DetailEmpty({
	icon: Icon,
	title,
	description,
}: {
	icon: typeof FileText;
	title: string;
	description: string;
}) {
	return (
		<Empty className="min-h-32 gap-3 bg-muted/50 p-5 md:p-6">
			<EmptyHeader>
				<EmptyMedia variant="icon">
					<Icon />
				</EmptyMedia>
				<EmptyTitle className="text-base">{title}</EmptyTitle>
				<EmptyDescription>{description}</EmptyDescription>
			</EmptyHeader>
		</Empty>
	);
}

/** Machine-provided labels and annotations remain copyable while long values cannot overflow the drawer. */
function PropertyList({ entries }: { entries: [string, string][] }) {
	return (
		<dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
			{entries.map(([key, value]) => (
				<Fragment key={key}>
					<dt className="text-muted-foreground">{key}</dt>
					<dd className="min-w-0 break-words font-mono text-sm">
						{key.toLowerCase() === "severity" &&
						value.toLowerCase() === "critical" ? (
							<Badge variant="destructive">{value}</Badge>
						) : (
							value
						)}
					</dd>
				</Fragment>
			))}
		</dl>
	);
}

function ObservationIcon({ effect }: { effect: ObservationSummary["effect"] }) {
	if (effect === "resolved" || effect === "resolved_first")
		return <CheckCircle2 className="size-4 text-success" aria-hidden="true" />;
	if (effect === "repeat_firing")
		return <RefreshCw className="size-4 text-destructive" aria-hidden="true" />;
	if (effect === "late_firing_after_resolved")
		return (
			<AlertTriangle className="size-4 text-destructive" aria-hidden="true" />
		);
	return <CircleDot className="size-4 text-destructive" aria-hidden="true" />;
}

function ObservationDot({ effect }: { effect: ObservationSummary["effect"] }) {
	const resolved = effect === "resolved" || effect === "resolved_first";
	return (
		<span
			className={cn(
				"absolute -left-[5px] top-5 size-2.5 rounded-full border border-background",
				resolved ? "bg-success" : "bg-destructive",
			)}
			aria-hidden="true"
		/>
	);
}

/** Attempt states are server projections; only map states the API explicitly returns. */
function AttemptStateIcon({ state }: { state: AttemptSummary["state"] }) {
	if (state === "Succeeded")
		return <CheckCircle2 className="size-4 text-success" aria-label="已完成" />;
	if (state === "Failed" || state === "Interrupted")
		return (
			<AlertTriangle className="size-4 text-destructive" aria-label={state} />
		);
	if (state === "Queued" || state === "Running")
		return (
			<Clock3 className="size-4 text-muted-foreground" aria-label={state} />
		);
	return (
		<CircleDot className="size-4 text-muted-foreground" aria-label={state} />
	);
}

function AlertDetailSheet({
	id,
	onClose,
	suspended,
	openEvidence,
}: {
	id: string | null;
	onClose: () => void;
	suspended: boolean;
	openEvidence: (id: string) => void;
}) {
	const stream = useAlertEventStream();
	const [occurrence, setOccurrence] = useState<AlertOccurrenceSummary | null>(
		null,
	);
	const [observations, setObservations] = useState<ObservationSummary[]>([]);
	const [error, setError] = useState("");
	const [analysisOpen, setAnalysisOpen] = useState(false);
	// The route and suspension state form a read scope. Advance it during render
	// so an old HTTP/SSE task cannot commit in the gap before effect cleanup.
	const scopeRef = useRef("");
	const generationRef = useRef(0);
	const versionRef = useRef(0);
	const eventQueueRef = useRef<Promise<void>>(Promise.resolve());
	const scope = `${id ?? ""}\u0000${suspended}`;
	if (scopeRef.current !== scope) {
		scopeRef.current = scope;
		generationRef.current += 1;
		versionRef.current = 0;
	}
	const load = useCallback(
		async (minimumVersion = 0, requestGeneration = generationRef.current) => {
			const occurrenceId = id;
			if (
				!occurrenceId ||
				suspended ||
				requestGeneration !== generationRef.current
			)
				return;
			setError("");
			try {
				const detail = await fetchOccurrence(occurrenceId);
				if (suspended || requestGeneration !== generationRef.current) return;
				// A response below the event's row version is not a usable projection.
				// Ask the shared stream's snapshot owner to rebuild rather than display
				// a stale occurrence or advance an unverified event.
				if (detail.rowVersion < minimumVersion) {
					stream.resync();
					return;
				}
				if (detail.rowVersion < versionRef.current) return;
				const timeline =
					detail.source === "platform"
						? { items: [] }
						: await fetchObservations(occurrenceId);
				if (
					suspended ||
					requestGeneration !== generationRef.current ||
					detail.rowVersion < versionRef.current
				)
					return;
				versionRef.current = detail.rowVersion;
				setOccurrence(detail);
				setObservations(timeline.items);
			} catch (reason) {
				if (requestGeneration !== generationRef.current) return;
				setOccurrence(null);
				setError(problem(reason, "无法加载告警详情。"));
			}
		},
		[id, stream, suspended],
	);
	// State reset and fetch intentionally follow the URL-selected occurrence.
	useEffect(() => {
		setOccurrence(null);
		setObservations([]);
		if (id && !suspended) void load();
	}, [id, load, suspended]);
	useEffect(() => {
		let cancelled = false;
		const unsubscribe = stream.onChange((event, sourceGeneration) => {
			if (event.occurrenceId !== id || suspended) return;
			const eventGeneration = generationRef.current;
			eventQueueRef.current = eventQueueRef.current
				.then(async () => {
					if (
						cancelled ||
						eventGeneration !== generationRef.current ||
						sourceGeneration !== stream.generation ||
						event.rowVersion <= versionRef.current
					)
						return;
					await load(event.rowVersion, eventGeneration);
				})
				.catch(() => {
					if (!cancelled && eventGeneration === generationRef.current)
						stream.resync();
				});
		});
		return () => {
			cancelled = true;
			unsubscribe();
		};
	}, [id, load, stream, suspended]);
	useEffect(
		() =>
			stream.onResync(() => {
				if (id && !suspended) void load(0, generationRef.current);
			}),
		[id, load, stream, suspended],
	);
	const annotationEntries = occurrence
		? Object.entries(occurrence.annotations ?? {}).filter(
				([key]) => key !== "description" && key !== "summary",
			)
		: [];
	const description =
		occurrence?.annotations?.description ?? occurrence?.annotations?.summary;
	// 真实归属徽标：优先新视图归属，旧历史行退回旧声明 key，平台故障展示组件。
	const attributionBadge = occurrence
		? occurrence.source === "platform"
			? `平台内部 · ${occurrence.component ?? "未知组件"}`
			: occurrence.viewAttribution?.status === "attributed"
				? `归属视图 · ${occurrence.viewAttribution.viewName || occurrence.viewAttribution.viewKey}`
				: occurrence.viewAttribution?.status === "ambiguous"
					? "归属歧义"
					: occurrence.viewAttribution?.status === "unattributed"
						? "未归属"
						: (occurrence.businessSystemKey ?? "未归属")
		: "";
	return (
		<Sheet
			open={Boolean(id)}
			onOpenChange={(open) => {
				if (!open) onClose();
			}}
		>
			<SheetContent
				side="right"
				className="flex h-dvh w-full flex-col gap-0 overflow-hidden p-0 sm:max-w-4xl"
			>
				<SheetHeader className="shrink-0 border-b px-6 py-5 pr-12">
					<SheetTitle className="text-lg">
						{occurrence?.labels.alertname ?? "告警详情"}
					</SheetTitle>
					<SheetDescription>
						{occurrence?.source === "platform"
							? "查看平台组件故障的原因和发生、恢复时间。"
							: "查看告警的真实属性、观察记录和初步分析。"}
					</SheetDescription>
					{occurrence && (
						<div className="flex flex-wrap items-center gap-2 pt-2">
							<Badge
								variant={
									occurrence.state === "Firing" ? "destructive" : "secondary"
								}
							>
								{occurrence.state}
							</Badge>
							<Separator
								orientation="vertical"
								className="hidden h-4 self-center sm:block"
							/>
							<Badge variant="outline">{attributionBadge}</Badge>
							<Separator
								orientation="vertical"
								className="hidden h-4 self-center sm:block"
							/>
							<Badge variant="outline">
								{occurrence.source === "platform"
									? "来源：平台内部"
									: "来源：Alertmanager"}
							</Badge>
							<Separator
								orientation="vertical"
								className="hidden h-4 self-center sm:block"
							/>
							<span className="flex w-full items-center gap-1 text-xs text-muted-foreground sm:w-auto">
								<Clock3 className="size-3.5" aria-hidden="true" />
								最近变更 {time(occurrence.lastStateChangeAt)}
							</span>
						</div>
					)}
				</SheetHeader>
				<div className="min-h-0 flex-1 overflow-hidden">
					{error ? (
						<div className="p-6">
							<Alert variant="destructive">
								<AlertTriangle />
								<AlertTitle>无法加载告警详情</AlertTitle>
								<AlertDescription>{error}</AlertDescription>
								<Button
									className="mt-2"
									size="sm"
									variant="outline"
									onClick={() => void load()}
								>
									<RefreshCw data-icon="inline-start" />
									重试
								</Button>
							</Alert>
						</div>
					) : !occurrence ? (
						<div
							className="flex flex-col gap-4 p-6"
							role="status"
							aria-label="正在加载告警详情"
						>
							<Skeleton className="h-7 w-1/3" />
							<Skeleton className="h-24 w-full" />
							<Skeleton className="h-24 w-full" />
						</div>
					) : (
						<Tabs
							defaultValue="overview"
							onValueChange={(value) => setAnalysisOpen(value === "analysis")}
							className="h-full min-h-0 gap-0"
						>
							<div className="shrink-0 border-b px-6">
								<TabsList variant="line" className="h-11">
									<TabsTrigger value="overview">概览</TabsTrigger>
									{occurrence.source !== "platform" && (
										<TabsTrigger value="timeline">时间线</TabsTrigger>
									)}
									{occurrence.source !== "platform" && (
										<TabsTrigger value="analysis">AI 分析</TabsTrigger>
									)}
								</TabsList>
							</div>
							<TabsContent
								value="overview"
								className="min-h-0 overflow-y-auto p-4 sm:p-6"
							>
								<div className="flex flex-col gap-6">
									<section className="flex flex-col gap-3">
										<h2 className="text-sm font-medium">告警说明</h2>
										{description ? (
											<p className="whitespace-pre-wrap text-sm leading-6">
												{description}
											</p>
										) : (
											<DetailEmpty
												icon={FileText}
												title="没有提供描述"
												description="上游告警未附带描述或摘要。"
											/>
										)}
									</section>
									{occurrence.source === "alertmanager" &&
										occurrence.viewAttribution && (
											<>
												<Separator />
												<section
													className="flex flex-col gap-3"
													aria-labelledby="attribution-diagnostic-title"
												>
													<h2
														id="attribution-diagnostic-title"
														className="text-sm font-medium"
													>
														归属诊断
													</h2>
													{occurrence.viewAttribution.status ===
														"attributed" && (
														<p className="text-sm text-muted-foreground">
															该告警首次接收时唯一匹配业务视图{" "}
															{occurrence.viewAttribution.viewName ||
																occurrence.viewAttribution.viewKey}
															，来源为
															Alertmanager；证据已冻结，不随视图配置变化。
														</p>
													)}
													{occurrence.viewAttribution.status !==
														"attributed" && (
														<ViewAttributionNotice
															attribution={occurrence.viewAttribution}
														/>
													)}
												</section>
											</>
										)}
									{occurrence.source === "alertmanager" &&
										!occurrence.viewAttribution &&
										occurrence.attribution &&
										occurrence.attribution.status !== "attributed" && (
											<>
												<Separator />
												<section
													className="flex flex-col gap-3"
													aria-labelledby="attribution-diagnostic-title"
												>
													<h2
														id="attribution-diagnostic-title"
														className="text-sm font-medium"
													>
														归属诊断
													</h2>
													<p className="text-sm text-muted-foreground">
														以下为旧业务声明时代的冻结证据，仅作历史事实展示。
													</p>
													<AttributionDiagnosticNotice
														attribution={occurrence.attribution}
													/>
												</section>
											</>
										)}
									{occurrence.source === "platform" && (
										<>
											<Separator />
											<section className="flex flex-col gap-3">
												<h2 className="text-sm font-medium">故障生命周期</h2>
												<PropertyList
													entries={[
														["发生时间", time(occurrence.firstSeenAt)],
														[
															"恢复时间",
															occurrence.resolvedAt
																? time(occurrence.resolvedAt)
																: "尚未恢复",
														],
														["原因", occurrence.reason ?? "未知"],
													]}
												/>
												<p className="text-sm text-muted-foreground">
													此平台故障没有业务采集声明，不支持初步分析。不会启动业务采集。
												</p>
											</section>
										</>
									)}
									{annotationEntries.length > 0 && (
										<>
											<Separator />
											<section className="flex flex-col gap-3">
												<h2 className="text-sm font-medium">注释</h2>
												<PropertyList entries={annotationEntries} />
											</section>
										</>
									)}
									<Separator />
									<section className="flex flex-col gap-3">
										<h2 className="text-sm font-medium">属性</h2>
										<PropertyList entries={Object.entries(occurrence.labels)} />
									</section>
								</div>
							</TabsContent>
							<TabsContent
								value="timeline"
								className="min-h-0 overflow-y-auto p-4 sm:p-6"
							>
								<section aria-labelledby="observation-title">
									<h2
										id="observation-title"
										className="mb-4 text-sm font-medium"
									>
										观察记录
									</h2>
									{observations.length === 0 ? (
										<DetailEmpty
											icon={Activity}
											title="没有观察记录"
											description="此告警尚未记录状态观察。"
										/>
									) : (
										<ul
											className="ml-2 border-l border-border"
											aria-label="观察记录时间线"
										>
											{observations.map((item) => (
												<li key={item.id} className="relative pl-5">
													<ObservationDot effect={item.effect} />
													<Item
														className="rounded-none border-0 px-0 py-4"
														size="sm"
													>
														<ItemMedia variant="icon">
															<ObservationIcon effect={item.effect} />
														</ItemMedia>
														<ItemContent>
															<ItemTitle>{item.observedState}</ItemTitle>
															<ItemDescription>
																效果：{item.effect} · 提交于{" "}
																{time(item.committedAt)}
															</ItemDescription>
														</ItemContent>
													</Item>
												</li>
											))}
										</ul>
									)}
								</section>
							</TabsContent>
							{occurrence.source !== "platform" && (
								<TabsContent
									value="analysis"
									className="min-h-0 overflow-y-auto p-4 sm:p-6"
								>
									{analysisOpen && (
										<InitialAnalysis
											key={occurrence.id}
											occurrenceId={occurrence.id}
											suspended={suspended}
											openEvidence={openEvidence}
										/>
									)}
								</TabsContent>
							)}
						</Tabs>
					)}
				</div>
			</SheetContent>
		</Sheet>
	);
}

function InitialAnalysis({
	occurrenceId,
	suspended,
	openEvidence,
}: {
	occurrenceId: string;
	suspended: boolean;
	openEvidence: (id: string) => void;
}) {
	const [analysis, setAnalysis] = useState<InitialAnalysisDetail | null>(null);
	const [attempts, setAttempts] = useState<AttemptSummary[]>([]);
	const [error, setError] = useState("");
	const [attemptsOpen, setAttemptsOpen] = useState(false);
	const started = useRef(false);
	const load = useCallback(
		async (retry = false) => {
			if (suspended) return;
			setError("");
			try {
				const summaries = await fetchAnalyses(occurrenceId);
				const newest = [...summaries.items].sort((a, b) =>
					b.createdAt.localeCompare(a.createdAt),
				);
				const current =
					newest.find((item) => isActive(item.state)) ?? newest[0];
				if (retry) {
					if (!analysis) return;
					const detail = await retryAnalysis(
						occurrenceId,
						analysis.id,
						analysisCommandId(),
					);
					setAnalysis(detail);
					setAttempts((await fetchAttempts(occurrenceId, detail.id)).items);
					return;
				}
				if (!current) {
					let request = creating.get(occurrenceId);
					if (!request) {
						request = createAnalysis(occurrenceId, analysisCommandId());
						creating.set(occurrenceId, request);
						request.finally(() => creating.delete(occurrenceId));
					}
					setAnalysis(await request);
					return;
				}
				const detail = await fetchAnalysis(occurrenceId, current.id);
				setAnalysis(detail);
				setAttempts((await fetchAttempts(occurrenceId, current.id)).items);
			} catch (reason) {
				setError(problem(reason, "无法读取或发起初步分析。"));
			}
		},
		[analysis, occurrenceId, suspended],
	);
	// Mounting the AI tab is the explicit user transition that may start analysis.
	useEffect(() => {
		if (!started.current) {
			started.current = true;
			// eslint-disable-next-line react-hooks/set-state-in-effect -- The first mount of the analysis tab is the deliberate user transition that may create an analysis.
			void load();
		}
	}, [load]);
	// Running work is server-owned, so the open tab polls its real projection until terminal or unmounted.
	useEffect(() => {
		if (!analysis || !isActive(analysis.state) || suspended) return;
		const timer = window.setInterval(() => void load(), 2000);
		return () => window.clearInterval(timer);
	}, [analysis, analysis?.id, analysis?.state, load, suspended]);
	if (error)
		return (
			<Alert variant="destructive">
				<AlertTriangle />
				<AlertTitle>无法读取初步分析</AlertTitle>
				<AlertDescription>{error}</AlertDescription>
				<Button
					className="mt-2"
					size="sm"
					variant="outline"
					disabled={suspended}
					onClick={() => void load()}
				>
					<RefreshCw data-icon="inline-start" />
					重新读取
				</Button>
			</Alert>
		);
	if (!analysis)
		return (
			<section
				className="flex flex-col gap-6"
				role="status"
				aria-label="正在准备初步分析"
			>
				<h2 className="text-sm font-medium">初步分析</h2>
				<div className="flex flex-col gap-3">
					<Skeleton className="h-4 w-1/4" />
					<Skeleton className="h-4 w-full" />
					<Skeleton className="h-4 w-4/5" />
				</div>
			</section>
		);
	const running = isActive(analysis.state);
	// 结论优先：正文用共享阅读器呈现；轻量元信息一行带过，执行调试留在折叠的执行记录里；
	// 告警属性、观察等原始数据仍在概览与时间线 Tab，不混入 AI 正文。
	return (
		<section className="flex flex-col gap-6">
			<div className="flex items-center justify-between gap-3">
				<h2 className="text-sm font-medium">初步分析</h2>
				<Badge>{stateLabel(analysis.state)}</Badge>
			</div>
			{analysis.output ? (
				<>
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">结论</h3>
						<div className="flex gap-2 rounded-md bg-muted/50 px-3 py-2 text-sm leading-6">
							<Bot
								className="mt-0.5 size-4 shrink-0 text-muted-foreground"
								aria-hidden="true"
							/>
							<div className="min-w-0 flex-1">
								<AiContent
									content={analysis.output.content}
									evidenceIds={analysis.output.evidenceIds}
									openEvidence={openEvidence}
								/>
							</div>
						</div>
						<p className="text-xs text-muted-foreground">
							模型 {analysis.output.modelId} · {time(analysis.output.createdAt)}
						</p>
					</section>
					<Separator />
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">关联证据</h3>
						{analysis.output.evidenceIds.length > 0 ? (
							<EvidenceLinks
								ids={analysis.output.evidenceIds}
								openEvidence={openEvidence}
							/>
						) : (
							<DetailEmpty
								icon={FileText}
								title="没有关联证据"
								description="本次分析没有返回证据记录。"
							/>
						)}
					</section>
				</>
			) : running ? (
				<>
					<div className="flex items-center gap-2 text-sm text-muted-foreground">
						<Bot className="size-4" aria-hidden="true" />
						<span>分析正在执行，关闭详情不会取消任务。</span>
					</div>
					<div className="flex flex-col gap-3">
						<Skeleton className="h-4 w-full" />
						<Skeleton className="h-4 w-5/6" />
						<Skeleton className="h-4 w-2/3" />
					</div>
				</>
			) : (
				<DetailEmpty
					icon={Bot}
					title="没有分析结果"
					description="此分析未返回结果内容。"
				/>
			)}
			{(analysis.state === "Failed" || analysis.state === "Interrupted") && (
				<Button
					variant="outline"
					disabled={suspended}
					onClick={() => void load(true)}
				>
					<RefreshCw data-icon="inline-start" />
					重新发起分析
				</Button>
			)}
			{attempts.length > 0 && (
				<Collapsible
					open={attemptsOpen}
					onOpenChange={setAttemptsOpen}
					className="rounded-md border"
				>
					<CollapsibleTrigger asChild>
						<Button
							className="w-full justify-between rounded-b-none"
							variant="ghost"
						>
							<span>执行记录</span>
							<ChevronDown
								className={cn(
									"size-4 transition-transform",
									attemptsOpen && "rotate-180",
								)}
								aria-hidden="true"
							/>
						</Button>
					</CollapsibleTrigger>
					<CollapsibleContent className="border-t px-3 py-2">
						<ul className="flex flex-col gap-3">
							{attempts.map((attempt) => (
								<li key={attempt.id} className="flex flex-col gap-1 text-sm">
									<div className="flex items-center gap-2">
										<AttemptStateIcon state={attempt.state} />
										<span className="font-mono text-xs">{attempt.type}</span>
										<span className="text-muted-foreground">
											{attempt.state}
										</span>
									</div>
									{attempt.terminationReason && (
										<p className="text-muted-foreground">
											{reasonLabel(attempt.terminationReason) ||
												attempt.terminationReason}
										</p>
									)}
								</li>
							))}
						</ul>
					</CollapsibleContent>
				</Collapsible>
			)}
		</section>
	);
}
