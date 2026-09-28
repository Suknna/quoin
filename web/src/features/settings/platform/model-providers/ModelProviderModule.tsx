/* eslint-disable react-hooks/exhaustive-deps -- Polling and abort seams intentionally key on the selected connection name only. */

import { Badge } from '@/components/ui/badge';
import { LoaderCircle, Play, Plus, Power, RotateCw } from "lucide-react";
import {
	type ComponentProps,
	type FormEvent,
	useEffect,
	useRef,
	useState,
} from "react";
import {
	type ConnectionDetailView,
	type ConnectionInput,
	type ConnectionRevisionView,
	type ConnectionSummaryView,
	type CredentialGenerationView,
	type ProbeAttemptView,
	type ProbeResultView,
	WorkbenchApiError,
	workbenchApi,
} from "@/api/workbench";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import { ErrorMessage, messageOf, notify } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
	AlertDialog,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Item,
	ItemActions,
	ItemContent,
	ItemDescription,
	ItemGroup,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { PropertyList } from "@/components/workbench/PropertyList";
import { usePolling } from "@/hooks/use-polling";
import { formatDateTime } from "@/lib/format";
import { ModelProviderEditor } from "./ModelProviderEditor";

const terminalStates = ["Succeeded", "Failed", "Cancelled", "Interrupted"];

const probeStateLabels: Record<string, string> = {
	Queued: "排队中",
	Assigned: "已分配",
	Running: "执行中",
	Cancelling: "取消中",
	Succeeded: "成功",
	Failed: "失败",
	Cancelled: "已取消",
	Interrupted: "已中断",
};

const probeOutcomeLabels: Record<string, string> = {
	passed: "通过",
	failed: "未通过",
	cancelled: "已取消",
	interrupted: "已中断",
};

// This legacy editor owns only the three built-in forms. Plugin HTTP kinds
// use the catalog-driven integration editor instead of silently inheriting
// these model-provider/metrics-specific fields.
type EditableConnectionType = "prometheus" | "thanos" | "model_provider";

/** Keeps secret values in component memory and removes them when the workspace is suspended. */
function ConnectionFields({
	type,
	value,
	onChange,
	disabled,
}: {
	type: EditableConnectionType;
	value: Record<string, string>;
	onChange: (field: string, next: string) => void;
	disabled: boolean;
}) {
	const input = (
		field: string,
		label: string,
		extra: Partial<ComponentProps<typeof Input>> = {},
	) => (
		<Field key={field}>
			<FieldLabel htmlFor={`connection-${field}`}>{label}</FieldLabel>
			<Input
				id={`connection-${field}`}
				value={value[field] ?? ""}
				onChange={(event) => onChange(field, event.target.value)}
				disabled={disabled}
				{...extra}
			/>
		</Field>
	);
	if (type === "model_provider")
		return (
			<>
				{input("baseUrl", "Base URL", { type: "url", required: true })}
				{input("apiKey", "API Key", {
					type: "password",
					autoComplete: "new-password",
					required: true,
				})}
				{input("chatModelId", "对话模型 ID", { required: true })}
				{input("embeddingModelId", "Embedding 模型 ID（可选，知识库使用）")}
				{input("contextBudgetTokens", "Context 预算 tokens", {
					type: "number",
					min: 1,
					required: true,
				})}
				{input("maxOutputTokens", "最大输出 tokens", {
					type: "number",
					min: 1,
					required: true,
				})}
			</>
		);
	return (
		<>
			{input("baseUrl", "Base URL", {
				type: "url",
				placeholder: "https://thanos.example",
				required: true,
			})}
			{input("username", "用户名（可选）", { autoComplete: "username" })}
			{input("password", "密码（可选）", {
				type: "password",
				autoComplete: "new-password",
			})}
			<Field>
				<FieldLabel htmlFor="tlsCaPem">TLS CA PEM（可选）</FieldLabel>
				<Textarea
					id="tlsCaPem"
					value={value.tlsCaPem ?? ""}
					onChange={(event) => onChange("tlsCaPem", event.target.value)}
					disabled={disabled}
				/>
			</Field>
			{input("tlsServerName", "TLS Server Name（可选）")}
			<div className="flex items-center gap-2">
				<Checkbox
					id="tlsSkipVerify"
					checked={value.tlsSkipVerify === "true"}
					onCheckedChange={(checked) =>
						onChange("tlsSkipVerify", checked === true ? "true" : "")
					}
					disabled={disabled}
				/>
				<FieldLabel htmlFor="tlsSkipVerify">
					跳过上游 TLS 证书验证（仅在确有必要时启用）
				</FieldLabel>
			</div>
		</>
	);
}

