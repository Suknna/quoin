/* eslint-disable react-hooks/exhaustive-deps -- 详情读取刻意只跟随条目 id/版本 id;数据获取函数与回调经闭包取最新。 */

import { useEffect, useState } from "react";
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
import { Button } from "@/components/ui/button";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { PropertyList } from "@/components/workbench/PropertyList";
import { FeedbackPanel } from "@/features/feedback/ui";
import {
	api,
	type CandidateDetail,
	candidateSourceLabels,
	embeddingStateLabels,
	type KnowledgeDetail,
	type KnowledgeVersionDetail,
	type KnowledgeVersionSummary,
} from "../api";
import { formatDateTime } from "./shared";

/** 结构化范围字段(scope/conditions/limitations)的键值展示;空对象不渲染。 */
function ScopeBlock({
	title,
	value,
}: {
	title: string;
	value?: Record<string, unknown>;
}) {
	const entries = Object.entries(value ?? {});
	if (entries.length === 0) return null;
	return (
		<section className="space-y-2">
			<h3 className="text-sm font-medium">{title}</h3>
			<PropertyList
				layout="inline"
				entries={entries.map(([key, item]) => ({
					label: key,
					value:
						typeof item === "object" && item !== null
							? JSON.stringify(item)
							: String(item),
				}))}
			/>
		</section>
	);
}

