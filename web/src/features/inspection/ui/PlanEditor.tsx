import { ChevronDown, LoaderCircle } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { parse as parseYaml } from "yaml";
import { type ConnectionSummaryView, workbenchApi } from "@/api/workbench";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
	Card,
	CardContent,
	CardDescription,
	CardHeader,
	CardTitle,
} from "@/components/ui/card";
import {
	Collapsible,
	CollapsibleContent,
	CollapsibleTrigger,
} from "@/components/ui/collapsible";
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
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import {
	createInspectionPlan,
	getInspectionPlan,
	type InspectionPlan,
	type InspectionPlanInput,
	type InspectionPlanScope,
	inspectionScopeKindText,
	updateInspectionPlan,
} from "@/features/inspection/api";
import { listIntegrationPlugins } from "@/features/integrations/api";
import type { IntegrationCatalogItem } from "@/features/integrations/types";
import { type BusinessView, listBusinessViews } from "@/features/systems/api";

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

/** Deep-link prefill for a fresh plan, derived from query hints. */
export interface PlanEditorPrefill {
	connectionName?: string;
	scopeKind?: InspectionPlanScope["kind"];
	businessViewKey?: string;
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
	return (
		Object.entries(params)
			// String values are JSON-quoted too, so colons, newlines, or quotes in a
			// value cannot forge extra YAML keys; the server re-validates regardless.
			.map(([key, value]) => `${key}: ${JSON.stringify(value)}`)
			.join("\n")
	);
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
			: { ok: false, message: "请选择巡检范围对应的业务视图。" };
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

type ResourceState<T> = {
	items: T[] | null;
	error: boolean;
};

interface ResourceOption {
	value: string;
	label: string;
	disabled?: boolean;
}

/**
 * Select fed from a live list endpoint. A failed read degrades to a plain input
 * plus retry so editing is never blocked; a value missing from the list still
 * renders as an explicit "当前值" option (e.g. a retired connection).
 */
function ResourceSelect({
	id,
	label,
	value,
	onChange,
	options,
	state,
	onRetry,
	placeholder,
	description,
	disabled,
}: {
	id: string;
	label: string;
	value: string;
	onChange: (value: string) => void;
	options: ResourceOption[];
	state: ResourceState<unknown>;
	onRetry: () => void;
	placeholder: string;
	description?: string;
	disabled?: boolean;
}) {
	if (state.error)
		return (
			<Field>
				<FieldLabel htmlFor={id}>{label}</FieldLabel>
				<Alert variant="destructive">
					<AlertDescription>读取选项失败，可重试或直接输入。</AlertDescription>
				</Alert>
				<Input
					id={id}
					aria-label={label}
					value={value}
					disabled={disabled}
					onChange={(event) => onChange(event.target.value)}
					placeholder={placeholder}
				/>
				<Button
					type="button"
					size="sm"
					variant="outline"
					className="w-fit"
					onClick={onRetry}
				>
					重试读取
				</Button>
				{description && <FieldDescription>{description}</FieldDescription>}
			</Field>
		);
	const effective =
		value && !options.some((option) => option.value === value)
			? [{ value, label: `${value}（当前值）` }, ...options]
			: options;
	return (
		<Field>
			<FieldLabel htmlFor={id}>{label}</FieldLabel>
			<Select
				value={value}
				onValueChange={onChange}
				disabled={disabled || state.items === null}
			>
				<SelectTrigger id={id} aria-label={label}>
					<SelectValue
						placeholder={state.items === null ? "正在读取…" : placeholder}
					/>
				</SelectTrigger>
				<SelectContent>
					<SelectGroup>
						{effective.map((option) => (
							<SelectItem
								key={option.value}
								value={option.value}
								disabled={option.disabled}
							>
								{option.label}
							</SelectItem>
						))}
					</SelectGroup>
				</SelectContent>
			</Select>
			{description && <FieldDescription>{description}</FieldDescription>}
		</Field>
	);
}

/**
 * Full-page create/edit form for one plan, sectioned in the order the operator
 * answers them: 基本信息 → 采证来源 → 巡检范围 → 调度 → 可选分析要求。
 * Pickers are fed by live lists (connections / plugins / business views) so the
 * operator never has to type identifiers they cannot enumerate.
 */
export function PlanEditor({
	suspended,
	navigate,
	editKey,
	prefill,
}: {
	suspended: boolean;
	navigate: (to: string) => void;
	/** Present in edit mode: the plan is re-read fresh so rowVersion stays current. */
	editKey?: string;
	prefill?: PlanEditorPrefill;
}) {
	const editing = Boolean(editKey);
	const [form, setForm] = useState<PlanFormState>(() =>
		emptyPlanForm(prefill?.connectionName ?? ""),
	);
	const [existing, setExisting] = useState<InspectionPlan>();
	const [loading, setLoading] = useState(editing);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [connections, setConnections] = useState<
		ResourceState<ConnectionSummaryView>
	>({
		items: null,
		error: false,
	});
	const [plugins, setPlugins] = useState<ResourceState<IntegrationCatalogItem>>(
		{
			items: null,
			error: false,
		},
	);
	const [views, setViews] = useState<ResourceState<BusinessView>>({
		items: null,
		error: false,
	});
	const loadResources = useCallback(() => {
		setConnections({ items: null, error: false });
		setPlugins({ items: null, error: false });
		setViews({ items: null, error: false });
		workbenchApi
			.listConnections()
			.then((items) => setConnections({ items, error: false }))
			.catch(() => setConnections({ items: null, error: true }));
		listIntegrationPlugins()
			.then((items) =>
				setPlugins({
					items: items.filter((item) =>
						item.capabilities.includes("inspection_templates"),
					),
					error: false,
				}),
			)
			.catch(() => setPlugins({ items: null, error: true }));
		listBusinessViews()
			.then((items) => setViews({ items, error: false }))
			.catch(() => setViews({ items: null, error: true }));
	}, []);
	useEffect(() => {
		loadResources();
	}, [loadResources]);
	useEffect(() => {
		if (!editKey) {
			const fresh = emptyPlanForm(prefill?.connectionName ?? "");
			setForm({
				...fresh,
				// 带业务视图提示的进入（如从视图详情“新建计划”）直接锁定业务视图范围。
				scopeKind:
					prefill?.scopeKind ??
					(prefill?.businessViewKey ? "businessView" : "integration"),
				...(prefill?.businessViewKey
					? { businessViewKey: prefill.businessViewKey }
					: {}),
				objects:
					prefill?.scopeKind === "objects"
						? [{ objectType: "", identityKey: "" }]
						: [],
			});
			return;
		}
		let cancelled = false;
		setLoading(true);
		void getInspectionPlan(editKey)
			.then((plan) => {
				if (cancelled) return;
				setExisting(plan);
				setForm(planFormOf(plan));
			})
			.catch((reason) => {
				if (!cancelled) setError(messageOf(reason, "无法读取巡检计划。"));
			})
			.finally(() => {
				if (!cancelled) setLoading(false);
			});
		return () => {
			cancelled = true;
		};
	}, [editKey, prefill]);
	function update(patch: Partial<PlanFormState>) {
		setForm((current) => ({ ...current, ...patch }));
	}
	/** 选中接入后按接入类型预选插件；用户仍可手动改选。 */
	function onConnectionChange(connectionName: string) {
		const connection = connections.items?.find(
			(item) => item.name === connectionName,
		);
		const matchedPlugin = connection
			? plugins.items?.find((plugin) => plugin.id === connection.type)
			: undefined;
		update({
			connectionName,
			...(matchedPlugin ? { pluginId: matchedPlugin.id } : {}),
		});
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
			setError("计划 Key、显示名称、接入连接、插件和模板 ID 必须填写。");
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
			if (existing)
				await updateInspectionPlan(existing.planKey, {
					...payload,
					expectedRowVersion: existing.rowVersion,
				});
			else await createInspectionPlan(payload);
			notify.success(existing ? "已保存巡检计划" : "已创建巡检计划");
			navigate("/inspections");
		} catch (reason) {
			setError(
				messageOf(
					reason,
					existing ? "无法更新巡检计划。" : "无法创建巡检计划。",
				),
			);
		} finally {
			setBusy(false);
		}
	}
	if (loading)
		return (
			<div className="space-y-4">
				<DetailSkeleton
					label="正在读取巡检计划"
					rows={["title", "line", "line", "line"]}
				/>
			</div>
		);
	const connectionOptions: ResourceOption[] =
		connections.items?.map((connection) => ({
			value: connection.name,
			label: `${connection.name}（${connection.type}）${
				connection.enabled ? "" : " · 已停用"
			}`,
			disabled: !connection.enabled,
		})) ?? [];
	const inspectionPlugins = plugins.items ?? [];
	const pluginOptions: ResourceOption[] = inspectionPlugins.map((plugin) => ({
		value: plugin.id,
		label: `${plugin.displayName}（${plugin.id}）`,
	}));
	const selectedPlugin = inspectionPlugins.find(
		(plugin) => plugin.id === form.pluginId,
	);
	const viewOptions: ResourceOption[] =
		views.items?.map((view) => ({
			value: view.viewKey,
			label: `${view.displayName}（${view.viewKey}）`,
		})) ?? [];
	return (
		<div className="space-y-6">
			<Button variant="ghost" onClick={() => navigate("/inspections")}>
				返回巡检
			</Button>
			<header>
				<h2 className="text-xl font-semibold">
					{editing ? "编辑巡检计划" : "新建巡检计划"}
				</h2>
				<p className="text-sm text-muted-foreground">
					计划绑定一个接入与模板，按范围定期或手动采证并生成分析报告。
				</p>
			</header>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<FieldGroup className="gap-6">
				<Card>
					<CardHeader>
						<CardTitle>基本信息</CardTitle>
						<CardDescription>计划的标识与启停。</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="plan-key">计划 Key</FieldLabel>
							<Input
								id="plan-key"
								value={form.planKey}
								disabled={editing || busy || suspended}
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
								placeholder="如 Prometheus 连通巡检"
							/>
						</Field>
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
							<FieldDescription>
								停用后不参与定时调度，也不能手动运行。
							</FieldDescription>
						</Field>
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>采证来源</CardTitle>
						<CardDescription>
							数据从哪个接入、由哪个插件按哪个模板采集。
						</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4">
						<div className="grid gap-4 md:grid-cols-2">
							<ResourceSelect
								id="plan-connection"
								label="接入连接"
								value={form.connectionName}
								onChange={onConnectionChange}
								options={connectionOptions}
								state={connections}
								onRetry={loadResources}
								placeholder="选择接入连接"
								description="采证数据来自该接入；已停用的接入不可选。"
								disabled={busy || suspended}
							/>
							<ResourceSelect
								id="plan-plugin"
								label="插件"
								value={form.pluginId}
								onChange={(pluginId) => update({ pluginId })}
								options={pluginOptions}
								state={plugins}
								onRetry={loadResources}
								placeholder="选择插件"
								description={
									selectedPlugin
										? selectedPlugin.description
										: "按接入类型自动预选，可手动改选。"
								}
								disabled={busy || suspended}
							/>
						</div>
						<div className="grid gap-4 md:grid-cols-2">
							<Field>
								<FieldLabel htmlFor="plan-template">模板 ID</FieldLabel>
								<Input
									id="plan-template"
									value={form.templateId}
									disabled={busy || suspended}
									onChange={(event) =>
										update({ templateId: event.target.value })
									}
									placeholder="如 promql-check"
								/>
								<FieldDescription>
									所选插件提供的巡检模板标识，随插件文档给出。
								</FieldDescription>
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
								<FieldDescription>留空使用插件当前默认版本。</FieldDescription>
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
								placeholder={"query: up == 0"}
							/>
							<FieldDescription>
								按模板要求的键值对填写，一行一个；服务端会按模板 schema
								复核，填错会在保存时提示。
							</FieldDescription>
						</Field>
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>巡检范围</CardTitle>
						<CardDescription>
							Run 创建时按当前范围展开并冻结目标，执行中不扩大。
						</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4">
						<Field>
							<FieldLabel htmlFor="plan-scope">范围类型</FieldLabel>
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
						</Field>
						{form.scopeKind === "businessView" && (
							<ResourceSelect
								id="plan-business-view"
								label="业务视图"
								value={form.businessViewKey}
								onChange={(businessViewKey) => update({ businessViewKey })}
								options={viewOptions}
								state={views}
								onRetry={loadResources}
								placeholder="选择业务视图"
								description="只巡检该视图范围内的对象。"
								disabled={busy || suspended}
							/>
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
															? {
																	...item,
																	objectType: event.target.value,
																}
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
															? {
																	...item,
																	identityKey: event.target.value,
																}
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
									className="w-fit"
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
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>调度</CardTitle>
						<CardDescription>
							留空表示仅人工运行；填写后按 cron 周期自动采证。
						</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="plan-cron">调度 Cron</FieldLabel>
							<Input
								id="plan-cron"
								value={form.cron}
								disabled={busy || suspended}
								onChange={(event) => update({ cron: event.target.value })}
								placeholder="*/5 * * * *"
							/>
							<FieldDescription>标准五字段 cron。</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="plan-timezone">时区</FieldLabel>
							<Input
								id="plan-timezone"
								value={form.timezone}
								disabled={busy || suspended}
								onChange={(event) => update({ timezone: event.target.value })}
								placeholder="Asia/Shanghai"
							/>
						</Field>
					</CardContent>
				</Card>
				<Collapsible className="rounded-lg border">
					<CollapsibleTrigger asChild>
						<Button variant="ghost" className="group w-full justify-between">
							分析与报告要求（可选）
							<ChevronDown
								className="transition-transform group-data-[state=open]:rotate-180"
								aria-hidden="true"
							/>
						</Button>
					</CollapsibleTrigger>
					<CollapsibleContent>
						<div className="grid gap-4 border-t p-4">
							<Field>
								<FieldLabel htmlFor="plan-check-description">
									检查说明
								</FieldLabel>
								<Textarea
									id="plan-check-description"
									aria-label="检查说明"
									className="min-h-16"
									value={form.checkDescription}
									disabled={busy || suspended}
									onChange={(event) =>
										update({ checkDescription: event.target.value })
									}
									placeholder="这项检查在观测什么、如何解读结果，如：接口可用性，1=在线 0=离线"
								/>
								<FieldDescription>
									随 Run 冻结并进入分析上下文；最长 2000 字。
								</FieldDescription>
							</Field>
							<div className="grid gap-4 md:grid-cols-2">
								<Field>
									<FieldLabel htmlFor="plan-metric-unit">指标单位</FieldLabel>
									<Input
										id="plan-metric-unit"
										value={form.metricUnit}
										disabled={busy || suspended}
										onChange={(event) =>
											update({ metricUnit: event.target.value })
										}
										placeholder="如 %、ms、1=在线"
									/>
									<FieldDescription>
										结果数值的语义单位；最长 100 字。
									</FieldDescription>
								</Field>
								<Field>
									<FieldLabel htmlFor="plan-report-instructions">
										初始报告要求
									</FieldLabel>
									<Textarea
										id="plan-report-instructions"
										aria-label="初始报告要求"
										className="min-h-16"
										value={form.reportInstructions}
										disabled={busy || suspended}
										onChange={(event) =>
											update({ reportInstructions: event.target.value })
										}
										placeholder="希望报告默认侧重什么，如：逐检查项给出结论和依据"
									/>
									<FieldDescription>
										每次 Run 冻结后作为分析默认要求；重分析可仅本次覆盖；最长
										4000 字。
									</FieldDescription>
								</Field>
							</div>
						</div>
					</CollapsibleContent>
				</Collapsible>
			</FieldGroup>
			<div className="flex justify-end gap-2">
				<Button
					variant="outline"
					onClick={() => navigate("/inspections")}
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
			</div>
		</div>
	);
}
