import { ConfirmAction } from "@/features/settings/platform/controls";
import { useCursorPages } from '@/hooks/use-cursor-pages';
import { formatDateTime } from "@/lib/format";
import { parseRoute } from "@/lib/parse-route";
/* eslint-disable react-refresh/only-export-components -- This route module intentionally colocates its view factory with route components. */

import {
	ChevronLeft,
	ChevronRight,
	Copy,
	LoaderCircle,
	Plus,
	RefreshCw,
	RotateCw,
	Search,
} from "lucide-react";
import {
	type FormEvent,
	type ReactNode,
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
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { SettingsNavigation, settingsNavGroups } from "@/features/settings/nav";

/** Integrations live under the settings platform group (平台接入). */
const INTEGRATIONS_BASE = "/settings/platform/integrations";

import { messageOf, notify } from "@/app/shared";
import { newClientCommandId, WorkbenchApiError } from "@/api/workbench";
import { EntityList } from "@/components/EntityList";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Card,
	CardContent,
	CardDescription,
	CardHeader,
	CardTitle,
} from "@/components/ui/card";
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
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Textarea } from "@/components/ui/textarea";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { CursorPagination } from "@/components/workbench/CursorPagination";
import { PropertyList } from "@/components/workbench/PropertyList";
import {
	acknowledgeIntakeIssue,
	fetchIntakeIssues,
	type IntakeIssue,
} from "@/features/alerts/api";
import {
	type ConnectionProbeObservation,
	type EventSourceCredential,
	type EventSourceInstance,
	type HttpConnectionInput,
	type HttpConnectionInstance,
	alertmanagerReceiverYaml,
	createEventSourceInstance,
	createHttpConnectionInstance,
	createMetricsInstance,
	disableEventSourceInstance,
	disableHttpConnectionInstance,
	disableMetricsInstance,
	enableHttpConnectionInstance,
	enableMetricsInstance,
	fetchEventSourceInstance,
	fetchHttpConnectionInstance,
	fetchMetricsInstance,
	fetchPublicReceiverEndpoint,
	listEventSourceCredentials,
	listEventSourceInstances,
	listHttpConnectionInstances,
	listIntegrationPlugins,
	listPluginEventDeadletters,
	type MetricsConnectionInput,
	type MetricsInstance,
	probeDiagnostic,
	probeHttpConnectionInstance,
	probeMetricsInstance,
	replayPluginEventDeadletter,
	retireEventSourceCredential,
	revealEventSourceCredential,
	rotateEventSourceCredential,
	rotateHttpConnectionInstance,
	rotateMetricsInstance,
	setEventSourceSettings,
} from "./api";
import {
	EVENT_SOURCE_CAPABILITY,
	HTTP_CONNECTION_CAPABILITY,
	type IntegrationCatalogItem,
	type IntegrationPlatform,
	allowedAuthModes,
	genericHttpConnectionKinds,
	isHttpConnectionCatalogItem,
	isSpecializedPlatform,
} from "./types";
import {
	type SettingsDraft,
	type SettingsDraftValue,
	type SettingsFieldSpec,
	draftFromSettings,
	emptyDraft,
	formatSettingValue,
	settingsFieldSpecs,
	settingsFromDraft,
} from "./settings-schema";

const formatEventTime = (value?: string | null) =>
	formatDateTime(value, "等待首条有效事件");
const formatTime = (value?: string | null) => formatDateTime(value);

