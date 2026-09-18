// Business views module (route /business-views). A business view is an
// optional, versioned scope-and-description organization (ADR 0004): nothing
// here is a precondition for alerts, observations, tools or basic inspections.
// The former business-declaration write mainline (upload/publish/refresh) is
// gone; its history stays readable through the admin/inspection APIs only.

import { ChevronRight, Layers } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf } from "@/app/shared";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyMedia,
	EmptyTitle,
} from "@/components/ui/empty";
import {
	Item,
	ItemContent,
	ItemDescription,
	ItemGroup,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import { ScrollArea } from "@/components/ui/scroll-area";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { type BusinessView, listBusinessViews } from "../api";
import { ViewDetail } from "./view-detail";
import { ViewEditor } from "./view-editor";

export function useSystemsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const [views, setViews] = useState<BusinessView[]>([]);
	const [loaded, setLoaded] = useState(false);
	const [error, setError] = useState("");
	const routeUrl = new URL(props.route, "https://workbench.invalid");
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

	const list = (
		<div className="flex h-full min-w-0 flex-col gap-3 p-3">
			<div className="flex items-center justify-between gap-2">
				<div className="truncate font-medium">业务视图</div>
				<Button size="sm" onClick={() => props.navigate("/business-views/new")}>
					新建
				</Button>
			</div>
			<ScrollArea className="min-h-0 min-w-0 flex-1 overflow-x-hidden [&_[data-slot=scroll-area-viewport]]:overflow-x-hidden">
				{views.length === 0 ? (
					!loaded ? (
						<DetailSkeleton label="正在加载业务视图" rows={["line", "line", "line", "line"]} />
					) : (
						<Empty className="min-h-40">
							<EmptyHeader>
								<EmptyTitle>尚无业务视图</EmptyTitle>
								<EmptyDescription>
									业务视图是可选的；新建一个后它会显示在这里。
								</EmptyDescription>
							</EmptyHeader>
						</Empty>
					)
				) : (
					<ItemGroup
						aria-label="业务视图列表"
						className="w-full min-w-0 max-w-full overflow-hidden"
					>
						{views.map((view) => (
							<Item
								asChild
								key={view.viewKey}
								size="sm"
								className="w-full min-w-0 max-w-full gap-2 overflow-hidden px-2 py-2 hover:bg-accent/50"
							>
								<button
									type="button"
									className="flex w-full min-w-0 max-w-full overflow-hidden items-center gap-2 text-left"
									onClick={() =>
										props.navigate(
											`/business-views?view=${encodeURIComponent(view.viewKey)}`,
										)
									}
								>
									<ItemMedia
										variant="icon"
										className="size-7 shrink-0 [&_svg]:size-3.5"
									>
										<Layers aria-hidden="true" />
									</ItemMedia>
									<ItemContent className="min-w-0 max-w-full overflow-hidden">
										<ItemTitle
											className="!w-full !min-w-0 truncate"
											title={view.displayName}
										>
											{view.displayName}
										</ItemTitle>
										<div className="flex min-w-0 items-center gap-1.5">
											<Badge
												className="shrink-0 px-1.5 py-0 text-[10px]"
												variant={
													view.scope.connectionName ? "default" : "secondary"
												}
											>
												{view.scope.connectionName ?? "全部来源"}
											</Badge>
											<ItemDescription
												className="min-w-0 flex-1 truncate"
												title={view.viewKey}
											>
												{view.viewKey}
											</ItemDescription>
										</div>
									</ItemContent>
									<ChevronRight
										className="size-4 shrink-0 text-muted-foreground"
										aria-hidden="true"
									/>
								</button>
							</Item>
						))}
					</ItemGroup>
				)}
			</ScrollArea>
		</div>
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
	) : selectedKey ? (
		<ViewDetail
			viewKey={selectedKey}
			suspended={props.suspended}
			navigate={props.navigate}
		/>
	) : (
		<div>
			{error ? (
				<Alert variant="destructive">
					<AlertTitle>无法读取业务视图</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			) : !loaded ? (
				<DetailSkeleton label="正在读取业务视图" rows={["title", "card", "card", "card"]} />
			) : (
				<Empty className="min-h-56">
					<EmptyHeader>
						<EmptyMedia variant="icon">
							<Layers aria-hidden="true" />
						</EmptyMedia>
						<EmptyTitle>
							{views.length ? "选择一个业务视图" : "尚无业务视图"}
						</EmptyTitle>
						<EmptyDescription>
							业务视图是可选的组织方式：没有业务视图也可以接收告警、观测对象、使用已授权工具和执行基础巡检。
							{views.length
								? " 从左侧选择一个视图，或新建一个。"
								: " 新建第一个业务视图后会在这里显示。"}
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			)}
		</div>
	);

	return { title: "业务视图", list, content };
}
