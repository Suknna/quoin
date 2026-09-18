import { IntegrationResources } from "./resources";
/* eslint-disable react-refresh/only-export-components -- This route module intentionally colocates its view factory with route components. */

import {
	BellRing,
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
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
	AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
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
	EmptyMedia,
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
	probeMetricsInstance,
	retireAlertmanagerCredential,
	revealAlertmanagerCredential,
	rotateAlertmanagerCredential,
	rotateMetricsInstance,
} from "./api";
import type { IntegrationCatalogItem, IntegrationPlatform } from "./types";

const messageOf = (reason: unknown) =>
	reason instanceof Error ? reason.message : "暂时无法完成操作，请重试。";
const formatEventTime = (value?: string | null) =>
	value ? new Date(value).toLocaleString() : "等待首条有效事件";
const formatTime = (value?: string | null) =>
	value ? new Date(value).toLocaleString() : "—";

function routeParts(route: string) {
	const pathname = new URL(route, "https://workbench.invalid").pathname;
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
				if (active) setError(messageOf(reason));
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
				<DetailSkeleton label="正在加载插件目录" rows={["card", "card", "card", "card", "card", "card"]} />
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
			setError(messageOf(reason));
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
			setError(messageOf(reason));
		} finally {
			setLoadingMore(false);
		}
	};
	useEffect(() => {
		void loadFirstPage();
	}, [loadFirstPage]);
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
					<Button
						variant="ghost"
						className="-ml-3"
						onClick={() => navigate(INTEGRATIONS_BASE)}
					>
						返回平台目录
					</Button>
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
			{error ? (
				<Alert variant="destructive">
					<AlertTitle>无法加载实例</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
					<Button
						className="mt-3"
						size="sm"
						variant="outline"
						onClick={() => void loadFirstPage()}
					>
						重试
					</Button>
				</Alert>
			) : loading ? (
				<DetailSkeleton label="正在加载实例" rows={["card", "card", "card", "card"]} />
			) : filtered.length === 0 ? (
				<Empty className="min-h-56">
					<EmptyHeader>
						<EmptyMedia variant="icon">
							<BellRing />
						</EmptyMedia>
						<EmptyTitle>
							{query ? "没有匹配的已加载实例" : "尚未接入实例"}
						</EmptyTitle>
						<EmptyDescription>
							{query
								? "请使用其他名称搜索。"
								: "创建接入后，在此管理探测、启用、停用与轮换。"}
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			) : (
				<Card>
					<CardContent className="overflow-x-auto p-0">
						<Table className="w-full min-w-180 table-fixed">
							<TableHeader>
								<TableRow>
									<TableHead className="w-[30%]">实例</TableHead>
									<TableHead className="w-[12%]">平台</TableHead>
									<TableHead className="w-[10%]">状态</TableHead>
									<TableHead className="w-[30%]">端点 / 最近事件</TableHead>
									<TableHead className="w-[18%]">
										<span className="sr-only">操作</span>
									</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{filtered.map((item) => (
									<TableRow
										key={`${item.platform}-${item.id}`}
										className="hover:bg-muted/50"
									>
										<TableCell
											className="truncate font-medium"
											title={item.displayName}
										>
											{item.displayName}
										</TableCell>
										<TableCell
											className="truncate"
											title={platformName(item.platform)}
										>
											{platformName(item.platform)}
										</TableCell>
										<TableCell>
											<Badge
												variant={
													item.status === "active" ? "secondary" : "outline"
												}
											>
												{statusLabel(item)}
											</Badge>
										</TableCell>
										<TableCell
											className="truncate"
											title={
												item.platform === "alertmanager"
													? formatEventTime(item.latestValidEventAt)
													: ((item as MetricsInstance).endpoint ?? "—")
											}
										>
											{item.platform === "alertmanager"
												? formatEventTime(item.latestValidEventAt)
												: ((item as MetricsInstance).endpoint ?? "—")}
										</TableCell>
										<TableCell>
											<Button
												size="sm"
												variant="ghost"
												onClick={() =>
													// Detail routes carry the stable connection name,
													// matching the server's name-keyed read contract.
													navigate(
														integrationRoute(item.platform, item.displayName),
													)
												}
											>
												管理
												<ChevronRight aria-hidden="true" />
											</Button>
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					</CardContent>
				</Card>
			)}
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
			if (probe.outcome !== "passed" || !probe.id)
				throw new Error(
					"连通性验证未通过。已创建的接入保持停用；修正服务端或网络后可重新验证，不会再次创建。",
				);
			const enabled = await enableMetricsInstance(instance, probe.id);
			setCreated(enabled);
			navigate(integrationRoute(platform, enabled.displayName));
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(INTEGRATIONS_BASE)}
				>
					返回接入管理
				</Button>
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
	resourceId,
}: {
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
	resourceId?: string;
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
			setError(messageOf(reason));
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
				if (!result || result.outcome !== "passed" || !result.id)
					throw new Error(
						"连通性验证未通过或未生成可启用的验证结果，接入保持当前状态。",
					);
				if (kind === "enable")
					setItem(await enableMetricsInstance(item, result.id));
			} else setItem(await disableMetricsInstance(item));
			await load();
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setBusy("");
		}
	}
	if (loading)
		return (
			<DetailSkeleton label="正在加载指标接入" rows={["title", "line", "card", "card", "card"]} />
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
				<Button
					className="self-start"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/instances`)}
				>
					返回实例列表
				</Button>
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
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/instances`)}
				>
					返回已接入实例
				</Button>
				<h1 className="text-2xl font-semibold tracking-tight">
					{item.displayName}
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					{item.platform === "prometheus" ? "Prometheus" : "Thanos"} 指标接入。
				</p>
			</div>
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
					<dl className="grid gap-4 text-sm sm:grid-cols-2">
						<div>
							<dt className="text-muted-foreground">端点</dt>
							<dd className="mt-1 break-all font-medium">
								{item.endpoint ?? "—"}
							</dd>
						</div>
						<div>
							<dt className="text-muted-foreground">最近验证</dt>
							<dd className="mt-1 font-medium">
								{item.lastProbe ? item.lastProbe.outcome : "尚未记录"}
							</dd>
						</div>
					</dl>
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
						<ConfirmButton
							title={`停用 ${item.displayName}？`}
							description="停用后不再开始新的观测、查询和巡检；配置和历史不会删除。"
							disabled={
								suspended || item.status === "disabled" || Boolean(busy)
							}
							action={() => void action("disable")}
						>
							{busy === "disable" ? "停用中…" : "停用接入"}
						</ConfirmButton>
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
			<IntegrationResources
				connectionName={item.displayName}
				navigate={navigate}
				suspended={suspended}
				enabled={item.status === "active"}
				platform={item.platform}
				resourceId={resourceId}
			/>
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
			.catch((reason) => setError(messageOf(reason)));
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
			navigate(integrationRoute(platform, id));
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setSaving(false);
			setPassword("");
			setBearerToken("");
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(integrationRoute(platform, id))}
				>
					返回接入详情
				</Button>
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
			setError(messageOf(reason));
		} finally {
			setSaving(false);
		}
	}
	return (
		<section className="flex flex-col gap-6">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(INTEGRATIONS_BASE)}
				>
					返回接入管理
				</Button>
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
										<AlertDescription>{error}</AlertDescription>
										{createdKey && (
											<Button
												className="mt-3"
												size="sm"
												variant="outline"
												type="button"
												onClick={() =>
													navigate(integrationRoute("alertmanager", createdKey))
												}
											>
												打开已创建的实例
											</Button>
										)}
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

