// Read-only detail of one business view. Views describe scope; they never own
// credentials or permissions, so the page states that boundary explicitly.

import { ClipboardCheck } from "lucide-react";
import { useEffect, useState } from "react";
import { messageOf } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
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
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { PropertyList } from "@/components/workbench/PropertyList";
import { type BusinessView, formatTime, getBusinessView } from "../api";

export function ViewDetail({
	viewKey,
	suspended,
	navigate,
}: {
	viewKey: string;
	suspended: boolean;
	navigate: (to: string) => void;
}) {
	const [prevKey, setPrevKey] = useState(viewKey);
	const [state, setState] = useState<{
		loadedKey: string;
		view?: BusinessView;
		error?: string;
	}>({ loadedKey: viewKey });
	// Resetting during render (not inside an effect) keeps the pane consistent
	// the moment the selected key changes, without cascading renders.
	if (prevKey !== viewKey) {
		setPrevKey(viewKey);
		setState({ loadedKey: viewKey });
	}

	useEffect(() => {
		let cancelled = false;
		const timer = setTimeout(() => {
			void getBusinessView(viewKey)
				.then((next) => {
					if (!cancelled) setState({ loadedKey: viewKey, view: next });
				})
				.catch((reason) => {
					if (!cancelled)
						setState({
							loadedKey: viewKey,
							error: messageOf(reason, "无法读取业务视图。"),
						});
				});
		}, 0);
		return () => {
			cancelled = true;
			clearTimeout(timer);
		};
	}, [viewKey]);

	const current = state.loadedKey === viewKey ? state : undefined;
	if (current?.error)
		return (
			<div>
				<Alert variant="destructive">
					<AlertDescription>{current.error}</AlertDescription>
				</Alert>
			</div>
		);
	if (!current?.view)
		return (
			<div role="status" aria-label="正在读取业务视图">
				<div className="flex flex-col gap-4">
					<Skeleton className="h-7 w-1/3" />
					<Skeleton className="h-4 w-1/4" />
					<Skeleton className="h-32 w-full" />
					<Skeleton className="h-32 w-full" />
				</div>
			</div>
		);
	const view = current.view;

	const conditions = Object.entries(view.scope.labelConditions);
	return (
		<div className="flex w-full flex-col gap-4">
			{/* 抽屉头部（DetailSheet）负责标题；操作入口保留在内容顶部。 */}
			<header className="flex flex-wrap items-start justify-end gap-3">
				<div className="flex flex-wrap gap-2">
					{/* Usage entry: hands the view scope to the inspection planner for preselection. */}
					<Button
						variant="outline"
						onClick={() =>
							navigate(
								`/inspections?businessViewKey=${encodeURIComponent(view.viewKey)}${view.scope.connectionName ? `&connectionName=${encodeURIComponent(view.scope.connectionName)}` : ""}`,
							)
						}
					>
						<ClipboardCheck data-icon="inline-start" />
						按此视图巡检
					</Button>
					<Button
						variant="outline"
						disabled={suspended}
						onClick={() =>
							navigate(
								`/business-views?view=${encodeURIComponent(view.viewKey)}&edit=1`,
							)
						}
					>
						编辑
					</Button>
				</div>
			</header>
			{view.description ? (
				<p className="text-sm">{view.description}</p>
			) : (
				<p className="text-sm text-muted-foreground">未填写业务说明。</p>
			)}
			<Card>
				<CardHeader>
					<CardTitle>范围</CardTitle>
					<CardDescription>
						范围只是组织与说明：视图不授予新的查询权限，凭据与访问边界仍在接入侧。
					</CardDescription>
				</CardHeader>
				<CardContent className="flex flex-col gap-4">
					<div className="flex flex-wrap items-center gap-2">
						<span className="text-sm font-medium">来源接入</span>
						{view.scope.connectionName ? (
							<Badge variant="default">{view.scope.connectionName}</Badge>
						) : (
							<Badge variant="secondary">全部候选来源</Badge>
						)}
					</div>
					<div className="flex flex-wrap items-center gap-2">
						<span className="text-sm font-medium">告警归属</span>
						{view.scope.alertSourceKeys?.length ? (
							view.scope.alertSourceKeys.map((key) => (
								<Badge key={key} variant="default" className="font-mono">
									{key}
								</Badge>
							))
						) : (
							<Badge variant="secondary">不参与告警归属</Badge>
						)}
					</div>
					<div className="flex flex-col gap-2">
						<span className="text-sm font-medium">标签条件</span>
						{conditions.length ? (
							<EntityList
								items={conditions.map(([key, value]) => ({
									id: key,
									title: key,
									subtitle: value,
								}))}
								columns={["title", "subtitle"]}
							/>
						) : (
							<Empty className="min-h-24">
								<EmptyHeader>
									<EmptyTitle>未设置标签条件</EmptyTitle>
									<EmptyDescription>
										留空表示不按标签收窄候选对象
										{view.scope.alertSourceKeys?.length
											? "；参与告警归属的视图必须设置标签条件"
											: ""}
										。
									</EmptyDescription>
								</EmptyHeader>
							</Empty>
						)}
					</div>
					<Separator />
					<PropertyList
						layout="grid-3"
						entries={[
							{ label: "创建时间", value: formatTime(view.createdAt) },
							{ label: "更新时间", value: formatTime(view.updatedAt) },
							{ label: "行版本", value: view.rowVersion },
						]}
					/>
				</CardContent>
			</Card>
		</div>
	);
}
