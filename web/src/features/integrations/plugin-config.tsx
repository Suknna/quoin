/**
 * Retained browser plugin configuration forms, disconnected from production
 * integration routes.
 *
 * Boundaries:
 *  - Browser gates every entry on the server-authoritative plugin catalog.
 *    While the plugin is disabled this page renders no entry at all. When
 *    enabled it shows the versioned Journey catalog and the identities:
 *    creation and revision edits are business-independent (standalone identity
 *    API, identity_key + nullable business_system_id per migration bc14), and
 *    legacy business-referenced identities are read-only references with an
 *    explicit migration state — the historical browser-login routes never
 *    receive writes from this surface.
 *  - The kubernetes plugin is retired (Kubernetes 插件退役): its
 *    `/integrations/kubernetes` forms were removed with the descriptor; the
 *    retired route now falls through to the shared not-found view while the
 *    connection type stays readable for historical data.
 *  - This module deliberately does not import from `@/features/systems/ui`:
 *    the identity panel below was extracted from it and is owned here now.
 */

import RFB from "@novnc/novnc";
import { LoaderCircle } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import { messageOf } from "@/app/shared";
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
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import {
	type BrowserIdentity,
	type BrowserOperation,
	type BusinessSystemSummary,
	getBrowserIdentity,
	getJourneyCatalog,
	type JourneyCatalogView,
	listBusinessSystems,
	newClientCommandId,
} from "@/features/admin/business-systems/api";
import { listIntegrationPlugins } from "./api";
import type { IntegrationPlatform } from "./types";

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

async function apiProblem(
	response: Response,
	fallback: string,
): Promise<Error> {
	let message = fallback;
	try {
		message =
			((await response.json()) as { message?: string }).message ?? message;
	} catch {
		// Non-JSON problem responses keep the safe fallback.
	}
	return new Error(message);
}

async function listBrowserIdentities(): Promise<StandaloneIdentity[]> {
	const response = await fetch(`${browserIdentitiesBase}?limit=100`, {
		credentials: "include",
	});
	if (!response.ok)
		throw await apiProblem(response, "暂时无法读取浏览器身份列表。");
	const page = (await response.json()) as { items?: BrowserIdentity[] };
	return (page.items ?? []).map(asStandaloneIdentity);
}

async function createBrowserIdentity(input: {
	name: string;
	startUrl: string;
	authenticationProbe: {
		journeyId: string;
		journeyVersion: number;
		params: Record<string, unknown>;
	};
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
		authenticationProbe: {
			journeyId: string;
			journeyVersion: number;
			params: Record<string, unknown>;
		};
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

async function getBrowserIdentityByIdentityKey(
	identityKey: string,
): Promise<StandaloneIdentity> {
	const response = await fetch(
		`${browserIdentitiesBase}/${encodeURIComponent(identityKey)}`,
		{
			credentials: "include",
		},
	);
	if (!response.ok) throw await apiProblem(response, "无法读取浏览器身份。");
	return asStandaloneIdentity((await response.json()) as BrowserIdentity);
}

async function startIdentityOperation(
	operationsBase: string,
	expectedRowVersion: number,
): Promise<BrowserOperation> {
	const response = await fetch(`${operationsBase}/operations`, {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({
			clientCommandId: newClientCommandId(),
			expectedRowVersion,
		}),
	});
	if (!response.ok) throw await apiProblem(response, "无法开始浏览器登录。");
	return (await response.json()) as BrowserOperation;
}

async function fetchIdentityOperation(
	operationsBase: string,
	operationId: string,
): Promise<BrowserOperation> {
	const response = await fetch(
		`${operationsBase}/operations/${encodeURIComponent(operationId)}`,
		{
			credentials: "include",
		},
	);
	if (!response.ok) throw await apiProblem(response, "无法读取浏览器操作。");
	return (await response.json()) as BrowserOperation;
}

async function commandIdentityOperation(
	operationsBase: string,
	operation: BrowserOperation,
	action: "publish" | "cancel",
): Promise<BrowserOperation> {
	const response = await fetch(
		`${operationsBase}/operations/${encodeURIComponent(operation.id)}/${action}`,
		{
			method: "POST",
			credentials: "include",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				expectedOperationRowVersion: operation.rowVersion,
			}),
		},
	);
	if (!response.ok) throw await apiProblem(response, "浏览器操作失败。");
	return (await response.json()) as BrowserOperation;
}

export interface PluginConfigurationProps
	extends Pick<WorkspaceModuleProps, "navigate" | "suspended"> {
	platform: IntegrationPlatform;
}