/** 知识详情:当前版本阅读 + 事实条 + 来源反馈 + 不可变版本历史。 */
export function KnowledgeItemSheet({
	id,
	suspended,
	navigate,
	openEvidence,
	onLoaded,
}: {
	id: string;
	suspended: boolean;
	navigate: (route: string) => void;
	openEvidence: (id: string) => void;
	/** 加载完成后把条目标题交给抽屉头。 */
	onLoaded?: (title: string) => void;
}) {
	const [detail, setDetail] = useState<KnowledgeDetail>();
	const [versions, setVersions] = useState<KnowledgeVersionSummary[]>([]);
	const [viewing, setViewing] = useState<KnowledgeVersionDetail>();
	const [source, setSource] = useState<CandidateDetail>();
	const [error, setError] = useState("");
	const [stopping, setStopping] = useState(false);
	const [busy, setBusy] = useState(false);

	async function load() {
		setError("");
		try {
			const [item, page] = await Promise.all([
				api.getKnowledge(id),
				api.listVersions(id),
			]);
			setDetail(item);
			setVersions(page.items);
			setViewing(await api.getVersion(id, item.currentVersionId));
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作,请重试。"));
		}
	}
	// onLoaded 是父级 setState,天然稳定;条目切换经 key 重挂载,只需跟随 id。
	useEffect(() => {
		let cancelled = false;
		void (async () => {
			try {
				const [item, page] = await Promise.all([
					api.getKnowledge(id),
					api.listVersions(id),
				]);
				if (cancelled) return;
				setDetail(item);
				setVersions(page.items);
				onLoaded?.(item.title);
				setViewing(await api.getVersion(id, item.currentVersionId));
			} catch (reason) {
				if (!cancelled)
					setError(messageOf(reason, "暂时无法完成操作,请重试。"));
			}
		})();
		return () => {
			cancelled = true;
		};
	}, [id]);

	// 来源候选取回后,只有诊断类来源(分析/报告/消息)才提供反馈面。
	const viewingCandidateId = viewing?.sourceCandidateId;
	useEffect(() => {
		if (!viewingCandidateId) return;
		let cancelled = false;
		api
			.getCandidate(viewingCandidateId)
			.then((candidate) => {
				if (!cancelled) setSource(candidate);
			})
			.catch(() => undefined);
		return () => {
			cancelled = true;
		};
	}, [viewingCandidateId]);

	if (!detail || !viewing) {
		return error ? (
			<Alert variant="destructive">
				<AlertDescription>{error}</AlertDescription>
			</Alert>
		) : (
			<DetailSkeleton
				label="正在读取知识"
				rows={["line", "title", "line", "line", "line", "line"]}
			/>
		);
	}

	const currentDetail = detail;
	const viewingHistory = viewing.id !== detail.currentVersionId;

	async function revise() {
		setBusy(true);
		try {
			const candidate = await api.createRevision(
				currentDetail.id,
				currentDetail.currentVersionId,
				currentDetail.rowVersion,
			);
			notify.success("已基于当前版本创建修订");
			navigate(`/knowledge/candidates/${candidate.id}`);
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作,请重试。");
		} finally {
			setBusy(false);
		}
	}
	async function stopReuse() {
		const version = versions.find(
			(value) => value.id === currentDetail.currentVersionId,
		);
		if (!version) return;
		setBusy(true);
		try {
			await api.stopReuse(
				currentDetail.id,
				version.id,
				version.retrievalStateRowVersion,
			);
			notify.success("已停止复用");
			setStopping(false);
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作,请重试。");
		} finally {
			setBusy(false);
		}
	}

	const feedbackTarget = (() => {
		const src = source?.originalSuggestion.source;
		if (
			src &&
			(src.type === "initial_analysis_output" ||
				src.type === "inspection_report" ||
				src.type === "investigation_message")
		)
			return { type: src.type, id: src.id };
		return null;
	})();

	return (
		<div className="space-y-6">
			{viewingHistory && (
				<Alert>
					<AlertDescription className="flex items-center justify-between gap-2">
						正在预览历史版本 v{viewing.versionSeq}
						<Button
							size="sm"
							variant="outline"
							onClick={() =>
								void api
									.getVersion(currentDetail.id, currentDetail.currentVersionId)
									.then(setViewing)
							}
						>
							回到当前版本
						</Button>
					</AlertDescription>
				</Alert>
			)}
			<article className="space-y-2">
				<h3 className="text-sm font-medium text-muted-foreground">
					v{viewing.versionSeq} · {viewing.title}
				</h3>
				<p className="text-sm leading-7 whitespace-pre-wrap">{viewing.body}</p>
			</article>
			<PropertyList
				layout="inline"
				entries={[
					{
						label: "版本",
						value: `v${detail.currentVersionSeq} · 共 ${detail.versionCount} 版`,
					},
					{
						label: "语义索引",
						value:
							embeddingStateLabels[viewing.embeddingState] ??
							viewing.embeddingState,
					},
					{ label: "创建时间", value: formatDateTime(viewing.createdAt) },
					...(source
						? [
								{
									label: "来源",
									value:
										candidateSourceLabels[
											source.originalSuggestion.source.type
										] ?? source.originalSuggestion.source.type,
								},
							]
						: []),
					...(viewing.exitedAt
						? [
								{
									label: "退出检索",
									value: `${formatDateTime(viewing.exitedAt)}${
										viewing.exitReason === "source_rejected"
											? " · 来源被标记不采纳"
											: ""
									}`,
								},
							]
						: []),
				]}
			/>
			<ScopeBlock title="适用范围" value={viewing.scope} />
			<ScopeBlock title="适用条件" value={viewing.conditions} />
			<ScopeBlock title="限制" value={viewing.limitations} />
			<div className="flex flex-wrap gap-2">
				<Button disabled={suspended || busy} onClick={() => void revise()}>
					创建修订
				</Button>
				{detail.eligible && (
					<Button
						variant="outline"
						className="text-destructive hover:text-destructive"
						disabled={suspended || busy}
						onClick={() => setStopping(true)}
					>
						停止复用
					</Button>
				)}
				{viewing.sourceCandidateId && (
					<Button
						variant="ghost"
						onClick={() => openEvidence(viewing.sourceCandidateId)}
					>
						查看来源
					</Button>
				)}
			</div>
			{feedbackTarget && (
				<div className="border-t pt-4">
					<FeedbackPanel target={feedbackTarget} suspended={suspended} />
				</div>
			)}
			<section className="space-y-2 border-t pt-4">
				<h3 className="text-sm font-medium">版本历史</h3>
				<EntityList
					items={versions.map((version) => ({
						id: version.id,
						title: `v${version.versionSeq} · ${version.title}`,
						subtitle: formatDateTime(version.createdAt),
						badge: {
							text: version.eligible ? "可检索" : "已退出",
							variant: version.eligible ? "outline" : "secondary",
						},
					}))}
					columns={["title", "subtitle", "status"]}
					selectedId={viewing.id}
					onSelect={(row) =>
						void api
							.getVersion(currentDetail.id, row.id)
							.then(setViewing)
							.catch((reason) =>
								notify.error(reason, "暂时无法完成操作,请重试。"),
							)
					}
					emptyTitle="尚无版本历史"
				/>
			</section>
			<AlertDialog open={stopping} onOpenChange={setStopping}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>停止复用当前版本?</AlertDialogTitle>
						<AlertDialogDescription>
							当前版本将永久退出检索,且不会再自动恢复。如需恢复内容,请从当前知识发起修订并确认新版本。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={busy}
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
		</div>
	);
}
