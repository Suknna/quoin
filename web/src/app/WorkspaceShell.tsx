import {
	Bell,
	BookOpen,
	Bot,
	ClipboardCheck,
	FileText,
	GalleryVerticalEnd,
	LayoutDashboard,
	LogOut,
	SearchCheck,
	Settings,
} from "lucide-react";
import { type CSSProperties, type ReactNode, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
	Sidebar,
	SidebarContent,
	SidebarFooter,
	SidebarGroup,
	SidebarGroupContent,
	SidebarHeader,
	SidebarInset,
	SidebarMenu,
	SidebarMenuButton,
	SidebarMenuItem,
	SidebarProvider,
	SidebarTrigger,
} from "@/components/ui/sidebar";
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { WorkspaceModuleView } from "./module-contract";

type OperationItem = {
	title: string;
	route: string;
	matches: (route: string) => boolean;
	icon: typeof Bell;
	adminOnly?: boolean;
};

const operationItems: readonly OperationItem[] = [
	{ title: "告警列表", route: "/alerts/list", matches: (route) => route.startsWith("/alerts"), icon: Bell },
	{ title: "故障复盘", route: "/postmortems", matches: (route) => route.startsWith("/postmortems"), icon: FileText },
	{ title: "巡检", route: "/inspections", matches: (route) => route.startsWith("/inspections"), icon: ClipboardCheck, adminOnly: true },
	{ title: "业务纳管", route: "/business-systems", matches: (route) => route.startsWith("/business-systems"), icon: LayoutDashboard, adminOnly: true },
	{ title: "接入管理", route: "/integrations", matches: (route) => route.startsWith("/integrations"), icon: Settings, adminOnly: true },
];

function isOperationsRoute(route: string) {
	return operationItems.some((item) => item.matches(route));
}

/** Operator work is intentionally limited to alerts and AI SRE, including direct route navigation. */
function canAccessOperationsRoute(route: string, user: UserSummary) {
	const item = operationItems.find((candidate) => candidate.matches(route));
	return !item?.adminOnly || user.role === "admin";
}

function isAiSreRoute(route: string) {
	return route.startsWith("/investigations") || route.startsWith("/knowledge");
}

function initials(name: string) {
	return name.trim().slice(0, 2).toUpperCase() || "U";
}

/** Keeps persistent rail actions discoverable with both pointer and keyboard input. */
function RailButton({ label, active, icon: Icon, onClick }: { label: string; active: boolean; icon: typeof Bell; onClick: () => void }) {
	return <Tooltip><TooltipTrigger asChild><SidebarMenuButton aria-label={label} isActive={active} onClick={onClick} className="size-9 p-0"><Icon /><span className="sr-only">{label}</span></SidebarMenuButton></TooltipTrigger><TooltipContent side="right" align="center">{label}</TooltipContent></Tooltip>;
}

/** Renders the route-owned module menu before any feature-specific object list. */
function ModuleNavigation({ route, user, navigate }: { route: string; user: UserSummary; navigate: (route: string) => void }) {
	if (isOperationsRoute(route) && canAccessOperationsRoute(route, user)) {
		return <nav className="flex flex-col gap-1 border-b p-3" aria-label="运维中心模块">
			{operationItems.filter((item) => !item.adminOnly || user.role === "admin").map((item) => <Button key={item.route} className="w-full justify-start" variant={item.matches(route) ? "secondary" : "ghost"} aria-current={item.matches(route) ? "page" : undefined} onClick={() => navigate(item.route)}><item.icon data-icon="inline-start" />{item.title}</Button>)}
		</nav>;
	}
	if (isAiSreRoute(route)) {
		return <nav className="flex flex-col gap-1 border-b p-3" aria-label="AI SRE 模块">
			<Button className="w-full justify-start" variant={route.startsWith("/investigations") ? "secondary" : "ghost"} aria-current={route.startsWith("/investigations") ? "page" : undefined} onClick={() => navigate("/investigations")}><SearchCheck data-icon="inline-start" />对话</Button>
			<Button className="w-full justify-start" variant={route.startsWith("/knowledge") ? "secondary" : "ghost"} aria-current={route.startsWith("/knowledge") ? "page" : undefined} onClick={() => navigate("/knowledge")}><BookOpen data-icon="inline-start" />知识</Button>
		</nav>;
	}
	return null;
}