/** Entry point wired by the integrations route host for the browser. */
export function PluginConfiguration({
	platform,
	navigate,
	suspended,
}: PluginConfigurationProps) {
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

type CatalogProperty = {
	type?: string;
	title?: string;
	enum?: unknown[];
	default?: unknown;
};
type CatalogJourney = {
	purpose?: string;
	version?: number;
	summary?: string;
	params_schema?: { properties?: Record<string, CatalogProperty> };
};

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
	const [selected, setSelected] = useState<{
		key: string;
		kind: "standalone" | "legacy";
	}>();
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
			<div
				className="flex flex-col gap-4"
				role="status"
				aria-label="正在读取浏览器接入状态"
			>
				<Skeleton className="h-7 w-1/3" />
				<Skeleton className="h-24 w-full" />
				<Skeleton className="h-24 w-full" />
				<Skeleton className="h-24 w-3/4" />
			</div>
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
						当前部署未启用受控浏览器插件，因此没有身份或 Journey
						入口。启用后这里会出现配置与登录入口。
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
				<h1 className="text-2xl font-semibold tracking-tight">
					配置受控浏览器
				</h1>
				<p className="mt-1 text-sm text-muted-foreground">
					管理认证探测 Journey
					与已保存的浏览器身份；登录始终通过受控远程浏览器完成。
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
							initial={{
								name: "",
								startUrl: "",
								journeyId: journeys[0]?.[0] ?? "",
								params: {},
							}}
							idPrefix="identity-create"
							suspended={suspended}
							submitLabel="创建身份"
							busyLabel="创建中…"
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
										selected?.kind === "standalone" &&
										selected.key === identity.identityKey
											? "secondary"
											: "outline"
									}
									onClick={() =>
										setSelected({
											key: identity.identityKey,
											kind: "standalone",
										})
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
									onClick={() => setSelected({ key: item.key, kind: "legacy" })}
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
		authenticationProbe: {
			journeyId: string;
			journeyVersion: number;
			params: Record<string, unknown>;
		};
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
		if (
			!selectedJourney ||
			suspended ||
			saving ||
			!name.trim() ||
			!startUrl.trim()
		)
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
					<FieldLabel htmlFor={`${idPrefix}-name`}>名称</FieldLabel>
					<Input
						id={`${idPrefix}-name`}
						value={name}
						onChange={(event) => setName(event.target.value)}
						disabled={suspended || saving}
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor={`${idPrefix}-url`}>起始 URL</FieldLabel>
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
							<FieldLabel htmlFor={`${idPrefix}-${key}`}>
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
									<SelectTrigger id={`${idPrefix}-${key}`} className="w-full">
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
									<SelectTrigger id={`${idPrefix}-${key}`} className="w-full">
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
						suspended ||
						saving ||
						!journeyId ||
						!name.trim() ||
						!startUrl.trim()
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
		if (
			!operationsBase ||
			suspended ||
			!operation ||
			!activeOperationStates.includes(operation.state)
		)
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
		if (
			!operationsBase ||
			!operation?.canAttach ||
			!viewport.current ||
			client.current
		)
			return;
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
			setOperation(
				await commandIdentityOperation(operationsBase, operation, action),
			);
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
			<div
				className="flex flex-col gap-3 rounded-md border p-4"
				role="status"
				aria-label="正在读取身份"
			>
				<Skeleton className="h-6 w-1/3" />
				<Skeleton className="h-4 w-1/4" />
				<Skeleton className="h-20 w-full" />
				<Skeleton className="h-9 w-1/3" />
			</div>
		);

	return (
		<section
			className="flex flex-col gap-3 rounded-md border p-4"
			aria-label={`浏览器身份：${identityKey}`}
		>
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
				<p className="text-sm text-muted-foreground">
					尚无已发布的浏览器配置。
				</p>
			)}
			{identity.lastProbe ? (
				<p className="text-xs text-muted-foreground">
					最近探测：{identity.lastProbe.result} ·{" "}
					{identity.lastProbe.observedAt}
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
									journeyId:
										identity.currentRevision.authenticationProbe.journeyId,
									params: Object.fromEntries(
										Object.entries(
											identity.currentRevision.authenticationProbe.params,
										).map(([key, value]) => [key, String(value)]),
									),
								}}
								idPrefix="identity-edit"
								suspended={suspended}
								submitLabel="保存新修订"
								busyLabel="保存中…"
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
								<LoaderCircle
									className="animate-spin"
									data-icon="inline-start"
								/>
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
									<Button
										disabled={suspended || busy}
										onClick={() => void command("publish")}
									>
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
