/* eslint-disable react-refresh/only-export-components, react-hooks/exhaustive-deps -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

import { useEffect, useRef, useState } from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf, notify } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { ErrorRetry } from "@/components/workbench/ErrorRetry";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import {
	api,
	batchStateLabels,
	type CandidateDetail,
	type CandidateSummary,
	candidateSourceLabels,
	candidateStateLabels,
	embeddingStateLabels,
	type ImportBatchDetail,
	type ImportBatchSummary,
	indexStateLabels,
	type KnowledgeDetail,
	type KnowledgeSearchHit,
	type KnowledgeVersionDetail,
	type KnowledgeVersionSummary,
	type Page,
} from "@/features/knowledge/api";
import { usePolling } from "@/hooks/use-polling";
import { parseRoute } from "@/lib/parse-route";

/**
 * One cursor-paged sidebar list: first page on mount, explicit “加载更多”
 * appends. Reads pause while suspended; in-flight responses are invalidated
 * by a generation bump and resuming re-reads the first page (alerts pattern).
 */
function usePagedList<T extends { id: string }>(
	fetchPage: (cursor?: string) => Promise<Page<T>>,
	suspended: boolean,
	fallbackError: string,
) {
	const fetchRef = useRef(fetchPage);
	fetchRef.current = fetchPage;
	const suspendedRef = useRef(suspended);
	suspendedRef.current = suspended;
	const generationRef = useRef(0);
	// 挂起切换必须在渲染期作废旧世代，堵住 effect 清理前的落地窗口。
	const pauseScopeRef = useRef(suspended);
	if (pauseScopeRef.current !== suspended) {
		pauseScopeRef.current = suspended;
		generationRef.current += 1;
	}
	const [state, setState] = useState<{
		items: T[];
		nextCursor?: string;
		loading: boolean;
		loadingMore: boolean;
		error: string;
	}>({ items: [], loading: true, loadingMore: false, error: "" });
	async function load(cursor?: string) {
		if (suspendedRef.current) return;
		// 只有最新世代的响应可以写状态。
		const generation = generationRef.current + 1;
		generationRef.current = generation;
		setState((current) => ({
			items: cursor ? current.items : [],
			nextCursor: cursor ? current.nextCursor : undefined,
			error: "",
			loading: !cursor,
			loadingMore: Boolean(cursor),
		}));
		try {
			const page = await fetchRef.current(cursor);
			if (generation !== generationRef.current) return;
			setState((current) => {
				// 追加按 id 去重：游标窗口重叠与页内重复 id 都只留一行。
				const seen = new Set(current.items.map((item) => item.id));
				const fresh = page.items.filter((item) => {
					if (seen.has(item.id)) return false;
					seen.add(item.id);
					return true;
				});
				return {
					...current,
					items: cursor ? [...current.items, ...fresh] : page.items,
					nextCursor: page.nextCursor,
					error: "",
					loading: false,
					loadingMore: false,
				};
			});
		} catch (reason) {
			if (generation !== generationRef.current) return;
			setState((current) => ({
				...current,
				error: messageOf(reason, fallbackError),
				loading: false,
				loadingMore: false,
			}));
		}
	}
	useEffect(() => {
		if (!suspended) void load();
		else {
			// 挂起：作废在途请求、收起加载指示；已读内容只读保留。
			generationRef.current += 1;
			setState((current) => ({
				...current,
				loading: false,
				loadingMore: false,
			}));
		}
	}, [suspended]);
	// 卸载后的迟到响应不允许落地。
	useEffect(() => {
		return () => {
			generationRef.current += 1;
		};
	}, []);
	return { ...state, load };
}

