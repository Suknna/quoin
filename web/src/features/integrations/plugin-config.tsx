/**
 * Integrations plugin configuration: the per-platform entry forms wired under
 * `/integrations/<platform>`.
 *
 * Boundaries:
 *  - Kubernetes reuses the shared connection store (`/api/v1/connections`,
 *    type `kubernetes`) with its probe/enable fencing. No new credential
 *    storage is introduced; secrets stay in component memory and are cleared
 *    on submit and on suspension.
 *  - Browser gates every entry on the server-authoritative plugin catalog.
 *    While the plugin is disabled this page renders no entry at all. When
 *    enabled it shows the versioned Journey catalog and the identities:
 *    creation and revision edits are business-independent (standalone identity
 *    API, identity_key + nullable business_system_id per migration bc14), and
 *    legacy business-referenced identities are read-only references with an
 *    explicit migration state — the historical browser-login routes never
 *    receive writes from this surface.
 *  - This module deliberately does not import from `@/features/systems/ui`:
 *    the identity panel below was extracted from it and is owned here now.
 */

import RFB from "@novnc/novnc";
import { ChevronRight, LoaderCircle, RefreshCw, RotateCw } from "lucide-react";
import { type FormEvent, useCallback, useEffect, useRef, useState } from "react";
import { messageOf } from "@/app/shared";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import { workbenchApi } from "@/api/workbench";
import {
	AlertDialog,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
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
import {
	type ConnectionDetailView,
	type ConnectionSummaryView,
	type ProbeAttemptView,
	createConnection,
	disableConnection,
	enableConnection,
	fetchConnection,
	fetchProbeAttempt,
	listConnections,
	newClientCommandId,
	probeConnection,
	rotateConnection,
} from "@/features/admin/connections/api";
import {
	type BrowserIdentity,
	type BrowserOperation,
	type BusinessSystemSummary,
	type JourneyCatalogView,
	getBrowserIdentity,
	getJourneyCatalog,
	listBusinessSystems,
} from "@/features/admin/business-systems/api";
import { listIntegrationPlugins } from "./api";
import type { IntegrationPlatform } from "./types";

const terminalProbeStates = ["Succeeded", "Failed", "Cancelled", "Interrupted"];
/** Browser operations only rest between these states; anything else is terminal. */
const activeOperationStates = [
	"Queued",
	"WaitingForCapacity",
	"Starting",
	"Running",
	"AwaitingReconnect",
];

/*
 * Standalone browser identity API, per the contract agreed with the plugin
 * backend (id = numeric record, identityKey = stable operation key; nullable
 * business_system_id per migration bc14). Every identity-scoped route uses the
 * stable identityKey — a missing key is a loud contract error, never silently
 * replaced by the numeric id.
 */
const browserIdentitiesBase = "/api/v1/browser-identities";

interface StandaloneIdentity extends BrowserIdentity {
	identityKey: string;
}

/** Rejects keyless identities instead of falling back to the numeric id. */
function asStandaloneIdentity(value: BrowserIdentity): StandaloneIdentity {
	const key = (value as { identityKey?: unknown }).identityKey;
	if (typeof key !== "string" || key.length === 0)
		throw new Error(
			`身份 ${String(value.id)} 缺少 identityKey；独立身份路径拒绝回退到数字 id。`,
		);
	return { ...value, identityKey: key };
}

async function apiProblem(response: Response, fallback: string): Promise<Error> {
	let message = fallback;
	try {
		message = ((await response.json()) as { message?: string }).message ?? message;
	} catch {
		// Non-JSON problem responses keep the safe fallback.
	}
	return new Error(message);
}

async function listBrowserIdentities(): Promise<StandaloneIdentity[]> {
	const response = await fetch(`${browserIdentitiesBase}?limit=100`, {
		credentials: "include",
	});
	if (!response.ok) throw await apiProblem(response, "暂时无法读取浏览器身份列表。");
	const page = (await response.json()) as { items?: BrowserIdentity[] };
	return (page.items ?? []).map(asStandaloneIdentity);
}

async function createBrowserIdentity(input: {
	name: string;
	startUrl: string;
	authenticationProbe: { journeyId: string; journeyVersion: number; params: Record<string, unknown> };
}): Promise<StandaloneIdentity> {
	const response = await fetch(browserIdentitiesBase, {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
	});
	if (!response.ok) throw await apiProblem(response, "无法创建浏览器身份。");
	return asStandaloneIdentity((await response.json()) as BrowserIdentity);
}

/** Freezes a new revision onto an existing standalone identity; row-version fenced. */
async function updateBrowserIdentity(
	identityKey: string,
	input: {
		name: string;
		startUrl: string;
		authenticationProbe: { journeyId: string; journeyVersion: number; params: Record<string, unknown> };
		expectedRowVersion: number;
	},
): Promise<StandaloneIdentity> {
	const response = await fetch(
		`${browserIdentitiesBase}/${encodeURIComponent(identityKey)}`,
		{
			method: "PUT",
			credentials: "include",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
		},
	);
	if (!response.ok) throw await apiProblem(response, "无法保存身份修订。");
	return asStandaloneIdentity((await response.json()) as BrowserIdentity);
}

async function getBrowserIdentityByIdentityKey(identityKey: string): Promise<StandaloneIdentity> {
	const response = await fetch(`${browserIdentitiesBase}/${encodeURIComponent(identityKey)}`, {
		credentials: "include",
	});
	if (!response.ok) throw await apiProblem(response, "无法读取浏览器身份。");
	return asStandaloneIdentity((await response.json()) as BrowserIdentity);
}

async function startIdentityOperation(operationsBase: string, expectedRowVersion: number): Promise<BrowserOperation> {
	const response = await fetch(`${operationsBase}/operations`, {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
	});
	if (!response.ok) throw await apiProblem(response, "无法开始浏览器登录。");
	return (await response.json()) as BrowserOperation;
}

async function fetchIdentityOperation(operationsBase: string, operationId: string): Promise<BrowserOperation> {
	const response = await fetch(`${operationsBase}/operations/${encodeURIComponent(operationId)}`, {
		credentials: "include",
	});
	if (!response.ok) throw await apiProblem(response, "无法读取浏览器操作。");
	return (await response.json()) as BrowserOperation;
}

async function commandIdentityOperation(
	operationsBase: string,
	operation: BrowserOperation,
	action: "publish" | "cancel",
): Promise<BrowserOperation> {
	const response = await fetch(`${operationsBase}/operations/${encodeURIComponent(operation.id)}/${action}`, {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedOperationRowVersion: operation.rowVersion }),
	});
	if (!response.ok) throw await apiProblem(response, "浏览器操作失败。");
	return (await response.json()) as BrowserOperation;
}

