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
import {
	api,
	type CandidateSourceType,
	type CandidateState,
	candidateSourceLabels,
	candidateStateLabels,
} from "../api";
import { usePagedList } from "./shared";

const stateOptions: Array<{ value: string; label: string }> = [
	{ value: "AwaitingConfirmation", label: "待确认" },
	{ value: "all", label: "全部状态" },
	{ value: "Confirmed", label: "已确认" },
	{ value: "Excluded", label: "已排除" },
	{ value: "Superseded", label: "已取代" },
	{ value: "SourceInvalid", label: "来源无效" },
];

const sourceOptions: Array<{ value: string; label: string }> = [
	{ value: "all", label: "全部来源" },
	...Object.entries(candidateSourceLabels).map(([value, label]) => ({
		value,
		label,
	})),
];

/** 候选工作队列:默认只看“待确认”,可切换状态与来源;点击进入编辑层。 */
export function CandidatesPage({
	suspended,
	onOpen,
}: {
	suspended: boolean;
	onOpen: (candidateId: string) => void;
}) {
	const [state, setState] = useState("AwaitingConfirmation");
	const [sourceType, setSourceType] = useState("all");
	const list = usePagedList(
		(cursor) =>
			api.listCandidates(
				{
					state: state === "all" ? undefined : (state as CandidateState),
					sourceType:
						sourceType === "all"
							? undefined
							: (sourceType as CandidateSourceType),
				},
				cursor,
			),
		suspended,
		"无法读取知识候选。",
		`${state}|${sourceType}`,
	);
	return (
		<section className="space-y-4">
			<header className="space-y-1">
				<h1 className="text-xl font-semibold">待确认</h1>
				<p className="text-sm text-muted-foreground">
					AI 产出的知识草稿,确认后才会进入知识库。
				</p>
			</header>
			<div className="flex flex-wrap gap-2">
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
				<Select value={sourceType} onValueChange={setSourceType}>
					<SelectTrigger size="sm" aria-label="按来源过滤">
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						{sourceOptions.map((option) => (
							<SelectItem key={option.value} value={option.value}>
								{option.label}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
			</div>
			<DataTable
				columns={[
					{ label: "标题" },
					{ label: "来源", className: "w-28" },
					{ label: "状态", className: "w-28" },
				]}
				loading={list.loading}
				loadingLabel="正在读取知识候选"
				error={list.items.length ? undefined : list.error || undefined}
				onRetry={
					suspended || !list.error || list.items.length > 0
						? undefined
						: () => void list.load()
				}
				emptyTitle={
					state === "AwaitingConfirmation"
						? "没有待确认的候选"
						: "没有符合条件的候选"
				}
				emptyDescription={
					state === "AwaitingConfirmation"
						? "在告警分析、调查对话或巡检报告中使用“整理为知识”即可生成候选。"
						: undefined
				}
			>
				{list.items.map((item) => (
					<TableRow
						key={item.id}
						className="cursor-pointer"
						onClick={() => onOpen(item.id)}
					>
						<TableCell className="font-medium">
							{item.draftTitle || "未命名候选"}
						</TableCell>
						<TableCell className="text-muted-foreground">
							{candidateSourceLabels[item.sourceType]}
						</TableCell>
						<TableCell>
							<Badge
								variant={
									item.state === "AwaitingConfirmation"
										? "default"
										: item.state === "Confirmed"
											? "outline"
											: "secondary"
								}
							>
								{candidateStateLabels[item.state]}
							</Badge>
						</TableCell>
					</TableRow>
				))}
			</DataTable>
			{/* 追加失败保留已读行;挂起时错误只读展示。 */}
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