/** Knowledge view keeps cursors tied to their server query and never polls while suspended. */
export function useKnowledgeModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const [query, setQuery] = useState("");
	const [hits, setHits] = useState<{
		exactTextMatches: KnowledgeSearchHit[];
		semanticMatches: KnowledgeSearchHit[];
	} | null>(null);
	const [items, setItems] = useState<
		Awaited<ReturnType<typeof api.browse>>["items"]
	>([]);
	const [next, setNext] = useState<string>();
	const candidatesList = usePagedList<CandidateSummary>(
		(cursor) => api.listCandidates(undefined, cursor),
		props.suspended,
		"无法读取待确认知识。",
	);
	const importsList = usePagedList<ImportBatchSummary>(
		(cursor) => api.listImportBatches(cursor),
		props.suspended,
		"无法读取导入批次。",
	);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(true);
	// The coordinator supplies absolute workspace routes; remove the module prefix
	// before interpreting the feature-local route. Item details are a drawer
	// driven by the `item` query flag, like the alert detail sheet.
	const parsed = parseRoute(props.route);
	const parts = parsed.pathname
		.split("/")
		.filter(Boolean)
		.filter((part, index) => !(index === 0 && part === "knowledge"));
	const candidateId = parts[0] === "candidates" ? parts[1] : undefined;
	const knowledgeId = parsed.searchParams.get("item") || undefined;
	const importId = parts[0] === "imports" ? parts[1] : undefined;

	async function load(cursor?: string) {
		setLoading(true);
		setError("");
		try {
			const page = await api.browse(cursor);
			setItems((value) => (cursor ? [...value, ...page.items] : page.items));
			setNext(page.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	}
	useEffect(() => {
		void load();
	}, []);
	async function search() {
		const value = query.trim();
		if (!value) {
			setHits(null);
			return;
		}
		setError("");
		try {
			const result = await api.search(value);
			setHits({
				exactTextMatches: result.exactTextMatches,
				semanticMatches: result.semanticMatches,
			});
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		}
	}
	const list = (
		<ScrollArea className="h-full px-3">
			<div className="space-y-3 py-3">
				<form
					className="flex gap-2"
					onSubmit={(event) => {
						event.preventDefault();
						void search();
					}}
				>
					<Input
						aria-label="检索知识"
						value={query}
						onChange={(event) => setQuery(event.target.value)}
						placeholder="检索知识"
					/>
					<Button type="submit" variant="secondary">
						搜索
					</Button>
				</form>
				{error && (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}
				{hits ? (
					<SearchGroups
						hits={hits}
						open={(id) => props.navigate(`/knowledge?item=${id}`)}
					/>
				) : (
					<>
						<EntityList
							items={items.map((item) => ({
								id: item.id,
								title: item.title,
								subtitle: `v${item.currentVersionSeq} · ${
									item.eligible ? "可复用" : "已退出检索"
								}`,
							}))}
							columns={["title", "subtitle"]}
							selectedId={knowledgeId}
							onSelect={(row) => props.navigate(`/knowledge?item=${row.id}`)}
							loading={loading && !items.length}
							loadingLabel="正在读取知识列表"
							emptyTitle="尚无已确认知识。"
						/>
						<LoadMoreButton
							loading={loading}
							hasMore={Boolean(next)}
							onLoadMore={() => void load(next)}
						/>
					</>
				)}
				<h3 className="text-sm font-medium">待确认</h3>
				<EntityList
					items={candidatesList.items.map((item) => ({
						id: item.id,
						title: item.draftTitle || `候选 ${item.id}`,
						subtitle: item.state,
					}))}
					columns={["title", "subtitle"]}
					onSelect={(row) =>
						props.navigate(`/knowledge/candidates/${row.id}`)
					}
					loading={candidatesList.loading}
					loadingLabel="正在读取待确认候选"
					error={candidatesList.items.length ? undefined : candidatesList.error}
					onRetry={
						props.suspended
							? undefined
							: () => void candidatesList.load(candidatesList.nextCursor)
					}
					emptyTitle="暂无待确认候选。"
				/>
				{candidatesList.items.length > 0 && candidatesList.error && (
					props.suspended ? (
						// 挂起=只读：错误事实保留，重试入口隐藏。
						<Alert variant="destructive">
							<AlertDescription>{candidatesList.error}</AlertDescription>
						</Alert>
					) : (
						<ErrorRetry
							message={candidatesList.error}
							onRetry={() =>
								void candidatesList.load(candidatesList.nextCursor)
							}
						/>
					)
				)}
				<LoadMoreButton
					loading={candidatesList.loadingMore}
					hasMore={!props.suspended && Boolean(candidatesList.nextCursor)}
					onLoadMore={() =>
						void candidatesList.load(candidatesList.nextCursor)
					}
				>
					加载更多候选
				</LoadMoreButton>
				<h3 className="text-sm font-medium">导入批次</h3>
				<EntityList
					items={importsList.items.map((item) => ({
						id: item.id,
						title: `导入 ${item.id}`,
						subtitle: item.state,
					}))}
					columns={["title", "subtitle"]}
					onSelect={(row) => props.navigate(`/knowledge/imports/${row.id}`)}
					loading={importsList.loading}
					loadingLabel="正在读取导入批次"
					error={importsList.items.length ? undefined : importsList.error}
					onRetry={
						props.suspended
							? undefined
							: () => void importsList.load(importsList.nextCursor)
					}
					emptyTitle="暂无导入批次。"
				/>
				{importsList.items.length > 0 && importsList.error && (
					props.suspended ? (
						<Alert variant="destructive">
							<AlertDescription>{importsList.error}</AlertDescription>
						</Alert>
					) : (
						<ErrorRetry
							message={importsList.error}
							onRetry={() =>
								void importsList.load(importsList.nextCursor)
							}
						/>
					)
				)}
				<LoadMoreButton
					loading={importsList.loadingMore}
					hasMore={!props.suspended && Boolean(importsList.nextCursor)}
					onLoadMore={() => void importsList.load(importsList.nextCursor)}
				>
					加载更多批次
				</LoadMoreButton>
			</div>
		</ScrollArea>
	);
	let content: React.ReactNode = (
		<section className="space-y-3">
			<h2 className="text-lg font-semibold">知识库</h2>
			<p className="text-sm text-muted-foreground">
				搜索同时展示全文匹配和语义相似结果。
			</p>
		</section>
	);
	const knowledgeTitle = knowledgeId
		? (items.find((item) => item.id === knowledgeId)?.title ?? "知识详情")
		: undefined;
	const itemSheet = knowledgeId && (
		<DetailSheet
			open
			onClose={() => props.navigate("/knowledge")}
			title={knowledgeTitle}
			description="已确认知识的当前版本与历史。"
		>
			<div className="min-h-0 flex-1 overflow-y-auto">
				<div className="p-4 sm:p-6">
					<KnowledgeItem
						id={knowledgeId}
						suspended={props.suspended}
						navigate={props.navigate}
						openEvidence={props.openEvidence}
					/>
				</div>
			</div>
		</DetailSheet>
	);
	if (candidateId)
		content = (
			<CandidateEditor
				id={candidateId}
				suspended={props.suspended}
				refresh={() => void load()}
				navigate={props.navigate}
			/>
		);
	else if (importId)
		content = (
			<ImportBatch
				id={importId}
				suspended={props.suspended}
				navigate={props.navigate}
			/>
		);
	else
		content = (
			<>
				{content}
				{itemSheet}
			</>
		);
	return {
		title: "知识",
		crumbs: candidateId
			? [{ label: "知识", to: "/knowledge" }, { label: "编辑知识候选" }]
			: importId
				? [
						{ label: "知识", to: "/knowledge" },
						{ label: importId === "new" ? "导入原文" : "导入批次" },
					]
				: undefined,
		list,
		content,
		actions: (
			<Button
				disabled={props.suspended}
				onClick={() => props.navigate("/knowledge/imports/new")}
			>
				导入原文
			</Button>
		),
	};
}

function SearchGroups({
	hits,
	open,
}: {
	hits: {
		exactTextMatches: KnowledgeSearchHit[];
		semanticMatches: KnowledgeSearchHit[];
	};
	open: (id: string) => void;
}) {
	const exactIds = new Set(
		hits.exactTextMatches.map((hit) => hit.knowledge.id),
	);
	return (
		<div className="space-y-4">
			<HitGroup title="全文匹配" hits={hits.exactTextMatches} open={open} />
			<HitGroup
				title="语义相似"
				hits={hits.semanticMatches.filter(
					(hit) => !exactIds.has(hit.knowledge.id),
				)}
				open={open}
				semantic
			/>
		</div>
	);
}
function HitGroup({
	title,
	hits,
	open,
	semantic = false,
}: {
	title: string;
	hits: KnowledgeSearchHit[];
	open: (id: string) => void;
	semantic?: boolean;
}) {
	return (
		<section>
			<h3 className="mb-1 text-sm font-medium">{title}</h3>
			{hits.length ? (
				<EntityList
					items={hits.map((hit) => ({
						id: hit.knowledge.id,
						title: hit.knowledge.title,
						subtitle: `分数 ${hit.score.toFixed(3)}${
							semantic && hit.indexState
								? ` · ${indexStateLabels[hit.indexState]}`
								: ""
						}`,
					}))}
					columns={["title", "subtitle"]}
					onSelect={(row) => open(row.id)}
				/>
			) : (
				<p className="text-sm text-muted-foreground">没有匹配项。</p>
			)}
		</section>
	);
}
function CandidateEditor({
	id,
	suspended,
	refresh,
	navigate,
}: {
	id: string;
	suspended: boolean;
	refresh: () => void;
	navigate: (route: string) => void;
}) {
	const [candidate, setCandidate] = useState<CandidateDetail>();
	const [title, setTitle] = useState("");
	const [body, setBody] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [confirming, setConfirming] = useState(false);
	const [excluding, setExcluding] = useState(false);
	const load = async () => {
		try {
			const value = await api.getCandidate(id);
			setCandidate(value);
			setTitle(value.draftTitle ?? value.originalSuggestion.title);
			setBody(value.draftBody ?? value.originalSuggestion.body);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		}
	};
	useEffect(() => {
		void load();
	}, [id]);
	useEffect(() => {
		if (suspended) setBusy(false);
	}, [suspended]);
	if (!candidate)
		return (
			<DetailSkeleton
				label="正在读取候选"
				rows={["line", "title", "line", "line", "line", "card"]}
			/>
		);
	const currentCandidate = candidate;
	async function save() {
		setBusy(true);
		setError("");
		try {
			const next = await api.editDraft(id, currentCandidate.draftRevision, {
				title,
				body,
			});
			setCandidate((current) => (current ? { ...current, ...next } : current));
			notify.success("已保存");
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
		}
	}
	async function confirm() {
		setBusy(true);
		try {
			const next = await api.confirm(id, currentCandidate.draftRevision);
			notify.success("已确认为知识");
			refresh();
			navigate(`/knowledge/items/${next.confirmedKnowledgeId}`);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
			setConfirming(false);
		}
	}
	async function exclude() {
		setBusy(true);
		try {
			await api.exclude(id, currentCandidate.rowVersion);
			notify.success("已排除");
			refresh();
			navigate("/knowledge");
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
		}
	}
	return (
		<section className="space-y-5">
			<div>
				<Badge>{candidateStateLabels[candidate.state]}</Badge>
				<h2 className="mt-2 text-xl font-semibold">编辑知识候选</h2>
				<p className="text-sm text-muted-foreground">
					来源：{candidateSourceLabels[candidate.sourceType]}
					。发生冲突时保留当前输入，刷新后重新确认。
				</p>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<Field>
				<FieldLabel htmlFor="candidate-title">标题</FieldLabel>
				<Input
					id="candidate-title"
					value={title}
					onChange={(e) => setTitle(e.target.value)}
					disabled={busy || suspended}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="candidate-body">正文</FieldLabel>
				<Textarea
					id="candidate-body"
					value={body}
					onChange={(e) => setBody(e.target.value)}
					disabled={busy || suspended}
					rows={14}
				/>
			</Field>
			<div className="flex gap-2">
				<Button disabled={busy || suspended} onClick={() => void save()}>
					保存草稿
				</Button>
				<Button
					variant="secondary"
					disabled={
						busy || suspended || candidate.state !== "AwaitingConfirmation"
					}
					onClick={() => setConfirming(true)}
				>
					确认知识
				</Button>
				<Button
					variant="destructive"
					disabled={
						busy || suspended || candidate.state !== "AwaitingConfirmation"
					}
					onClick={() => setExcluding(true)}
				>
					排除
				</Button>
			</div>
			<AlertDialog open={confirming} onOpenChange={setConfirming}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>确认发布此知识？</AlertDialogTitle>
						<AlertDialogDescription>
							确认后将创建不可变知识版本并进入检索。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void confirm();
							}}
						>
							确认
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
			<AlertDialog open={excluding} onOpenChange={setExcluding}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>排除此知识候选？</AlertDialogTitle>
						<AlertDialogDescription>
							排除后该候选将永久移出确认流程，不可恢复。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								setExcluding(false);
								void exclude();
							}}
						>
							排除
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}
function KnowledgeItem({
	id,
	suspended,
	navigate,
	openEvidence,
}: {
	id: string;
	suspended: boolean;
	navigate: (route: string) => void;
	openEvidence: (id: string) => void;
}) {
	const [detail, setDetail] = useState<KnowledgeDetail>();
	const [versions, setVersions] = useState<KnowledgeVersionSummary[]>([]);
	const [current, setCurrent] = useState<KnowledgeVersionDetail>();
	const [error, setError] = useState("");
	const [stop, setStop] = useState(false);
	const load = async () => {
		try {
			const [item, page] = await Promise.all([
				api.getKnowledge(id),
				api.listVersions(id),
			]);
			setDetail(item);
			setVersions(page.items);
			setCurrent(await api.getVersion(id, item.currentVersionId));
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		}
	};
	useEffect(() => {
		queueMicrotask(() => {
			void load();
		});
	}, [id]);
	if (!detail)
		return (
			<DetailSkeleton
				label="正在读取知识"
				rows={["line", "title", "line", "line", "line", "line"]}
			/>
		);
	const currentDetail = detail;
	async function revise() {
		try {
			const candidate = await api.createRevision(
				currentDetail.id,
				currentDetail.currentVersionId,
				currentDetail.rowVersion,
			);
			notify.success("已创建修订");
			navigate(`/knowledge/candidates/${candidate.id}`);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		}
	}
	async function stopReuse() {
		const version = versions.find(
			(value) => value.id === currentDetail.currentVersionId,
		);
		if (!version) return;
		try {
			await api.stopReuse(
				currentDetail.id,
				version.id,
				version.retrievalStateRowVersion,
			);
			notify.success("已停止复用");
			setStop(false);
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		}
	}
	return (
		<section className="space-y-5">
			{/* 抽屉头部（DetailSheet）负责条目标题；这里保留复用状态。 */}
			<div>
				<Badge variant={detail.eligible ? "default" : "secondary"}>
					{detail.eligible ? "可复用" : "已退出检索"}
				</Badge>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<article>
				<h3 className="font-medium">当前正文</h3>
				<p className="mt-2 whitespace-pre-wrap text-sm">{current?.body}</p>
				<FieldDescription className="flex items-center gap-2">
					语义索引：
					{current ? (
						(embeddingStateLabels[current.embeddingState] ??
						current.embeddingState)
					) : (
						<Skeleton className="inline-block h-4 w-16" />
					)}
				</FieldDescription>
			</article>
			<div className="flex gap-2">
				<Button disabled={suspended} onClick={() => void revise()}>
					创建修订
				</Button>
				{detail.eligible && (
					<Button
						variant="destructive"
						disabled={suspended}
						onClick={() => setStop(true)}
					>
						停止复用
					</Button>
				)}
				<Button
					variant="outline"
					onClick={() => openEvidence(current?.sourceCandidateId ?? "")}
					disabled={!current?.sourceCandidateId}
				>
					查看来源
				</Button>
			</div>
			<section>
				<h3 className="font-medium">版本历史</h3>
				<EntityList
					items={versions.map((version) => ({
						id: version.id,
						title: `v${version.versionSeq} · ${version.title}`,
						subtitle: version.eligible ? "可检索" : "已退出",
					}))}
					columns={["title", "subtitle"]}
					onSelect={(row) =>
						void api
							.getVersion(currentDetail.id, row.id)
							.then(setCurrent)
							.catch((reason) =>
								setError(messageOf(reason, "暂时无法完成操作，请重试。")),
							)
					}
					emptyTitle="尚无版本历史"
				/>
			</section>
			<AlertDialog open={stop} onOpenChange={setStop}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>停止复用当前版本？</AlertDialogTitle>
						<AlertDialogDescription>
							此操作会使当前版本永久退出检索。要恢复内容，必须创建新的修订。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							onClick={(event) => {
								event.preventDefault();
								void stopReuse();
							}}
						>
							停止复用
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}
function ImportBatch({
	id,
	suspended,
	navigate,
}: {
	id: string;
	suspended: boolean;
	navigate: (route: string) => void;
}) {
	const [text, setText] = useState("");
	const [batch, setBatch] = useState<ImportBatchDetail>();
	const [selected, setSelected] = useState<Set<string>>(new Set());
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const load = async (batchId: string) => {
		try {
			const value = await api.getImportBatch(batchId);
			setBatch(value);
			setSelected(
				new Set(
					value.candidates
						.filter((candidate) => candidate.state === "AwaitingConfirmation")
						.map((candidate) => candidate.id),
				),
			);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		}
	};
	useEffect(() => {
		if (id !== "new") void load(id);
	}, [id]);
	usePolling(
		() => {
			if (batch) void load(batch.id);
		},
		2000,
		batch?.state === "Processing" && !suspended,
	);
	async function start() {
		setBusy(true);
		try {
			const value = await api.startImport(text);
			notify.success("已开始导入");
			setText("");
			setBatch(value);
			navigate(`/knowledge/imports/${value.id}`);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
		}
	}
	async function confirm() {
		if (!batch) return;
		setBusy(true);
		try {
			await api.confirmBatch(
				batch.id,
				batch.candidates
					.filter((c) => selected.has(c.id))
					.map((c) => ({
						candidateId: c.id,
						expectedRevision: c.draftRevision,
					})),
			);
			notify.success("已确认所选候选");
			await load(batch.id);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
		}
	}
	async function cancel() {
		if (!batch) return;
		setBusy(true);
		try {
			await api.cancelBatch(batch.id, batch.rowVersion);
			notify.success("已取消批次");
			await load(batch.id);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setBusy(false);
		}
	}
	if (id === "new")
		return (
			<section className="space-y-4">
				<h2 className="text-xl font-semibold">导入原文</h2>
				<Field>
					<FieldLabel htmlFor="import-text">文本</FieldLabel>
					<Textarea
						id="import-text"
						rows={16}
						value={text}
						onChange={(e) => setText(e.target.value)}
						disabled={suspended || busy}
					/>
					<FieldDescription>
						原文将被分解为待确认候选；不会使用通用 JSON 代替正文。
					</FieldDescription>
				</Field>
				{error && (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}
				<Button
					disabled={!text.trim() || suspended || busy}
					onClick={() => void start()}
				>
					开始导入
				</Button>
			</section>
		);
	return (
		<section className="space-y-4">
			<h2 className="text-xl font-semibold">导入批次</h2>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{!batch ? (
				<DetailSkeleton
					label="正在读取批次"
					rows={["line", "card", "card", "card"]}
				/>
			) : (
				<>
					<Badge>{batchStateLabels[batch.state] ?? batch.state}</Badge>
					{batch.state === "Processing" && (
						<p className="text-sm text-muted-foreground">
							正在处理；会在此页面可见时刷新。
						</p>
					)}
					<div className="space-y-2">
						{batch.candidates.map((candidate) => (
							<label
								key={candidate.id}
								className="flex items-start gap-2 rounded border p-3"
							>
								<Checkbox
									checked={selected.has(candidate.id)}
									disabled={
										candidate.state !== "AwaitingConfirmation" ||
										busy ||
										suspended
									}
									onCheckedChange={(checked) =>
										setSelected((value) => {
											const next = new Set(value);
											if (checked) next.add(candidate.id);
											else next.delete(candidate.id);
											return next;
										})
									}
								/>
								<span>
									<strong>{candidate.draftTitle ?? "未命名候选"}</strong>
									<small className="block text-muted-foreground">
										r{candidate.draftRevision} ·{" "}
										{candidateStateLabels[candidate.state]}
									</small>
								</span>
							</label>
						))}
					</div>
					<div className="flex gap-2">
						<Button
							disabled={
								!selected.size ||
								busy ||
								suspended ||
								batch.state !== "AwaitingConfirmation"
							}
							onClick={() => void confirm()}
						>
							事务性确认所选项
						</Button>
						<Button
							variant="destructive"
							disabled={
								busy ||
								suspended ||
								!["Processing", "AwaitingConfirmation"].includes(batch.state)
							}
							onClick={() => void cancel()}
						>
							取消批次
						</Button>
					</div>
				</>
			)}
		</section>
	);
}