function routeParts(route: string) {
	const pathname = parseRoute(route).pathname;
	const sub =
		pathname === INTEGRATIONS_BASE
			? ""
			: pathname.startsWith(`${INTEGRATIONS_BASE}/`)
				? pathname.slice(INTEGRATIONS_BASE.length + 1)
				: pathname.replace(/^\//, "");
	return sub.split("/").filter(Boolean);
}
function integrationRoute(
	platform?: IntegrationPlatform | "instances" | string,
	instanceId?: string,
) {
	return [INTEGRATIONS_BASE, platform, instanceId].filter(Boolean).join("/");
}
/** 已接入实例列表与实例详情都是接入管理页上的右侧抽屉（与告警一致），
 * 由 URL query 标志驱动、可深链，不再是独立页面路由。 */
function instanceSheetRoute(platform: string, name: string) {
	return `${INTEGRATIONS_BASE}?platform=${encodeURIComponent(platform)}&instance=${encodeURIComponent(name)}`;
}

function CatalogCard({
	item,
	navigate,
}: {
	item: IntegrationCatalogItem;
	navigate: (to: string) => void;
}) {
	const route = `/integrations/${encodeURIComponent(item.id)}`;
	const configurable =
		item.id === "prometheus" ||
		item.id === "thanos" ||
		item.id === "alertmanager" ||
		(Boolean(item.sourceKind) && item.capabilities.includes("event_source") &&
			item.capabilities.includes("alert_normalizer")) ||
		isHttpConnectionCatalogItem(item);
	return (
		<Card className="flex flex-col">
			<CardHeader>
				<CardTitle>{item.displayName}</CardTitle>
				<CardDescription>{item.description}</CardDescription>
			</CardHeader>
			<CardContent className="mt-auto flex flex-col gap-3">
				<div className="flex flex-wrap gap-2">
					{item.capabilities.includes("event_source") && (
						<Badge variant="secondary">事件接入</Badge>
					)}
					{item.capabilities.includes(HTTP_CONNECTION_CAPABILITY) && (
						<Badge variant="secondary">HTTP 连接</Badge>
					)}
					{item.capabilities.includes("discover") && (
						<Badge variant="secondary">自动观测</Badge>
					)}
					{item.capabilities.includes("tools") && (
						<Badge variant="secondary">Agent 工具</Badge>
					)}
				</div>
				<Button disabled={!configurable} onClick={() => navigate(route)}>
					{configurable ? `配置 ${item.displayName}` : "暂无配置入口"}
					<ChevronRight data-icon="inline-end" />
				</Button>
			</CardContent>
		</Card>
	);
}

/** Operational backlog for post-commit subscribers. Replaying a deadletter
 * is explicit and does not change the already committed source fact. */
function PluginEventDeadletters() {
	const [backlog, setBacklog] = useState<{ count: number; items: Awaited<ReturnType<typeof listPluginEventDeadletters>>["items"] }>();
	const [error, setError] = useState("");
	const [expanded, setExpanded] = useState(false);
	const [busy, setBusy] = useState<number>();
	const [revision, setRevision] = useState(0);
	// Retain the same ledger key across a failed HTTP response. If the server
	// already committed before the response was lost, retry returns its result
	// instead of launching another subscriber delivery.
	const commandIDs = useRef(new Map<number, string>());
	useEffect(() => {
		let active = true;
		listPluginEventDeadletters()
			.then((result) => { if (active) { setBacklog(result); setError(""); } })
			.catch((reason) => { if (active) setError(messageOf(reason, "无法读取插件事件死信。")); });
		return () => { active = false; };
	}, [revision]);
	if (!error && !backlog?.count) return null;
	return (
		<Alert variant="destructive">
			<AlertTitle>插件事件死信{backlog?.count ? ` · ${backlog.count} 条` : ""}</AlertTitle>
			<AlertDescription>
				{error || "事件已持久提交，但订阅者处理失败；原始告警与日报事实不受影响。"}
			</AlertDescription>
			<div className="mt-3 flex flex-wrap gap-2">
				<Button variant="outline" size="sm" onClick={() => setRevision((value) => value + 1)}>刷新死信</Button>
				{Boolean(backlog?.count) && <Button variant="outline" size="sm" onClick={() => setExpanded((value) => !value)}>{expanded ? "收起详情" : "查看详情"}</Button>}
			</div>
			{expanded && backlog && (
				<div className="mt-3 space-y-2">
					{backlog.count > backlog.items.length && <p className="text-xs">仅显示前 {backlog.items.length} 条；刷新后继续处理。</p>}
					{backlog.items.map((entry) => (
						<div key={entry.deliveryId} className="flex flex-wrap items-center gap-2 rounded-md border p-2 text-sm">
							<span>事件 #{entry.eventId} · 订阅者 {entry.subscriberId} · 失败 {entry.attempts} 次</span>
							<span className="break-all text-xs text-muted-foreground">{entry.lastError}</span>
							<ConfirmAction
								title={`重放死信 #${entry.deliveryId}？`}
								description="订阅者可能再次处理同一事件，必须自行按事件 ID 幂等；原始事实不会重写。"
								disabled={busy !== undefined}
								onConfirm={() => {
								setBusy(entry.deliveryId);
								const commandID = commandIDs.current.get(entry.deliveryId) ?? newClientCommandId();
								commandIDs.current.set(entry.deliveryId, commandID);
								void replayPluginEventDeadletter(entry.deliveryId, commandID)
									.then(() => { commandIDs.current.delete(entry.deliveryId); notify.success("死信已重新排队"); setRevision((value) => value + 1); })
									.catch((reason) => notify.error(reason, "重放失败，请重试。"))
									.finally(() => setBusy(undefined));
							}}
							>重放</ConfirmAction>
						</div>
					))}
				</div>
			)}
		</Alert>
	);
}
function IntegrationCatalog({ navigate }: { navigate: (to: string) => void }) {
	const [search, setSearch] = useState("");
	const [catalog, setCatalog] = useState<IntegrationCatalogItem[]>([]);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
	const [revision, setRevision] = useState(0);
	useEffect(() => {
		let active = true;
		listIntegrationPlugins()
			.then((items) => {
				if (active) setCatalog(items.filter((item) => item.enabled));
			})
			.catch((reason) => {
				if (active) setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			})
			.finally(() => {
				if (active) setLoading(false);
			});
		return () => {
			active = false;
		};
	}, [revision]);
	const items = useMemo(() => {
		const needle = search.trim().toLowerCase();
		return catalog.filter((item) =>
			`${item.displayName} ${item.description}`.toLowerCase().includes(needle),
		);
	}, [catalog, search]);
	return (
		<section className="flex flex-col gap-5">
			<PluginEventDeadletters />
			<div className="flex flex-wrap items-end justify-between gap-3">
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">接入管理</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						配置已启用的平台能力。
					</p>
				</div>
				<Button
					variant="outline"
					onClick={() => navigate(`${INTEGRATIONS_BASE}?instances`)}
				>
					查看已接入实例
					<ChevronRight data-icon="inline-end" />
				</Button>
			</div>
			<Input
				value={search}
				onChange={(event) => setSearch(event.target.value)}
				placeholder="搜索支持的平台"
				aria-label="搜索支持的平台"
			/>
			{loading ? (
				<DetailSkeleton
					label="正在加载插件目录"
					rows={["card", "card", "card", "card", "card", "card"]}
				/>
			) : error ? (
				<Alert variant="destructive">
					<AlertTitle>无法加载插件目录</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
					<Button
						variant="outline"
						onClick={() => {
							setLoading(true);
							setError("");
							setRevision((value) => value + 1);
						}}
					>
						重试
					</Button>
				</Alert>
			) : items.length === 0 ? (
				<Empty>
					<EmptyHeader>
						<EmptyTitle>
							{search ? "没有匹配的平台" : "没有已启用的接入插件"}
						</EmptyTitle>
						<EmptyDescription>
							{search
								? "尝试使用平台名称重新搜索。"
								: "请检查部署中的插件启用配置。"}
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			) : (
				<div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
					{items.map((item) => (
						<CatalogCard key={item.id} item={item} navigate={navigate} />
					))}
				</div>
			)}
		</section>
	);
}

function Instances({
	navigate,
	suspended,
}: Pick<WorkspaceModuleProps, "navigate" | "suspended">) {
	const [query, setQuery] = useState("");
	// 两套权威游标按阶段遍历：先走完全部连接页，再接续事件源页。
	// 连接接口跨平台分页，单页即便全是模型连接也不能假装没有实例；
	// 插件目录读取失败则显式失败，不得静默隐藏通用 HTTP 连接。
	const list = useCursorPages<
		EventSourceInstance | MetricsInstance | HttpConnectionInstance
	>(
		async (cursor) => {
			if (cursor?.startsWith("events:")) {
				const events = await listEventSourceInstances(cursor.slice("events:".length));
				return { items: events.items, nextCursor: events.nextCursor ? `events:${events.nextCursor}` : undefined };
			}
			const catalog = await listIntegrationPlugins();
			const connections = cursor?.startsWith("connections:") ? cursor.slice("connections:".length) : undefined;
			const page = await listHttpConnectionInstances(
				["prometheus", "thanos", ...genericHttpConnectionKinds(catalog)],
				connections,
			);
			if (page.nextCursor) return { items: page.items, nextCursor: `connections:${page.nextCursor}` };
			const events = await listEventSourceInstances();
			return {
				items: [...page.items, ...events.items],
				nextCursor: events.nextCursor ? `events:${events.nextCursor}` : undefined,
			};
		},
		{ suspended, fallbackError: "暂时无法完成操作，请重试。" },
	);
	const { items, loading } = list;
	const filtered = items.filter((item) =>
		item.displayName
			.toLocaleLowerCase()
			.includes(query.trim().toLocaleLowerCase()),
	);
	const platformName = (platform: string) =>
		platform === "alertmanager"
			? "Alertmanager"
			: platform === "prometheus"
				? "Prometheus"
				: platform === "thanos"
					? "Thanos"
					: platform;
	const isMetrics = (
		item:
			| EventSourceInstance
			| MetricsInstance
			| HttpConnectionInstance,
	): item is MetricsInstance =>
		item.platform === "prometheus" || item.platform === "thanos";
	const isHttpConnection = (
		item:
			| EventSourceInstance
			| MetricsInstance
			| HttpConnectionInstance,
	): item is HttpConnectionInstance => "authType" in item;
	const statusLabel = (
		item:
			| EventSourceInstance
			| MetricsInstance
			| HttpConnectionInstance,
	) =>
		item.status === "active"
			? "已启用"
			: item.status === "revalidation_required"
				? "需要重新验证"
				: "已停用";
	return (
		<section className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
			<div className="flex items-center gap-2">
				<div className="relative flex-1">
					<Search
						className="pointer-events-none absolute top-2.5 left-3 size-4 text-muted-foreground"
						aria-hidden="true"
					/>
					<Input
						className="pl-9"
						value={query}
						onChange={(event) => setQuery(event.target.value)}
						placeholder="搜索已加载实例"
						aria-label="搜索已加载实例"
					/>
				</div>
				<Button
					size="sm"
					onClick={() => navigate(integrationRoute("prometheus"))}
					disabled={suspended}
				>
					<Plus data-icon="inline-start" />
					新建指标接入
				</Button>
			</div>
			<EntityList
				items={filtered.map((item) => ({
					id: `${item.platform}-${item.id}`,
					title: item.displayName,
					subtitle: isMetrics(item)
						? `${platformName(item.platform)} · ${item.endpoint ?? "—"}`
						: isHttpConnection(item)
							? `${item.platform} · ${authModeLabel[item.authType]} · ${item.endpoint ?? "—"}`
							: `${platformName(item.platform)} · ${formatEventTime(item.latestValidEventAt)}`,
					badge: {
						text: statusLabel(item),
						variant:
							item.status === "active"
								? ("secondary" as const)
								: ("outline" as const),
					},
					item,
				}))}
				columns={["title", "subtitle", "status"]}
				onSelect={(row) =>
					// 详情是同一抽屉内的视图，按稳定连接名寻址，与服务端 name-keyed 读取契约一致。
					navigate(instanceSheetRoute(row.item.platform, row.item.displayName))
				}
				loading={loading}
				loadingLabel="正在加载实例"
				error={list.error}
				onRetry={list.retry}
				emptyTitle={query ? "没有匹配的已加载实例" : list.hasNext ? "本页没有匹配的实例，请继续翻页" : "尚未接入实例"}
				emptyDescription={
					query
						? "请使用其他名称搜索，或继续翻页。"
						: list.hasNext
							? "连接可能位于下一页；本页只展示当前游标范围内可管理的来源。"
						: "创建接入后，在此管理探测、启用、停用与轮换。"
				}
			/>
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

/** Full-workbench one-time reveal, cleared by the owning form/detail on close or suspension.
 * The receiver YAML is an Alertmanager-specific deployment artifact; generic
 * source kinds reveal only the public receiver URL and the bearer. */
function SecretReveal({
	open,
	secret,
	receiverUrl,
	showYaml = false,
	onClose,
}: {
	open: boolean;
	secret: string;
	receiverUrl: string;
	showYaml?: boolean;
	onClose: () => void;
}) {
	const [copied, setCopied] = useState("");
	const yaml =
		showYaml && secret && receiverUrl
			? alertmanagerReceiverYaml(receiverUrl, secret)
			: "";
	async function copy(value: string, label: string) {
		try {
			await navigator.clipboard.writeText(value);
			setCopied(label);
		} catch {
			setCopied("复制失败，请手动复制。");
		}
	}
	return (
		<Dialog
			open={open}
			onOpenChange={(next) => {
				if (!next) onClose();
			}}
		>
			<DialogContent className="flex h-[100svh] max-h-none w-screen max-w-none flex-col overflow-y-auto rounded-none">
				<DialogHeader>
					<DialogTitle>一次性接收凭据</DialogTitle>
					<DialogDescription>
						关闭后不能再次查看。凭据仅保存在当前工作台内存中，不会写入浏览器存储。
					</DialogDescription>
				</DialogHeader>
				<div className="flex flex-col gap-4">
					<Field>
						<FieldLabel>公共接收地址</FieldLabel>
						<div className="flex gap-2">
							<Input readOnly value={receiverUrl} />
							<Button
								type="button"
								variant="outline"
								size="icon"
								aria-label="复制公共接收地址"
								onClick={() => void copy(receiverUrl, "已复制公共接收地址")}
							>
								<Copy />
							</Button>
						</div>
					</Field>
					<Field>
						<FieldLabel>Bearer 凭据</FieldLabel>
						<div className="flex gap-2">
							<Input readOnly value={secret} />
							<Button
								type="button"
								variant="outline"
								size="icon"
								aria-label="复制 Bearer 凭据"
								onClick={() => void copy(secret, "已复制 Bearer 凭据")}
							>
								<Copy />
							</Button>
						</div>
					</Field>
					{showYaml && (
						<Field>
							<FieldLabel>Alertmanager receiver YAML</FieldLabel>
							<pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 text-xs leading-5 whitespace-pre-wrap">
								{yaml}
							</pre>
							<Button
								type="button"
								size="sm"
								variant="outline"
								onClick={() => void copy(yaml, "已复制 YAML")}
							>
								<Copy data-icon="inline-start" />
								复制 YAML
							</Button>
						</Field>
					)}
					{copied && (
						<p role="status" className="text-sm text-muted-foreground">
							{copied}
						</p>
					)}
				</div>
				<DialogFooter>
					<Button onClick={onClose}>我已安全保存</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}

function metricsPayload(
	platform: "prometheus" | "thanos",
	baseUrl: string,
	authType: "none" | "basic" | "bearer",
	username: string,
	password: string,
	bearerToken: string,
	tlsCaPem: string,
	tlsServerName: string,
	tlsSkipVerify: boolean,
): MetricsConnectionInput {
	return {
		type: platform,
		baseUrl: baseUrl.trim(),
		authType,
		...(authType === "basic" ? { username: username.trim(), password } : {}),
		...(authType === "bearer" ? { bearerToken } : {}),
		...(tlsCaPem.trim() ? { tlsCaPem } : {}),
		...(tlsServerName.trim() ? { tlsServerName: tlsServerName.trim() } : {}),
		...(tlsSkipVerify ? { tlsSkipVerify: true } : {}),
	};
}
function MetricsForm({
	platform,
	navigate,
	suspended,
}: {
	platform: "prometheus" | "thanos";
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [name, setName] = useState("");
	const [baseUrl, setBaseUrl] = useState("");
	const [authType, setAuthType] = useState<"none" | "basic" | "bearer">("none");
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [bearerToken, setBearerToken] = useState("");
	const [tlsCaPem, setTlsCaPem] = useState("");
	const [tlsServerName, setTlsServerName] = useState("");
	const [tlsSkipVerify, setTlsSkipVerify] = useState(false);
	const [created, setCreated] = useState<MetricsInstance>();
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	const [result, setResult] = useState<MetricsInstance["lastProbe"]>();
	const displayName = platform === "prometheus" ? "Prometheus" : "Thanos";
	useEffect(() => {
		if (suspended) {
			setPassword("");
			setBearerToken("");
		}
	}, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (suspended) return;
		setSaving(true);
		setError("");
		setResult(undefined);
		try {
			let instance = created;
			if (!instance) {
				const input = metricsPayload(
					platform,
					baseUrl,
					authType,
					username,
					password,
					bearerToken,
					tlsCaPem,
					tlsServerName,
					tlsSkipVerify,
				);
				const creating = createMetricsInstance(name.trim(), input);
				setPassword("");
				setBearerToken("");
				instance = await creating;
				setCreated(instance);
			}
			const probe = await probeMetricsInstance(instance.displayName);
			if (!probe) throw new Error("未收到连通性验证结果。");
			setResult(probe);
			if (probe.outcome !== "passed" || !probe.id) {
				const diagnostic = probeDiagnostic(probe.details);
				throw new Error(
					`连通性验证未通过${diagnostic ? `：${diagnostic}` : ""}。已创建的接入保持停用；修正服务端或网络后可重新验证，不会再次创建。`,
				);
			}
			const enabled = await enableMetricsInstance(instance, probe.id);
			setCreated(enabled);
			notify.success("接入已启用");
			navigate(integrationRoute(platform, enabled.displayName));
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	const resultDiagnostic = probeDiagnostic(result?.details);
	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">
					配置 {displayName}
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					创建后验证并启用接入，随后自动观测授权范围内的监控对象。
				</p>
			</div>
			<form onSubmit={submit}>
				<Card>
					<CardHeader>
						<CardTitle>接入信息</CardTitle>
						<CardDescription>
							{created
								? `“${created.displayName}” 已创建且保持停用。重新验证不会再次创建。`
								: `${displayName} 与其他指标接入独立配置、验证和轮换。`}
						</CardDescription>
					</CardHeader>
					<CardContent>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertTitle>无法完成接入生命周期</AlertTitle>
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							{result && (
								<Alert>
									<AlertTitle>
										{result.outcome === "passed"
											? "验证通过，正在启用或已启用"
											: "验证未通过"}
									</AlertTitle>
									<AlertDescription>
										{result.finishedAt
											? `完成时间：${formatTime(result.finishedAt)}`
											: "可在修正后重新验证。"}
										{resultDiagnostic && (
											<span className="block break-all">
												诊断：{resultDiagnostic}
											</span>
										)}
									</AlertDescription>
								</Alert>
							)}
							<Field>
								<FieldLabel htmlFor={`${platform}-name`}>实例名称</FieldLabel>
								<Input
									id={`${platform}-name`}
									value={name}
									onChange={(event) => setName(event.target.value)}
									required
									maxLength={200}
									disabled={saving || suspended || Boolean(created)}
									autoFocus
								/>
								<FieldDescription>
									用于稳定识别数据来源；不能与同名接入重复。
								</FieldDescription>
							</Field>
							<Field>
								<FieldLabel htmlFor={`${platform}-url`}>端点 URL</FieldLabel>
								<Input
									id={`${platform}-url`}
									type="url"
									value={baseUrl}
									onChange={(event) => setBaseUrl(event.target.value)}
									required
									disabled={saving || suspended || Boolean(created)}
									placeholder="https://metrics.example"
								/>
								<FieldDescription>
									使用兼容 Prometheus HTTP API 的受控端点。
								</FieldDescription>
							</Field>
							<Field>
								<FieldLabel>认证方式</FieldLabel>
								<AuthTypeSelect
									value={authType}
									onChange={setAuthType}
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							{authType === "basic" && (
								<>
									<Field>
										<FieldLabel htmlFor={`${platform}-username`}>
											用户名
										</FieldLabel>
										<Input
											id={`${platform}-username`}
											value={username}
											onChange={(event) => setUsername(event.target.value)}
											required
											disabled={saving || suspended || Boolean(created)}
										/>
									</Field>
									<Field>
										<FieldLabel htmlFor={`${platform}-password`}>
											密码
										</FieldLabel>
										<Input
											id={`${platform}-password`}
											type="password"
											value={password}
											onChange={(event) => setPassword(event.target.value)}
											required
											disabled={saving || suspended || Boolean(created)}
										/>
										<FieldDescription>
											提交即清除；不会写入 URL、浏览器存储或响应显示。
										</FieldDescription>
									</Field>
								</>
							)}
							{authType === "bearer" && (
								<Field>
									<FieldLabel htmlFor={`${platform}-token`}>
										Bearer Token
									</FieldLabel>
									<Input
										id={`${platform}-token`}
										type="password"
										value={bearerToken}
										onChange={(event) => setBearerToken(event.target.value)}
										required
										disabled={saving || suspended || Boolean(created)}
									/>
									<FieldDescription>
										提交即清除；不会写入 URL、浏览器存储或响应显示。
									</FieldDescription>
								</Field>
							)}
							<Separator />
							<Field>
								<FieldLabel htmlFor={`${platform}-ca`}>
									自定义 CA（可选）
								</FieldLabel>
								<Textarea
									id={`${platform}-ca`}
									value={tlsCaPem}
									onChange={(event) => setTlsCaPem(event.target.value)}
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor={`${platform}-server-name`}>
									TLS Server Name（可选）
								</FieldLabel>
								<Input
									id={`${platform}-server-name`}
									value={tlsServerName}
									onChange={(event) => setTlsServerName(event.target.value)}
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<label className="flex items-start gap-2 text-sm">
								<input
									type="checkbox"
									checked={tlsSkipVerify}
									onChange={(event) => setTlsSkipVerify(event.target.checked)}
									disabled={saving || suspended || Boolean(created)}
								/>
								<span>跳过 TLS 证书校验（仅限已知受控环境；默认严格验证）</span>
							</label>
							<Button
								type="submit"
								disabled={
									!name.trim() ||
									!baseUrl.trim() ||
									saving ||
									suspended ||
									(!created &&
										authType === "basic" &&
										(!username.trim() || !password)) ||
									(!created && authType === "bearer" && !bearerToken)
								}
							>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
									/>
								)}
								{saving
									? "处理中…"
									: created
										? "重新验证并启用"
										: "创建、验证并启用"}
							</Button>
						</FieldGroup>
					</CardContent>
				</Card>
			</form>
			<aside className="flex flex-col gap-4">
				<h2 className="text-base font-semibold">配置说明</h2>
				<ol className="flex flex-col gap-3 text-sm text-muted-foreground">
					<li>1. 创建后接入保持停用，避免未经验证即被业务声明使用。</li>
					<li>2. 验证通过后才显式启用；失败时保留该停用对象供重试。</li>
					<li>3. 启用后查看自动观测结果，或直接开始基础巡检。</li>
				</ol>
				<Separator />
				<p className="text-sm text-muted-foreground">
					无认证、Basic、Bearer 与 TLS 字段均由服务端验证。不要将秘密写入业务
					YAML。
				</p>
			</aside>
		</section>
	);
}
function MetricsDetail({
	id,
	navigate,
	suspended,
}: {
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [item, setItem] = useState<MetricsInstance>();
	const [loading, setLoading] = useState(true);
	const [busy, setBusy] = useState("");
	const [error, setError] = useState("");
	const load = useCallback(async () => {
		setLoading(true);
		try {
			setItem(await fetchMetricsInstance(id));
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}, [id]);
	useEffect(() => {
		if (!suspended) void load();
	}, [load, suspended]);
	async function action(kind: "probe" | "enable" | "disable") {
		if (!item) return;
		setBusy(kind);
		setError("");
		try {
			if (kind === "probe" || kind === "enable") {
				const result = await probeMetricsInstance(item.displayName);
				if (!result || result.outcome !== "passed" || !result.id) {
					const diagnostic = probeDiagnostic(result?.details);
					throw new Error(
						`连通性验证未通过${diagnostic ? `：${diagnostic}` : ""}或未生成可启用的验证结果，接入保持当前状态。`,
					);
				}
				if (kind === "enable")
					setItem(await enableMetricsInstance(item, result.id));
			} else setItem(await disableMetricsInstance(item));
			if (kind === "enable") notify.success("接入已启用");
			else if (kind === "disable") notify.success("接入已停用");
			else notify.success("验证通过");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	if (loading)
		return (
			<DetailSkeleton
				label="正在加载指标接入"
				rows={["title", "line", "card", "card", "card"]}
			/>
		);
	if (!item)
		return (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法加载接入实例</AlertTitle>
					<AlertDescription>
						{error || "该实例不存在或暂时无法读取。"}
					</AlertDescription>
				</Alert>
			</section>
		);
	const needsRevalidation = item.revalidationRequired;
	const statusLabel =
		item.status === "active"
			? "已启用"
			: needsRevalidation
				? "需要重新验证"
				: "已停用";
	return (
		<section className="flex flex-col gap-6">
			{/* 抽屉头部（DetailSheet）负责实例名标题。 */}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<Card>
				<CardHeader>
					<div className="flex flex-wrap items-center justify-between gap-3">
						<div>
							<CardTitle>接入状态</CardTitle>
							<CardDescription>
								端点和凭据不回显；轮换会创建新配置与凭据代次。
							</CardDescription>
						</div>
						<Badge variant={item.status === "active" ? "secondary" : "outline"}>
							{statusLabel}
						</Badge>
					</div>
				</CardHeader>
				<CardContent className="flex flex-col gap-5">
					<PropertyList
						layout="grid-2"
						entries={[
							{
								label: "端点",
								value: (
									<span className="break-all font-medium">
										{item.endpoint ?? "—"}
									</span>
								),
							},
							{
								label: "最近验证",
								value: (
									<span className="font-medium">
										{item.lastProbe ? item.lastProbe.outcome : "尚未记录"}
									</span>
								),
							},
						]}
					/>
					{needsRevalidation && (
						<Alert>
							<AlertTitle>需要重新验证</AlertTitle>
							<AlertDescription>
								凭据已轮换。请执行验证并启用，以当前 revision
								和凭据代次的通过结果恢复使用。
							</AlertDescription>
						</Alert>
					)}
					<Separator />
					<div className="flex flex-wrap gap-2">
						<Button
							variant="outline"
							disabled={suspended || Boolean(busy)}
							onClick={() => void action("probe")}
						>
							<RefreshCw data-icon="inline-start" />
							{busy === "probe" ? "验证中…" : "验证连通性"}
						</Button>
						<Button
							disabled={
								suspended ||
								(item.status === "active" && !needsRevalidation) ||
								Boolean(busy)
							}
							onClick={() => void action("enable")}
						>
							{needsRevalidation ? "重新验证并启用" : "验证并启用"}
						</Button>
						<ConfirmAction
							title={`停用 ${item.displayName}？`}
							description="停用后不再开始新的观测、查询和巡检；配置和历史不会删除。"
							disabled={
								suspended || item.status === "disabled" || Boolean(busy)
							}
							destructive
							onConfirm={() => void action("disable")}
						>
							{busy === "disable" ? "停用中…" : "停用接入"}
						</ConfirmAction>
						<Button
							variant="outline"
							disabled={suspended || Boolean(busy)}
							onClick={() =>
								navigate(
									`${integrationRoute(item.platform, item.displayName)}/rotate`,
								)
							}
						>
							<RotateCw data-icon="inline-start" />
							轮换凭据
						</Button>
					</div>
				</CardContent>
			</Card>
		</section>
	);
}
function MetricsRotate({
	platform,
	id,
	navigate,
	suspended,
}: {
	platform: "prometheus" | "thanos";
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [item, setItem] = useState<MetricsInstance>();
	const [baseUrl, setBaseUrl] = useState("");
	const [authType, setAuthType] = useState<"none" | "basic" | "bearer">("none");
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [bearerToken, setBearerToken] = useState("");
	const [tlsCaPem, setTlsCaPem] = useState("");
	const [tlsServerName, setTlsServerName] = useState("");
	const [tlsSkipVerify, setTlsSkipVerify] = useState(false);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	useEffect(() => {
		void fetchMetricsInstance(id)
			.then((current) => {
				setItem(current);
				setBaseUrl(current.endpoint ?? "");
				setAuthType(current.authType);
				setUsername(current.username ?? "");
				setTlsCaPem(current.tlsCaPem ?? "");
				setTlsServerName(current.tlsServerName ?? "");
				setTlsSkipVerify(current.tlsSkipVerify);
			})
			.catch((reason) =>
				setError(messageOf(reason, "暂时无法完成操作，请重试。")),
			);
	}, [id]);
	useEffect(() => {
		if (suspended) {
			setPassword("");
			setBearerToken("");
		}
	}, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (!item || suspended) return;
		setSaving(true);
		setError("");
		try {
			const payload = metricsPayload(
				platform,
				baseUrl,
				authType,
				username,
				password,
				bearerToken,
				tlsCaPem,
				tlsServerName,
				tlsSkipVerify,
			);
			const rotating = rotateMetricsInstance(item, payload);
			setPassword("");
			setBearerToken("");
			await rotating;
			notify.success("已保存");
			navigate(integrationRoute(platform, id));
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">编辑 {id}</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					提交将创建新配置与凭据代次；已保存的非秘密字段已预填，旧秘密不会回显。
				</p>
			</div>
			<form onSubmit={submit}>
				<Card>
					<CardHeader>
						<CardTitle>接入信息</CardTitle>
						<CardDescription>
							可修正端点或 TLS 配置后重新验证。选择 Basic 或 Bearer
							时必须提供新秘密。
						</CardDescription>
					</CardHeader>
					<CardContent>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							<Field>
								<FieldLabel htmlFor="rotate-url">端点 URL</FieldLabel>
								<Input
									id="rotate-url"
									type="url"
									value={baseUrl}
									onChange={(event) => setBaseUrl(event.target.value)}
									required
									disabled={saving || suspended}
								/>
							</Field>
							<Field>
								<FieldLabel>认证方式</FieldLabel>
								<AuthTypeSelect
									value={authType}
									onChange={setAuthType}
									disabled={saving || suspended}
								/>
							</Field>
							{authType === "basic" && (
								<>
									<Field>
										<FieldLabel htmlFor="rotate-username">用户名</FieldLabel>
										<Input
											id="rotate-username"
											value={username}
											onChange={(event) => setUsername(event.target.value)}
											required
											disabled={saving || suspended}
										/>
									</Field>
									<Field>
										<FieldLabel htmlFor="rotate-password">密码</FieldLabel>
										<Input
											id="rotate-password"
											type="password"
											value={password}
											onChange={(event) => setPassword(event.target.value)}
											required
											disabled={saving || suspended}
										/>
										<FieldDescription>
											旧密码不会回显；提交即清除新密码。
										</FieldDescription>
									</Field>
								</>
							)}
							{authType === "bearer" && (
								<Field>
									<FieldLabel htmlFor="rotate-token">Bearer Token</FieldLabel>
									<Input
										id="rotate-token"
										type="password"
										value={bearerToken}
										onChange={(event) => setBearerToken(event.target.value)}
										required
										disabled={saving || suspended}
									/>
									<FieldDescription>
										旧 Token 不会回显；提交即清除新 Token。
									</FieldDescription>
								</Field>
							)}
							<Separator />
							<Field>
								<FieldLabel htmlFor="rotate-ca">自定义 CA（可选）</FieldLabel>
								<Textarea
									id="rotate-ca"
									value={tlsCaPem}
									onChange={(event) => setTlsCaPem(event.target.value)}
									disabled={saving || suspended}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="rotate-server-name">
									TLS Server Name（可选）
								</FieldLabel>
								<Input
									id="rotate-server-name"
									value={tlsServerName}
									onChange={(event) => setTlsServerName(event.target.value)}
									disabled={saving || suspended}
								/>
							</Field>
							<label className="flex items-start gap-2 text-sm">
								<input
									type="checkbox"
									checked={tlsSkipVerify}
									onChange={(event) => setTlsSkipVerify(event.target.checked)}
									disabled={saving || suspended}
								/>
								<span>跳过 TLS 证书校验（仅限已知受控环境）</span>
							</label>
							<Button
								type="submit"
								disabled={
									!item ||
									!baseUrl.trim() ||
									saving ||
									suspended ||
									(authType === "basic" && (!username.trim() || !password)) ||
									(authType === "bearer" && !bearerToken)
								}
							>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
										aria-hidden="true"
									/>
								)}
								{saving ? "保存中…" : "保存新版本"}
							</Button>
						</FieldGroup>
					</CardContent>
				</Card>
			</form>
		</section>
	);
}

function AlertmanagerForm({
	navigate,
	suspended,
}: Pick<WorkspaceModuleProps, "navigate" | "suspended">) {
	const [key, setKey] = useState("");
	const [createdKey, setCreatedKey] = useState("");
	const [saving, setSaving] = useState(false);
	// 创建失败必须以常驻内联错误留在表单上：一次性凭据只有这一条取得途径，
	// 只靠自动消失的 toast 会让失败表现得像“什么都没发生”。
	const [error, setError] = useState("");
	const [secret, setSecret] = useState("");
	const [receiverUrl, setReceiverUrl] = useState("");
	const revealEpoch = useRef(0);
	useEffect(() => {
		if (suspended) {
			revealEpoch.current += 1;
			setSecret("");
			setReceiverUrl("");
		}
	}, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (suspended) return;
		const epoch = revealEpoch.current;
		setSaving(true);
		setError("");
		try {
			const endpoint = await fetchPublicReceiverEndpoint();
			const sourceKey = key.trim();
			const result = await createEventSourceInstance(sourceKey, "alertmanager");
			setCreatedKey(sourceKey);
			if (!result.revealHandle)
				throw new Error(
					"来源已创建，但服务端未返回一次性凭据句柄。请从实例详情轮换凭据。",
				);
			const token = await revealEventSourceCredential(result.revealHandle);
			if (!suspended && epoch === revealEpoch.current) {
				setSecret(token);
				setReceiverUrl(endpoint.publicReceiverUrl);
				setKey("");
			}
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSaving(false);
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">
					配置 Alertmanager
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					创建逻辑告警源，生成面向部署公共入口的 receiver 配置。
				</p>
			</div>
			<div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(18rem,.75fr)]">
				<form onSubmit={submit}>
					<Card>
						<CardHeader>
							<CardTitle>来源信息</CardTitle>
							<CardDescription>
								来源键用于稳定识别一个 Alertmanager 集群。
							</CardDescription>
						</CardHeader>
						<CardContent>
							<FieldGroup>
								{error && (
									<Alert variant="destructive">
										<AlertTitle>无法创建告警源</AlertTitle>
										<AlertDescription>{error}</AlertDescription>
									</Alert>
								)}
								<Field>
									<FieldLabel htmlFor="alertmanager-key">来源键</FieldLabel>
									<Input
										id="alertmanager-key"
										value={key}
										onChange={(event) => setKey(event.target.value)}
										required
										maxLength={200}
										disabled={saving || suspended || Boolean(createdKey)}
										autoFocus
									/>
									<FieldDescription>
										例如 production-alertmanager。不要输入凭据或内部地址。
									</FieldDescription>
								</Field>
								<Button
									type="submit"
									disabled={
										!key.trim() || saving || suspended || Boolean(createdKey)
									}
								>
									{saving && (
										<LoaderCircle
											className="animate-spin"
											data-icon="inline-start"
										/>
									)}
									{saving ? "创建中…" : "创建并显示一次凭据"}
								</Button>
							</FieldGroup>
						</CardContent>
					</Card>
				</form>
				<aside className="flex flex-col gap-4">
					<h2 className="text-base font-semibold">部署说明</h2>
					<ol className="flex flex-col gap-3 text-sm text-muted-foreground">
						<li>1. 创建后立即复制一次性 Bearer 凭据和 receiver YAML。</li>
						<li>2. 将 YAML 合并到上游 Alertmanager 的 receiver 与路由配置。</li>
						<li>
							3. 发送测试事件，页面会显示最近有效事件。等待首条事件不是故障。
						</li>
					</ol>
					<Separator />
					<p className="text-sm text-muted-foreground">
						Stele 本地持久入队后即确认投递；Quoin 会异步处理。认证失败或入口不可用时请由 Alertmanager 重试。
					</p>
				</aside>
			</div>
			<SecretReveal
				open={!suspended && Boolean(secret)}
				secret={secret}
				receiverUrl={receiverUrl}
				showYaml
				onClose={() => {
					setSecret("");
					setReceiverUrl("");
					navigate(`${INTEGRATIONS_BASE}?instances`);
				}}
			/>
		</section>
	);
}

/** Alert delivery faults belong with the Alertmanager lifecycle that produces them. */
function AlertIntakeIssues({
	suspended,
}: Pick<WorkspaceModuleProps, "suspended">) {
	const [items, setItems] = useState<IntakeIssue[]>([]);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState<string>();
	const [loading, setLoading] = useState(true);
	const load = useCallback(async () => {
		if (suspended) {
			setLoading(false);
			return;
		}
		try {
			setLoading(true);
			setError("");
			setItems((await fetchIntakeIssues()).items);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}, [suspended]);
	useEffect(() => {
		void load();
	}, [load]);
	async function acknowledge(item: IntakeIssue) {
		if (suspended) return;
		try {
			setBusy(item.id);
			setError("");
			await acknowledgeIntakeIssue(item.id, item.rowVersion);
			notify.success("已确认");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(undefined);
		}
	}
	return (
		<section className="flex flex-col gap-5">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">告警接入问题</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					查看上游投递异常并确认已处理的问题。
				</p>
			</div>
			{loading ? (
				<DetailSkeleton
					label="正在加载接入问题"
					rows={["line", "line", "line", "line"]}
				/>
			) : (
				<>
					{error && (
						<Alert variant="destructive">
							<AlertDescription>{error}</AlertDescription>
						</Alert>
					)}
					{!error && items.length === 0 ? (
						<Empty>
							<EmptyHeader>
								<EmptyTitle>没有待处理接入问题</EmptyTitle>
								<EmptyDescription>
									上游投递正常，新异常会出现在这里。
								</EmptyDescription>
							</EmptyHeader>
						</Empty>
					) : (
						<EntityList
							items={items.map((item) => ({
								id: item.id,
								title: item.issueKey,
								subtitle: `类型 ${item.kind} · 出现 ${item.occurrenceCount} 次`,
								issue: item,
							}))}
							columns={["title", "subtitle", "actions"]}
							renderActions={(row) => (
								<Button
									size="sm"
									variant="outline"
									disabled={suspended || busy === row.issue.id}
									onClick={() => void acknowledge(row.issue)}
								>
									{busy === row.issue.id ? (
										<>
											<LoaderCircle
												className="animate-spin"
												data-icon="inline-start"
												aria-hidden="true"
											/>
											确认中…
										</>
									) : (
										"确认"
									)}
								</Button>
							)}
							emptyTitle="没有待处理接入问题"
						/>
					)}
				</>
			)}
		</section>
	);
}

const credentialStateLabels: Record<string, string> = {
	Active: "使用中",
	PendingRetirement: "待退休",
	Retired: "已退休",
};

/** Per-instance non-secret settings card (ADR-0014 story 2): read view plus
 * row-version-guarded editing. Rendered only when the owning plugin declares
 * a settings schema with fields; catalog outages degrade to no card instead
 * of blocking the lifecycle drawer. Never displays credential material. */
function EventSourceSettingsCard({
	kind,
	source,
	onSaved,
	onReload,
	suspended,
}: {
	kind: string;
	source: EventSourceInstance;
	onSaved: (updated: EventSourceInstance) => void;
	onReload: () => Promise<void>;
	suspended: boolean;
}) {
	const entry = useCatalogEntry(kind, "sourceKind");
	const specs = useMemo(
		() => settingsFieldSpecs(entry.item?.eventSourceConfigSchema),
		[entry.item],
	);
	const [editing, setEditing] = useState(false);
	const [draft, setDraft] = useState<SettingsDraft>(() => emptyDraft(specs));
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	const [stale, setStale] = useState(false);
	// Entering edit mode (and any parent reload while editing) re-syncs the
	// draft from the authoritative document; failed saves keep local edits.
	useEffect(() => {
		if (editing) setDraft(draftFromSettings(specs, source.settings));
	}, [editing, specs, source]);
	if (!entry.ready || !entry.item || specs.length === 0) return null;
	const current = source.settings ?? {};
	const entries = Object.entries(current);
	async function save() {
		const validated = settingsFromDraft(specs, draft);
		if (!validated.ok) {
			setStale(false);
			setError(validated.error);
			return;
		}
		setSaving(true);
		setError("");
		try {
			const updated = await setEventSourceSettings(
				source.id,
				validated.settings,
				source.rowVersion,
			);
			setEditing(false);
			setError("");
			setStale(false);
			notify.success("设置已保存");
			onSaved(updated);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			// A 409 means another operator advanced the document; offer the
			// reload-retry recovery instead of leaving a dead end.
			setStale(
				reason instanceof WorkbenchApiError &&
					(reason.status === 409 || reason.code === "row_version_conflict"),
			);
		} finally {
			setSaving(false);
		}
	}
	async function reloadAndRetry() {
		setError("");
		setStale(false);
		await onReload();
	}
	return (
		<Card>
			<CardHeader>
				<CardTitle>来源设置</CardTitle>
				<CardDescription>
					非秘密实例参数，由插件声明的封闭 schema
					校验；保存以行版本防并发，凭据永远不会出现在这里。
				</CardDescription>
			</CardHeader>
			<CardContent className="flex flex-col gap-4">
				{!editing ? (
					<>
						{entries.length === 0 ? (
							<p className="text-sm text-muted-foreground">
								尚未配置任何设置；保存空文档等同于清除全部参数。
							</p>
						) : (
							<PropertyList
								layout="grid-2"
								entries={entries.map(([settingKey, value]) => ({
									label: settingKey,
									value: (
										<span className="break-all font-medium">
											{formatSettingValue(value)}
										</span>
									),
								}))}
							/>
						)}
						<div>
							<Button
								variant="outline"
								disabled={suspended}
								onClick={() => {
									setError("");
									setStale(false);
									setEditing(true);
								}}
							>
								编辑设置
							</Button>
						</div>
					</>
				) : (
					<form
						onSubmit={(event) => {
							event.preventDefault();
							void save();
						}}
					>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertTitle>无法保存设置</AlertTitle>
									<AlertDescription>
										{error}
										{stale &&
											" 本地修改已保留；加载最新设置后可直接重试。"}
									</AlertDescription>
									{stale && (
										<Button
											type="button"
											variant="outline"
											size="sm"
											className="mt-3"
											disabled={saving || suspended}
											onClick={() => void reloadAndRetry()}
										>
											<RefreshCw data-icon="inline-start" />
											重新加载最新设置
										</Button>
									)}
								</Alert>
							)}
							<SettingsFieldsEditor
								specs={specs}
								draft={draft}
								onChange={(changed, value) =>
									setDraft((current) => ({ ...current, [changed]: value }))
								}
								disabled={saving || suspended}
								idPrefix="event-source-settings-edit"
							/>
						<div className="flex flex-wrap gap-2">
							<Button type="submit" disabled={saving || suspended}>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
									/>
								)}
								{saving ? "保存中…" : "保存设置"}
							</Button>
							<Button
								type="button"
								variant="outline"
								disabled={saving || suspended}
								onClick={() => {
									setEditing(false);
									setError("");
									setStale(false);
								}}
							>
								取消
							</Button>
						</div>
						</FieldGroup>
					</form>
				)}
			</CardContent>
		</Card>
	);
}

/** Shared lifecycle detail for every alert event source: status, one-time
 * credential reveal on rotate, and credential-generation management. The
 * receiver YAML in the reveal dialog stays exclusive to alertmanager. */
function EventSourceDetail({
	kind,
	id,
	suspended,
}: {
	kind: string;
	id: string;
	suspended: boolean;
}) {
	const [source, setSource] = useState<EventSourceInstance>();
	const [credentials, setCredentials] = useState<EventSourceCredential[]>([]);
	const [loading, setLoading] = useState(true);
	const [busy, setBusy] = useState("");
	const [error, setError] = useState("");
	const [secret, setSecret] = useState("");
	const [receiverUrl, setReceiverUrl] = useState("");
	const revealEpoch = useRef(0);
	const load = useCallback(async () => {
		// 刷新不回退到骨架屏：已展示的状态卡（含设置编辑草稿）保持挂载，
		// 数据原地更新；初始加载仍由 loading 骨架覆盖。
		setError("");
		try {
			const [sourceItem, credentialItems] = await Promise.all([
				fetchEventSourceInstance(id),
				listEventSourceCredentials(id),
			]);
			setSource(sourceItem);
			setCredentials(credentialItems);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}, [id]);
	useEffect(() => {
		if (!suspended) void load();
		else {
			revealEpoch.current += 1;
			setSecret("");
			setReceiverUrl("");
		}
	}, [load, suspended]);
	async function rotate() {
		if (!source) return;
		const epoch = revealEpoch.current;
		setBusy("rotate");
		setError("");
		try {
			const endpoint = await fetchPublicReceiverEndpoint(source.platform);
			const result = await rotateEventSourceCredential(id);
			if (!result.revealHandle)
				throw new Error(
					"凭据已轮换，但服务端未返回一次性凭据句柄。请再次轮换取得新值。",
				);
			const token = await revealEventSourceCredential(result.revealHandle);
			if (!suspended && epoch === revealEpoch.current) {
				setSecret(token);
				setReceiverUrl(endpoint.publicReceiverUrl);
			}
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	async function disable() {
		if (!source) return;
		setBusy("disable");
		try {
			await disableEventSourceInstance(source);
			notify.success("已停用");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	async function retire(credential: EventSourceCredential) {
		setBusy(credential.id);
		try {
			await retireEventSourceCredential(id, credential);
			notify.success("已退休");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	if (loading)
		return (
			<DetailSkeleton
				label="正在加载接入实例"
				rows={["title", "line", "card", "card", "card"]}
			/>
		);
	if (!source)
		return (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法加载接入实例</AlertTitle>
					<AlertDescription>
						{error || "该实例不存在或暂时无法读取。"}
					</AlertDescription>
				</Alert>
			</section>
		);
	return (
		<section className="flex flex-col gap-6">
			{/* 抽屉头部（DetailSheet）负责实例名标题。 */}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(18rem,.75fr)]">
				<Card>
					<CardHeader>
						<div className="flex flex-wrap items-center justify-between gap-3">
							<div>
								<CardTitle>接入状态</CardTitle>
								<CardDescription>接入状态和事件状态独立展示。</CardDescription>
							</div>
							<Badge
								variant={source.status === "active" ? "secondary" : "outline"}
							>
								{source.status === "active" ? "已启用" : "已停用"}
							</Badge>
						</div>
					</CardHeader>
					<CardContent className="flex flex-col gap-5">
						<PropertyList
							layout="grid-2"
							entries={[
								{
									label: "创建时间",
									value: (
										<span className="font-medium">
											{formatTime(source.createdAt)}
										</span>
									),
								},
								{
									label: "最近有效事件",
									value: (
										<span className="font-medium">
											{formatEventTime(source.latestValidEventAt)}
										</span>
									),
								},
							]}
						/>
						<Separator />
						<div className="flex flex-wrap gap-2">
							<Button
								variant="outline"
								disabled={suspended || Boolean(busy)}
								onClick={() => void load()}
							>
								<RefreshCw data-icon="inline-start" />
								刷新事件状态
							</Button>
							<Button
								disabled={
									suspended || source.status !== "active" || Boolean(busy)
								}
								onClick={() => void rotate()}
							>
								<RotateCw data-icon="inline-start" />
								{busy === "rotate" ? "轮换中…" : "轮换凭据"}
							</Button>
							<ConfirmAction
								title={`停用 ${source.displayName}？`}
								description="停用后此来源不再接收告警；已保存的历史不会删除。"
								disabled={
									suspended || source.status !== "active" || Boolean(busy)
								}
								destructive
								onConfirm={() => void disable()}
							>
								停用来源
							</ConfirmAction>
						</div>
					</CardContent>
				</Card>
				<aside className="flex flex-col gap-4">
					<h2 className="text-base font-semibold">轮换说明</h2>
					<p className="text-sm text-muted-foreground">
						更新上游配置，确认新凭据已出现首次有效事件，再显式退休旧凭据。系统不会猜测切换已完成。
					</p>
					<Separator />
					<p className="text-sm text-muted-foreground">
						没有事件仅表示仍在等待上游投递，不会自动标记此来源故障。
					</p>
				</aside>
			</div>
			<EventSourceSettingsCard
				kind={kind}
				source={source}
				onSaved={setSource}
				onReload={load}
				suspended={suspended}
			/>
			<section className="flex flex-col gap-3">
				<div>
					<h2 className="text-base font-semibold">凭据代次</h2>
					<p className="mt-1 text-sm text-muted-foreground">
						仅显示非秘密元数据。仅“等待退休”凭据可在确认切换后退休。
					</p>
				</div>
				{credentials.length === 0 ? (
					<Empty className="min-h-40">
						<EmptyHeader>
							<EmptyTitle>没有凭据元数据</EmptyTitle>
							<EmptyDescription>
								请刷新，或检查来源是否已被退休。
							</EmptyDescription>
						</EmptyHeader>
					</Empty>
				) : (
					<EntityList
						items={credentials.map((credential) => ({
							id: credential.id,
							title: credential.id,
							subtitle: `创建 ${formatTime(credential.createdAt)} · 首次使用 ${formatTime(credential.firstUsedAt)}`,
							badge: {
								text:
									credentialStateLabels[credential.state] ?? credential.state,
								variant:
									credential.state === "Retired"
										? ("outline" as const)
										: ("secondary" as const),
							},
							credential,
						}))}
						columns={["title", "subtitle", "status", "actions"]}
						renderActions={(row) =>
							row.credential.state === "PendingRetirement" ? (
								<ConfirmAction
									title="退休此凭据？"
									description="确认新凭据已经在上游使用。退休后旧凭据无法恢复。"
									disabled={suspended || Boolean(busy)}
									destructive
									onConfirm={() => void retire(row.credential)}
								>
									{busy === row.credential.id ? "退休中…" : "退休"}
								</ConfirmAction>
							) : null
						}
						emptyTitle="没有凭据元数据"
						emptyDescription="请刷新，或检查来源是否已被退休。"
					/>
				)}
			</section>
			<SecretReveal
				open={!suspended && Boolean(secret)}
				secret={secret}
				receiverUrl={receiverUrl}
				showYaml={kind === "alertmanager"}
				onClose={() => {
					setSecret("");
					setReceiverUrl("");
				}}
			/>
		</section>
	);
}

/** Shared unknown-route view for invalid integration paths. Unregistered or
 * disabled source kinds are ordinary unknown routes, not empty forms. */
function IntegrationNotFound() {
	return (
		<Empty>
			<EmptyHeader>
				<EmptyTitle>找不到此页面</EmptyTitle>
				<EmptyDescription>该链接无效或页面已被移动。</EmptyDescription>
			</EmptyHeader>
		</Empty>
	);
}

/** Loads the server plugin catalog and resolves one entry; shared by every
 * catalog-validated generic route. Plugin-id routes resolve by `id`, the
 * connection rotate/detail routes resolve by the registered connectionKind,
 * and event-source detail routes resolve by the registered sourceKind —
 * three distinct identities (#110) that must never collapse into each other. */
function useCatalogEntry(
	kind: string,
	by: "id" | "connectionKind" | "sourceKind",
): {
	ready: boolean;
	item?: IntegrationCatalogItem;
	error: string;
	retry: () => void;
} {
	const [catalog, setCatalog] = useState<IntegrationCatalogItem[]>();
	const [error, setError] = useState("");
	const [revision, setRevision] = useState(0);
	useEffect(() => {
		let active = true;
		listIntegrationPlugins()
			.then((items) => {
				if (active) setCatalog(items);
			})
			.catch((reason) => {
				if (active) setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			});
		return () => {
			active = false;
		};
	}, [revision]);
	const item = catalog?.find((candidate) =>
		by === "id"
			? candidate.id === kind
			: by === "connectionKind"
				? candidate.connectionKind === kind
				: candidate.sourceKind === kind,
	);
	return {
		ready: Boolean(catalog),
		item,
		error,
		retry: () => {
			setError("");
			setRevision((value) => value + 1);
		},
	};
}

/** Shared gate for catalog-validated routes: loading/error states, then the
 * not-found view when the entry is missing or fails its capability check. */
function CatalogGate({
	entry,
	children,
}: {
	entry: ReturnType<typeof useCatalogEntry>;
	children: (item: IntegrationCatalogItem) => ReactNode;
}) {
	if (!entry.ready) {
		return entry.error ? (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法加载插件目录</AlertTitle>
					<AlertDescription>{entry.error}</AlertDescription>
					<Button variant="outline" onClick={entry.retry}>
						重试
					</Button>
				</Alert>
			</section>
		) : (
			<DetailSkeleton
				label="正在加载插件目录"
				rows={["title", "line", "card"]}
			/>
		);
	}
	if (!entry.item) return <IntegrationNotFound />;
	return <>{children(entry.item)}</>;
}

/** Generic admin route for any catalog plugin that has no specialized brand
 * workbench. The plugin id is validated against the server catalog before any
 * form renders: event-source-only plugins get the source form, http_connection
 * plugins get the connection manager, and a plugin with both keeps both
 * affordances side by side. Everything else fails closed. */
function GenericPluginRoute({
	kind,
	navigate,
	suspended,
}: {
	kind: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const entry = useCatalogEntry(kind, "id");
	return (
		<CatalogGate entry={entry}>
			{(item) => {
				const eventSourceReady =
					item.enabled &&
					Boolean(item.sourceKind) &&
					item.capabilities.includes(EVENT_SOURCE_CAPABILITY) &&
					item.capabilities.includes("alert_normalizer");
				const httpReady = isHttpConnectionCatalogItem(item);
				if (eventSourceReady && httpReady)
					return (
						<CombinedPluginRoute
							item={item}
							navigate={navigate}
							suspended={suspended}
						/>
					);
				if (httpReady)
					return (
						<HttpConnectionManager
							item={item}
							navigate={navigate}
							suspended={suspended}
						/>
					);
				if (eventSourceReady)
					return (
						<EventSourceForm
							item={item}
							kind={item.sourceKind as string}
							navigate={navigate}
							suspended={suspended}
						/>
					);
				return <IntegrationNotFound />;
			}}
		</CatalogGate>
	);
}

/** A plugin that both receives events and owns an HTTP connection kind keeps
 * both affordances on one page; neither replaces the other (#110). */
function CombinedPluginRoute({
	item,
	navigate,
	suspended,
}: {
	item: IntegrationCatalogItem;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	return (
		<section className="flex flex-col gap-8">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">
					配置 {item.displayName}
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					该插件同时提供事件接入与受控 HTTP
					连接；两类能力独立配置、互不替代。
				</p>
			</div>
			<HttpConnectionManager
				item={item}
				navigate={navigate}
				suspended={suspended}
				embedded
			/>
			<Separator />
			<EventSourceForm
				item={item}
				kind={item.sourceKind as string}
				navigate={navigate}
				suspended={suspended}
				embedded
			/>
		</section>
	);
}

/** Request body for one generic HTTP connection; mirrors the shared bounded
 * HTTP contract (credentials ride request-only and never echo back). */
function httpConnectionPayload(
	kind: string,
	baseUrl: string,
	authType: AuthType,
	username: string,
	password: string,
	bearerToken: string,
	tlsCaPem: string,
	tlsServerName: string,
	tlsSkipVerify: boolean,
): HttpConnectionInput {
	return {
		type: kind,
		baseUrl: baseUrl.trim(),
		authType,
		...(authType === "basic" ? { username: username.trim(), password } : {}),
		...(authType === "bearer" ? { bearerToken } : {}),
		...(tlsCaPem.trim() ? { tlsCaPem } : {}),
		...(tlsServerName.trim() ? { tlsServerName: tlsServerName.trim() } : {}),
		...(tlsSkipVerify ? { tlsSkipVerify: true } : {}),
	};
}

/** Full management page for one generic HTTP connection kind: the existing
 * independent instances plus the create → probe → enable lifecycle (#110).
 * Enablement always requires a passed real probe over the current pair; a
 * kind without a declared probe path cannot enable new instances at all. */
function HttpConnectionManager({
	item,
	navigate,
	suspended,
	embedded = false,
}: {
	item: IntegrationCatalogItem;
	navigate: (to: string) => void;
	suspended: boolean;
	embedded?: boolean;
}) {
	const kind = item.connectionKind as string;
	const modes = allowedAuthModes(item.connectionAuthModes);
	const canEnable = Boolean(item.connectionProbePath);
	// 游标翻页复用 useCursorPages：服务端对全部连接按名称整体分页且无 kind
	// 过滤（HTTP-PAGE-001），每页按 kind 过滤后可能为空但仍有下一页；空页
	// 不得渲染“尚未创建”空态，而是引导继续翻页，避免把后续页的实例误报
	// 为不存在。保存/启用后回第一页重读（refresh 语义与保存后刷新一致）。
	const list = useCursorPages(
		(cursor) => listHttpConnectionInstances([kind], cursor),
		{
			suspended,
			resetKey: kind,
			fallbackError: "暂时无法加载连接实例，请重试。",
		},
	);
	const instances = list.items;
	const [name, setName] = useState("");
	const [baseUrl, setBaseUrl] = useState("");
	const [authType, setAuthType] = useState<AuthType>(modes[0] ?? "none");
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [bearerToken, setBearerToken] = useState("");
	const [tlsCaPem, setTlsCaPem] = useState("");
	const [tlsServerName, setTlsServerName] = useState("");
	const [tlsSkipVerify, setTlsSkipVerify] = useState(false);
	const [created, setCreated] = useState<HttpConnectionInstance>();
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	const [result, setResult] = useState<ConnectionProbeObservation>();
	useEffect(() => {
		if (suspended) {
			setPassword("");
			setBearerToken("");
		}
	}, [suspended]);
	function resetForm() {
		setName("");
		setBaseUrl("");
		setUsername("");
		setTlsCaPem("");
		setTlsServerName("");
		setTlsSkipVerify(false);
		setCreated(undefined);
		setResult(undefined);
	}
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (suspended) return;
		setSaving(true);
		setError("");
		setResult(undefined);
		try {
			let instance = created;
			if (!instance) {
				const payload = httpConnectionPayload(
					kind,
					baseUrl,
					authType,
					username,
					password,
					bearerToken,
					tlsCaPem,
					tlsServerName,
					tlsSkipVerify,
				);
				const creating = createHttpConnectionInstance(name.trim(), payload);
				setPassword("");
				setBearerToken("");
				instance = await creating;
				setCreated(instance);
			}
			if (!canEnable) {
				// Fail closed in the UI: without a plugin-declared probe path the
				// server refuses to qualify this kind, so the instance stays
				// disabled instead of pretending to be a complete integration.
				resetForm();
				void list.refresh();
				notify.success("连接已创建并保持停用");
				return;
			}
			const probe = await probeHttpConnectionInstance(instance.displayName);
			if (!probe) throw new Error("未收到连通性验证结果。");
			setResult(probe);
			if (probe.outcome !== "passed" || !probe.id) {
				const diagnostic = probeDiagnostic(probe.details);
				throw new Error(
					`连通性验证未通过${diagnostic ? `：${diagnostic}` : ""}。已创建的接入保持停用；修正服务端或网络后可重新验证，不会再次创建。`,
				);
			}
			await enableHttpConnectionInstance(instance, probe.id);
			resetForm();
			void list.refresh();
			notify.success("接入已启用");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	const resultDiagnostic = probeDiagnostic(result?.details);
	const heading = embedded ? (
		<h2 className="text-base font-semibold">HTTP 连接</h2>
	) : (
		<div>
			<h1 className="text-2xl font-semibold tracking-tight">
				配置 {item.displayName}
			</h1>
			<p className="mt-1 text-sm text-muted-foreground">
				创建多个相互独立的 HTTP 连接实例；启用前必须通过插件声明的只读探测。
			</p>
		</div>
	);
	return (
		<section className="flex flex-col gap-6">
			{heading}
			<section className="flex flex-col gap-3">
				<div>
					<h3 className="text-sm font-medium">已创建的连接实例</h3>
					<p className="mt-1 text-xs text-muted-foreground">
						同一连接类型可创建多个相互独立的实例；点击查看状态、停用或轮换。
					</p>
				</div>
				{/* 空页 ≠ 不存在：后续页可能仍有该类型实例，诚实区分三种空。 */}
				<EntityList
					items={instances.map((instance) => ({
						id: instance.id,
						title: instance.displayName,
						subtitle: `${authModeLabel[instance.authType]} · ${instance.endpoint ?? "—"}`,
						badge: {
							text:
								instance.status === "active"
									? "已启用"
									: instance.status === "revalidation_required"
										? "需要重新验证"
										: "已停用",
							variant:
								instance.status === "active"
									? ("secondary" as const)
									: ("outline" as const),
						},
						item: instance,
					}))}
					columns={["title", "subtitle", "status"]}
					onSelect={(row) =>
						navigate(
							instanceSheetRoute(
								row.item.platform,
								row.item.displayName,
							),
						)
					}
					error={list.error}
					onRetry={list.retry}
					emptyTitle={
						list.hasNext
							? "本页没有该类型的连接实例"
							: list.page > 1
								? "已浏览全部连接实例"
								: "尚未创建连接实例"
					}
					emptyDescription={
						list.hasNext
							? "后续页可能仍包含该类型的实例；请点击“下一页”继续查看。"
							: list.page > 1
								? "后续页没有更多该类型的实例。"
								: "使用下方表单创建第一个连接。"
					}
				/>
				<CursorPagination
					page={list.page}
					hasPrev={list.hasPrev}
					hasNext={list.hasNext}
					loading={list.navigating}
					onPrev={list.goPrev}
					onNext={list.goNext}
				/>
			</section>
			<form onSubmit={submit}>
				<Card>
					<CardHeader>
						<CardTitle>{created ? "重新验证" : "新建连接"}</CardTitle>
						<CardDescription>
							{created
								? `“${created.displayName}” 已创建且保持停用。重新验证不会再次创建。`
								: canEnable
									? `创建后保持停用；通过插件声明的只读探测（GET ${item.connectionProbePath}，期望 200）后才能启用。`
									: "该连接类型未声明探测路径，新实例无法启用；创建仅用于已有授权消费的停用配置。"}
						</CardDescription>
					</CardHeader>
					<CardContent>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertTitle>无法完成接入生命周期</AlertTitle>
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							{result && (
								<Alert>
									<AlertTitle>
										{result.outcome === "passed"
											? "验证通过，正在启用或已启用"
											: "验证未通过"}
									</AlertTitle>
									<AlertDescription>
										{result.finishedAt
											? `完成时间：${formatTime(result.finishedAt)}`
											: "可在修正后重新验证。"}
										{resultDiagnostic && (
											<span className="block break-all">
												诊断：{resultDiagnostic}
											</span>
										)}
									</AlertDescription>
								</Alert>
							)}
							<Field>
								<FieldLabel htmlFor="http-connection-name">
									实例名称
								</FieldLabel>
								<Input
									id="http-connection-name"
									value={name}
									onChange={(event) => setName(event.target.value)}
									required
									maxLength={200}
									disabled={saving || suspended || Boolean(created)}
									autoFocus
								/>
								<FieldDescription>
									用于稳定识别该实例；同一连接类型可创建多个实例，不能重名。
								</FieldDescription>
							</Field>
							<Field>
								<FieldLabel htmlFor="http-connection-url">
									端点 URL
								</FieldLabel>
								<Input
									id="http-connection-url"
									type="url"
									value={baseUrl}
									onChange={(event) => setBaseUrl(event.target.value)}
									required
									disabled={saving || suspended || Boolean(created)}
									placeholder="https://platform.example"
								/>
								<FieldDescription>
									该连接类型绑定的外部平台受控端点。
								</FieldDescription>
							</Field>
							<Field>
								<FieldLabel>认证方式</FieldLabel>
								<AuthTypeSelect
									value={authType}
									onChange={setAuthType}
									disabled={saving || suspended || Boolean(created)}
									modes={modes}
								/>
								<FieldDescription>
									仅提供插件声明支持的认证方式。
								</FieldDescription>
							</Field>
							{authType === "basic" && (
								<>
									<Field>
										<FieldLabel htmlFor="http-connection-username">
											用户名
										</FieldLabel>
										<Input
											id="http-connection-username"
											value={username}
											onChange={(event) => setUsername(event.target.value)}
											required
											disabled={saving || suspended || Boolean(created)}
										/>
									</Field>
									<Field>
										<FieldLabel htmlFor="http-connection-password">
											密码
										</FieldLabel>
										<Input
											id="http-connection-password"
											type="password"
											value={password}
											onChange={(event) => setPassword(event.target.value)}
											required
											disabled={saving || suspended || Boolean(created)}
										/>
										<FieldDescription>
											提交即清除；不会写入 URL、浏览器存储或响应显示。
										</FieldDescription>
									</Field>
								</>
							)}
							{authType === "bearer" && (
								<Field>
									<FieldLabel htmlFor="http-connection-token">
										Bearer Token
									</FieldLabel>
									<Input
										id="http-connection-token"
										type="password"
										value={bearerToken}
										onChange={(event) => setBearerToken(event.target.value)}
										required
										disabled={saving || suspended || Boolean(created)}
									/>
									<FieldDescription>
										提交即清除；不会写入 URL、浏览器存储或响应显示。
									</FieldDescription>
								</Field>
							)}
							<Separator />
							<Field>
								<FieldLabel htmlFor="http-connection-ca">
									自定义 CA（可选）
								</FieldLabel>
								<Textarea
									id="http-connection-ca"
									value={tlsCaPem}
									onChange={(event) => setTlsCaPem(event.target.value)}
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="http-connection-server-name">
									TLS Server Name（可选）
								</FieldLabel>
								<Input
									id="http-connection-server-name"
									value={tlsServerName}
									onChange={(event) => setTlsServerName(event.target.value)}
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<label className="flex items-start gap-2 text-sm">
								<input
									type="checkbox"
									checked={tlsSkipVerify}
									onChange={(event) => setTlsSkipVerify(event.target.checked)}
									disabled={saving || suspended || Boolean(created)}
								/>
								<span>
									跳过 TLS 证书校验（仅限已知受控环境；默认严格验证）
								</span>
							</label>
							<Button
								type="submit"
								disabled={
									!name.trim() ||
									!baseUrl.trim() ||
									saving ||
									suspended ||
									(!created &&
										authType === "basic" &&
										(!username.trim() || !password)) ||
									(!created && authType === "bearer" && !bearerToken)
								}
							>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
									/>
								)}
								{saving
									? "处理中…"
									: created
										? "重新验证并启用"
										: canEnable
											? "创建、验证并启用"
											: "创建（保持停用）"}
							</Button>
						</FieldGroup>
					</CardContent>
				</Card>
			</form>
			<aside className="flex flex-col gap-4">
				<h2 className="text-base font-semibold">配置说明</h2>
				<ol className="flex flex-col gap-3 text-sm text-muted-foreground">
					<li>1. 创建后接入保持停用，避免未经验证即被业务使用。</li>
					<li>
						2. 探测地址由插件声明冻结（只读 GET），不能由实例配置修改；
						未声明探测路径的类型不能启用新实例。
					</li>
					<li>
						3. 启用必须引用当前 revision 与凭据代次的通过结果；轮换后需重新验证。
					</li>
				</ol>
				<Separator />
				<p className="text-sm text-muted-foreground">
					凭据仅用于本次提交，服务端加密保存后立即丢弃；页面与错误信息不会回显任何秘密。
				</p>
			</aside>
		</section>
	);
}

/** One schema-projected settings field: native controls for the common
 * string/enum/number/boolean/array-of-string vocabulary, a JSON textarea
 * for anything the plugin schema declares beyond it (never a per-brand
 * branch). Field identity is the schema property key. */
function SettingsFieldEditor({
	spec,
	value,
	onChange,
	disabled,
	id,
}: {
	spec: SettingsFieldSpec;
	value: SettingsDraftValue;
	onChange: (value: SettingsDraftValue) => void;
	disabled?: boolean;
	id: string;
}) {
	if (spec.kind === "boolean")
		return (
			<label className="flex items-start gap-2 text-sm">
				<input
					type="checkbox"
					checked={value === true}
					onChange={(event) => onChange(event.target.checked)}
					disabled={disabled}
				/>
				<span>
					{spec.key}
					{spec.description && (
						<span className="block text-xs text-muted-foreground">
							{spec.description}
						</span>
					)}
				</span>
			</label>
		);
	const label = spec.required ? spec.key : `${spec.key}（可选）`;
	return (
		<Field>
			<FieldLabel htmlFor={id}>{label}</FieldLabel>
			{spec.kind === "enum" ? (
				<Select
					value={typeof value === "string" && value ? value : "__unset__"}
					onValueChange={(next) => onChange(next === "__unset__" ? "" : next)}
					disabled={disabled}
				>
					<SelectTrigger id={id} aria-label={label} className="w-full">
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						{!spec.required && (
							<SelectItem value="__unset__">未设置</SelectItem>
						)}
						{(spec.enumValues ?? []).map((option) => (
							<SelectItem key={option} value={option}>
								{option}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
			) : spec.kind === "stringArray" || spec.kind === "json" ? (
				<Textarea
					id={id}
					value={typeof value === "string" ? value : ""}
					onChange={(event) => onChange(event.target.value)}
					disabled={disabled}
					placeholder={
						spec.kind === "stringArray" ? "每行一项" : '例如 {"key": "value"}'
					}
				/>
			) : (
				<Input
					id={id}
					type={spec.kind === "number" ? "number" : "text"}
					step={spec.kind === "number" ? "any" : undefined}
					maxLength={spec.maxLength}
					value={typeof value === "string" ? value : ""}
					onChange={(event) => onChange(event.target.value)}
					disabled={disabled}
				/>
			)}
			{spec.description && (
				<FieldDescription>{spec.description}</FieldDescription>
			)}
		</Field>
	);
}

/** Renders every schema-projected field from one shared draft object. */
function SettingsFieldsEditor({
	specs,
	draft,
	onChange,
	disabled,
	idPrefix,
}: {
	specs: SettingsFieldSpec[];
	draft: SettingsDraft;
	onChange: (key: string, value: SettingsDraftValue) => void;
	disabled?: boolean;
	idPrefix: string;
}) {
	return (
		<>
			{specs.map((spec) => (
				<SettingsFieldEditor
					key={spec.key}
					spec={spec}
					value={draft[spec.key]}
					onChange={(value) => onChange(spec.key, value)}
					disabled={disabled}
					id={`${idPrefix}-${spec.key}`}
				/>
			))}
		</>
	);
}

/** Generic creation form for one registered source kind: a stable source key,
 * a one-time bearer reveal and the kind's public receiver URL
 * (/stele/webhook/{kind}). The server rejects unregistered or disabled
 * protocols; the catalog check above only avoids offering dead forms. */
function EventSourceForm({
	item,
	kind,
	navigate,
	suspended,
	embedded = false,
}: {
	item: IntegrationCatalogItem;
	kind: string;
	navigate: (to: string) => void;
	suspended: boolean;
	/** Demoted heading when rendered below the combined-plugin page title. */
	embedded?: boolean;
}) {
	const [key, setKey] = useState("");
	const [createdKey, setCreatedKey] = useState("");
	const [saving, setSaving] = useState(false);
	// 与 Alertmanager 表单一致：创建失败必须以常驻内联错误留在表单上，
	// 一次性凭据只有这一条取得途径，只靠 toast 会让失败像“什么都没发生”。
	const [error, setError] = useState("");
	const [secret, setSecret] = useState("");
	const [receiverUrl, setReceiverUrl] = useState("");
	const revealEpoch = useRef(0);
	// Schema-driven non-secret settings (ADR-0014 story 2); an empty spec list
	// means the kind accepts only the empty document and renders no fields.
	const settingsSpecs = useMemo(
		() => settingsFieldSpecs(item.eventSourceConfigSchema),
		[item.eventSourceConfigSchema],
	);
	const [settingsDraft, setSettingsDraft] = useState<SettingsDraft>(() =>
		emptyDraft(settingsSpecs),
	);
	const [settingsError, setSettingsError] = useState("");
	useEffect(() => {
		if (suspended) {
			revealEpoch.current += 1;
			setSecret("");
			setReceiverUrl("");
		}
	}, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (suspended) return;
		// Validate the settings document before any network call: a rejecting
		// server produces no row, but the one-time credential flow should not
		// start on a request known to be invalid.
		let settings: Record<string, unknown> | undefined;
		if (settingsSpecs.length > 0) {
			const validated = settingsFromDraft(settingsSpecs, settingsDraft);
			if (!validated.ok) {
				setSettingsError(validated.error);
				return;
			}
			setSettingsError("");
			settings = validated.settings;
		}
		const epoch = revealEpoch.current;
		setSaving(true);
		setError("");
		try {
			const endpoint = await fetchPublicReceiverEndpoint(kind);
			const sourceKey = key.trim();
			const result = await createEventSourceInstance(sourceKey, kind, settings);
			setCreatedKey(sourceKey);
			if (!result.revealHandle)
				throw new Error(
					"来源已创建，但服务端未返回一次性凭据句柄。请从实例详情轮换凭据。",
				);
			const token = await revealEventSourceCredential(result.revealHandle);
			if (!suspended && epoch === revealEpoch.current) {
				setSecret(token);
				setReceiverUrl(endpoint.publicReceiverUrl);
				setKey("");
				// 下一枚同类实例从空白设置开始；不同实例允许不同参数。
				setSettingsDraft(emptyDraft(settingsSpecs));
			}
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSaving(false);
		}
	}
	return (
		<section className="flex flex-col gap-6">
			{embedded ? (
				<h2 className="text-base font-semibold">事件接入</h2>
			) : (
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">
						配置 {item.displayName}
					</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						创建逻辑事件源，生成面向部署公共入口的接收地址与一次性凭据。
					</p>
				</div>
			)}
			<div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(18rem,.75fr)]">
				<form onSubmit={submit}>
					<Card>
						<CardHeader>
							<CardTitle>来源信息</CardTitle>
							<CardDescription>
								来源键用于稳定识别一个同类事件来源。
							</CardDescription>
						</CardHeader>
						<CardContent>
							<FieldGroup>
								{error && (
									<Alert variant="destructive">
										<AlertTitle>无法创建事件源</AlertTitle>
										<AlertDescription>{error}</AlertDescription>
									</Alert>
								)}
								<Field>
									<FieldLabel htmlFor="event-source-key">
										来源键
									</FieldLabel>
									<Input
										id="event-source-key"
										value={key}
										onChange={(event) => setKey(event.target.value)}
										required
										maxLength={200}
										disabled={saving || suspended || Boolean(createdKey)}
										autoFocus
									/>
								<FieldDescription>
									例如 production-{kind}。不要输入凭据或内部地址。
								</FieldDescription>
							</Field>
							{settingsSpecs.length > 0 && (
								<>
									<Separator />
									<div>
										<h3 className="text-sm font-medium">
											来源设置（非秘密）
										</h3>
										<p className="mt-1 text-xs text-muted-foreground">
											由插件声明的封闭 schema
											校验的实例参数；同一来源类型的每个实例可使用不同设置，且永不包含凭据。
										</p>
									</div>
									{settingsError && (
										<Alert variant="destructive">
											<AlertDescription>
												{settingsError}
											</AlertDescription>
										</Alert>
									)}
									<SettingsFieldsEditor
										specs={settingsSpecs}
										draft={settingsDraft}
										onChange={(changed, value) =>
											setSettingsDraft((current) => ({
												...current,
												[changed]: value,
											}))
										}
										disabled={
											saving || suspended || Boolean(createdKey)
										}
										idPrefix="event-source-settings"
									/>
								</>
							)}
							<Button
									type="submit"
									disabled={
										!key.trim() || saving || suspended || Boolean(createdKey)
									}
								>
									{saving && (
										<LoaderCircle
											className="animate-spin"
											data-icon="inline-start"
										/>
									)}
									{saving ? "创建中…" : "创建并显示一次凭据"}
								</Button>
							</FieldGroup>
						</CardContent>
					</Card>
				</form>
				<aside className="flex flex-col gap-4">
					<h2 className="text-base font-semibold">部署说明</h2>
					<ol className="flex flex-col gap-3 text-sm text-muted-foreground">
						<li>1. 创建后立即复制一次性 Bearer 凭据和接收地址。</li>
						<li>
							2. 在上游平台配置 webhook：POST 到接收地址，并携带
							Authorization: Bearer 凭据。
						</li>
						<li>
							3. 发送测试事件，实例列表会显示最近有效事件。等待首条事件不是故障。
						</li>
					</ol>
					<Separator />
					<p className="text-sm text-muted-foreground">
						Stele 本地持久入队后即确认投递；Quoin 会异步处理。认证失败或入口不可用时请由上游重试。
					</p>
				</aside>
			</div>
			<SecretReveal
				open={!suspended && Boolean(secret)}
				secret={secret}
				receiverUrl={receiverUrl}
				onClose={() => {
					setSecret("");
					setReceiverUrl("");
					navigate(`${INTEGRATIONS_BASE}?instances`);
				}}
			/>
		</section>
	);
}

/** Drawer detail for one generic HTTP connection instance: status, real-probe
 * verification, explicit enablement and confirmed disable/rotate (#110). */
function HttpConnectionDetail({
	id,
	navigate,
	suspended,
}: {
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [item, setItem] = useState<HttpConnectionInstance>();
	const [loading, setLoading] = useState(true);
	const [busy, setBusy] = useState("");
	const [error, setError] = useState("");
	const load = useCallback(async () => {
		setLoading(true);
		try {
			setItem(await fetchHttpConnectionInstance(id));
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}, [id]);
	useEffect(() => {
		if (!suspended) void load();
	}, [load, suspended]);
	async function action(kind: "probe" | "enable" | "disable") {
		if (!item) return;
		setBusy(kind);
		setError("");
		try {
			if (kind === "probe" || kind === "enable") {
				const result = await probeHttpConnectionInstance(item.displayName);
				if (!result || result.outcome !== "passed" || !result.id) {
					const diagnostic = probeDiagnostic(result?.details);
					throw new Error(
						`连通性验证未通过${diagnostic ? `：${diagnostic}` : ""}或未生成可启用的验证结果，接入保持当前状态。`,
					);
				}
				if (kind === "enable")
					setItem(await enableHttpConnectionInstance(item, result.id));
			} else setItem(await disableHttpConnectionInstance(item));
			if (kind === "enable") notify.success("接入已启用");
			else if (kind === "disable") notify.success("接入已停用");
			else notify.success("验证通过");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	if (loading)
		return (
			<DetailSkeleton
				label="正在加载连接实例"
				rows={["title", "line", "card", "card", "card"]}
			/>
		);
	if (!item)
		return (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法加载接入实例</AlertTitle>
					<AlertDescription>
						{error || "该实例不存在或暂时无法读取。"}
					</AlertDescription>
				</Alert>
			</section>
		);
	const needsRevalidation = item.revalidationRequired;
	const statusLabel =
		item.status === "active"
			? "已启用"
			: needsRevalidation
				? "需要重新验证"
				: "已停用";
	return (
		<section className="flex flex-col gap-6">
			{/* 抽屉头部（DetailSheet）负责实例名标题。 */}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<Card>
				<CardHeader>
					<div className="flex flex-wrap items-center justify-between gap-3">
						<div>
							<CardTitle>接入状态</CardTitle>
							<CardDescription>
								端点和凭据不回显；轮换会创建新配置与凭据代次。
							</CardDescription>
						</div>
						<Badge
							variant={item.status === "active" ? "secondary" : "outline"}
						>
							{statusLabel}
						</Badge>
					</div>
				</CardHeader>
				<CardContent className="flex flex-col gap-5">
					<PropertyList
						layout="grid-2"
						entries={[
							{
								label: "连接类型",
								value: <span className="font-medium">{item.platform}</span>,
							},
							{
								label: "端点",
								value: (
									<span className="break-all font-medium">
										{item.endpoint ?? "—"}
									</span>
								),
							},
							{
								label: "认证方式",
								value: (
									<span className="font-medium">
										{authModeLabel[item.authType]}
									</span>
								),
							},
							{
								label: "最近验证",
								value: (
									<span className="font-medium">
										{item.lastProbe ? item.lastProbe.outcome : "尚未记录"}
									</span>
								),
							},
						]}
					/>
					{needsRevalidation && (
						<Alert>
							<AlertTitle>需要重新验证</AlertTitle>
							<AlertDescription>
								凭据已轮换。请执行验证并启用，以当前 revision
								和凭据代次的通过结果恢复使用。
							</AlertDescription>
						</Alert>
					)}
					<Separator />
					<div className="flex flex-wrap gap-2">
						<Button
							variant="outline"
							disabled={suspended || Boolean(busy)}
							onClick={() => void action("probe")}
						>
							<RefreshCw data-icon="inline-start" />
							{busy === "probe" ? "验证中…" : "验证连通性"}
						</Button>
						<Button
							disabled={
								suspended ||
								(item.status === "active" && !needsRevalidation) ||
								Boolean(busy)
							}
							onClick={() => void action("enable")}
						>
							{needsRevalidation ? "重新验证并启用" : "验证并启用"}
						</Button>
						<ConfirmAction
							title={`停用 ${item.displayName}？`}
							description="停用后不再开始新的观测、查询和巡检；配置和历史不会删除。"
							disabled={
								suspended || item.status === "disabled" || Boolean(busy)
							}
							destructive
							onConfirm={() => void action("disable")}
						>
							{busy === "disable" ? "停用中…" : "停用接入"}
						</ConfirmAction>
						<Button
							variant="outline"
							disabled={suspended || Boolean(busy)}
							onClick={() =>
								navigate(integrationRoute(item.platform, item.displayName))
							}
						>
							<RotateCw data-icon="inline-start" />
							轮换凭据
						</Button>
					</div>
				</CardContent>
			</Card>
		</section>
	);
}

/** Drawer dispatcher for non-specialized instance platforms (#110): an
 * enabled http_connection catalog kind renders the connection detail;
 * everything else — registered generic event sources, unknown kinds, and
 * catalog outages — keeps the long-standing event-source detail, whose
 * server reads fail closed on their own. */
function GenericInstanceDetailRoute({
	kind,
	id,
	navigate,
	suspended,
}: {
	kind: string;
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const entry = useCatalogEntry(kind, "connectionKind");
	if (entry.ready && entry.item && isHttpConnectionCatalogItem(entry.item))
		return (
			<HttpConnectionDetail
				id={id}
				navigate={navigate}
				suspended={suspended}
			/>
		);
	return <EventSourceDetail kind={kind} id={id} suspended={suspended} />;
}

/** Rotation form for one generic HTTP connection: non-secret fields prefill,
 * secrets never echo; submit creates a new revision/generation pair and the
 * connection stays withheld until a fresh exact-pair probe requalifies it. */
function HttpConnectionRotate({
	kind,
	id,
	navigate,
	suspended,
}: {
	kind: string;
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [item, setItem] = useState<HttpConnectionInstance>();
	const [baseUrl, setBaseUrl] = useState("");
	const [authType, setAuthType] = useState<AuthType>("none");
	const [authModes, setAuthModes] = useState<AuthType[]>([
		"none",
		"basic",
		"bearer",
	]);
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [bearerToken, setBearerToken] = useState("");
	const [tlsCaPem, setTlsCaPem] = useState("");
	const [tlsServerName, setTlsServerName] = useState("");
	const [tlsSkipVerify, setTlsSkipVerify] = useState(false);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	useEffect(() => {
		let active = true;
		void (async () => {
			try {
				const [current, catalog] = await Promise.all([
					fetchHttpConnectionInstance(id),
					listIntegrationPlugins(),
				]);
				if (!active) return;
				setItem(current);
				setBaseUrl(current.endpoint ?? "");
				setAuthType(current.authType);
				setUsername(current.username ?? "");
				setTlsCaPem(current.tlsCaPem ?? "");
				setTlsServerName(current.tlsServerName ?? "");
				setTlsSkipVerify(current.tlsSkipVerify);
				const owner = catalog.find(
					(candidate) => candidate.connectionKind === kind,
				);
				const declared = allowedAuthModes(owner?.connectionAuthModes);
				if (declared.length > 0) setAuthModes(declared);
			} catch (reason) {
				if (active)
					setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			}
		})();
		return () => {
			active = false;
		};
	}, [id, kind]);
	useEffect(() => {
		if (suspended) {
			setPassword("");
			setBearerToken("");
		}
	}, [suspended]);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (!item || suspended) return;
		setSaving(true);
		setError("");
		try {
			const payload = httpConnectionPayload(
				kind,
				baseUrl,
				authType,
				username,
				password,
				bearerToken,
				tlsCaPem,
				tlsServerName,
				tlsSkipVerify,
			);
			const rotating = rotateHttpConnectionInstance(item, payload);
			setPassword("");
			setBearerToken("");
			await rotating;
			notify.success("已保存");
			navigate(instanceSheetRoute(kind, id));
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">编辑 {id}</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					提交将创建新配置与凭据代次；已保存的非秘密字段已预填，旧秘密不会回显。
				</p>
			</div>
			<form onSubmit={submit}>
				<Card>
					<CardHeader>
						<CardTitle>接入信息</CardTitle>
						<CardDescription>
							可修正端点或 TLS 配置后重新验证。认证方式仅限插件声明的范围；切换到
							Basic 或 Bearer 时必须提供新秘密。
						</CardDescription>
					</CardHeader>
					<CardContent>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							<Field>
								<FieldLabel htmlFor="http-rotate-url">端点 URL</FieldLabel>
								<Input
									id="http-rotate-url"
									type="url"
									value={baseUrl}
									onChange={(event) => setBaseUrl(event.target.value)}
									required
									disabled={saving || suspended}
								/>
							</Field>
							<Field>
								<FieldLabel>认证方式</FieldLabel>
								<AuthTypeSelect
									value={authType}
									onChange={setAuthType}
									disabled={saving || suspended}
									modes={authModes}
								/>
							</Field>
							{authType === "basic" && (
								<>
									<Field>
										<FieldLabel htmlFor="http-rotate-username">
											用户名
										</FieldLabel>
										<Input
											id="http-rotate-username"
											value={username}
											onChange={(event) => setUsername(event.target.value)}
											required
											disabled={saving || suspended}
										/>
									</Field>
									<Field>
										<FieldLabel htmlFor="http-rotate-password">
											密码
										</FieldLabel>
										<Input
											id="http-rotate-password"
											type="password"
											value={password}
											onChange={(event) => setPassword(event.target.value)}
											required
											disabled={saving || suspended}
										/>
										<FieldDescription>
											旧密码不会回显；提交即清除新密码。
										</FieldDescription>
									</Field>
								</>
							)}
							{authType === "bearer" && (
								<Field>
									<FieldLabel htmlFor="http-rotate-token">
										Bearer Token
									</FieldLabel>
									<Input
										id="http-rotate-token"
										type="password"
										value={bearerToken}
										onChange={(event) => setBearerToken(event.target.value)}
										required
										disabled={saving || suspended}
									/>
									<FieldDescription>
										旧 Token 不会回显；提交即清除新 Token。
									</FieldDescription>
								</Field>
							)}
							<Separator />
							<Field>
								<FieldLabel htmlFor="http-rotate-ca">
									自定义 CA（可选）
								</FieldLabel>
								<Textarea
									id="http-rotate-ca"
									value={tlsCaPem}
									onChange={(event) => setTlsCaPem(event.target.value)}
									disabled={saving || suspended}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="http-rotate-server-name">
									TLS Server Name（可选）
								</FieldLabel>
								<Input
									id="http-rotate-server-name"
									value={tlsServerName}
									onChange={(event) => setTlsServerName(event.target.value)}
									disabled={saving || suspended}
								/>
							</Field>
							<label className="flex items-start gap-2 text-sm">
								<input
									type="checkbox"
									checked={tlsSkipVerify}
									onChange={(event) => setTlsSkipVerify(event.target.checked)}
									disabled={saving || suspended}
								/>
								<span>跳过 TLS 证书校验（仅限已知受控环境）</span>
							</label>
							<Button
								type="submit"
								disabled={
									!item ||
									!baseUrl.trim() ||
									saving ||
									suspended ||
									(authType === "basic" &&
										(!username.trim() || !password)) ||
									(authType === "bearer" && !bearerToken)
								}
							>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
										aria-hidden="true"
									/>
								)}
								{saving ? "保存中…" : "保存新版本"}
							</Button>
						</FieldGroup>
					</CardContent>
				</Card>
			</form>
		</section>
	);
}

/** Catalog-validated rotate route for one connection kind; unknown, disabled
 * or specialized kinds fail closed. */
function GenericConnectionRotateRoute({
	kind,
	name,
	navigate,
	suspended,
}: {
	kind: string;
	name: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const entry = useCatalogEntry(kind, "connectionKind");
	return (
		<CatalogGate entry={entry}>
			{(item) =>
				isHttpConnectionCatalogItem(item) ? (
					<HttpConnectionRotate
						kind={kind}
						id={name}
						navigate={navigate}
						suspended={suspended}
					/>
				) : (
					<IntegrationNotFound />
				)
			}
		</CatalogGate>
	);
}

/** The integration workbench has a deliberately small routing surface: two
 * specialized brand workbenches plus one catalog-driven generic route, rather
 * than a per-brand route table. */
export function useIntegrationsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	// 实例列表与详情在同一抽屉内切换：详情视图卸载列表，返回时列表重挂载并
	// 重新拉取，徽标自然不会停留在旧状态。
	if (props.user.role !== "admin")
		return {
			title: "接入管理",
			list: null,
			content: (
				<Alert variant="destructive">
					<AlertTitle>无权访问</AlertTitle>
					<AlertDescription>接入管理仅向管理员开放。</AlertDescription>
				</Alert>
			),
		};
	const [platform, id] = routeParts(props.route);
	const third = routeParts(props.route)[2];
	const routeQuery = parseRoute(props.route).searchParams;
	// 实例列表与实例详情是接入管理页上同一个右侧抽屉内的两个视图（与告警一致），
	// 由 query 标志驱动、可深链；旧 /instances 路径照常渲染目录页并打开抽屉。
	const sheetPlatform = routeQuery.get("platform");
	const sheetInstance = routeQuery.get("instance");
	const detailRef =
		sheetPlatform && sheetInstance
			? { platform: sheetPlatform, name: sheetInstance }
			: null;
	const isCatalogRoute = !platform || platform === "instances";
	const instancePlatformName = (platform: string) =>
		platform === "alertmanager"
			? "Alertmanager"
			: platform === "prometheus"
				? "Prometheus"
				: platform === "thanos"
					? "Thanos"
					: platform;
	const instancesDrawer =
		isCatalogRoute &&
		(detailRef || platform === "instances" || routeQuery.has("instances")) ? (
			<DetailSheet
				open
				onClose={() => props.navigate(INTEGRATIONS_BASE)}
				title={detailRef ? decodeURIComponent(detailRef.name) : "已接入实例"}
				description={
					detailRef
						? detailRef.platform === "alertmanager"
							? "Alertmanager 告警来源。"
							: detailRef.platform === "prometheus" ||
									 detailRef.platform === "thanos"
							? `${instancePlatformName(detailRef.platform)} 指标接入。`
							: "接入实例。"
						: "所有已接入实例。"
				}
			>
				{detailRef ? (
					<div className="min-h-0 flex-1 overflow-y-auto">
						<div className="flex flex-col gap-4 p-4 sm:p-6">
							<Button
								variant="ghost"
								size="sm"
								className="self-start"
								onClick={() =>
									props.navigate(`${INTEGRATIONS_BASE}?instances`)
								}
							>
								<ChevronLeft data-icon="inline-start" aria-hidden="true" />
								返回实例列表
							</Button>
							{detailRef.platform === "prometheus" ||
								detailRef.platform === "thanos" ? (
								<MetricsDetail
									id={decodeURIComponent(detailRef.name)}
									navigate={props.navigate}
									suspended={props.suspended}
								/>
							) : detailRef.platform === "alertmanager" ? (
								<EventSourceDetail
									kind={detailRef.platform}
									id={decodeURIComponent(detailRef.name)}
									suspended={props.suspended}
								/>
							) : (
								// 通用平台先按连接类型目录分派：HTTP 连接实例走连接详情；
								// 其余保持既有事件源详情，服务端读取自身失败关闭。
								<GenericInstanceDetailRoute
									kind={detailRef.platform}
									id={decodeURIComponent(detailRef.name)}
									navigate={props.navigate}
									suspended={props.suspended}
								/>
							)}
						</div>
					</div>
				) : (
					<Instances
						navigate={props.navigate}
						suspended={props.suspended}
					/>
				)}
			</DetailSheet>
		) : null;
	const content =
		platform === "alertmanager" && id === "issues" ? (
			<AlertIntakeIssues suspended={props.suspended} />
		) : platform === "alertmanager" ? (
			<AlertmanagerForm navigate={props.navigate} suspended={props.suspended} />
		) : platform === "prometheus" || platform === "thanos" ? (
			id && routeParts(props.route)[2] === "rotate" ? (
				<MetricsRotate
					platform={platform}
					id={decodeURIComponent(id)}
					navigate={props.navigate}
					suspended={props.suspended}
				/>
			) : (
				<MetricsForm
					platform={platform}
					navigate={props.navigate}
					suspended={props.suspended}
				/>
			)
		) : platform && platform !== "instances" && !isSpecializedPlatform(platform) && !id ? (
			// Any other first segment is a catalog plugin id resolved against
			// the server catalog; unregistered or disabled plugins render the
			// shared not-found view. Only the bare /integrations root (and the
			// legacy /instances path) show the catalog.
			<GenericPluginRoute
				kind={platform}
				navigate={props.navigate}
				suspended={props.suspended}
			/>
		) : platform && platform !== "instances" && !isSpecializedPlatform(platform) && id && !third ? (
			// {connectionKind}/{name}: catalog-validated rotation page for one
			// generic HTTP connection instance.
			<GenericConnectionRotateRoute
				kind={platform}
				name={decodeURIComponent(id)}
				navigate={props.navigate}
				suspended={props.suspended}
			/>
		) : platform && platform !== "instances" ? (
			<IntegrationNotFound />
		) : (
			<>
				<IntegrationCatalog navigate={props.navigate} />
				{instancesDrawer}
			</>
		);
	return {
		title: "接入管理",
		crumbs: integrationCrumbs(props.route),
		list: (
			<SettingsNavigation
				groups={settingsNavGroups}
				route={props.route}
				user={props.user}
				navigate={props.navigate}
			/>
		),
		content,
	};
}

/** Breadcrumb trail for editor-style pages; instance details are drawers without crumbs. */
function integrationCrumbs(route: string) {
	const segments = routeParts(route);
	const [platform, id] = segments;
	const catalog = { label: "接入管理", to: INTEGRATIONS_BASE };
	const instances = {
		label: "已接入实例",
		to: `${INTEGRATIONS_BASE}?instances`,
	};
	if (!platform || platform === "instances") return undefined;
	if (platform === "alertmanager" && id === "issues")
		return [
			catalog,
			{ label: "Alertmanager", to: `${INTEGRATIONS_BASE}/alertmanager` },
			{ label: "告警接入问题" },
		];
	if (platform === "alertmanager")
		return [catalog, { label: "配置 Alertmanager" }];
	if (platform === "prometheus" || platform === "thanos") {
		const name = platform === "prometheus" ? "Prometheus" : "Thanos";
		if (id && segments[2] === "rotate")
			return [
				catalog,
				instances,
				{
					label: decodeURIComponent(id),
					to: instanceSheetRoute(platform, decodeURIComponent(id)),
				},
				{ label: "编辑接入" },
			];
		return [catalog, { label: `配置 ${name}` }];
	}
	// Generic plugin ids stay on the stable kind (the form's heading carries
	// the display name); {connectionKind}/{name} is the rotation page.
	if (!id) return [catalog, { label: `配置 ${platform}` }];
	if (!segments[2])
		return [
			catalog,
			instances,
			{
				label: decodeURIComponent(id),
				to: instanceSheetRoute(platform, decodeURIComponent(id)),
			},
			{ label: "编辑接入" },
		];
	return undefined;
}

type AuthType = "none" | "basic" | "bearer";

export const authModeLabel: Record<AuthType, string> = {
	none: "无认证",
	basic: "HTTP Basic",
	bearer: "Bearer Token",
};

/** Shared authentication-mode picker; `modes` restricts the options to the
 * plugin-declared vocabulary (#110) and defaults to the full set. */
function AuthTypeSelect({
	value,
	onChange,
	disabled,
	modes = ["none", "basic", "bearer"],
	ariaLabel = "认证方式",
}: {
	value: AuthType;
	onChange: (value: AuthType) => void;
	disabled?: boolean;
	modes?: AuthType[];
	ariaLabel?: string;
}) {
	return (
		<Select
			value={value}
			onValueChange={(next) => onChange(next as AuthType)}
			disabled={disabled}
		>
			<SelectTrigger aria-label={ariaLabel} className="w-full">
				<SelectValue />
			</SelectTrigger>
			<SelectContent>
				{modes.map((mode) => (
					<SelectItem key={mode} value={mode}>
						{authModeLabel[mode]}
					</SelectItem>
				))}
			</SelectContent>
		</Select>
	);
}