export interface PluginConfigurationProps
	extends Pick<WorkspaceModuleProps, "navigate" | "suspended"> {
	platform: IntegrationPlatform;
	/** Current workbench route; the sub-path after the platform segment selects an instance. */
	route: string;
}

/** Entry point wired by the integrations route host for kubernetes and browser. */
export function PluginConfiguration({
	platform,
	route,
	navigate,
	suspended,
}: PluginConfigurationProps) {
	if (platform === "kubernetes")
		return (
			<KubernetesConfiguration
				route={route}
				navigate={navigate}
				suspended={suspended}
			/>
		);
	if (platform === "browser")
		return <BrowserConfiguration navigate={navigate} suspended={suspended} />;
	return (
		<Empty className="min-h-56">
			<EmptyHeader>
				<EmptyTitle>该平台没有独立配置页</EmptyTitle>
				<EmptyDescription>
					此平台由业务声明或其他模块直接使用，请返回平台目录。
				</EmptyDescription>
			</EmptyHeader>
		</Empty>
	);
}

function connectionNameFromRoute(route: string): string | undefined {
	const parts = new URL(route, "https://workbench.invalid")
		.pathname.split("/")
		.filter(Boolean);
	return parts[0] === "integrations" &&
		parts[1] === "kubernetes" &&
		parts[2]
		? decodeURIComponent(parts[2])
		: undefined;
}

const statusLabel = (item: ConnectionSummaryView) =>
	item.revalidationRequired ? "需要重新验证" : item.enabled ? "已启用" : "已停用";

/** Polls the authoritative attempt endpoint until its terminal state is visible. */
async function waitForProbeAttempt(
	name: string,
	attemptId: string,
): Promise<ProbeAttemptView> {
	for (let poll = 0; poll < 120; poll += 1) {
		const attempt = await fetchProbeAttempt(name, attemptId);
		if (terminalProbeStates.includes(attempt.state)) return attempt;
		await new Promise((resolve) => setTimeout(resolve, 500));
	}
	throw new Error("连通性验证超时，请稍后重新探测。");
}

/**
 * Enable fencing: only a passed result bound to the connection's current
 * revision and credential generation may enable it, mirroring the shared
 * connection store contract. Results come from the workbench client, whose
 * projection carries the revision/generation identity used here.
 */
async function qualifiedProbeResult(
	name: string,
	revisionId?: string,
	generationId?: string,
) {
	const results = await workbenchApi.listProbeResults(name);
	return results.find(
		(result) =>
			result.connectionType === "kubernetes" &&
			result.outcome === "passed" &&
			result.connectionRevisionId === revisionId &&
			result.credentialGenerationId === generationId,
	);
}

function KubernetesConfiguration({
	route,
	navigate,
	suspended,
}: Omit<PluginConfigurationProps, "platform">) {
	const name = connectionNameFromRoute(route);
	return name ? (
		<KubernetesConnectionDetail
			name={name}
			navigate={navigate}
			suspended={suspended}
		/>
	) : (
		<KubernetesOverview navigate={navigate} suspended={suspended} />
	);
}