function toInput(
	type: EditableConnectionType,
	fields: Record<string, string>,
): ConnectionInput {
	if (type === "model_provider")
		return {
			type,
			baseUrl: fields.baseUrl,
			apiKey: fields.apiKey,
			chatModelId: fields.chatModelId,
			...(fields.embeddingModelId
				? { embeddingModelId: fields.embeddingModelId }
				: {}),
			contextBudgetTokens: Number(fields.contextBudgetTokens),
			maxOutputTokens: Number(fields.maxOutputTokens),
		};
	return {
		type,
		baseUrl: fields.baseUrl,
		...(fields.username ? { username: fields.username } : {}),
		...(fields.password ? { password: fields.password } : {}),
		...(fields.tlsCaPem ? { tlsCaPem: fields.tlsCaPem } : {}),
		...(fields.tlsServerName ? { tlsServerName: fields.tlsServerName } : {}),
		...(fields.tlsSkipVerify === "true" ? { tlsSkipVerify: true } : {}),
	};
}

const configFieldLabels: Record<string, string> = {
	baseUrl: "Base URL",
	chatModelId: "对话模型 ID",
	embeddingModelId: "Embedding 模型 ID",
	contextBudgetTokens: "Context 预算 tokens",
	maxOutputTokens: "最大输出 tokens",
};

/** Renders the known model-provider config keys as facts; unknown keys fall
 * back to their raw JSON so future server fields stay visible. */
function ConfigFacts({ config }: { config: Record<string, unknown> }) {
	const known = Object.keys(configFieldLabels).filter(
		(key) => config[key] !== undefined && config[key] !== "",
	);
	const rest = Object.fromEntries(
		// type 已由抽屉标题表达，不再作为原始 JSON 回显。
		Object.entries(config).filter(
			([key]) => !configFieldLabels[key] && key !== "type",
		),
	);
	return (
		<>
			<PropertyList
				layout="grid-2"
				className="mt-2"
				entries={known.map((key) => ({
					label: configFieldLabels[key],
					value: (
						<span className="wrap-anywhere font-medium">
							{String(config[key])}
						</span>
					),
				}))}
			/>
			{Object.keys(rest).length > 0 && (
				<pre className="overflow-auto text-xs">
					{JSON.stringify(rest, null, 2)}
				</pre>
			)}
		</>
	);
}