/** Shared shell: a narrow global rail and a route-owned module navigation pane. */
export function WorkspaceShell({
	user,
	route,
	view,
	navigate,
	onLogout,
}: {
	user: UserSummary;
	route: string;
	view: WorkspaceModuleView;
	navigate: (route: string) => void;
	onLogout: () => Promise<void> | void;
	/** Rendered as read-only during session expiration or system maintenance. */
	suspended?: boolean;
}) {
	const [logoutError, setLogoutError] = useState("");
	const operations = isOperationsRoute(route);
	const aiSre = isAiSreRoute(route);
	const moduleHeader = operations ? "运维中心" : aiSre ? "AI SRE" : view.title;
	// Alert and postmortem views have no page actions, so their desktop content begins directly below the module pane.
	const hideDesktopHeader = !view.actions && (route.startsWith("/alerts") || route.startsWith("/postmortems"));
	const moduleNavigation = <ModuleNavigation route={route} user={user} navigate={navigate} />;
	const moduleList = moduleNavigation || view.list ? <>{moduleNavigation}{view.list}</> : null;

	async function logout() {
		setLogoutError("");
		try {
			await onLogout();
		} catch (reason) {
			setLogoutError(reason instanceof Error ? reason.message : "退出登录失败，请重试。");
		}
	}

	return <SidebarProvider style={{ "--sidebar-width": "calc(12.5rem + var(--sidebar-width-icon))", "--sidebar-width-icon": "3.25rem" } as CSSProperties}>
		<Sidebar collapsible="none" className="sticky top-0 h-svh w-(--sidebar-width-icon)! self-start overflow-hidden md:flex md:w-(--sidebar-width)! md:flex-row">
			<div className="flex h-full w-[calc(var(--sidebar-width-icon)+1px)] shrink-0 flex-col border-r">
				<SidebarHeader className="px-2 py-3">
					<SidebarMenu><SidebarMenuItem><SidebarMenuButton tooltip="Quoin" aria-label="Quoin" onClick={() => navigate("/alerts/list")} className="size-9 p-0"><div className="flex size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground"><GalleryVerticalEnd className="size-4" /></div><span className="sr-only">Quoin</span></SidebarMenuButton></SidebarMenuItem></SidebarMenu>
				</SidebarHeader>
				<SidebarContent><SidebarGroup><SidebarGroupContent className="px-2"><SidebarMenu>
					<SidebarMenuItem><RailButton label="运维中心" active={operations} icon={Bell} onClick={() => navigate("/alerts/list")} /></SidebarMenuItem>
					<SidebarMenuItem><RailButton label="AI SRE" active={aiSre} icon={Bot} onClick={() => navigate("/investigations")} /></SidebarMenuItem>
				</SidebarMenu></SidebarGroupContent></SidebarGroup></SidebarContent>
				<SidebarFooter className="px-2 py-3"><SidebarMenu>{user.role === "admin" && <SidebarMenuItem><SidebarMenuButton tooltip="管理" isActive={route.startsWith("/admin")} onClick={() => navigate("/admin")} className="size-9 p-0"><Settings /><span className="sr-only">管理</span></SidebarMenuButton></SidebarMenuItem>}<SidebarMenuItem><DropdownMenu><DropdownMenuTrigger asChild><SidebarMenuButton tooltip={user.displayName} aria-label={`${user.displayName} ${user.role}`} className="size-9 p-0"><Avatar className="size-7"><AvatarFallback>{initials(user.displayName)}</AvatarFallback></Avatar><span className="sr-only">{user.displayName}</span></SidebarMenuButton></DropdownMenuTrigger><DropdownMenuContent side="right" align="end" sideOffset={8} className="min-w-56 rounded-lg"><DropdownMenuLabel><div className="flex flex-col"><span>{user.displayName}</span><span className="text-xs font-normal text-muted-foreground">{user.role}</span></div></DropdownMenuLabel><DropdownMenuSeparator /><DropdownMenuItem onClick={() => navigate("/account/profile")}><Settings />设置</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem onClick={() => void logout()}><LogOut />退出登录</DropdownMenuItem></DropdownMenuContent></DropdownMenu></SidebarMenuItem></SidebarMenu></SidebarFooter>
			</div>
			<div className="hidden h-full min-w-0 flex-1 flex-col bg-sidebar md:flex"><SidebarHeader className="border-b p-4"><div className="text-sm font-semibold">{moduleHeader}</div></SidebarHeader><SidebarContent>{moduleList}</SidebarContent></div>
		</Sidebar>
		{logoutError && <div className="fixed right-4 bottom-4 z-50"><p role="alert" className="rounded-md border border-destructive bg-background p-3 text-sm text-destructive">{logoutError}</p></div>}
			<SidebarInset>{!hideDesktopHeader && <header className="sticky top-0 z-10 hidden shrink-0 items-center gap-2 border-b bg-background p-4 md:flex"><SidebarTrigger className="-ml-1" /><div className="text-sm font-medium">{moduleHeader}</div><div className="ml-auto flex gap-2">{view.actions}</div></header>}<header className="sticky top-0 z-10 flex shrink-0 items-center gap-2 border-b bg-background p-4 md:hidden"><SidebarTrigger className="-ml-1" /><div className="text-sm font-medium">{moduleHeader}</div><Sheet><SheetTrigger asChild><Button variant="outline" size="sm" className="ml-2">菜单</Button></SheetTrigger><SheetContent side="left"><SheetHeader><SheetTitle>{moduleHeader}</SheetTitle></SheetHeader>{moduleList}</SheetContent></Sheet><div className="ml-auto flex gap-2">{view.actions}</div></header><main className={`mx-auto w-full p-6 ${operations ? "max-w-6xl" : "max-w-3xl"}`}>{!canAccessOperationsRoute(route, user) ? <Alert variant="destructive"><AlertDescription>此页面仅向管理员开放。</AlertDescription></Alert> : view.content}</main></SidebarInset>
	</SidebarProvider>;
}

export function ModuleFallback({ title, children }: { title: string; children?: ReactNode }) {
	return { title, list: null, content: children ?? <div className="text-sm text-muted-foreground">正在加载…</div> } satisfies WorkspaceModuleView;
}
