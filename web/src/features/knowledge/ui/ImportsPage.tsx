import { useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { TableCell, TableRow } from "@/components/ui/table";
import { DataTable } from "@/components/workbench/DataTable";
import { ErrorRetry } from "@/components/workbench/ErrorRetry";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { api, batchStateLabels, type ImportBatchSummary } from "../api";
import { formatDateTime, usePagedList } from "./shared";

const stateOptions: Array<{ value: string; label: string }> = [
	{ value: "all", label: "全部状态" },
	...Object.entries(batchStateLabels).map(([value, label]) => ({
		value,
		label,
	})),
];

/** 导入批次列表:按状态过滤,点击进入批次逐条处理候选。 */
export function ImportsPage({
	suspended,
	onOpen,
}: {
	suspended: boolean;
	onOpen: (batchId: string) => void;
}) {
	const [state, setState] = useState("all");
	const list = usePagedList(
		(cursor) =>
			api.listImportBatches(
				{
					state:
						state === "all"
							? undefined
							: (state as ImportBatchSummary["state"]),
				},
				cursor,
			),
		suspended,
		"无法读取导入批次。",
		state,
	);
	return (
		<section className="space-y-4">
			<header className="space-y-1">
				<h1 className="text-xl font-semibold">导入批次</h1>
				<p className="text-sm text-muted-foreground">
					粘贴一段原文,AI 会把它分解为候选,逐条确认后入库。
				</p>
			</header>
			<div>
				<Select value={state} onValueChange={setState}>
					<SelectTrigger size="sm" aria-label="按状态过滤">
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						{stateOptions.map((option) => (
							<SelectItem key={option.value} value={option.value}>
								{option.label}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
			</div>
			<DataTable
				columns={[{ label: "导入时间" }, { label: "状态", className: "w-28" }]}
				loading={list.loading}
				loadingLabel="正在读取导入批次"
				error={list.items.length ? undefined : list.error || undefined}
				onRetry={
					suspended || !list.error || list.items.length > 0
						? undefined
						: () => void list.load()
				}
				emptyTitle="还没有导入批次"
				emptyDescription="点击右上角“导入原文”,把现有文档批量搬进知识库。"
			>
				{list.items.map((item) => (
					<TableRow
						key={item.id}
						className="cursor-pointer"
						onClick={() => onOpen(item.id)}
					>
						<TableCell className="font-medium">
							{formatDateTime(item.createdAt)}
						</TableCell>
						<TableCell>
							<BatchBadge state={item.state} />
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
		</section>
	);
}

export function BatchBadge({ state }: { state: ImportBatchSummary["state"] }) {
	const variant =
		state === "AwaitingConfirmation"
			? "default"
			: state === "Processing"
				? "outline"
				: state === "Failed" || state === "Cancelled"
					? "destructive"
					: "secondary";
	return <Badge variant={variant}>{batchStateLabels[state]}</Badge>;
}