function ConnectionDetail({
	selected,
	onUpdate,
	onRefresh,
	readOnly,
	suspended = false,
}: {
	selected: ConnectionDetailView;
	onUpdate: (connection: ConnectionSummaryView) => void;
	onRefresh: () => void;
	readOnly: boolean;
	suspended?: boolean;
}) {
	const [attempt, setAttempt] = useState<ProbeAttemptView | undefined>(
		selected.activeProbeAttempt,
	);
	const [results, setResults] = useState<ProbeResultView[]>([]);
	const [revisions, setRevisions] = useState<ConnectionRevisionView[]>([]);
	const [generations, setGenerations] = useState<CredentialGenerationView[]>(
		[],
	);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [confirmation, setConfirmation] = useState<
		"enable" | "disable" | undefined
	>();
	const [rotating, setRotating] = useState(false);
	const [rotationFields, setRotationFields] = useState<Record<string, string>>(
		() =>
			Object.entries(selected.config).reduce<Record<string, string>>(
				(fields, [key, value]) =>
					typeof value === "string" ? { ...fields, [key]: value } : fields,
				{},
			),
	);
	const polling = useRef(false);
	const alive = useRef(true);
	const editableType: EditableConnectionType = "model_provider";
	async function loadHistory() {
		try {
			const [nextResults, nextRevisions, nextGenerations] = await Promise.all([
				workbenchApi.listProbeResults(selected.name),
				workbenchApi.listRevisions(selected.name),
				workbenchApi.listCredentialGenerations(selected.name),
			]);
			if (alive.current) {
				setResults(nextResults);
				setRevisions(nextRevisions);
				setGenerations(nextGenerations);
			}
		} catch (reason) {
			if (alive.current) setError(messageOf(reason, "连接历史读取失败。"));
		}
	}
	useEffect(() => {
		alive.current = true;
		setAttempt(selected.activeProbeAttempt);
		setError("");
		void loadHistory();
		return () => {
			alive.current = false;
		};
	}, [selected.name]);
	useEffect(() => {
		if (suspended)
			setRotationFields((current) => ({
				...current,
				password: "",
				kubeconfig: "",
				apiKey: "",
			}));
	}, [suspended]);
	usePolling(
		() => {
			if (!attempt || polling.current) return;
			polling.current = true;
			void workbenchApi
				.fetchProbeAttempt(selected.name, attempt.id)
				.then((next) => {
					if (!alive.current) return;
					setAttempt((current) =>
						current &&
						current.id === next.id &&
						current.rowVersion > next.rowVersion
							? current
							: next,
					);
					if (terminalStates.includes(next.state)) {
						void loadHistory();
						onRefresh();
					}
				})
				.catch((reason) => {
					if (alive.current) setError(messageOf(reason, "探测状态读取失败。"));
				})
				.finally(() => {
					polling.current = false;
				});
		},
		1500,
		Boolean(attempt && !terminalStates.includes(attempt.state) && !suspended),
	);
	async function probe() {
		setBusy(true);
		setError("");
		try {
			const next = await workbenchApi.probeConnection(selected.name);
			setAttempt(next);
			if (terminalStates.includes(next.state)) {
				await loadHistory();
				onRefresh();
			}
		} catch (reason) {
			notify.error(reason, "暂时无法发起探测。");
		} finally {
			setBusy(false);
		}
	}
	async function cancelProbe() {
		if (!attempt) return;
		setBusy(true);
		setError("");
		try {
			setAttempt(
				await workbenchApi.cancelProbeAttempt(
					selected.name,
					attempt.id,
					attempt.rowVersion,
				),
			);
		} catch (reason) {
			notify.error(reason, "暂时无法取消探测。");
		} finally {
			setBusy(false);
		}
	}
	async function changeEnabled() {
		setBusy(true);
		setError("");
		try {
			if (confirmation === "disable")
				onUpdate(
					await workbenchApi.disableConnection(
						selected.name,
						selected.rowVersion,
					),
				);
			else {
				// Enable accepts only a passed result bound to this connection's current type, revision, and credential generation.
				const qualified = results
					.filter(
						(result) =>
							result.connectionType === selected.type &&
							result.outcome === "passed" &&
							result.connectionRevisionId === selected.currentRevisionId &&
							result.credentialGenerationId ===
								selected.currentCredentialGenerationId,
					)
					.sort((left, right) =>
						right.finishedAt.localeCompare(left.finishedAt),
					)[0];
				// A rotation sets revalidationRequired until this exact current-pair
				// result enables the connection. The result identity, rather than the
				// flag itself, proves that this is a fresh qualified revalidation.
				if (!qualified) {
					setError(
						`${selected.type === "model_provider" ? "模型提供方" : "指标连接"}必须使用当前 revision 和凭据 generation 的已通过探测结果启用。`,
					);
					return;
				}
				onUpdate(
					await workbenchApi.enableConnection(
						selected.name,
						selected.rowVersion,
						qualified.id,
					),
				);
			}
			notify.success(confirmation === "disable" ? "已停用连接" : "已启用连接");
			setConfirmation(undefined);
		} catch (reason) {
			if (
				reason instanceof WorkbenchApiError &&
				reason.status === 409 &&
				reason.code === "row_version_conflict"
			) {
				notify.warning("连接版本冲突，正在刷新详情；请核对最新版本后重试。");
				onRefresh();
			} else notify.error(reason, "暂时无法更新连接。");
		} finally {
			setBusy(false);
		}
	}
	async function rotate(event: FormEvent) {
		event.preventDefault();
		setBusy(true);
		setError("");
		try {
			onUpdate(
				await workbenchApi.rotateConnection(
					selected.name,
					selected.rowVersion,
					toInput(editableType, rotationFields),
				),
			);
			setRotationFields((current) => ({
				...current,
				password: "",
				kubeconfig: "",
				apiKey: "",
			}));
			setRotating(false);
			notify.success("已轮换连接凭据");
		} catch (reason) {
			notify.error(reason, "暂时无法轮换连接凭据。");
		} finally {
			setBusy(false);
		}
	}
	const mutationDisabled = readOnly || busy || suspended;
	const probing = Boolean(attempt && !terminalStates.includes(attempt.state));
	return (
		<section className="flex flex-col gap-4">
			{/* 抽屉头部（DetailSheet）负责名称、类型与版本标题。 */}
			{error && <ErrorMessage>{error}</ErrorMessage>}
			<Tabs defaultValue="overview">
				<TabsList variant="line" className="w-full">
					<TabsTrigger value="overview">概览</TabsTrigger>
					<TabsTrigger value="history">历史</TabsTrigger>
				</TabsList>
				<TabsContent value="overview" className="flex flex-col gap-6 pt-4">
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">当前配置</h3>
						<ConfigFacts config={selected.config} />
					</section>
					{attempt && (
						<div className="flex items-center gap-2 rounded-lg border px-3 py-2 text-sm">
							{probing && (
								<LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
							)}
							<span>
								最近一次探测：{probeStateLabels[attempt.state] ?? attempt.state}
								{attempt.terminationReason
									? `（${attempt.terminationReason}）`
									: ""}
							</span>
							{probing && (
								<Button
									className="ml-auto"
									variant="ghost"
									size="sm"
									onClick={() => void cancelProbe()}
									disabled={mutationDisabled}
								>
									取消探测
								</Button>
							)}
						</div>
					)}
					<Separator />
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">操作</h3>
						<ItemGroup>
							<Item size="sm" className="px-0">
								<ItemMedia variant="icon">
									<Play aria-hidden="true" />
								</ItemMedia>
								<ItemContent>
									<ItemTitle>探测连接</ItemTitle>
									<ItemDescription>
										启用前需一次通过的探测。
									</ItemDescription>
								</ItemContent>
								<ItemActions>
									<Button
										size="sm"
										variant="outline"
										onClick={() => void probe()}
										disabled={mutationDisabled || probing}
									>
										探测连接
									</Button>
								</ItemActions>
							</Item>
							<Item size="sm" className="px-0">
								<ItemMedia variant="icon">
									<Power aria-hidden="true" />
								</ItemMedia>
								<ItemContent>
									<ItemTitle>
										{selected.enabled ? "停用连接" : "启用连接"}
									</ItemTitle>
									{!selected.enabled && (
										<ItemDescription>
											需当前配置与凭据代次的已通过探测。
										</ItemDescription>
									)}
								</ItemContent>
								<ItemActions>
									<Button
										size="sm"
										variant="outline"
										onClick={() =>
											setConfirmation(selected.enabled ? "disable" : "enable")
										}
										disabled={mutationDisabled}
									>
										{selected.enabled ? "停用连接" : "启用连接"}
									</Button>
								</ItemActions>
							</Item>
							<Item size="sm" className="px-0">
								<ItemMedia variant="icon">
									<RotateCw aria-hidden="true" />
								</ItemMedia>
								<ItemContent>
									<ItemTitle>轮换凭据</ItemTitle>
								</ItemContent>
								<ItemActions>
									<Button
										size="sm"
										variant="outline"
										onClick={() => setRotating(true)}
										disabled={mutationDisabled}
									>
										轮换凭据
									</Button>
								</ItemActions>
							</Item>
						</ItemGroup>
					</section>
				</TabsContent>
				<TabsContent value="history" className="flex flex-col gap-6 pt-4">
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">探测历史</h3>
						{results.length === 0 ? (
							<p className="text-sm text-muted-foreground">尚无探测记录。</p>
						) : (
							<ItemGroup>
								{results.map((result) => (
									<Item key={result.id} size="sm" className="px-0">
										<ItemContent>
											<ItemTitle className="flex items-center gap-2">
												<Badge
													variant={
														result.outcome === "passed"
															? "secondary"
															: "destructive"
													}
												>
													{probeOutcomeLabels[result.outcome] ?? result.outcome}
												</Badge>
												<span className="text-xs tabular-nums text-muted-foreground">
													{formatDateTime(result.finishedAt)}
												</span>
											</ItemTitle>
											<ItemDescription>
												revision {result.connectionRevisionId} · generation{" "}
												{result.credentialGenerationId}
											</ItemDescription>
											{Object.keys(result.details).length > 0 && (
												<PropertyList
													mono
													className="mt-1"
													entries={Object.entries(result.details).map(
														([key, value]) => ({
															label: key,
															value:
																typeof value === "string"
																	? value
																	: JSON.stringify(value),
														}),
													)}
												/>
											)}
										</ItemContent>
									</Item>
								))}
							</ItemGroup>
						)}
					</section>
					<Separator />
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">
							配置版本（{selected.revisionCount}）
						</h3>
						{revisions.map((revision) => (
							<div
								key={revision.id}
								className="rounded-lg border px-3 py-2 text-sm"
							>
								<div className="flex items-center gap-2">
									<span className="font-medium">#{revision.revisionSeq}</span>
									<span className="text-xs tabular-nums text-muted-foreground">
										{formatDateTime(revision.createdAt)}
									</span>
								</div>
								<ConfigFacts config={revision.config} />
							</div>
						))}
					</section>
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">
							凭据代次（{selected.generationCount}）
						</h3>
						{generations.map((generation) => (
							<div key={generation.id} className="flex items-center gap-2 text-sm">
								<span className="font-medium">#{generation.generationSeq}</span>
								<span className="text-xs tabular-nums text-muted-foreground">
									{formatDateTime(generation.createdAt)}
								</span>
								{generation.createdBy && (
									<span className="text-xs text-muted-foreground">
										· 创建者 {generation.createdBy}
									</span>
								)}
							</div>
						))}
					</section>
				</TabsContent>
			</Tabs>
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
								? "停用会立即阻止该连接继续被使用。"
								: "启用会将当前已验证的连接投入使用。"}
						</AlertDialogDescription>
						{error && <ErrorMessage>{error}</ErrorMessage>}
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
						<Button disabled={busy} onClick={() => void changeEnabled()}>
							{confirmation === "disable" ? "确认停用" : "确认启用"}
						</Button>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
			{/* 低频表单收进对话框（与修改密码一致），不再内嵌可折叠表单块。 */}
			<Dialog open={rotating} onOpenChange={setRotating}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>轮换凭据</DialogTitle>
						<DialogDescription>
							字段名称和创建时相同；秘密不会回显，必须重新提供。
						</DialogDescription>
					</DialogHeader>
					<form className="flex flex-col gap-4" onSubmit={rotate}>
						<ConnectionFields
							type={editableType}
							value={rotationFields}
							onChange={(field, next) =>
								setRotationFields((current) => ({ ...current, [field]: next }))
							}
							disabled={mutationDisabled}
						/>
						<DialogFooter>
							<Button type="submit" disabled={mutationDisabled}>
								确认轮换
							</Button>
						</DialogFooter>
					</form>
				</DialogContent>
			</Dialog>
		</section>
	);
}

