// Create/edit surface for one business view. The form and the YAML tab are two
// projections of the same ViewDraft; every save goes through toPayload() into
// the single create/update API path. Unknown YAML fields are rejected instead
// of silently dropped, and server conflicts keep the local draft untouched.

import { LoaderCircle, Plus, X } from "lucide-react";
import { useEffect, useState } from "react";
import { messageOf } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Card,
	CardContent,
	CardDescription,
	CardHeader,
	CardTitle,
} from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import {
	listAlertmanagerInstances,
	listMetricsInstances,
	type MetricsInstance,
} from "@/features/integrations/api";
import {
	type BusinessView,
	createBusinessView,
	getBusinessView,
	updateBusinessView,
} from "../api";
import {
	draftOf,
	draftProblems,
	draftYaml,
	emptyDraft,
	parseViewYaml,
	toPayload,
	type ViewDraft,
} from "../view-draft";

/** Sentinel for "candidate sources are all integrations"; Radix Select forbids empty-string values. */
const ALL_SOURCES = "*";

export function ViewEditor({
	suspended,
	navigate,
	editKey,
	onSaved,
}: {
	suspended: boolean;
	navigate: (to: string) => void;
	/** Present in edit mode: the stable viewKey is loaded and frozen. */
	editKey?: string;
	onSaved: (view: BusinessView) => void;
}) {
	const [draft, setDraft] = useState<ViewDraft>(() => emptyDraft());
	const [existing, setExisting] = useState<BusinessView>();
	const [tab, setTab] = useState<"form" | "yaml">("form");
	const [yamlText, setYamlText] = useState("");
	const [yamlError, setYamlError] = useState("");
	const [connections, setConnections] = useState<MetricsInstance[]>();
	const [alertSources, setAlertSources] = useState<string[]>();
	const [sourcesError, setSourcesError] = useState(false);
	const [loading, setLoading] = useState(Boolean(editKey));
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");

	useEffect(() => {
		if (!editKey) return;
		let cancelled = false;
		void getBusinessView(editKey)
			.then((view) => {
				if (!cancelled) {
					setExisting(view);
					setDraft(draftOf(view));
				}
			})
			.catch((reason) => {
				if (!cancelled) setError(messageOf(reason, "无法读取业务视图。"));
			})
			.finally(() => {
				if (!cancelled) setLoading(false);
			});
		return () => {
			cancelled = true;
		};
	}, [editKey]);

	// The metrics list only feeds the optional scope selector; a failure must
	// never block creating or editing a view.
	useEffect(() => {
		let cancelled = false;
		void listMetricsInstances()
			.then((items) => {
				if (!cancelled)
					setConnections(items.filter((item) => item.status === "active"));
			})
			.catch(() => {
				if (!cancelled) setConnections([]);
			});
		return () => {
			cancelled = true;
		};
	}, []);

	// 告警源列表只服务于可选的告警归属约束；加载失败不阻塞视图编辑。
	useEffect(() => {
		let cancelled = false;
		void listAlertmanagerInstances()
			.then((page) => {
				if (!cancelled) setAlertSources(page.items.map((item) => item.id));
			})
			.catch(() => {
				if (!cancelled) {
					setAlertSources([]);
					setSourcesError(true);
				}
			});
		return () => {
			cancelled = true;
		};
	}, []);

	const problems = draftProblems(draft);
	const conditions = Object.entries(draft.labelConditions);

	function setCondition(index: number, key: string, value: string) {
		setDraft((current) => ({
			...current,
			labelConditions: Object.fromEntries(
				Object.entries(current.labelConditions).map(([k, v], i) =>
					i === index ? [key, value] : [k, v],
				),
			),
		}));
	}
	function removeCondition(index: number) {
		setDraft((current) => ({
			...current,
			labelConditions: Object.fromEntries(
				Object.entries(current.labelConditions).filter((_, i) => i !== index),
			),
		}));
	}
	function addCondition() {
		setDraft((current) => ({
			...current,
			labelConditions: { ...current.labelConditions, "": "" },
		}));
	}
	function toggleAlertSource(key: string) {
		setDraft((current) => ({
			...current,
			alertSourceKeys: current.alertSourceKeys.includes(key)
				? current.alertSourceKeys.filter((item) => item !== key)
				: [...current.alertSourceKeys, key],
		}));
	}

	function applyYaml(text: string) {
		setYamlText(text);
		try {
			setDraft(parseViewYaml(text));
			setYamlError("");
		} catch (reason) {
			setYamlError(messageOf(reason, "YAML 无法解析。"));
		}
	}

	/** Switching tabs is the sync point: leaving YAML requires the text to parse, so the draft never forks. */
	function switchTab(next: "form" | "yaml") {
		if (next === tab) return;
		if (next === "yaml") {
			setYamlText(draftYaml(draft));
			setYamlError("");
			setTab("yaml");
			return;
		}
		try {
			setDraft(parseViewYaml(yamlText));
			setYamlError("");
			setTab("form");
		} catch (reason) {
			setYamlError(messageOf(reason, "YAML 无法解析。"));
		}
	}

	async function save() {
		if (suspended || busy || problems.length || yamlError) return;
		setBusy(true);
		setError("");
		try {
			const payload = toPayload(draft);
			const saved =
				existing && editKey
					? await updateBusinessView(
							editKey,
							{
								displayName: payload.displayName,
								description: payload.description,
								scope: payload.scope,
							},
							existing.rowVersion,
						)
					: await createBusinessView(payload);
			onSaved(saved);
		} catch (reason) {
			setError(messageOf(reason, "无法保存业务视图。"));
		} finally {
			setBusy(false);
		}
	}

	const blocked = Boolean(yamlError) || problems.length > 0;

	return (
		<section className="mx-auto flex w-full max-w-5xl flex-col gap-4">
			<Card>
				<CardHeader>
					<CardTitle>{editKey ? "编辑业务视图" : "新建业务视图"}</CardTitle>
					<CardDescription>
						业务视图是可选的范围与说明组织；它不拥有凭据，也不授予新的查询权限。表单和
						YAML 编辑同一份草稿，保存走同一写路径。
					</CardDescription>
				</CardHeader>
				<CardContent>
					{loading ? (
						<DetailSkeleton label="正在读取业务视图" rows={["line", "line", "card", "line", "line"]} />
					) : (
						<FieldGroup>
							{error && (
								<Alert variant="destructive">
									<AlertDescription>{error}</AlertDescription>
								</Alert>
							)}
							<Tabs
								value={tab}
								onValueChange={(value) =>
									switchTab(value === "yaml" ? "yaml" : "form")
								}
							>
								<TabsList>
									<TabsTrigger value="form">表单</TabsTrigger>
									<TabsTrigger value="yaml">YAML</TabsTrigger>
								</TabsList>
								<TabsContent value="form" className="mt-3">
									<FieldGroup>
										<div className="grid gap-3 md:grid-cols-2">
											<Field>
												<FieldLabel htmlFor="view-editor-key">
													视图标识
												</FieldLabel>
												<Input
													id="view-editor-key"
													value={draft.viewKey}
													onChange={(event) =>
														setDraft({ ...draft, viewKey: event.target.value })
													}
													disabled={suspended || busy || Boolean(editKey)}
													placeholder="checkout"
												/>
												<FieldDescription>
													创建后不可修改；历史记录依靠稳定标识关联。
												</FieldDescription>
											</Field>
											<Field>
												<FieldLabel htmlFor="view-editor-name">
													显示名称
												</FieldLabel>
												<Input
													id="view-editor-name"
													value={draft.displayName}
													onChange={(event) =>
														setDraft({
															...draft,
															displayName: event.target.value,
														})
													}
													disabled={suspended || busy}
												/>
											</Field>
										</div>
										<Field>
											<FieldLabel htmlFor="view-editor-description">
												业务说明
											</FieldLabel>
											<Textarea
												id="view-editor-description"
												value={draft.description}
												onChange={(event) =>
													setDraft({
														...draft,
														description: event.target.value,
													})
												}
												disabled={suspended || busy}
												className="min-h-20"
											/>
										</Field>
										<Field
											data-disabled={connections === undefined || undefined}
										>
											<FieldLabel>来源接入</FieldLabel>
											<Select
												value={draft.connectionName ?? ALL_SOURCES}
												onValueChange={(value) =>
													setDraft((current) => ({
														...current,
														connectionName:
															value === ALL_SOURCES ? undefined : value,
													}))
												}
												disabled={
													suspended || busy || connections === undefined
												}
											>
												<SelectTrigger aria-label="来源接入">
													<SelectValue
														placeholder={
															connections === undefined ? "读取中" : "选择接入"
														}
													/>
												</SelectTrigger>
												<SelectContent>
													<SelectItem value={ALL_SOURCES}>
														全部候选来源
													</SelectItem>
													{(connections ?? []).map((item) => (
														<SelectItem key={item.id} value={item.displayName}>
															{item.displayName}
														</SelectItem>
													))}
												</SelectContent>
											</Select>
											<FieldDescription>
												不选择表示候选来源为全部接入；这只是范围说明，不授予新的查询权限，凭据仍在接入边界管理。
											</FieldDescription>
										</Field>
										<Field>
											<FieldLabel>标签条件</FieldLabel>
											<div className="flex flex-col gap-2">
												{conditions.map(([key, value], index) => (
													<div className="flex items-center gap-2" key={index}>
														<Input
															aria-label="标签"
															value={key}
															onChange={(event) =>
																setCondition(index, event.target.value, value)
															}
															disabled={suspended || busy}
															placeholder="service"
														/>
														<Input
															aria-label="值"
															value={value}
															onChange={(event) =>
																setCondition(index, key, event.target.value)
															}
															disabled={suspended || busy}
															placeholder="checkout"
														/>
														<Button
															type="button"
															variant="ghost"
															size="icon"
															aria-label={`移除条件 ${key || index + 1}`}
															onClick={() => removeCondition(index)}
															disabled={suspended || busy}
														>
															<X data-icon="inline-start" />
														</Button>
													</div>
												))}
												<div>
													<Button
														type="button"
														variant="outline"
														size="sm"
														onClick={addCondition}
														disabled={suspended || busy}
													>
														<Plus data-icon="inline-start" />
														添加条件
													</Button>
												</div>
											</div>
											<FieldDescription>
												匹配对象的标签条件；留空表示不按标签收窄。
											</FieldDescription>
										</Field>
										<Field>
											<FieldLabel>告警归属（Alertmanager 告警源）</FieldLabel>
											<div
												className="flex flex-col gap-2"
												role="group"
												aria-label="参与告警归属的告警源"
											>
												{(alertSources ?? []).map((key) => (
													<label
														key={key}
														className="flex items-center gap-2 text-sm"
													>
														<Checkbox
															checked={draft.alertSourceKeys.includes(key)}
															onCheckedChange={() => toggleAlertSource(key)}
															disabled={suspended || busy}
															aria-label={`告警源 ${key}`}
														/>
														<span className="font-mono text-xs">{key}</span>
													</label>
												))}
												{alertSources?.length === 0 && (
													<p className="text-sm text-muted-foreground">
														{sourcesError
															? "无法读取告警源列表，可稍后重试或改用 YAML 编辑。"
															: "尚未创建任何 Alertmanager 告警源。"}
													</p>
												)}
												{alertSources === undefined && (
													<DetailSkeleton label="正在读取告警源" rows={["line", "line"]} />
												)}
											</div>
											<FieldDescription>
												勾选后该视图参与这些告警源的告警归属：交付来源必须精确匹配，且上方标签条件全部命中；两者缺一告警保持未归属。不勾选表示本视图不参与告警归属（与来源接入是两种身份，互不替换）。
											</FieldDescription>
											{draft.alertSourceKeys.length > 0 && (
												<div className="flex flex-wrap gap-1">
													{draft.alertSourceKeys.map((key) => (
														<Badge key={key} variant="outline">
															{key}
														</Badge>
													))}
												</div>
											)}
										</Field>
									</FieldGroup>
								</TabsContent>
								<TabsContent value="yaml" className="mt-3">
									<Field>
										<FieldLabel htmlFor="view-editor-yaml">
											业务视图 YAML
										</FieldLabel>
										<Textarea
											id="view-editor-yaml"
											aria-label="业务视图 YAML"
											value={yamlText}
											onChange={(event) => applyYaml(event.target.value)}
											disabled={suspended || busy}
											className="min-h-[22rem] font-mono text-xs"
										/>
										<FieldDescription>
											与表单编辑同一份草稿；保存时使用同一写路径。不支持的字段会被显式拒绝。
										</FieldDescription>
									</Field>
									{yamlError && (
										<Alert variant="destructive" className="mt-3">
											<AlertDescription>{yamlError}</AlertDescription>
										</Alert>
									)}
								</TabsContent>
							</Tabs>
							{problems.length > 0 && !yamlError && (
								<Alert variant="destructive">
									<AlertDescription>
										<ul className="list-inside list-disc">
											{problems.map((problem) => (
												<li key={problem}>{problem}</li>
											))}
										</ul>
									</AlertDescription>
								</Alert>
							)}
							<Separator />
							<div className="flex flex-wrap gap-2">
								<Button
									type="button"
									onClick={() => void save()}
									disabled={suspended || busy || blocked}
								>
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
										"保存业务视图"
									)}
								</Button>
								<Button
									type="button"
									variant="outline"
									disabled={suspended || busy}
									onClick={() =>
										navigate(
											editKey
												? `/business-views?view=${encodeURIComponent(editKey)}`
												: "/business-views",
										)
									}
								>
									取消
								</Button>
							</div>
						</FieldGroup>
					)}
				</CardContent>
			</Card>
		</section>
	);
}