function ConfirmButton({
	title,
	description,
	action,
	disabled,
	children,
}: {
	title: string;
	description: string;
	action: () => void;
	disabled: boolean;
	children: string;
}) {
	return (
		<AlertDialog>
			<AlertDialogTrigger asChild>
				<Button size="sm" variant="outline" disabled={disabled}>
					{children}
				</Button>
			</AlertDialogTrigger>
			<AlertDialogContent>
				<AlertDialogHeader>
					<AlertDialogTitle>{title}</AlertDialogTitle>
					<AlertDialogDescription>{description}</AlertDialogDescription>
				</AlertDialogHeader>
				<AlertDialogFooter>
					<AlertDialogCancel>取消</AlertDialogCancel>
					<AlertDialogAction onClick={action}>确认</AlertDialogAction>
				</AlertDialogFooter>
			</AlertDialogContent>
		</AlertDialog>
	);
}
/** Alert delivery faults belong with the Alertmanager lifecycle that produces them. */
function AlertIntakeIssues({
	navigate,
	suspended,
}: Pick<WorkspaceModuleProps, "navigate" | "suspended">) {
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
			setError(messageOf(reason));
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
			await load();
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setBusy(undefined);
		}
	}
	return (
		<section className="flex flex-col gap-5">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/alertmanager`)}
				>
					返回 Alertmanager
				</Button>
				<h1 className="text-2xl font-semibold tracking-tight">告警接入问题</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					查看上游投递异常并确认已处理的问题。
				</p>
			</div>
			{loading ? (
				<DetailSkeleton label="正在加载接入问题" rows={["line", "line", "line", "line"]} />
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
						<Card>
							<CardContent className="p-0">
								<Table>
									<TableHeader>
										<TableRow>
											<TableHead>类型</TableHead>
											<TableHead>问题键</TableHead>
											<TableHead>出现次数</TableHead>
											<TableHead>
												<span className="sr-only">操作</span>
											</TableHead>
										</TableRow>
									</TableHeader>
									<TableBody>
										{items.map((item) => (
											<TableRow key={item.id}>
												<TableCell>{item.kind}</TableCell>
												<TableCell>{item.issueKey}</TableCell>
												<TableCell>{item.occurrenceCount}</TableCell>
												<TableCell>
													<Button
														size="sm"
														variant="outline"
														disabled={suspended || busy === item.id}
														onClick={() => void acknowledge(item)}
													>
														{busy === item.id ? (
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
												</TableCell>
											</TableRow>
										))}
									</TableBody>
								</Table>
							</CardContent>
						</Card>
					)}
				</>
			)}
		</section>
	);
}

function AlertmanagerDetail({
	id,
	navigate,
	suspended,
}: {
	id: string;
	navigate: (to: string) => void;
	suspended: boolean;
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
			setError(messageOf(reason));
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
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setBusy("");
		}
	}
	async function disable() {
		if (!source) return;
		setBusy("disable");
		try {
			await disableAlertmanagerInstance(source);
			await load();
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setBusy("");
		}
	}
	async function retire(credential: AlertmanagerCredential) {
		setBusy(credential.id);
		try {
			await retireAlertmanagerCredential(id, credential);
			await load();
		} catch (reason) {
			setError(messageOf(reason));
		} finally {
			setBusy("");
		}
	}
	if (loading)
		return (
			<DetailSkeleton label="正在加载 Alertmanager 实例" rows={["title", "line", "card", "card", "card"]} />
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
				<Button
					className="self-start"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/instances`)}
				>
					返回实例列表
				</Button>
			</section>
		);
	return (
		<section className="flex flex-col gap-6">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate(`${INTEGRATIONS_BASE}/instances`)}
				>
					返回已接入实例
				</Button>
				<h1 className="text-2xl font-semibold tracking-tight">
					{source.displayName}
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					Alertmanager 告警来源。
				</p>
			</div>
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
						<dl className="grid gap-4 text-sm sm:grid-cols-2">
							<div>
								<dt className="text-muted-foreground">创建时间</dt>
								<dd className="mt-1 font-medium">
									{formatTime(source.createdAt)}
								</dd>
							</div>
							<div>
								<dt className="text-muted-foreground">最近有效事件</dt>
								<dd className="mt-1 font-medium">
									{formatEventTime(source.latestValidEventAt)}
								</dd>
							</div>
						</dl>
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
							<ConfirmButton
								title={`停用 ${source.displayName}？`}
								description="停用后此来源不再接收告警；已保存的历史不会删除。"
								disabled={
									suspended || source.status !== "active" || Boolean(busy)
								}
								action={() => void disable()}
							>
								停用来源
							</ConfirmButton>
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
					<Card>
						<CardContent className="p-0">
							<Table>
								<TableHeader>
									<TableRow>
										<TableHead>凭据 ID</TableHead>
										<TableHead>状态</TableHead>
										<TableHead>创建时间</TableHead>
										<TableHead>首次使用</TableHead>
										<TableHead>
											<span className="sr-only">操作</span>
										</TableHead>
									</TableRow>
								</TableHeader>
								<TableBody>
									{credentials.map((credential) => (
										<TableRow key={credential.id}>
											<TableCell className="font-mono text-xs">
												{credential.id}
											</TableCell>
											<TableCell>
												<Badge
													variant={
														credential.state === "Retired"
															? "outline"
															: "secondary"
													}
												>
													{credential.state}
												</Badge>
											</TableCell>
											<TableCell>{formatTime(credential.createdAt)}</TableCell>
											<TableCell>
												{formatTime(credential.firstUsedAt)}
											</TableCell>
											<TableCell>
												{credential.state === "PendingRetirement" && (
													<ConfirmButton
														title="退休此凭据？"
														description="确认新凭据已经在上游 Alertmanager 中使用。退休后旧凭据无法恢复。"
														disabled={suspended || Boolean(busy)}
														action={() => void retire(credential)}
													>
														{busy === credential.id ? "退休中…" : "退休"}
													</ConfirmButton>
												)}
											</TableCell>
										</TableRow>
									))}
								</TableBody>
							</Table>
						</CardContent>
					</Card>
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
	const content =
		platform === "alertmanager" && id === "issues" ? (
			<AlertIntakeIssues
				navigate={props.navigate}
				suspended={props.suspended}
			/>
		) : platform === "alertmanager" && id ? (
			<AlertmanagerDetail
				id={decodeURIComponent(id)}
				navigate={props.navigate}
				suspended={props.suspended}
			/>
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
			) : id ? (
				<MetricsDetail
					resourceId={
						routeParts(props.route)[2] === "resources"
							? decodeURIComponent(routeParts(props.route)[3] ?? "")
							: undefined
					}
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
			<Instances navigate={props.navigate} suspended={props.suspended} />
		) : platform ? (
			// Unknown platform segments are ordinary unknown routes and render the
			// shared not-found view (the retired /integrations/browser and
			// /integrations/kubernetes land here); only the bare /integrations
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