/** Owns connection service state while returning only the shell's shared sidebar module contract. */
/** Provider management renders inside the settings module: the shared settings
 * navigation stays in the sidebar and this page owns only its own content. */
export function ModelProviderPage({
	user,
	route,
	navigate,
	suspended,
}: WorkspaceModuleProps) {
	const [connections, setConnections] = useState<ConnectionSummaryView[]>([]);
	const [selected, setSelected] = useState<ConnectionDetailView>();
	const [primaryDetail, setPrimaryDetail] = useState<ConnectionDetailView>();
	const [maintenance, setMaintenance] = useState(false);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);
	const [detailLoading, setDetailLoading] = useState(false);
	const detailController = useRef<AbortController | undefined>(undefined);
	const primaryController = useRef<AbortController | undefined>(undefined);
	const refreshing = useRef(false);
	const readOnly = user.role !== "admin" || maintenance || suspended;
	const suffix = "/settings/platform/model-providers";
	const selectedName = route.startsWith(`${suffix}/`)
		? route.slice(suffix.length + 1)
		: undefined;
	const isNew = route === `${suffix}/new` || route.startsWith(`${suffix}/new?`);
	// 运行时（inspection/analysis/investigation/embedding）永远只消费最近启用
	// 的一个提供方（LIMIT 1），页面语义与之一致：主面板即当前提供方。
	const primary =
		connections.find((item) => item.enabled && !item.revalidationRequired) ??
		connections[0];
	const others = connections.filter((item) => item.name !== primary?.name);
	async function load() {
		if (refreshing.current) return;
		refreshing.current = true;
		setError("");
		try {
			const state = await workbenchApi.maintenance();
			setMaintenance(state?.active === true);
			setConnections(
				(await workbenchApi.listConnections()).filter(
					(item) => item.type === "model_provider",
				),
			);
		} catch (reason) {
			setError(messageOf(reason, "模型提供方服务当前不可用。"));
		} finally {
			setLoading(false);
			refreshing.current = false;
		}
	}
	async function chooseByName(name: string) {
		setDetailLoading(true);
		setSelected(undefined);
		setError("");
		detailController.current?.abort();
		const controller = new AbortController();
		detailController.current = controller;
		try {
			const next = await workbenchApi.fetchConnection(name, controller.signal);
			if (!controller.signal.aborted) {
				if (next.type !== "model_provider") throw new Error("该连接不是模型提供方，请在接入管理页查看。");
				setSelected(next);
			}
		} catch (reason) {
			if (reason instanceof DOMException && reason.name === "AbortError")
				return;
			setError(messageOf(reason, "无法读取连接详情。"));
		} finally {
			if (detailController.current === controller) setDetailLoading(false);
		}
	}
	async function loadPrimaryDetail(name: string) {
		primaryController.current?.abort();
		const controller = new AbortController();
		primaryController.current = controller;
		try {
			const next = await workbenchApi.fetchConnection(name, controller.signal);
			if (!controller.signal.aborted) setPrimaryDetail(next);
		} catch (reason) {
			if (reason instanceof DOMException && reason.name === "AbortError")
				return;
			if (!controller.signal.aborted)
				setError(messageOf(reason, "无法读取连接详情。"));
		}
	}
	useEffect(() => {
		void load();
	}, []);
	// 当前提供方的详情直接上页；名称变化才重取，操作内的状态流转由
	// ConnectionDetail 的 onUpdate 负责同步。
	const primaryName = primary?.name;
	useEffect(() => {
		if (!primaryName) {
			primaryController.current?.abort();
			setPrimaryDetail(undefined);
			return;
		}
		void loadPrimaryDetail(primaryName);
	}, [primaryName]);
	useEffect(() => {
		if (!selectedName || isNew) {
			detailController.current?.abort();
			setSelected(undefined);
			setDetailLoading(false);
			return;
		}
		try {
			const name = decodeURIComponent(selectedName);
			// 当前提供方已在主面板内联展示，深链不再起抽屉、也不重复取详情。
			if (name === primaryName) {
				detailController.current?.abort();
				setSelected(undefined);
				setDetailLoading(false);
				return;
			}
			void chooseByName(name);
		} catch {
			setError("连接地址无效。");
		}
	}, [route, primaryName]);
	usePolling(() => void load(), 15_000, !suspended);
	useEffect(
		() => () => {
			detailController.current?.abort();
			primaryController.current?.abort();
		},
		[],
	);
	const choose = (connection: ConnectionSummaryView) =>
		navigate(`${suffix}/${encodeURIComponent(connection.name)}`);
	const update = (connection: ConnectionSummaryView) => {
		setConnections((items) =>
			items.map((item) => (item.name === connection.name ? connection : item)),
		);
		if (connection.name === primaryName) void loadPrimaryDetail(connection.name);
		else void chooseByName(connection.name);
	};
	return (
		<section className="space-y-4">
			<div className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h2 className="text-xl font-semibold">模型提供方</h2>
					<p className="text-sm text-muted-foreground">
						管理 AI 对话与知识库使用的模型接入与凭据。
					</p>
				</div>
				<Button
					variant="outline"
					size="sm"
					onClick={() => navigate(`${suffix}/new`)}
					disabled={readOnly}
				>
					<Plus /> 新建模型提供方
				</Button>
			</div>
			{maintenance && (
				<Alert>
					<AlertDescription>
						维护模式已启用：所有写入操作已阻止。
					</AlertDescription>
				</Alert>
			)}
			{error && <ErrorMessage>{error}</ErrorMessage>}
			{loading || detailLoading ? (
				<div
					className="flex flex-col gap-3"
					role="status"
					aria-label="正在读取模型提供方服务"
				>
					<Skeleton className="h-6 w-1/4" />
					<Skeleton className="h-24 w-full" />
					<Skeleton className="h-4 w-2/3" />
				</div>
			) : error ? null : isNew ? (
				<ModelProviderEditor
					onCreated={(connection) => {
						setConnections((items) => [...items, connection]);
						// 新建的提供方尚未启用：已有生效提供方时进“其他”抽屉，
						// 首个提供方则直接成为主面板。
						choose(connection);
					}}
					readOnly={readOnly}
					suspended={suspended}
				/>
			) : !primary ? (
				<Empty>
					<EmptyHeader>
						<EmptyTitle>尚未配置模型提供方</EmptyTitle>
						<EmptyDescription>
							创建后需探测通过并启用，AI 对话与知识库才能使用。
						</EmptyDescription>
					</EmptyHeader>
					<Button
						variant="outline"
						size="sm"
						onClick={() => navigate(`${suffix}/new`)}
						disabled={readOnly}
					>
						<Plus /> 新建模型提供方
					</Button>
				</Empty>
			) : (
				<>
					{/* 当前提供方：详情直接上页，不经列表 + 抽屉两跳。 */}
					{primaryDetail ? (
						<>
							<div className="flex flex-wrap items-center gap-2">
								<h3 className="text-base font-semibold">
									{primaryDetail.name}
								</h3>
								<Badge
									variant={primaryDetail.enabled ? "secondary" : "outline"}
								>
									{primaryDetail.enabled ? "已启用" : "未启用"}
								</Badge>
								{primaryDetail.revalidationRequired && (
									<Badge variant="destructive">需要重新验证</Badge>
								)}
								<span className="text-xs tabular-nums text-muted-foreground">
									版本 {primaryDetail.rowVersion}
								</span>
							</div>
							<ConnectionDetail
								key={primaryDetail.name}
								selected={primaryDetail}
								onUpdate={update}
								onRefresh={() => void loadPrimaryDetail(primaryDetail.name)}
								readOnly={readOnly}
								suspended={suspended}
							/>
						</>
					) : (
						<div
							className="flex flex-col gap-3"
							role="status"
							aria-label="正在读取模型提供方详情"
						>
							<Skeleton className="h-24 w-full" />
						</div>
					)}
					{others.length > 0 && (
						<>
							<Separator />
							<section className="flex flex-col gap-3">
								<h3 className="text-sm font-medium">
									其他提供方（不生效，仅用于切换与回滚）
								</h3>
								<EntityList
									items={others.map((connection) => ({
										id: connection.name,
										title: connection.name,
										subtitle: connection.lastProbe
											? `最近探测 ${probeOutcomeLabels[connection.lastProbe.outcome] ?? connection.lastProbe.outcome} · ${formatDateTime(connection.lastProbe.finishedAt)}`
											: "尚未探测",
										badge: connection.revalidationRequired
											? { text: "需要重新验证", variant: "destructive" as const }
											: connection.enabled
												? undefined
												: { text: "未启用", variant: "secondary" as const },
									}))}
									columns={["title", "subtitle", "status"]}
									onSelect={(row) => {
										const connection = connections.find(
											(item) => item.name === row.id,
										);
										if (connection) choose(connection);
									}}
									loading={false}
									loadingLabel="正在读取模型提供方"
									emptyTitle=""
								/>
							</section>
						</>
					)}
					{/* 非当前提供方的详情仍是右侧抽屉；当前提供方已在页面上。 */}
					{selected && selected.name !== primary?.name && (
						<DetailSheet
							open
							onClose={() => navigate(suffix)}
							title={selected.name}
							description={`模型提供方 · ${selected.enabled ? "已启用" : "未启用"} · 版本 ${selected.rowVersion}`}
						>
							<div className="min-h-0 flex-1 overflow-y-auto">
								<div className="p-4 sm:p-6">
									<ConnectionDetail
										key={selected.name}
										selected={selected}
										onUpdate={update}
										onRefresh={() => void chooseByName(selected.name)}
										readOnly={readOnly}
										suspended={suspended}
									/>
								</div>
							</div>
						</DetailSheet>
					)}
				</>
			)}
		</section>
	);
}
