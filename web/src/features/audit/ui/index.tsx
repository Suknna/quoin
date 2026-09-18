import { LoaderCircle, Search } from "lucide-react";
import { useEffect, useState } from "react";
import { newClientCommandId, WorkbenchApiError } from "@/api/workbench";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Card,
	CardAction,
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
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { EntityList } from "@/components/EntityList";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import {
	type AuditEvent,
	type AuditEventFilter,
	type AuditOutcome,
	type AuditSettings,
	type AuditSettingsPreview,
	actorLabels,
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

const outcomeVariant = (outcome: AuditOutcome) =>
	outcome === "success"
		? "default"
		: outcome === "failure"
			? "destructive"
			: outcome === "rejected"
				? "outline"
				: "secondary";

function OutcomeBadge({ outcome }: { outcome: AuditOutcome }) {
	return <Badge variant={outcomeVariant(outcome)}>{outcomeLabels[outcome]}</Badge>;
}

/** 统一审计入口（docs/audit-design.md §6）：仅管理员，按时间/主体/动作/结果/关联筛选。 */
export function AuditPage({ suspended }: { suspended: boolean }) {
	const [items, setItems] = useState<AuditEvent[]>([]);
	const [cursor, setCursor] = useState<string>();
	const [applied, setApplied] = useState<AuditEventFilter>({});
	const [draft, setDraft] = useState<FilterDraft>(emptyDraft);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);
	const [loadingMore, setLoadingMore] = useState(false);
	const [settings, setSettings] = useState<AuditSettings>();
	const [settingsError, setSettingsError] = useState("");
	const [viewingCorrelation, setViewingCorrelation] = useState<string>();

	useEffect(() => {
		let cancelled = false;
		setLoading(true);
		setError("");
		listAuditEvents(applied)
			.then((page) => {
				if (cancelled) return;
				setItems(page.items ?? []);
				setCursor(page.nextCursor);
			})
			.catch((reason: unknown) => {
				if (!cancelled)
					setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			})
			.finally(() => {
				if (!cancelled) setLoading(false);
			});
		return () => {
			cancelled = true;
		};
	}, [applied]);

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

	async function loadMore() {
		if (!cursor) return;
		setLoadingMore(true);
		try {
			const page = await listAuditEvents(applied, cursor);
			setItems((previous) => [...previous, ...(page.items ?? [])]);
			setCursor(page.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoadingMore(false);
		}
	}

	return (
		<section className="flex flex-col gap-4">
			<h2 className="text-xl font-semibold">审计日志</h2>
			{settingsError && (
				<Alert variant="destructive">
					<AlertDescription>保留设置读取失败：{settingsError}</AlertDescription>
				</Alert>
			)}
			{settings && (
				<RetentionCard
					settings={settings}
					suspended={suspended}
					onSaved={setSettings}
				/>
			)}
			<form
				className="flex flex-col gap-3"
				onSubmit={(event) => {
					event.preventDefault();
					setApplied(draftToFilter(draft));
				}}
			>
				<div className="grid gap-3 md:grid-cols-3 xl:grid-cols-6">
					<Field>
						<FieldLabel htmlFor="audit-filter-since">开始时间</FieldLabel>
						<Input
							id="audit-filter-since"
							type="datetime-local"
							value={draft.since}
							onChange={(event) =>
								setDraft((value) => ({ ...value, since: event.target.value }))
							}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="audit-filter-until">结束时间</FieldLabel>
						<Input
							id="audit-filter-until"
							type="datetime-local"
							value={draft.until}
							onChange={(event) =>
								setDraft((value) => ({ ...value, until: event.target.value }))
							}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="audit-filter-actor">主体类型</FieldLabel>
						<Select
							value={draft.actorType}
							onValueChange={(value) =>
								setDraft((current) => ({ ...current, actorType: value }))
							}
						>
							<SelectTrigger id="audit-filter-actor">
								<SelectValue placeholder="全部" />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									<SelectItem value="user">用户</SelectItem>
									<SelectItem value="service">服务</SelectItem>
									<SelectItem value="system">系统</SelectItem>
								</SelectGroup>
							</SelectContent>
						</Select>
					</Field>
					<Field>
						<FieldLabel htmlFor="audit-filter-outcome">结果</FieldLabel>
						<Select
							value={draft.outcome}
							onValueChange={(value) =>
								setDraft((current) => ({ ...current, outcome: value }))
							}
						>
							<SelectTrigger id="audit-filter-outcome">
								<SelectValue placeholder="全部" />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									<SelectItem value="success">成功</SelectItem>
									<SelectItem value="failure">失败</SelectItem>
									<SelectItem value="rejected">已拒绝</SelectItem>
									<SelectItem value="unknown">未知</SelectItem>
								</SelectGroup>
							</SelectContent>
						</Select>
					</Field>
					<Field>
						<FieldLabel htmlFor="audit-filter-action">操作</FieldLabel>
						<Input
							id="audit-filter-action"
							placeholder="如 user.create"
							value={draft.action}
							onChange={(event) =>
								setDraft((value) => ({ ...value, action: event.target.value }))
							}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="audit-filter-correlation">关联 ID</FieldLabel>
						<Input
							id="audit-filter-correlation"
							value={draft.correlationId}
							onChange={(event) =>
								setDraft((value) => ({
									...value,
									correlationId: event.target.value,
								}))
							}
						/>
					</Field>
				</div>
				<div className="flex gap-2">
					<Button type="submit">应用筛选</Button>
					<Button
						type="button"
						variant="ghost"
						onClick={() => {
							setDraft(emptyDraft);
							setApplied({});
						}}
					>
						重置
					</Button>
				</div>
			</form>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{loading ? (
				<DetailSkeleton
					label="正在读取审计事件"
					rows={["line", "line", "line"]}
				/>
			) : items.length === 0 && !error ? (
				<Empty>
					<EmptyHeader>
						<EmptyMedia variant="icon">
							<Search />
						</EmptyMedia>
						<EmptyTitle>没有匹配的审计事件</EmptyTitle>
						<EmptyDescription>
							当前筛选没有可显示的记录；请调整时间范围或其他条件。
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			) : items.length > 0 ? (
				<>
					<EntityList
						items={items.map((event) => ({
							id: event.id,
							title: `${actorLabels[event.actorType]} · ${event.action}`,
							subtitle: [
								event.actorId,
								phaseLabel(event.phase),
								event.domainRefType
									? `${event.domainRefType} · ${event.domainRefId ?? "—"}`
									: "",
							]
								.filter(Boolean)
								.join(" · "),
							badge: {
								text: outcomeLabels[event.outcome],
								variant: outcomeVariant(event.outcome),
							},
							time: formatTimestamp(event.createdAt),
							event,
						}))}
						columns={["title", "subtitle", "status", "time", "actions"]}
						renderActions={(row) =>
							row.event.correlationId ? (
								<Button
									size="sm"
									variant="outline"
									onClick={() => setViewingCorrelation(row.event.correlationId)}
								>
									查看关联
								</Button>
							) : (
								<Badge variant="secondary">历史无关联</Badge>
							)
						}
						emptyTitle="没有匹配的审计事件"
					/>
					{cursor && (
						<div>
							<LoadMoreButton
								loading={loadingMore}
								hasMore={Boolean(cursor)}
								onLoadMore={() => void loadMore()}
							/>
						</div>
					)}
				</>
			) : null}
			{viewingCorrelation !== undefined && (
				<CorrelationDetails
					correlationId={viewingCorrelation}
					onClose={() => setViewingCorrelation(undefined)}
				/>
			)}
		</section>
	);
}

const chronological = (a: AuditEvent, b: AuditEvent) =>
	a.createdAt.localeCompare(b.createdAt);

/** 关联详情复用列表接口按 correlationId 过滤，按时间正序呈现发起、准入、执行尝试与结果。 */
function CorrelationDetails({
	correlationId,
	onClose,
}: {
	correlationId: string;
	onClose: () => void;
}) {
	const [events, setEvents] = useState<AuditEvent[]>([]);
	const [cursor, setCursor] = useState<string>();
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);
	const [loadingMore, setLoadingMore] = useState(false);

	useEffect(() => {
		let cancelled = false;
		listAuditEvents({ correlationId })
			.then((page) => {
				if (cancelled) return;
				setEvents([...(page.items ?? [])].sort(chronological));
				setCursor(page.nextCursor);
			})
			.catch((reason: unknown) => {
				if (!cancelled)
					setError(messageOf(reason, "暂时无法完成操作，请重试。"));
			})
			.finally(() => {
				if (!cancelled) setLoading(false);
			});
		return () => {
			cancelled = true;
		};
	}, [correlationId]);

	async function loadMore() {
		if (!cursor) return;
		setLoadingMore(true);
		try {
			const page = await listAuditEvents({ correlationId }, cursor);
			setEvents((previous) =>
				[...previous, ...(page.items ?? [])].sort(chronological),
			);
			setCursor(page.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoadingMore(false);
		}
	}

	return (
		<Dialog
			open
			onOpenChange={(open) => {
				if (!open) onClose();
			}}
		>
			<DialogContent className="max-h-[85svh] overflow-y-auto sm:max-w-2xl">
				<DialogHeader>
					<DialogTitle>关联详情</DialogTitle>
					<DialogDescription className="font-mono">
						{correlationId}
					</DialogDescription>
				</DialogHeader>
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
										<span className="text-sm font-medium">{event.action}</span>
										<OutcomeBadge outcome={event.outcome} />
									</div>
									<div className="text-xs text-muted-foreground">
										{formatTimestamp(event.createdAt)} ·{" "}
										{actorLabels[event.actorType]} {event.actorId}
										{event.domainRefType
											? ` · ${event.domainRefType} ${event.domainRefId ?? ""}`
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
				{cursor && (
					<DialogFooter>
						<LoadMoreButton
							loading={loadingMore}
							hasMore={Boolean(cursor)}
							onLoadMore={() => void loadMore()}
						/>
					</DialogFooter>
				)}
			</DialogContent>
		</Dialog>
	);
}

function RetentionCard({
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
		<Card>
			<CardHeader>
				<CardTitle>保留与清理</CardTitle>
				<CardDescription>
					在线审计默认且最短保留{" "}
					{settings.minRetentionMonths || MIN_RETENTION_MONTHS}{" "}
					个自然月；保留期内内容不可修改或删除，到期后仅由后台受控清理。此处的调整也会记录审计。
				</CardDescription>
				<CardAction>
					<Button
						variant="outline"
						size="sm"
						disabled={suspended}
						onClick={() => setEditing(true)}
					>
						修改保留期
					</Button>
				</CardAction>
			</CardHeader>
			<CardContent className="flex flex-col gap-2 text-sm">
				<div>
					当前保留期：{settings.retentionMonths} 个自然月
					{settings.updatedAt && (
						<span className="text-muted-foreground">
							（{formatTimestamp(settings.updatedAt)}
							{settings.updatedBy ? ` 由 ${settings.updatedBy} 调整` : ""}）
						</span>
					)}
				</div>
				<div>
					上次清理运行：{formatTimestamp(cleanup?.lastRunAt)}
					{cleanup?.lastSuccessCutoffAt && (
						<span className="text-muted-foreground">
							{" "}
							· 已清至 {formatTimestamp(cleanup.lastSuccessCutoffAt)}，共{" "}
							{(cleanup.lastSuccessDeletedCount ?? 0).toLocaleString("zh-CN")}{" "}
							条
						</span>
					)}
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
			</CardContent>
			{editing && (
				<RetentionDialog
					settings={settings}
					suspended={suspended}
					onSaved={onSaved}
					onClose={() => setEditing(false)}
				/>
			)}
		</Card>
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
