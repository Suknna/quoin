import {
	Bell,
	BookOpen,
	ChevronsUpDown,
	ClipboardCheck,
	GalleryVerticalEnd,
	LayoutDashboard,
	LogOut,
	SearchCheck,
	Settings,
	UserRound,
} from "lucide-react";
import { type CSSProperties, type ReactNode, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import {
	Breadcrumb,
	BreadcrumbItem,
	BreadcrumbList,
	BreadcrumbPage,
	BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Separator } from "@/components/ui/separator";
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

const navigation: { title: string; route: string; icon: typeof Bell; adminOnly?: boolean }[] = [
	{ title: "告警", route: "/alerts", icon: Bell },
	{ title: "调查", route: "/investigations", icon: SearchCheck },
	{ title: "巡检", route: "/inspections", icon: ClipboardCheck },
	{ title: "业务系统", route: "/business-systems", icon: LayoutDashboard },
	{ title: "知识", route: "/knowledge", icon: BookOpen },
	{ title: "管理", route: "/admin", icon: Settings, adminOnly: true },
] as const;

function isCurrent(route: string, destination: string) {
	return route === destination || route.startsWith(`${destination}/`);
}

/** The sole sidebar09-based desktop and mobile workbench frame. */
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
	const account = () => navigate("/account/profile");
	async function logout() {
		setLogoutError("");
		try {
			await onLogout();
		} catch (reason) {
			setLogoutError(reason instanceof Error ? reason.message : "退出登录失败，请重试。");
		}
	}
	return <SidebarProvider style={{ "--sidebar-width": "350px" } as CSSProperties}>
		<Sidebar collapsible="icon" className="overflow-hidden *:data-[sidebar=sidebar]:flex-row">
			<Sidebar collapsible="none" className="w-[calc(var(--sidebar-width-icon)+1px)]! border-r">
				<SidebarHeader><SidebarMenu><SidebarMenuItem><SidebarMenuButton size="lg" onClick={() => navigate("/alerts")} className="md:h-8 md:p-0"><div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground"><GalleryVerticalEnd className="size-4" /></div><div className="grid flex-1 text-left text-sm leading-tight"><span className="truncate font-medium">Quoin</span><span className="truncate text-xs">工作台</span></div></SidebarMenuButton></SidebarMenuItem></SidebarMenu></SidebarHeader>
				<SidebarContent><SidebarGroup><SidebarGroupContent className="px-1.5 md:px-0"><SidebarMenu>{navigation.filter((item) => !item.adminOnly || user.role === "admin").map((item) => <SidebarMenuItem key={item.route}><SidebarMenuButton tooltip={item.title} isActive={isCurrent(route, item.route)} onClick={() => navigate(item.route)} className="px-2.5 md:px-2"><item.icon /><span>{item.title}</span></SidebarMenuButton></SidebarMenuItem>)}</SidebarMenu></SidebarGroupContent></SidebarGroup></SidebarContent>
				<SidebarFooter><SidebarMenu><SidebarMenuItem><DropdownMenu><DropdownMenuTrigger asChild><SidebarMenuButton size="lg" className="data-[state=open]:bg-sidebar-accent md:h-8 md:p-0"><UserRound /><div className="grid flex-1 text-left text-sm leading-tight"><span className="truncate font-medium">{user.displayName}</span><span className="truncate text-xs">{user.role}</span></div><ChevronsUpDown className="ml-auto size-4" /></SidebarMenuButton></DropdownMenuTrigger><DropdownMenuContent side="right" align="end" sideOffset={4} className="min-w-56 rounded-lg"><DropdownMenuLabel>{user.displayName}</DropdownMenuLabel><DropdownMenuSeparator /><DropdownMenuItem onClick={account}><UserRound />账户</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem onClick={() => void logout()}><LogOut />退出登录</DropdownMenuItem></DropdownMenuContent></DropdownMenu></SidebarMenuItem></SidebarMenu></SidebarFooter>
			</Sidebar>
			<Sidebar collapsible="none" className="hidden flex-1 md:flex"><SidebarHeader className="gap-3.5 border-b p-4"><div className="flex w-full items-center justify-between"><div className="text-base font-medium text-foreground">{view.title}</div><span className="text-xs text-muted-foreground">{user.role === "admin" ? "管理员" : "只读"}</span></div></SidebarHeader><SidebarContent>{view.list}</SidebarContent></Sidebar>
		</Sidebar>
		{logoutError && <div className="fixed right-4 bottom-4 z-50"><p role="alert" className="rounded-md border border-destructive bg-background p-3 text-sm text-destructive">{logoutError}</p></div>}
		<SidebarInset><header className="sticky top-0 z-10 flex shrink-0 items-center gap-2 border-b bg-background p-4"><SidebarTrigger className="-ml-1" /><Separator orientation="vertical" className="mr-2 data-[orientation=vertical]:h-4" /><Sheet><SheetTrigger asChild><Button variant="outline" size="sm" className="md:hidden">列表</Button></SheetTrigger><SheetContent side="left"><SheetHeader><SheetTitle>{view.title}</SheetTitle></SheetHeader>{view.list}</SheetContent></Sheet><Breadcrumb><BreadcrumbList><BreadcrumbItem className="hidden md:block"><BreadcrumbPage>Quoin 工作台</BreadcrumbPage></BreadcrumbItem><BreadcrumbSeparator className="hidden md:block" /><BreadcrumbItem><BreadcrumbPage>{view.title}</BreadcrumbPage></BreadcrumbItem></BreadcrumbList></Breadcrumb><div className="ml-auto flex gap-2">{view.actions}</div></header><main className="mx-auto w-full max-w-3xl p-6">{view.content}</main></SidebarInset>
	</SidebarProvider>;
}

export function ModuleFallback({ title, children }: { title: string; children?: ReactNode }) {
	return { title, list: null, content: children ?? <div className="text-sm text-muted-foreground">正在加载…</div> } satisfies WorkspaceModuleView;
}
