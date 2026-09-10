import { type ComponentType, type LazyExoticComponent } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import type { WorkspaceModuleProps } from "../module-contract";
import { WorkspaceShell } from "../WorkspaceShell";

export type RouteHostProps = {
	props: WorkspaceModuleProps;
	onLogout: () => Promise<void>;
};

type RouteHostComponent = ComponentType<RouteHostProps>;
export type RouteHostComponents = Record<string, LazyExoticComponent<RouteHostComponent>>;

function NotFoundRoute({ props, onLogout }: RouteHostProps) {
	const view = {
		title: "找不到页面",
		list: null,
		content: <Empty><EmptyHeader><EmptyTitle>找不到此页面</EmptyTitle><EmptyDescription>该链接无效或页面已被移动。</EmptyDescription></EmptyHeader></Empty>,
	};
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}

/** Client guard mirrors the server boundary and gives an ordinary recovery path for direct links. */
function ForbiddenRoute({ props, onLogout }: RouteHostProps) {
	const view = {
		title: "访问受限",
		list: null,
		content: <Alert variant="destructive"><AlertTitle>无权访问此页面</AlertTitle><AlertDescription>此管理功能仅向管理员开放。</AlertDescription><Button className="mt-3" size="sm" variant="outline" onClick={() => props.navigate("/alerts/list")}>返回告警列表</Button></Alert>,
	};
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}

/** Selects an independently lazy-loaded host without statically importing any feature module. */
export function RouteHosts({ components, props, onLogout }: { components: RouteHostComponents; props: WorkspaceModuleProps; onLogout: () => Promise<void> }) {
	// Module selection is pathname-only; the query belongs to the selected module's view state.
	const pathname = new URL(props.route, "https://workbench.invalid").pathname;
	const routeIsConnections = pathname === "/admin/connections" || pathname.startsWith("/admin/connections/");
	const operatorManagementRoute = pathname.startsWith("/integrations") || pathname.startsWith("/business-systems") || pathname.startsWith("/inspections") || pathname.startsWith("/admin") || pathname.startsWith("/audit") || pathname.startsWith("/browser-login");
	if (props.user.role === "operator" && operatorManagementRoute) return <ForbiddenRoute props={props} onLogout={onLogout} />;
	const Host = routeIsConnections ? components.connections
		: pathname === "/alerts" || pathname.startsWith("/alerts/") || pathname === "/postmortems" ? components.alerts
		: pathname.startsWith("/investigations") ? components.investigations
		: pathname.startsWith("/inspections") ? components.inspections
		: pathname === "/integrations" || pathname.startsWith("/integrations/") ? components.integrations
		: pathname.startsWith("/business-systems") ? components.systems
		: pathname.startsWith("/knowledge") ? components.knowledge
		: pathname.startsWith("/account") ? components.account
		: pathname === "/admin" || pathname.startsWith("/admin/") ? components.administration
		: undefined;
	return Host ? <Host props={props} onLogout={onLogout} /> : <NotFoundRoute props={props} onLogout={onLogout} />;
}
