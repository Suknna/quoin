import { ConfirmAction } from "@/features/settings/platform/controls";
import { formatDateTime } from "@/lib/format";
import { parseRoute } from "@/lib/parse-route";
/* eslint-disable react-refresh/only-export-components -- This route module intentionally colocates its view factory with route components. */

import {
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
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { PropertyList } from "@/components/workbench/PropertyList";
import {
	acknowledgeIntakeIssue,
	fetchIntakeIssues,
	type IntakeIssue,
} from "@/features/alerts/api";
import {
	type AlertmanagerCredential,
	type AlertmanagerInstance,
	alertmanagerReceiverYaml,
	createAlertmanagerInstance,
	createMetricsInstance,
	disableAlertmanagerInstance,
	disableMetricsInstance,
	enableMetricsInstance,
	fetchAlertmanagerInstance,
	fetchMetricsInstance,
	fetchPublicReceiverEndpoint,
	listAlertmanagerCredentials,
	listAlertmanagerInstances,
	listIntegrationPlugins,
	listMetricsInstances,
	type MetricsConnectionInput,
	type MetricsInstance,
	probeDiagnostic,
	probeMetricsInstance,
	retireAlertmanagerCredential,
	revealAlertmanagerCredential,
	rotateAlertmanagerCredential,
	rotateMetricsInstance,
} from "./api";
import type { IntegrationCatalogItem, IntegrationPlatform } from "./types";

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
	platform?: IntegrationPlatform | "instances",
	instanceId?: string,
) {
	return [INTEGRATIONS_BASE, platform, instanceId].filter(Boolean).join("/");
}
/** Instance details are a right-hand drawer over the instances list, not routes. */
function instanceSheetRoute(platform: IntegrationPlatform, name: string) {
	return `${INTEGRATIONS_BASE}/instances?platform=${encodeURIComponent(platform)}&instance=${encodeURIComponent(name)}`;
}

