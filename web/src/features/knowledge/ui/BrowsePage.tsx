/* eslint-disable react-hooks/exhaustive-deps -- 分页/检索读取刻意只跟随挂载与显式条件;fetch 经 ref 或闭包取最新。 */

import { Search, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { messageOf } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { TableCell, TableRow } from "@/components/ui/table";
import { DataTable } from "@/components/workbench/DataTable";
import { ErrorRetry } from "@/components/workbench/ErrorRetry";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { api, indexStateLabels, type KnowledgeSearchHit } from "../api";
import { usePagedList } from "./shared";

/**
 * 知识库主页:一个搜索框同时请求全文与语义两个通道;
 * 无查询时展示可复用知识的浏览表格。
 */
export function BrowsePage({
	suspended,
	onOpen,
}: {
	suspended: boolean;
	onOpen: (knowledgeId: string) => void;
}) {
	const [query, setQuery] = useState("");
	const [submitted, setSubmitted] = useState("");
	return (
		<section className="space-y-6">
			<header className="space-y-1">
				<h1 className="text-xl font-semibold">知识库</h1>
				<p className="text-sm text-muted-foreground">
					沉淀下来的排查与处置经验,支持全文与语义检索。
				</p>
			</header>
			<form
				className="flex gap-2"
				onSubmit={(event) => {
					event.preventDefault();
					setSubmitted(query.trim());
				}}
			>
				<div className="relative flex-1">
					<Search className="absolute top-2.5 left-3 size-4 text-muted-foreground" />
					<Input
						aria-label="检索知识"
						className="pr-9 pl-9"
						value={query}
						onChange={(event) => setQuery(event.target.value)}
						placeholder="用自然语言描述问题,例如“结算延迟升高怎么排查”"
					/>
					{query && (
						<Button
							type="button"
							variant="ghost"
							size="icon"
							className="absolute top-0.5 right-1 size-8"
							aria-label="清空搜索"
							onClick={() => {
								setQuery("");
								setSubmitted("");
							}}
						>
							<X />
						</Button>
					)}
				</div>
				<Button type="submit" disabled={!query.trim()}>
					搜索
				</Button>
			</form>
			{submitted ? (
				<SearchResults
					key={submitted}
					query={submitted}
					suspended={suspended}
					onOpen={onOpen}
				/>
			) : (
				<BrowseTable suspended={suspended} onOpen={onOpen} />
			)}
		</section>
	);
}

function BrowseTable({
	suspended,
	onOpen,
}: {
	suspended: boolean;
	onOpen: (knowledgeId: string) => void;
}) {
	const list = usePagedList(
		async (cursor) => {
			const page = await api.browse(cursor);
			return { items: page.items, nextCursor: page.nextCursor };
		},
		suspended,
		"无法读取知识列表。",
	);
	return (
		<div className="space-y-3">
			<DataTable
				columns={[
					{ label: "知识" },
					{ label: "当前版本", className: "w-24" },
					{ label: "状态", className: "w-28" },
				]}
				loading={list.loading}
				loadingLabel="正在读取知识列表"
				error={list.items.length ? undefined : list.error || undefined}
				onRetry={
					suspended || !list.error || list.items.length > 0
						? undefined
						: () => void list.load()
				}
				emptyTitle="知识库还是空的"
				emptyDescription="在告警分析、调查对话或巡检报告中使用“整理为知识”,经确认后就会出现在这里。"
			>
				{list.items.map((item) => (
					<TableRow
						key={item.id}
						className="cursor-pointer"
						onClick={() => onOpen(item.id)}
					>
						<TableCell className="font-medium">{item.title}</TableCell>
						<TableCell className="text-muted-foreground">
							v{item.currentVersionSeq}
						</TableCell>
						<TableCell>
							<Badge variant={item.eligible ? "default" : "secondary"}>
								{item.eligible ? "可复用" : "已退出检索"}
							</Badge>
						</TableCell>
					</TableRow>
				))}
			</DataTable>
			{list.items.length > 0 && list.error &&
				(suspended ? (
					<Alert variant="destructive">
						<AlertDescription>{list.error}</AlertDescription>
					</Alert>
				) : (
					<ErrorRetry
						message={list.error}
						onRetry={() => void list.load(list.nextCursor)}
					/>
				))}
			<LoadMoreButton
				loading={list.loadingMore}
				hasMore={!suspended && Boolean(list.nextCursor)}
				onLoadMore={() => void list.load(list.nextCursor)}
			/>
		</div>
	);
}

/** 一次查询的两通道结果;同一知识命中两组时只在全文组出现,并标注语义依据。 */
function SearchResults({
	query,
	suspended,
	onOpen,
}: {
	query: string;
	suspended: boolean;
	onOpen: (knowledgeId: string) => void;
}) {
	const [exact, setExact] = useState<KnowledgeSearchHit[]>([]);
	const [semantic, setSemantic] = useState<KnowledgeSearchHit[]>([]);
	const [nextCursor, setNextCursor] = useState<string>();
	const [loading, setLoading] = useState(true);
	const [loadingMore, setLoadingMore] = useState(false);
	const [error, setError] = useState("");
	const generationRef = useRef(0);

	async function load(cursor?: string) {
		const generation = generationRef.current + 1;
		generationRef.current = generation;
		setError("");
		if (cursor) setLoadingMore(true);
		else setLoading(true);
		try {
			const result = await api.search(query, cursor);
			if (generation !== generationRef.current) return;
			setExact((current) =>
				cursor
					? [...current, ...result.exactTextMatches]
					: result.exactTextMatches,
			);
			setSemantic((current) =>
				cursor
					? [...current, ...result.semanticMatches]
					: result.semanticMatches,
			);
			setNextCursor(result.nextCursor);
		} catch (reason) {
			if (generation !== generationRef.current) return;
			setError(messageOf(reason, "搜索暂时不可用,请重试。"));
		} finally {
			if (generation === generationRef.current) {
				setLoading(false);
				setLoadingMore(false);
			}
		}
	}
	// 仅挂载时读取一次;换查询词通过 key 重挂组件。
	useEffect(() => {
		void load();
		// 卸载后迟到响应不落地。
		return () => {
			generationRef.current += 1;
		};
	}, []);

	const exactIds = new Set(exact.map((hit) => hit.knowledge.id));
	const semanticOnly = semantic.filter(
		(hit) => !exactIds.has(hit.knowledge.id),
	);
	const empty =
		!loading && !error && exact.length === 0 && semanticOnly.length === 0;

	return (
		<div className="space-y-6">
			<p className="text-sm text-muted-foreground">“{query}” 的检索结果</p>
			{error && !exact.length && !semantic.length ? (
				suspended ? (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				) : (
					<ErrorRetry message={error} onRetry={() => void load()} />
				)
			) : (
				<>
					<HitGroup
						title="全文匹配"
						hits={exact}
						loading={loading}
						onOpen={onOpen}
						alsoSemantic={(hit) =>
							semantic.find((other) => other.knowledge.id === hit.knowledge.id)
						}
					/>
					<HitGroup
						title="语义相似"
						hits={semanticOnly}
						loading={loading}
						onOpen={onOpen}
						semantic
					/>
				</>
			)}
			{empty && (
				<p className="text-sm text-muted-foreground">
					没有匹配的知识。换个说法试试,或把这次处置经验整理为知识。
				</p>
			)}
			{error && (exact.length > 0 || semantic.length > 0) &&
				(suspended ? (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				) : (
					<ErrorRetry message={error} onRetry={() => void load(nextCursor)} />
				))}
			<LoadMoreButton
				loading={loadingMore}
				hasMore={!suspended && Boolean(nextCursor)}
				onLoadMore={() => void load(nextCursor)}
			>
				加载更多结果
			</LoadMoreButton>
		</div>
	);
}

function HitGroup({
	title,
	hits,
	loading,
	onOpen,
	semantic = false,
	alsoSemantic,
}: {
	title: string;
	hits: KnowledgeSearchHit[];
	loading: boolean;
	onOpen: (knowledgeId: string) => void;
	semantic?: boolean;
	alsoSemantic?: (hit: KnowledgeSearchHit) => KnowledgeSearchHit | undefined;
}) {
	if (loading)
		return (
			<section
				className="space-y-2"
				role="status"
				aria-label={`正在搜索${title}`}
			>
				<h2 className="text-sm font-medium text-muted-foreground">{title}</h2>
				<div className="space-y-2">
					<div className="h-12 animate-pulse rounded-md bg-muted" />
					<div className="h-12 animate-pulse rounded-md bg-muted" />
				</div>
			</section>
		);
	if (hits.length === 0) return null;
	return (
		<section className="space-y-2">
			<h2 className="text-sm font-medium text-muted-foreground">{title}</h2>
			<ul className="divide-y overflow-hidden rounded-lg border">
				{hits.map((hit) => {
					const extra = alsoSemantic?.(hit);
					return (
						<li key={hit.knowledge.id}>
							<button
								type="button"
								className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-muted/50"
								onClick={() => onOpen(hit.knowledge.id)}
							>
								<span className="min-w-0 flex-1">
									<span className="block truncate text-sm font-medium">
										{hit.knowledge.title}
									</span>
									<span className="mt-0.5 block text-xs text-muted-foreground">
										相关度 {hit.score.toFixed(2)}
										{extra && ` · 语义相似 ${extra.score.toFixed(2)}`}
										{semantic &&
											hit.indexState &&
											hit.indexState !== "ready" &&
											` · ${indexStateLabels[hit.indexState] ?? hit.indexState}`}
									</span>
								</span>
								<Badge
									variant={hit.knowledge.eligible ? "default" : "secondary"}
								>
									{hit.knowledge.eligible ? "可复用" : "已退出"}
								</Badge>
							</button>
						</li>
					);
				})}
			</ul>
		</section>
	);
}
