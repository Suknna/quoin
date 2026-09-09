import {
	Bell,
	BookOpen,
	Bot,
	ChevronsUpDown,
	ClipboardCheck,
	GalleryVerticalEnd,
	LayoutDashboard,
	LogOut,
	SearchCheck,
	Settings,
} from "lucide-react";
import { type CSSProperties, type ReactNode, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
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
import type { WorkspaceModuleView } from "./module-contract";

const modules = [
	{ title: "告警中心", route: "/alerts/list", icon: Bell },
	{ title: "AI SRE", route: "/investigations", icon: Bot },
	{ title: "巡检", route: "/inspections", icon: ClipboardCheck },
	{ title: "业务系统", route: "/business-systems", icon: LayoutDashboard },
] as const;

function isCurrent(route: string, destination: string) {
	if (destination === "/alerts/list") return route.startsWith("/alerts") || route.startsWith("/postmortems");
	return route === destination || route.startsWith(`${destination}/`);
}

function moduleFor(route: string) {
	if (route.startsWith("/knowledge")) return modules[1];
	return modules.find((item) => isCurrent(route, item.route)) ?? modules[0];
}

function initials(name: string) {
	return name.trim().slice(0, 2).toUpperCase() || "U";
}

/** Shared shell: narrow global rail, module switcher, and route-owned module navigation. */
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
	const activeModule = moduleFor(route);
	const alertCenter = isCurrent(route, "/alerts/list");
	const aiSre = route.startsWith("/investigations") || route.startsWith("/knowledge");
		const internalNavigation = aiSre ? <div className="flex flex-col gap-1 border-b p-3"><Button className="w-full justify-start" variant={route.startsWith("/investigations") ? "secondary" : "ghost"} onClick={() => navigate("/investigations")}><SearchCheck />对话</Button><Button className="w-full justify-start" variant={route.startsWith("/knowledge") ? "secondary" : "ghost"} onClick={() => navigate("/knowledge")}><BookOpen />知识</Button></div> : null;
	const moduleList = view.list || internalNavigation ? <>{internalNavigation}{view.list}</> : null;

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
				<SidebarMenu><SidebarMenuItem><SidebarMenuButton tooltip="告警中心" isActive={alertCenter} onClick={() => navigate("/alerts/list")} className="size-9 p-0"><div className="flex size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground"><GalleryVerticalEnd className="size-4" /></div><span className="sr-only">Quoin</span></SidebarMenuButton></SidebarMenuItem></SidebarMenu>
			</SidebarHeader>
			<SidebarContent><SidebarGroup><SidebarGroupContent className="px-2"><SidebarMenu><SidebarMenuItem><DropdownMenu><DropdownMenuTrigger asChild><SidebarMenuButton tooltip="切换模块" className="size-9 p-0"><ChevronsUpDown /><span className="sr-only">切换模块</span></SidebarMenuButton></DropdownMenuTrigger><DropdownMenuContent side="right" align="start" sideOffset={8} className="min-w-48 rounded-lg"><DropdownMenuLabel>切换模块</DropdownMenuLabel><DropdownMenuSeparator />{modules.map((item) => <DropdownMenuItem key={item.route} onClick={() => navigate(item.route)}><item.icon />{item.title}{item.route === activeModule.route && <span className="ml-auto text-xs text-muted-foreground">当前</span>}</DropdownMenuItem>)}</DropdownMenuContent></DropdownMenu></SidebarMenuItem></SidebarMenu></SidebarGroupContent></SidebarGroup></SidebarContent>
			<SidebarFooter className="px-2 py-3"><SidebarMenu>{user.role === "admin" && <SidebarMenuItem><SidebarMenuButton tooltip="管理" isActive={route.startsWith("/admin")} onClick={() => navigate("/admin")} className="size-9 p-0"><Settings /><span className="sr-only">管理</span></SidebarMenuButton></SidebarMenuItem>}<SidebarMenuItem><DropdownMenu><DropdownMenuTrigger asChild><SidebarMenuButton tooltip={user.displayName} aria-label={`${user.displayName} ${user.role}`} className="size-9 p-0"><Avatar className="size-7"><AvatarFallback>{initials(user.displayName)}</AvatarFallback></Avatar><span className="sr-only">{user.displayName}</span></SidebarMenuButton></DropdownMenuTrigger><DropdownMenuContent side="right" align="end" sideOffset={8} className="min-w-56 rounded-lg"><DropdownMenuLabel><div className="flex flex-col"><span>{user.displayName}</span><span className="text-xs font-normal text-muted-foreground">{user.role}</span></div></DropdownMenuLabel><DropdownMenuSeparator /><DropdownMenuItem onClick={() => navigate("/account/profile")}><Settings />设置</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem onClick={() => void logout()}><LogOut />退出登录</DropdownMenuItem></DropdownMenuContent></DropdownMenu></SidebarMenuItem></SidebarMenu></SidebarFooter>
			</div>
			<div className="hidden h-full min-w-0 flex-1 flex-col bg-sidebar md:flex"><SidebarHeader className="border-b p-4"><div className="text-sm font-semibold">{aiSre ? "AI SRE" : view.title}</div></SidebarHeader><SidebarContent>{moduleList}</SidebarContent></div>
		</Sidebar>
		{logoutError && <div className="fixed right-4 bottom-4 z-50"><p role="alert" className="rounded-md border border-destructive bg-background p-3 text-sm text-destructive">{logoutError}</p></div>}
		<SidebarInset>{alertCenter ? <><div className="flex border-b p-3 md:hidden"><SidebarTrigger /><Sheet><SheetTrigger asChild><Button variant="outline" size="sm" className="ml-2">菜单</Button></SheetTrigger><SheetContent side="left"><SheetHeader><SheetTitle>{view.title}</SheetTitle></SheetHeader>{moduleList}</SheetContent></Sheet></div><main className="mx-auto w-full max-w-6xl p-6">{view.content}</main></> : <><header className="sticky top-0 z-10 flex shrink-0 items-center gap-2 border-b bg-background p-4"><SidebarTrigger className="-ml-1" /><div className="text-sm font-medium">{view.title}</div><div className="ml-auto flex gap-2">{view.actions}</div></header><main className="mx-auto w-full max-w-3xl p-6">{view.content}</main></>}</SidebarInset>
	</SidebarProvider>;
}

export function ModuleFallback({ title, children }: { title: string; children?: ReactNode }) {
	return { title, list: null, content: children ?? <div className="text-sm text-muted-foreground">正在加载…</div> } satisfies WorkspaceModuleView;
}