function CatalogCard({
	item,
	navigate,
}: {
	item: IntegrationCatalogItem;
	navigate: (to: string) => void;
}) {
	const route = `/integrations/${encodeURIComponent(item.id)}`;
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
					{item.capabilities.includes("discover") && (
						<Badge variant="secondary">自动观测</Badge>
					)}
					{item.capabilities.includes("tools") && (
						<Badge variant="secondary">Agent 工具</Badge>
					)}
				</div>
				<Button onClick={() => navigate(route)}>
					配置 {item.displayName}
					<ChevronRight data-icon="inline-end" />
				</Button>
			</CardContent>
		</Card>
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
			<div className="flex flex-wrap items-end justify-between gap-3">
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">接入管理</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						配置已启用的平台能力。验证启用后自动观测，无需先定义业务系统。
					</p>
				</div>
				<Button
					variant="outline"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/instances`)}
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
	revision,
}: Pick<WorkspaceModuleProps, "navigate" | "suspended"> & {
		/** 抽屉内启用/停用/轮换成功后递增，让背后的列表行重新拉取，避免徽标停留在旧状态。 */
		revision: number;
	}) {
	const [items, setItems] = useState<
		(AlertmanagerInstance | MetricsInstance)[]
	>([]);
	const [cursor, setCursor] = useState<string>();
	const [query, setQuery] = useState("");
	const [loading, setLoading] = useState(true);
	const [loadingMore, setLoadingMore] = useState(false);
	const [error, setError] = useState("");
	const loadFirstPage = useCallback(async () => {
		if (suspended) return;
		setLoading(true);
		setError("");
		try {
			const [alerts, metrics] = await Promise.all([
				listAlertmanagerInstances(),
				listMetricsInstances(),
			]);
			setItems([...alerts.items, ...metrics]);
			setCursor(alerts.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}, [suspended]);
	const loadMore = async () => {
		if (!cursor || suspended) return;
		setLoadingMore(true);
		setError("");
		try {
			const page = await listAlertmanagerInstances(cursor);
			setItems((current) => [...current, ...page.items]);
			setCursor(page.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoadingMore(false);
		}
	};
	useEffect(() => {
		void loadFirstPage();
	}, [loadFirstPage, revision]);
	const filtered = items.filter((item) =>
		item.displayName
			.toLocaleLowerCase()
			.includes(query.trim().toLocaleLowerCase()),
	);
	const platformName = (platform: IntegrationPlatform) =>
		platform === "alertmanager"
			? "Alertmanager"
			: platform === "prometheus"
				? "Prometheus"
				: "Thanos";
	const statusLabel = (item: AlertmanagerInstance | MetricsInstance) =>
		item.status === "active"
			? "已启用"
			: item.status === "revalidation_required"
				? "需要重新验证"
				: "已停用";
	return (
		<section className="flex flex-col gap-5">
			<div className="flex flex-wrap items-end justify-between gap-3">
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">已接入实例</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						Alertmanager、Prometheus 与 Thanos 接入实例。
					</p>
				</div>
				<Button
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
					subtitle: `${platformName(item.platform)} · ${
						item.platform === "alertmanager"
							? formatEventTime(item.latestValidEventAt)
							: ((item as MetricsInstance).endpoint ?? "—")
					}`,
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
					// 详情抽屉按稳定连接名寻址，与服务端 name-keyed 读取契约一致。
					navigate(instanceSheetRoute(row.item.platform, row.item.displayName))
				}
				loading={loading}
				loadingLabel="正在加载实例"
				error={error}
				onRetry={() => void loadFirstPage()}
				emptyTitle={query ? "没有匹配的已加载实例" : "尚未接入实例"}
				emptyDescription={
					query
						? "请使用其他名称搜索。"
						: "创建接入后，在此管理探测、启用、停用与轮换。"
				}
				controls={
					<div className="relative max-w-md">
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
				}
			/>
			<LoadMoreButton
				loading={loadingMore}
				hasMore={Boolean(cursor)}
				onLoadMore={() => void loadMore()}
				className="self-start"
			>
				加载更多 Alertmanager 实例
			</LoadMoreButton>
		</section>
	);
}

/** Full-workbench one-time reveal, cleared by the owning form/detail on close or suspension. */
function SecretReveal({
	open,
	secret,
	receiverUrl,
	onClose,
}: {
	open: boolean;
	secret: string;
	receiverUrl: string;
	onClose: () => void;
}) {
	const [copied, setCopied] = useState("");
	const yaml =
		secret && receiverUrl ? alertmanagerReceiverYaml(receiverUrl, secret) : "";
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
	onMutated,
}: {
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
	/** 状态变更（启用/停用）成功后通知宿主失效实例列表。 */
	onMutated?: () => void;
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
			// 抽屉背后的列表行还带着旧徽标；探测不改变状态，无需失效。
			if (kind === "enable" || kind === "disable") onMutated?.();
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
			const result = await createAlertmanagerInstance(sourceKey);
			setCreatedKey(sourceKey);
			if (!result.revealHandle)
				throw new Error(
					"来源已创建，但服务端未返回一次性凭据句柄。请从实例详情轮换凭据。",
				);
			const token = await revealAlertmanagerCredential(result.revealHandle);
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
						Quoin 仅在成功持久化后确认投递；认证失败或暂时错误应由 Alertmanager
						重试。
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
					navigate(`${INTEGRATIONS_BASE}/instances`);
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

function AlertmanagerDetail({
	id,
	suspended,
	onMutated,
}: {
	id: string;
	suspended: boolean;
	/** 状态变更（停用/轮换）成功后通知宿主失效实例列表。 */
	onMutated?: () => void;
}) {
	const [source, setSource] = useState<AlertmanagerInstance>();
	const [credentials, setCredentials] = useState<AlertmanagerCredential[]>([]);
	const [loading, setLoading] = useState(true);
	const [busy, setBusy] = useState("");
	const [error, setError] = useState("");
	const [secret, setSecret] = useState("");
	const [receiverUrl, setReceiverUrl] = useState("");
	const revealEpoch = useRef(0);
	const load = useCallback(async () => {
		setLoading(true);
		setError("");
		try {
			const [sourceItem, credentialItems] = await Promise.all([
				fetchAlertmanagerInstance(id),
				listAlertmanagerCredentials(id),
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
		const epoch = revealEpoch.current;
		setBusy("rotate");
		setError("");
		try {
			const endpoint = await fetchPublicReceiverEndpoint();
			const result = await rotateAlertmanagerCredential(id);
			if (!result.revealHandle)
				throw new Error(
					"凭据已轮换，但服务端未返回一次性凭据句柄。请再次轮换取得新值。",
				);
			const token = await revealAlertmanagerCredential(result.revealHandle);
			if (!suspended && epoch === revealEpoch.current) {
				setSecret(token);
				setReceiverUrl(endpoint.publicReceiverUrl);
			}
			await load();
			onMutated?.();
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
			await disableAlertmanagerInstance(source);
			notify.success("已停用");
			await load();
			onMutated?.();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy("");
		}
	}
	async function retire(credential: AlertmanagerCredential) {
		setBusy(credential.id);
		try {
			await retireAlertmanagerCredential(id, credential);
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
				label="正在加载 Alertmanager 实例"
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
						更新 Alertmanager
						配置，确认新凭据已出现首次有效事件，再显式退休旧凭据。系统不会猜测切换已完成。
					</p>
					<Separator />
					<p className="text-sm text-muted-foreground">
						没有事件仅表示仍在等待上游投递，不会自动标记此来源故障。
					</p>
				</aside>
			</div>
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
									description="确认新凭据已经在上游 Alertmanager 中使用。退休后旧凭据无法恢复。"
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
				onClose={() => {
					setSecret("");
					setReceiverUrl("");
				}}
			/>
		</section>
	);
}

/** The integration workbench has a deliberately small routing surface, rather than a generic plugin framework. */
export function useIntegrationsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	// 实例列表与右侧详情抽屉是两棵独立数据流：抽屉里的启用/停用/轮换只刷新
	// 抽屉自身，列表行徽标会停留在旧状态（关抽屉也不重挂载）。用递增的
	// revision 让详情在变更成功后主动失效父列表。
	const [instancesRevision, setInstancesRevision] = useState(0);
	const bumpInstances = () => setInstancesRevision((value) => value + 1);
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
	const routeQuery = parseRoute(props.route).searchParams;
	// 实例详情是实例列表页上的右侧抽屉（与告警一致），由 query 标志驱动。
	const sheetPlatform = routeQuery.get(
		"platform",
	) as IntegrationPlatform | null;
	const sheetInstance = routeQuery.get("instance");
	const instanceSheet =
		platform === "instances" && sheetPlatform && sheetInstance ? (
			<DetailSheet
				open
				onClose={() => props.navigate(`${INTEGRATIONS_BASE}/instances`)}
				title={sheetInstance}
				description={
					sheetPlatform === "alertmanager"
						? "Alertmanager 告警来源。"
						: `${sheetPlatform === "prometheus" ? "Prometheus" : "Thanos"} 指标接入。`
				}
			>
				<div className="min-h-0 flex-1 overflow-y-auto">
					<div className="p-4 sm:p-6">
						{sheetPlatform === "alertmanager" ? (
							<AlertmanagerDetail
								id={decodeURIComponent(sheetInstance)}
								suspended={props.suspended}
								onMutated={bumpInstances}
							/>
						) : (
							<MetricsDetail
								id={decodeURIComponent(sheetInstance)}
								navigate={props.navigate}
								suspended={props.suspended}
								onMutated={bumpInstances}
							/>
						)}
					</div>
				</div>
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
		) : platform === "instances" ? (
			<>
				<Instances
					navigate={props.navigate}
					suspended={props.suspended}
					revision={instancesRevision}
				/>
				{instanceSheet}
			</>
		) : platform ? (
			// Unknown platform segments are ordinary unknown routes and render the
			// shared not-found view; only the bare /integrations
			// root shows the catalog.
			<Empty>
				<EmptyHeader>
					<EmptyTitle>找不到此页面</EmptyTitle>
					<EmptyDescription>该链接无效或页面已被移动。</EmptyDescription>
				</EmptyHeader>
			</Empty>
		) : (
			<IntegrationCatalog navigate={props.navigate} />
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
		to: `${INTEGRATIONS_BASE}/instances`,
	};
	if (!platform) return undefined;
	if (platform === "instances") return [catalog, { label: "已接入实例" }];
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
	return undefined;
}

type AuthType = "none" | "basic" | "bearer";

/** Shared authentication-mode picker for Alertmanager metrics endpoints. */
function AuthTypeSelect({
	value,
	onChange,
	disabled,
}: {
	value: AuthType;
	onChange: (value: AuthType) => void;
	disabled?: boolean;
}) {
	return (
		<Select
			value={value}
			onValueChange={(next) => onChange(next as AuthType)}
			disabled={disabled}
		>
			<SelectTrigger aria-label="认证方式" className="w-full">
				<SelectValue />
			</SelectTrigger>
			<SelectContent>
				<SelectItem value="none">无认证</SelectItem>
				<SelectItem value="basic">HTTP Basic</SelectItem>
				<SelectItem value="bearer">Bearer Token</SelectItem>
			</SelectContent>
		</Select>
	);
}
