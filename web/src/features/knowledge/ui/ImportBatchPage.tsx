import { useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
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
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { usePolling } from "@/hooks/use-polling";
import {
	api,
	type CandidateSummary,
	candidateStateLabels,
	type ImportBatchDetail,
} from "../api";
import { BatchBadge } from "./ImportsPage";
import { formatDateTime } from "./shared";

/** 粘贴一段原文启动导入;AI 分解出候选后可离开,稍后回来逐条处理。 */
export function ImportBatchNew({
	suspended,
	navigate,
}: {
	suspended: boolean;
	navigate: (route: string) => void;
}) {
	const [text, setText] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	async function start() {
		setBusy(true);
		setError("");
		try {
			const batch = await api.startImport(text);
			notify.success("已开始导入");
			navigate(`/knowledge/imports/${batch.id}`);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作,请重试。"));
		} finally {
			setBusy(false);
		}
	}
	return (
		<section className="space-y-4">
			<header className="space-y-1">
				<h1 className="text-xl font-semibold">导入原文</h1>
				<p className="text-sm text-muted-foreground">
					粘贴运维文档、复盘或排查手册,AI 会分解为候选;确认前不会进入知识库。
				</p>
			</header>
			<Field>
				<FieldLabel htmlFor="import-text">原文</FieldLabel>
				<Textarea
					id="import-text"
					rows={14}
					value={text}
					onChange={(event) => setText(event.target.value)}
					disabled={suspended || busy}
					placeholder="粘贴原文……"
				/>
				<FieldDescription>
					分解需要一点时间;提交后可以离开,稍后到导入批次里继续确认。
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
}

/** 批次详情:逐条勾选候选事务性确认;任一条冲突则全部不提交。 */
export function ImportBatchDetailPage({
	id,
	suspended,
	navigate,
}: {
	id: string;
	suspended: boolean;
	navigate: (route: string) => void;
}) {
	const [batch, setBatch] = useState<ImportBatchDetail>();
	const [selected, setSelected] = useState<Set<string>>(new Set());
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [cancelling, setCancelling] = useState(false);

	async function load(batchId: string) {
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
			setError(messageOf(reason, "暂时无法完成操作,请重试。"));
		}
	}
	// biome-ignore lint/correctness/useExhaustiveDependencies: load 读取最新状态即可;仅在批次 id 变化时重载。
	useEffect(() => {
		void load(id);
	}, [id]);
	usePolling(
		() => {
			if (batch) void load(batch.id);
		},
		2000,
		batch?.state === "Processing" && !suspended,
	);

	async function confirm() {
		if (!batch) return;
		setBusy(true);
		setError("");
		try {
			await api.confirmBatch(
				batch.id,
				batch.candidates
					.filter((candidate) => selected.has(candidate.id))
					.map((candidate) => ({
						candidateId: candidate.id,
						expectedRevision: candidate.draftRevision,
					})),
			);
			notify.success("已确认所选候选");
			await load(batch.id);
		} catch (reason) {
			// 事务性确认任一冲突则全部不提交;刷新后按最新修订再来。
			notify.error(reason, "确认未完成,请重试。");
			await load(batch.id);
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
			notify.error(reason, "暂时无法完成操作,请重试。");
		} finally {
			setBusy(false);
			setCancelling(false);
		}
	}

	if (!batch)
		return error ? (
			<Alert variant="destructive">
				<AlertDescription>{error}</AlertDescription>
			</Alert>
		) : (
			<DetailSkeleton
				label="正在读取批次"
				rows={["line", "card", "card", "card"]}
			/>
		);

	const awaiting = batch.candidates.filter(
		(candidate) => candidate.state === "AwaitingConfirmation",
	);
	const allSelected =
		awaiting.length > 0 && awaiting.every((item) => selected.has(item.id));
	const actionable = ["Processing", "AwaitingConfirmation"].includes(
		batch.state,
	);

	return (
		<section className="space-y-4">
			<header className="flex flex-wrap items-center gap-3">
				<h1 className="text-xl font-semibold">导入批次</h1>
				<BatchBadge state={batch.state} />
				<span className="text-sm text-muted-foreground">
					{formatDateTime(batch.createdAt)}
				</span>
			</header>
			{batch.state === "Processing" && (
				<Alert>
					<AlertDescription>
						正在分解原文,页面打开时会自动刷新;可以先离开,稍后回来确认。
					</AlertDescription>
				</Alert>
			)}
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{batch.candidates.length > 0 ? (
				<ul className="divide-y overflow-hidden rounded-lg border">
					{batch.candidates.map((candidate) => (
						<CandidateRow
							key={candidate.id}
							candidate={candidate}
							checked={selected.has(candidate.id)}
							disabled={
								candidate.state !== "AwaitingConfirmation" || busy || suspended
							}
							onToggle={(checked) =>
								setSelected((current) => {
									const next = new Set(current);
									if (checked) next.add(candidate.id);
									else next.delete(candidate.id);
									return next;
								})
							}
							onEdit={() => navigate(`/knowledge/candidates/${candidate.id}`)}
						/>
					))}
				</ul>
			) : (
				batch.state !== "Processing" && (
					<p className="text-sm text-muted-foreground">原文没有分解出候选。</p>
				)
			)}
			{batch.state === "AwaitingConfirmation" && (
				<div className="flex flex-wrap items-center gap-2 border-t pt-4">
					<Button
						variant="ghost"
						size="sm"
						disabled={busy || suspended || awaiting.length === 0}
						onClick={() =>
							setSelected(
								allSelected
									? new Set()
									: new Set(awaiting.map((item) => item.id)),
							)
						}
					>
						{allSelected ? "取消全选" : "全选"}
					</Button>
					<span className="text-sm text-muted-foreground">
						已选 {selected.size} 条
					</span>
					<div className="ml-auto flex gap-2">
						<Button
							variant="ghost"
							className="text-destructive hover:text-destructive"
							disabled={busy || suspended || !actionable}
							onClick={() => setCancelling(true)}
						>
							取消批次
						</Button>
						<Button
							disabled={!selected.size || busy || suspended}
							onClick={() => void confirm()}
						>
							确认所选({selected.size})
						</Button>
					</div>
				</div>
			)}
			{batch.state === "Processing" && actionable && (
				<div className="border-t pt-4">
					<Button
						variant="ghost"
						className="text-destructive hover:text-destructive"
						disabled={busy || suspended}
						onClick={() => setCancelling(true)}
					>
						取消批次
					</Button>
				</div>
			)}
			<AlertDialog open={cancelling} onOpenChange={setCancelling}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>取消整个导入批次?</AlertDialogTitle>
						<AlertDialogDescription>
							取消后本批候选将不能再编辑或确认。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>返回</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void cancel();
							}}
						>
							取消批次
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}

function CandidateRow({
	candidate,
	checked,
	disabled,
	onToggle,
	onEdit,
}: {
	candidate: CandidateSummary;
	checked: boolean;
	disabled: boolean;
	onToggle: (checked: boolean) => void;
	onEdit: () => void;
}) {
	return (
		<li className="flex items-start gap-3 px-4 py-3">
			<Checkbox
				className="mt-0.5"
				checked={checked}
				disabled={disabled}
				aria-label={`选择 ${candidate.draftTitle ?? "未命名候选"}`}
				onCheckedChange={(value) => onToggle(value === true)}
			/>
			<div className="min-w-0 flex-1">
				<p className="truncate text-sm font-medium">
					{candidate.draftTitle ?? "未命名候选"}
				</p>
				<p className="mt-0.5 text-xs text-muted-foreground">
					{candidateStateLabels[candidate.state]}
				</p>
			</div>
			<Button variant="ghost" size="sm" onClick={onEdit}>
				编辑
			</Button>
		</li>
	);
}
