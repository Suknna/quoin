// Business views module (route /business-views). A business view is an
// optional, versioned scope-and-description organization (ADR 0004): nothing
// here is a precondition for alerts, observations, tools or basic inspections.
// The former business-declaration write mainline (upload/publish/refresh) is
// gone; its history stays readable through the admin/inspection APIs only.

import { useCallback, useEffect, useState } from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { parseRoute } from "@/lib/parse-route";
import { type BusinessView, listBusinessViews } from "../api";
import { ViewDetail } from "./view-detail";
import { ViewEditor } from "./view-editor";

export function useSystemsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const [views, setViews] = useState<BusinessView[]>([]);
	const [loaded, setLoaded] = useState(false);
	const [error, setError] = useState("");
	const routeUrl = parseRoute(props.route);
	const selectedKey = routeUrl.searchParams.get("view");
	const editing = routeUrl.searchParams.get("edit") === "1";
	const creating = routeUrl.pathname.endsWith("/new");

	const load = useCallback(async () => {
		try {
			setViews(await listBusinessViews());
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "无法读取业务视图。"));
		} finally {
			setLoaded(true);
		}
	}, []);
	useEffect(() => {
		const timer = setTimeout(() => void load(), 0);
		return () => clearTimeout(timer);
	}, [load]);

	const saved = (view: BusinessView) => {
		void load();
		props.navigate(`/business-views?view=${encodeURIComponent(view.viewKey)}`);
	};

	// 列表与详情同页：详情是右侧抽屉（与告警一致），编辑器仍是面包屑页。
	// 新建移入内容页头（告警页同款），顶栏因此隐去，与告警列表完全同构。
	const list = null;
	const actions = undefined;
	const overview = (
		<div className="space-y-4">
			<div className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">业务视图</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						可选的范围与说明组织方式；没有业务视图也可以接收告警、观测对象和执行基础巡检。
					</p>
				</div>
				<Button onClick={() => props.navigate("/business-views/new")}>
					新建
				</Button>
			</div>
			{error ? (
				<Alert variant="destructive">
					<AlertTitle>无法读取业务视图</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			) : (
				<EntityList
					items={views.map((view) => ({
						id: view.viewKey,
						title: view.displayName,
						subtitle: view.viewKey,
						badge: {
							text: view.scope.connectionName ?? "全部来源",
							variant: view.scope.connectionName
								? ("default" as const)
								: ("secondary" as const),
						},
						view,
					}))}
					columns={["title", "subtitle", "status"]}
					selectedId={selectedKey}
					onSelect={(item) =>
						props.navigate(
							`/business-views?view=${encodeURIComponent(item.view.viewKey)}`,
						)
					}
					loading={!loaded && !error}
					loadingLabel="正在读取业务视图"
					emptyTitle="尚无业务视图"
					emptyDescription="点击右上角的“新建”创建第一个业务视图。"
				/>
			)}
		</div>
	);
	const viewName = selectedKey
		? (views.find((view) => view.viewKey === selectedKey)?.displayName ??
			selectedKey)
		: undefined;
	const detailSheet = selectedKey && !editing && !creating && (
		<DetailSheet
			open
			onClose={() => props.navigate("/business-views")}
			title={viewName}
			description={`视图标识 ${selectedKey}`}
		>
			<div className="min-h-0 flex-1 overflow-y-auto">
				<div className="p-4 sm:p-6">
					<ViewDetail
						viewKey={selectedKey}
						suspended={props.suspended}
						navigate={props.navigate}
					/>
				</div>
			</div>
		</DetailSheet>
	);
	const content = creating ? (
		<ViewEditor
			suspended={props.suspended}
			navigate={props.navigate}
			onSaved={saved}
		/>
	) : selectedKey && editing ? (
		<div>
			<ViewEditor
				suspended={props.suspended}
				navigate={props.navigate}
				editKey={selectedKey}
				onSaved={saved}
			/>
		</div>
	) : (
		<>
			{overview}
			{detailSheet}
		</>
	);
	const crumbs = creating
		? [{ label: "业务视图", to: "/business-views" }, { label: "新建业务视图" }]
		: selectedKey && editing
			? [
					{ label: "业务视图", to: "/business-views" },
					{ label: `编辑 ${viewName}` },
				]
			: undefined;
	return {
		title: creating
			? "新建业务视图"
			: selectedKey && editing
				? "编辑业务视图"
				: "业务视图",
		crumbs,
		list,
		actions,
		content,
	};
}