/** Overview: the real create form on the shared connection store plus the kubernetes subset listing. */
function KubernetesOverview({
	navigate,
	suspended,
}: Pick<PluginConfigurationProps, "navigate" | "suspended">) {
	const [items, setItems] = useState<ConnectionSummaryView[]>([]);
	const [listError, setListError] = useState("");
	const [loading, setLoading] = useState(true);
	const [name, setName] = useState("");
	const [contextName, setContextName] = useState("");
	const [defaultNamespace, setDefaultNamespace] = useState("");
	const [kubeconfig, setKubeconfig] = useState("");
	const [created, setCreated] = useState("");
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");

	const loadList = useCallback(async () => {
		try {
			const page = await listConnections();
			setItems(page.filter((item) => item.type === "kubernetes"));
			setListError("");
		} catch (reason) {
			setListError(messageOf(reason, "暂时无法读取连接列表。"));
		} finally {
			setLoading(false);
		}
	}, []);
	useEffect(() => {
		void loadList();
	}, [loadList]);

	useEffect(() => {
		if (suspended) setKubeconfig("");
	}, [suspended]);

	async function submit(event: FormEvent) {
		event.preventDefault();
		if (saving || suspended) return;
		const connectionName = created || name.trim();
		setSaving(true);
		setError("");
		try {
			if (!created) {
				// The store creates connections disabled; kubeconfig is a write-only secret.
				const creating = createConnection(connectionName, {
					type: "kubernetes",
					contextName: contextName.trim(),
					defaultNamespace: defaultNamespace.trim(),
					kubeconfig,
				});
				setKubeconfig("");
				await creating;
				setCreated(connectionName);
			}
			const detail = await fetchConnection(connectionName);
			const attempt = await probeConnection(connectionName);
			const settled = await waitForProbeAttempt(connectionName, attempt.id);
			if (settled.state !== "Succeeded")
				throw new Error(
					"连通性验证未通过。已创建的连接保持停用；修正集群访问后可重新验证，不会再次创建。",
				);
			const qualified = await qualifiedProbeResult(
				connectionName,
				detail.currentRevisionId,
				detail.currentCredentialGenerationId,
			);
			if (!qualified)
				throw new Error(
					"未收到绑定当前配置修订的通过结果，连接保持停用；请重新验证。",
				);
			await enableConnection(connectionName, detail.rowVersion, qualified.id);
			void loadList();
			navigate(`/integrations/kubernetes/${encodeURIComponent(connectionName)}`);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成连接生命周期。"));
		} finally {
			setSaving(false);
			setKubeconfig("");
		}
	}

	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">
					配置 Kubernetes
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					通过共享连接存储接入集群：先创建停用连接，再验证连通性并显式启用。
				</p>
			</div>
			<form onSubmit={submit}>
				<Card>
					<CardHeader>
						<CardTitle>新建连接</CardTitle>
						<CardDescription>
							{created
								? `“${created}” 已创建且保持停用。重新验证不会再次创建。`
								: "Kubeconfig 仅用于提交，不会回显或写入业务声明。"}
						</CardDescription>
					</CardHeader>
					<CardContent>
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertTitle>无法完成连接生命周期</AlertTitle>
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							<Field>
								<FieldLabel htmlFor="kubernetes-name">实例名称</FieldLabel>
								<Input
									id="kubernetes-name"
									value={created || name}
									onChange={(event) => setName(event.target.value)}
									required
									disabled={saving || suspended || Boolean(created)}
								/>
								<FieldDescription>
									业务声明通过该名称显式引用连接；不能与现有连接重名。
								</FieldDescription>
							</Field>
							<Field>
								<FieldLabel htmlFor="kubernetes-context">
									Context 名称
								</FieldLabel>
								<Input
									id="kubernetes-context"
									value={contextName}
									onChange={(event) => setContextName(event.target.value)}
									required
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="kubernetes-namespace">
									默认 Namespace
								</FieldLabel>
								<Input
									id="kubernetes-namespace"
									value={defaultNamespace}
									onChange={(event) => setDefaultNamespace(event.target.value)}
									required
									disabled={saving || suspended || Boolean(created)}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="kubernetes-kubeconfig">
									Kubeconfig
								</FieldLabel>
								<Textarea
									id="kubernetes-kubeconfig"
									value={kubeconfig}
									onChange={(event) => setKubeconfig(event.target.value)}
									required={!created}
									disabled={saving || suspended || Boolean(created)}
								/>
								<FieldDescription>
									提交即清除；不会写入业务声明、日志或浏览器存储。
								</FieldDescription>
							</Field>
							<Button
								type="submit"
								disabled={
									saving ||
									suspended ||
									!name.trim() ||
									!contextName.trim() ||
									!defaultNamespace.trim() ||
									(!created && !kubeconfig.trim())
								}
							>
								{saving && (
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
									/>
								)}
								{saving
									? "正在处理…"
									: created
										? "重新验证并启用"
										: "创建、验证并启用"}
							</Button>
						</FieldGroup>
					</CardContent>
				</Card>
			</form>
			<Card>
				<CardHeader>
					<CardTitle>已有连接</CardTitle>
					<CardDescription>
						仅显示 Kubernetes 连接；指标与模型连接在各自模块管理。
					</CardDescription>
				</CardHeader>
				<CardContent className="flex flex-col gap-4">
					{listError && (
						<Alert variant="destructive">
							<AlertTitle>无法读取连接列表</AlertTitle>
							<AlertDescription>{listError}</AlertDescription>
						</Alert>
					)}
					{loading ? (
						<p role="status" className="text-sm text-muted-foreground">
							正在读取连接列表…
						</p>
					) : !listError && items.length === 0 ? (
						<Empty className="min-h-40">
							<EmptyHeader>
								<EmptyTitle>尚无 Kubernetes 连接</EmptyTitle>
								<EmptyDescription>
									创建第一个连接后，业务声明即可引用该集群。
								</EmptyDescription>
							</EmptyHeader>
						</Empty>
					) : (
						<Table>
							<TableHeader>
								<TableRow>
									<TableHead className="w-[40%]">实例名称</TableHead>
									<TableHead className="w-[25%]">状态</TableHead>
									<TableHead className="w-[20%]">版本</TableHead>
									<TableHead>
										<span className="sr-only">操作</span>
									</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{items.map((item) => (
									<TableRow key={item.name} className="hover:bg-muted/50">
										<TableCell className="truncate font-medium">
											{item.name}
										</TableCell>
										<TableCell>
											<Badge
												variant={
													item.enabled && !item.revalidationRequired
														? "secondary"
														: "outline"
												}
											>
												{statusLabel(item)}
											</Badge>
										</TableCell>
										<TableCell>v{item.rowVersion}</TableCell>
										<TableCell>
											<Button
												size="sm"
												variant="ghost"
												onClick={() =>
													navigate(
														`/integrations/kubernetes/${encodeURIComponent(item.name)}`,
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
					)}
				</CardContent>
			</Card>
		</section>
	);
}

/** Detail: probe, enable/disable fencing, and rotation on one connection. */
function KubernetesConnectionDetail({
	name,
	navigate,
	suspended,
}: {
	name: string;
	navigate: (to: string) => void;
	suspended: boolean;
}) {
	const [detail, setDetail] = useState<ConnectionDetailView>();
	const [attempt, setAttempt] = useState<ProbeAttemptView>();
	const [rotating, setRotating] = useState(false);
	const [rotationContext, setRotationContext] = useState("");
	const [rotationNamespace, setRotationNamespace] = useState("");
	const [rotationKubeconfig, setRotationKubeconfig] = useState("");
	const [confirmation, setConfirmation] = useState<"enable" | "disable">();
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);

	const load = useCallback(async () => {
		try {
			const next = await fetchConnection(name);
			setDetail(next);
			setRotationContext(
				typeof next.config.contextName === "string"
					? next.config.contextName
					: "",
			);
			setRotationNamespace(
				typeof next.config.defaultNamespace === "string"
					? next.config.defaultNamespace
					: "",
			);
			if (next.activeProbeAttempt && !terminalProbeStates.includes(next.activeProbeAttempt.state))
				setAttempt(next.activeProbeAttempt);
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法读取连接详情。"));
		} finally {
			setLoading(false);
		}
	}, [name]);
	useEffect(() => {
		void load();
	}, [load]);

	// Probe polling belongs to the detail view: it refreshes qualified results
	// once the attempt settles so enable fencing sees the fresh pair.
	useEffect(() => {
		if (!attempt || terminalProbeStates.includes(attempt.state)) return;
		const timer = window.setInterval(() => {
			void fetchProbeAttempt(name, attempt.id)
				.then(async (next) => {
					setAttempt(next);
					if (terminalProbeStates.includes(next.state)) await load();
				})
				.catch((reason) => setError(messageOf(reason, "探测状态读取失败。")));
		}, 1500);
		return () => window.clearInterval(timer);
	}, [attempt, load, name]);

	useEffect(() => {
		if (suspended) setRotationKubeconfig("");
	}, [suspended]);

	const mutationDisabled = suspended || busy || loading || !detail;

	async function probe() {
		setBusy(true);
		setError("");
		try {
			setAttempt(await probeConnection(name));
		} catch (reason) {
			setError(messageOf(reason, "暂时无法发起探测。"));
		} finally {
			setBusy(false);
		}
	}

	async function changeEnabled() {
		if (!detail || !confirmation) return;
		setBusy(true);
		setError("");
		try {
			if (confirmation === "disable") {
				await disableConnection(name, detail.rowVersion);
			} else {
				const qualified = await qualifiedProbeResult(
					name,
					detail.currentRevisionId,
					detail.currentCredentialGenerationId,
				);
				if (!qualified)
					throw new Error(
						"必须使用当前 revision 和凭据 generation 的已通过探测结果启用。请先探测连接。",
					);
				await enableConnection(name, detail.rowVersion, qualified.id);
			}
			setConfirmation(undefined);
			await load();
		} catch (reason) {
			setError(messageOf(reason, "暂时无法更新连接。"));
		} finally {
			setBusy(false);
		}
	}

	async function rotate(event: FormEvent) {
		event.preventDefault();
		if (!detail || suspended) return;
		setBusy(true);
		setError("");
		try {
			await rotateConnection(name, detail.rowVersion, {
				type: "kubernetes",
				contextName: rotationContext.trim(),
				defaultNamespace: rotationNamespace.trim(),
				kubeconfig: rotationKubeconfig,
			});
			setRotationKubeconfig("");
			setRotating(false);
			await load();
		} catch (reason) {
			setError(messageOf(reason, "暂时无法轮换连接凭据。"));
		} finally {
			setBusy(false);
		}
	}

	if (loading)
		return (
			<p role="status" className="text-sm text-muted-foreground">
				正在读取 Kubernetes 连接…
			</p>
		);
	if (!detail)
		return (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法读取连接</AlertTitle>
					<AlertDescription>
						{error || "该连接不存在或暂时无法读取。"}
					</AlertDescription>
				</Alert>
				<Button
					className="self-start"
					onClick={() => navigate("/integrations/kubernetes")}
				>
					返回 Kubernetes
				</Button>
			</section>
		);

	return (
		<section className="flex flex-col gap-6">
			<div>
				<Button
					variant="ghost"
					className="-ml-3"
					onClick={() => navigate("/integrations/kubernetes")}
				>
					返回 Kubernetes
				</Button>
				<h1 className="text-2xl font-semibold tracking-tight">{detail.name}</h1>
				<p className="mt-1 flex items-center gap-2 text-sm text-muted-foreground">
					<Badge variant="outline">Kubernetes</Badge>
					<Badge
						variant={
							detail.enabled && !detail.revalidationRequired
								? "secondary"
								: "outline"
						}
					>
						{statusLabel(detail)}
					</Badge>
					<span>版本 {detail.rowVersion}</span>
				</p>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<Card>
				<CardHeader>
					<CardTitle>连接状态</CardTitle>
					<CardDescription>
						端点与凭据不回显；轮换会创建新的配置修订与凭据代次。
					</CardDescription>
				</CardHeader>
				<CardContent className="flex flex-col gap-5">
					<dl className="grid gap-4 text-sm sm:grid-cols-2">
						<div>
							<dt className="text-muted-foreground">Context</dt>
							<dd className="mt-1 font-medium">
								{String(detail.config.contextName ?? "—")}
							</dd>
						</div>
						<div>
							<dt className="text-muted-foreground">默认 Namespace</dt>
							<dd className="mt-1 font-medium">
								{String(detail.config.defaultNamespace ?? "—")}
							</dd>
						</div>
					</dl>
					{attempt && (
						<p className="rounded-md bg-muted p-3 text-sm">
							探测 {attempt.id}：{attempt.state}
							{attempt.terminationReason ? `（${attempt.terminationReason}）` : ""}
						</p>
					)}
					<Separator />
					<div className="flex flex-wrap gap-2">
						<Button
							variant="outline"
							disabled={mutationDisabled}
							onClick={() => void probe()}
						>
							<RefreshCw data-icon="inline-start" />
							探测连接
						</Button>
						{detail.enabled ? (
							<Button
								variant="secondary"
								disabled={mutationDisabled}
								onClick={() => setConfirmation("disable")}
							>
								停用连接
							</Button>
						) : (
							<Button
								disabled={mutationDisabled}
								onClick={() => setConfirmation("enable")}
							>
								启用连接
							</Button>
						)}
						<Button
							variant="outline"
							disabled={mutationDisabled}
							onClick={() => setRotating((current) => !current)}
						>
							<RotateCw data-icon="inline-start" />
							轮换凭据
						</Button>
					</div>
					<AlertDialog
						open={Boolean(confirmation)}
						onOpenChange={(open) => {
							if (!open && !busy) setConfirmation(undefined);
						}}
					>
						<AlertDialogContent>
							<AlertDialogHeader>
								<AlertDialogTitle>
									{confirmation === "disable" ? "停用此连接？" : "启用此连接？"}
								</AlertDialogTitle>
								<AlertDialogDescription>
									{confirmation === "disable"
										? "停用会立即阻止业务声明继续使用该连接。"
										: "启用要求存在绑定当前配置修订与凭据代次的通过探测结果。"}
								</AlertDialogDescription>
							</AlertDialogHeader>
							<AlertDialogFooter>
								<AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
								<Button disabled={busy} onClick={() => void changeEnabled()}>
									{confirmation === "disable" ? "确认停用" : "确认启用"}
								</Button>
							</AlertDialogFooter>
						</AlertDialogContent>
					</AlertDialog>
				</CardContent>
			</Card>
			{rotating && (
				<form onSubmit={rotate}>
					<Card>
						<CardHeader>
							<CardTitle>轮换凭据</CardTitle>
							<CardDescription>
								Context 与 Namespace 已按当前修订预填；Kubeconfig 不会回显，必须重新提供。
							</CardDescription>
						</CardHeader>
						<CardContent>
							<FieldGroup>
								<Field>
									<FieldLabel htmlFor="rotate-context">Context 名称</FieldLabel>
									<Input
										id="rotate-context"
										value={rotationContext}
										onChange={(event) => setRotationContext(event.target.value)}
										required
										disabled={busy || suspended}
									/>
								</Field>
								<Field>
									<FieldLabel htmlFor="rotate-namespace">
										默认 Namespace
									</FieldLabel>
									<Input
										id="rotate-namespace"
										value={rotationNamespace}
										onChange={(event) =>
											setRotationNamespace(event.target.value)
										}
										required
										disabled={busy || suspended}
									/>
								</Field>
								<Field>
									<FieldLabel htmlFor="rotate-kubeconfig">Kubeconfig</FieldLabel>
									<Textarea
										id="rotate-kubeconfig"
										value={rotationKubeconfig}
										onChange={(event) =>
											setRotationKubeconfig(event.target.value)
										}
										required
										disabled={busy || suspended}
									/>
									<FieldDescription>
										提交即清除；仅服务端持久化，不会回显或进入普通响应。
									</FieldDescription>
								</Field>
								<Button
									type="submit"
									disabled={
										busy ||
										suspended ||
										!rotationContext.trim() ||
										!rotationNamespace.trim() ||
										!rotationKubeconfig.trim()
									}
								>
									{busy && (
										<LoaderCircle
											className="animate-spin"
											data-icon="inline-start"
										/>
									)}
									保存新版本
								</Button>
							</FieldGroup>
						</CardContent>
					</Card>
				</form>
			)}
		</section>
	);
}

type CatalogProperty = { type?: string; title?: string; enum?: unknown[]; default?: unknown };
type CatalogJourney = { purpose?: string; version?: number; summary?: string; params_schema?: { properties?: Record<string, CatalogProperty> } };

/** Uses the versioned server catalog, never free-form journey IDs or versions. */
function authenticationJourneys(
	catalog: Record<string, unknown> | undefined,
): Array<[string, CatalogJourney]> {
	const journeys = catalog?.journeys;
	if (!journeys || typeof journeys !== "object") return [];
	return Object.entries(journeys as Record<string, CatalogJourney>).filter(
		([, item]) => item.purpose === "authentication_probe",
	);
}

/**
 * Browser plugin entry: strictly capability-driven. While the catalog marks
 * the plugin disabled there is no entry at all — not even read-only ones.
 */
// navigate is part of the shared props contract even though this entry only
// opens in-place panels today; the catalog/gate rendering stays self-contained.
function BrowserConfiguration({
	suspended,
}: Pick<PluginConfigurationProps, "navigate" | "suspended">) {
	const [checking, setChecking] = useState(true);
	const [enabled, setEnabled] = useState(false);
	const [catalog, setCatalog] = useState<JourneyCatalogView>();
	const [systems, setSystems] = useState<BusinessSystemSummary[]>([]);
	const [identities, setIdentities] = useState<StandaloneIdentity[]>([]);
	const [selected, setSelected] = useState<{ key: string; kind: "standalone" | "legacy" }>();
	const [error, setError] = useState("");

	const load = useCallback(async () => {
		setChecking(true);
		setError("");
		try {
			const plugins = await listIntegrationPlugins();
			const browser = plugins.find((item) => item.id === "browser");
			if (!browser?.enabled) {
				// No capability, no entry: skip every dependent read.
				setEnabled(false);
				setCatalog(undefined);
				setSystems([]);
				setIdentities([]);
				return;
			}
			setEnabled(true);
			const [nextCatalog, nextSystems, nextIdentities] = await Promise.all([
				getJourneyCatalog(),
				listBusinessSystems(),
				listBrowserIdentities(),
			]);
			setCatalog(nextCatalog);
			setSystems(nextSystems);
			setIdentities(nextIdentities);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法读取浏览器接入状态。"));
		} finally {
			setChecking(false);
		}
	}, []);
	useEffect(() => {
		void load();
	}, [load]);

	if (checking)
		return (
			<p role="status" className="text-sm text-muted-foreground">
				正在读取浏览器接入状态…
			</p>
		);
	if (error)
		return (
			<section className="flex flex-col gap-4">
				<Alert variant="destructive">
					<AlertTitle>无法读取浏览器接入</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
				<Button
					className="self-start"
					variant="outline"
					onClick={() => void load()}
				>
					重试
				</Button>
			</section>
		);
	if (!enabled)
		return (
			<Empty className="min-h-56">
				<EmptyHeader>
					<EmptyTitle>浏览器接入未启用</EmptyTitle>
					<EmptyDescription>
						当前部署未启用受控浏览器插件，因此没有身份或 Journey 入口。启用后这里会出现配置与登录入口。
					</EmptyDescription>
				</EmptyHeader>
			</Empty>
		);

	const journeys = authenticationJourneys(catalog?.catalogJson);
	const savedIdentities = systems.filter(
		(item) => item.browserIdentityState !== "none",
	);
	return (
		<section className="flex flex-col gap-6">
			<div>
				<h1 className="text-2xl font-semibold tracking-tight">配置受控浏览器</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					管理认证探测 Journey 与已保存的浏览器身份；登录始终通过受控远程浏览器完成。
				</p>
			</div>
			<Card>
				<CardHeader>
					<CardTitle>Journey 目录</CardTitle>
					<CardDescription>
						仅服务器版本化目录中的 authentication_probe 可用于身份认证探测。
					</CardDescription>
				</CardHeader>
				<CardContent className="flex flex-col gap-4">
					{journeys.length === 0 ? (
						<Empty className="min-h-32">
							<EmptyHeader>
								<EmptyTitle>没有可用的认证探测 Journey</EmptyTitle>
								<EmptyDescription>
									请先在部署中更新 Journey 目录。
								</EmptyDescription>
							</EmptyHeader>
						</Empty>
					) : (
						<Table>
							<TableHeader>
								<TableRow>
									<TableHead className="w-[35%]">Journey</TableHead>
									<TableHead className="w-[15%]">版本</TableHead>
									<TableHead>说明</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{journeys.map(([id, journey]) => (
									<TableRow key={id}>
										<TableCell className="font-medium">{id}</TableCell>
										<TableCell>v{journey.version ?? 1}</TableCell>
										<TableCell className="truncate text-muted-foreground">
											{journey.summary ?? "—"}
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>
			<Card>
				<CardHeader>
					<CardTitle>浏览器身份</CardTitle>
					<CardDescription>
						独立身份与业务引用的历史身份在此统一管理；创建身份不绑定任何业务系统，业务声明通过授权引用使用身份。
					</CardDescription>
				</CardHeader>
				<CardContent className="flex flex-col gap-4">
					<div className="flex flex-col gap-3">
						<div>
							<h3 className="font-semibold">创建独立身份</h3>
							<p className="text-sm text-muted-foreground">
								身份独立于业务系统创建；业务声明通过授权引用使用身份，默认不跨业务共享。
							</p>
						</div>
						<IdentityRevisionForm
							journeys={journeys}
							initial={{ name: "", startUrl: "", journeyId: journeys[0]?.[0] ?? "", params: {} }}
							idPrefix="identity-create"
							suspended={suspended}
							submitLabel="创建身份"
							busyLabel="正在创建…"
							errorFallback="无法创建浏览器身份。"
							onSubmit={async (input) => {
								const identity = await createBrowserIdentity(input);
								setIdentities((current) => [identity, ...current]);
								// Selection and every identity-scoped route key off the
								// stable identityKey; the numeric id only backs React keys.
								setSelected({ key: identity.identityKey, kind: "standalone" });
							}}
						/>
					</div>
					<Separator />
					{identities.length === 0 && savedIdentities.length === 0 ? (
						<Empty className="min-h-32">
							<EmptyHeader>
								<EmptyTitle>尚无已保存的浏览器身份</EmptyTitle>
								<EmptyDescription>
									创建第一个独立身份后，这里会列出并可直接发起人工登录。
								</EmptyDescription>
							</EmptyHeader>
						</Empty>
					) : (
						<div className="flex flex-wrap gap-2">
							{identities.map((identity) => (
								<Button
									key={identity.id}
									variant={
										selected?.kind === "standalone" && selected.key === identity.identityKey
											? "secondary"
											: "outline"
									}
									onClick={() =>
										setSelected({ key: identity.identityKey, kind: "standalone" })
									}
								>
									{identity.currentRevision.name}
									<Badge variant="secondary">独立</Badge>
									<Badge
										variant={
											identity.state === "Ready" ? "secondary" : "outline"
										}
									>
										{identity.state === "Ready" ? "就绪" : "需要重新登录"}
									</Badge>
								</Button>
							))}
							{savedIdentities.map((item) => (
								<Button
									key={item.key}
									variant={
										selected?.kind === "legacy" && selected.key === item.key
											? "secondary"
											: "outline"
									}
									onClick={() =>
										setSelected({ key: item.key, kind: "legacy" })
									}
								>
									{item.displayName}
									<Badge
										variant={
											item.browserIdentityState === "Ready"
												? "secondary"
												: "outline"
										}
									>
										{item.browserIdentityState === "Ready"
											? "就绪"
											: "需要重新登录"}
									</Badge>
								</Button>
							))}
						</div>
					)}
					{selected && (
						<SavedIdentityPanel
							identityKey={selected.key}
							kind={selected.kind}
							journeys={journeys}
							suspended={suspended}
							onIdentityChanged={(next) =>
								setIdentities((current) =>
									current.map((item) =>
										item.identityKey === next.identityKey ? next : item,
									),
								)
							}
						/>
					)}
				</CardContent>
			</Card>
		</section>
	);
}

/**
 * Shared create/edit form for standalone identity revisions. Journeys come
 * from the versioned server catalog; the chosen journey's id, version and
 * typed params freeze into the new revision exactly like the legacy contract.
 * The caller owns the write (POST create or PUT new revision) and the
 * post-success wiring; the form owns input state, catalog defaults and errors.
 */
interface IdentityFormValue {
	name: string;
	startUrl: string;
	journeyId: string;
	params: Record<string, string>;
}

function IdentityRevisionForm({
	journeys,
	initial,
	idPrefix,
	suspended,
	submitLabel,
	busyLabel,
	errorFallback,
	onSubmit,
}: {
	journeys: Array<[string, CatalogJourney]>;
	initial: IdentityFormValue;
	/** Unique per rendered form instance: create and edit forms can coexist. */
	idPrefix: string;
	suspended: boolean;
	submitLabel: string;
	busyLabel: string;
	errorFallback: string;
	onSubmit: (input: {
		name: string;
		startUrl: string;
		authenticationProbe: { journeyId: string; journeyVersion: number; params: Record<string, unknown> };
	}) => Promise<void>;
}) {
	const [name, setName] = useState(initial.name);
	const [startUrl, setStartUrl] = useState(initial.startUrl);
	const [journeyId, setJourneyId] = useState(initial.journeyId);
	const [params, setParams] = useState<Record<string, string>>(initial.params);
	const [saving, setSaving] = useState(false);
	const [error, setError] = useState("");
	const selectedJourney = journeys.find(([id]) => id === journeyId)?.[1];

	// Catalog defaults fill params not yet touched; user edits always win.
	useEffect(() => {
		if (!selectedJourney) return;
		const properties = selectedJourney.params_schema?.properties ?? {};
		setParams((current) =>
			Object.fromEntries(
				Object.entries(properties).map(([key, schema]) => [
					key,
					current[key] ??
						(schema.default === undefined ? "" : String(schema.default)),
				]),
			),
		);
	}, [journeyId, selectedJourney]);

	async function submit() {
		if (!selectedJourney || suspended || saving || !name.trim() || !startUrl.trim())
			return;
		setSaving(true);
		setError("");
		try {
			const typedParams = Object.fromEntries(
				Object.entries(selectedJourney.params_schema?.properties ?? {}).map(
					([key, schema]) => {
						const raw = params[key] ?? "";
						if (schema.type === "integer" || schema.type === "number")
							return [key, Number(raw)];
						if (schema.type === "boolean") return [key, raw === "true"];
						return [key, raw];
					},
				),
			);
			await onSubmit({
				name: name.trim(),
				startUrl: startUrl.trim(),
				authenticationProbe: {
					journeyId,
					journeyVersion: selectedJourney.version ?? 1,
					params: typedParams,
				},
			});
		} catch (reason) {
			setError(messageOf(reason, errorFallback));
		} finally {
			setSaving(false);
		}
	}

	if (!journeys.length)
		return (
			<Alert variant="destructive">
				<AlertDescription>
					Journey Catalog 中没有可用的认证探测，无法安全配置身份。
				</AlertDescription>
			</Alert>
		);
	return (
		<div className="flex flex-col gap-3">
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<FieldGroup>
				<Field>
					<FieldLabel htmlFor={`${idPrefix}-name`}>
						名称
					</FieldLabel>
					<Input
						id={`${idPrefix}-name`}
						value={name}
						onChange={(event) => setName(event.target.value)}
						disabled={suspended || saving}
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor={`${idPrefix}-url`}>
						起始 URL
					</FieldLabel>
					<Input
						id={`${idPrefix}-url`}
						type="url"
						value={startUrl}
						onChange={(event) => setStartUrl(event.target.value)}
						disabled={suspended || saving}
						placeholder="https://target.example"
					/>
				</Field>
				<Field>
					<FieldLabel>认证探测 Journey</FieldLabel>
					<Select
						value={journeyId}
						onValueChange={setJourneyId}
						disabled={suspended || saving}
					>
						<SelectTrigger aria-label="认证探测 Journey" className="w-full">
							<SelectValue placeholder="选择认证探测" />
						</SelectTrigger>
						<SelectContent>
							{journeys.map(([id, journey]) => (
								<SelectItem key={id} value={id}>
									{journey.summary ?? id} · v{journey.version ?? 1}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<FieldDescription>
						仅显示目录中的 authentication_probe；提交会冻结所选 Journey 的
						ID、版本和类型化参数为新修订。
					</FieldDescription>
				</Field>
				{Object.entries(selectedJourney?.params_schema?.properties ?? {}).map(
					([key, schema]) => (
						<Field key={key}>
							<FieldLabel
								htmlFor={`${idPrefix}-${key}`}
							>
								{schema.title ?? key}
							</FieldLabel>
							{schema.enum ? (
								<Select
									value={params[key] ?? ""}
									onValueChange={(value) =>
										setParams((current) => ({ ...current, [key]: value }))
									}
									disabled={suspended || saving}
								>
									<SelectTrigger
										id={`${idPrefix}-${key}`}
										className="w-full"
									>
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										{schema.enum.map((value) => (
											<SelectItem key={String(value)} value={String(value)}>
												{String(value)}
											</SelectItem>
										))}
									</SelectContent>
								</Select>
							) : schema.type === "boolean" ? (
								<Select
									value={params[key] ?? "false"}
									onValueChange={(value) =>
										setParams((current) => ({ ...current, [key]: value }))
									}
									disabled={suspended || saving}
								>
									<SelectTrigger
										id={`${idPrefix}-${key}`}
										className="w-full"
									>
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										<SelectItem value="true">true</SelectItem>
										<SelectItem value="false">false</SelectItem>
									</SelectContent>
								</Select>
							) : (
								<Input
									id={`${idPrefix}-${key}`}
									type={
										schema.type === "integer" || schema.type === "number"
											? "number"
											: "text"
									}
									value={params[key] ?? ""}
									onChange={(event) =>
										setParams((current) => ({
											...current,
											[key]: event.target.value,
										}))
									}
									disabled={suspended || saving}
								/>
							)}
						</Field>
					),
				)}
				<Button
					className="self-start"
					disabled={
						suspended || saving || !journeyId || !name.trim() || !startUrl.trim()
					}
					onClick={() => void submit()}
				>
					{saving && (
						<LoaderCircle className="animate-spin" data-icon="inline-start" />
					)}
					{saving ? busyLabel : submitLabel}
				</Button>
			</FieldGroup>
		</div>
	);
}

/**
 * Saved-identity lifecycle extracted from the legacy systems module and owned
 * by the plugin surface now. Writes are single-tracked: only standalone
 * identities operate (login/publish/cancel/edit) through the identity-scoped
 * routes. Legacy business-referenced identities are read-only references with
 * an explicit migration state — the historical browser-login routes stay
 * read-for-reference only and never receive writes from this surface.
 */
function SavedIdentityPanel({
	identityKey,
	kind,
	journeys,
	suspended,
	onIdentityChanged,
}: {
	identityKey: string;
	kind: "standalone" | "legacy";
	journeys: Array<[string, CatalogJourney]>;
	suspended: boolean;
	onIdentityChanged: (identity: StandaloneIdentity) => void;
}) {
	// Writes exist only on the standalone path; undefined disables every
	// operation effect below for legacy references.
	const operationsBase =
		kind === "standalone"
			? `${browserIdentitiesBase}/${encodeURIComponent(identityKey)}`
			: undefined;
	const viewport = useRef<HTMLDivElement>(null);
	const client = useRef<RFB | null>(null);
	const [identity, setIdentity] = useState<BrowserIdentity>();
	const [operation, setOperation] = useState<BrowserOperation>();
	const [message, setMessage] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [editing, setEditing] = useState(false);

	const load = useCallback(async () => {
		try {
			const next =
				kind === "standalone"
					? await getBrowserIdentityByIdentityKey(identityKey)
					: await getBrowserIdentity(identityKey);
			setIdentity(next);
			setOperation(next.currentOperation ?? undefined);
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "无法读取浏览器身份。"));
		}
	}, [identityKey, kind]);
	useEffect(() => {
		void load();
	}, [load]);

	useEffect(() => {
		if (!operationsBase || suspended || !operation || !activeOperationStates.includes(operation.state))
			return;
		const timer = setTimeout(
			() =>
				void fetchIdentityOperation(operationsBase, operation.id)
					.then(setOperation)
					.catch(() => undefined),
			1000,
		);
		return () => clearTimeout(timer);
	}, [operation, operationsBase, suspended]);

	useEffect(() => {
		if (!operationsBase || !operation?.canAttach || !viewport.current || client.current) return;
		const scheme = location.protocol === "https:" ? "wss" : "ws";
		const rfb = new RFB(
			viewport.current,
			`${scheme}://${location.host}${operationsBase}/operations/${encodeURIComponent(operation.id)}/ws`,
			{ shared: false },
		);
		rfb.scaleViewport = true;
		rfb.viewOnly = false;
		rfb.addEventListener("connect", () =>
			setMessage("安全浏览器已连接。登录输入不会被记录。"),
		);
		rfb.addEventListener("disconnect", () =>
			setMessage("远程浏览器已断开，可等待重连或取消操作。"),
		);
		client.current = rfb;
		return () => {
			rfb.disconnect();
			if (client.current === rfb) client.current = null;
		};
	}, [operation?.canAttach, operation?.id, operationsBase]);

	async function start() {
		if (!identity || !operationsBase || suspended) return;
		setBusy(true);
		setError("");
		try {
			setOperation(
				await startIdentityOperation(operationsBase, identity.rowVersion),
			);
		} catch (reason) {
			setError(messageOf(reason, "无法开始浏览器登录。"));
		} finally {
			setBusy(false);
		}
	}

	async function command(action: "publish" | "cancel") {
		if (!operation || !operationsBase || suspended) return;
		setBusy(true);
		setError("");
		try {
			setOperation(await commandIdentityOperation(operationsBase, operation, action));
		} catch (reason) {
			setError(messageOf(reason, "浏览器操作失败。"));
		} finally {
			setBusy(false);
		}
	}

	if (!identity)
		return error ? (
			<Alert variant="destructive">
				<AlertDescription>{error}</AlertDescription>
			</Alert>
		) : (
			<p role="status" className="text-sm text-muted-foreground">
				正在读取身份…
			</p>
		);

	return (
		<section className="flex flex-col gap-3 rounded-md border p-4" aria-label={`浏览器身份：${identityKey}`}>
			<div className="flex flex-wrap items-center justify-between gap-2">
				<div>
					<h3 className="font-semibold">{identity.currentRevision.name}</h3>
					<p className="text-sm text-muted-foreground">
						revision {identity.currentRevision.revision} ·{" "}
						{identity.currentRevision.startUrl}
					</p>
				</div>
				<Badge variant={identity.state === "Ready" ? "secondary" : "outline"}>
					{identity.state === "Ready" ? "就绪" : "需要重新登录"}
				</Badge>
			</div>
			{identity.currentProfile ? (
				<p className="text-sm text-muted-foreground">
					已发布配置：generation {identity.currentProfile.generation} ·{" "}
					{identity.currentProfile.chromiumRevision}
				</p>
			) : (
				<p className="text-sm text-muted-foreground">尚无已发布的浏览器配置。</p>
			)}
			{identity.lastProbe ? (
				<p className="text-xs text-muted-foreground">
					最近探测：{identity.lastProbe.result} · {identity.lastProbe.observedAt}
				</p>
			) : null}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{message && (
				<Alert>
					<AlertDescription>{message}</AlertDescription>
				</Alert>
			)}
			{kind === "legacy" ? (
				<Alert>
					<AlertTitle>历史业务引用身份（仅可查看）</AlertTitle>
					<AlertDescription>
						该身份随旧业务绑定保留为只读引用；登录、发布与取消已统一迁移到独立身份路径，此处不再提供写入口。
					</AlertDescription>
				</Alert>
			) : (
				<>
					<div className="flex gap-2">
						<Button
							variant="outline"
							size="sm"
							disabled={suspended}
							onClick={() => setEditing((current) => !current)}
						>
							编辑修订
						</Button>
					</div>
					{editing && (
						<div className="flex flex-col gap-3 rounded-md border p-3">
							<h4 className="text-sm font-medium">保存新修订</h4>
							<IdentityRevisionForm
								journeys={journeys}
								initial={{
									name: identity.currentRevision.name,
									startUrl: identity.currentRevision.startUrl,
									journeyId: identity.currentRevision.authenticationProbe.journeyId,
									params: Object.fromEntries(
										Object.entries(
											identity.currentRevision.authenticationProbe.params,
										).map(([key, value]) => [key, String(value)]),
									),
								}}
								idPrefix="identity-edit"
								suspended={suspended}
								submitLabel="保存新修订"
								busyLabel="正在保存…"
								errorFallback="无法保存身份修订。"
								onSubmit={async (input) => {
									const next = await updateBrowserIdentity(identityKey, {
										...input,
										expectedRowVersion: identity.rowVersion,
									});
									setIdentity(next);
									setOperation(next.currentOperation ?? undefined);
									setEditing(false);
									onIdentityChanged(next);
								}}
							/>
						</div>
					)}
					{!operation || !activeOperationStates.includes(operation.state) ? (
						<Button
							className="self-start"
							disabled={suspended || busy}
							onClick={() => void start()}
						>
							{busy && (
								<LoaderCircle className="animate-spin" data-icon="inline-start" />
							)}
							开始人工登录
						</Button>
					) : (
						<>
							<p className="text-sm">
								操作 {operation.state}
								{operation.reconnectDeadline
									? `，重连截止 ${operation.reconnectDeadline}`
									: ""}
							</p>
							{operation.canAttach ? (
								<>
									<div
										ref={viewport}
										aria-label="安全远程浏览器"
										className="h-80 rounded-md border bg-muted"
									/>
									<p className="text-xs text-muted-foreground">
										使用远程窗口完成登录；不会保存或显示秘密内容。
									</p>
								</>
							) : (
								<p className="text-sm text-muted-foreground">
									运行时正在准备远程浏览器…
								</p>
							)}
							<div className="flex gap-2">
								{operation.canPublish ? (
									<Button disabled={suspended || busy} onClick={() => void command("publish")}>
										完成并发布
									</Button>
								) : null}
								{operation.canCancel ? (
									<Button
										variant="outline"
										disabled={suspended || busy}
										onClick={() => void command("cancel")}
									>
										取消
									</Button>
								) : null}
							</div>
						</>
					)}
				</>
			)}
		</section>
	);
}
