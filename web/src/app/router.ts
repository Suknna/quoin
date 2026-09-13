export interface WorkspaceRoute {
	pathname: string;
	search: string;
	/** A normalized path used by module contracts. */
	route: string;
}

const legacyConnectionPrefix = "/admin/connections";

/**
 * Consolidation keeps saved links useful without leaving a second management
 * surface alive. Settings owns only model providers; operational integrations
 * own metrics and Alertmanager lifecycle.
 */
export function consolidatedRouteTarget(pathname: string): string | undefined {
	if (pathname === legacyConnectionPrefix || pathname.startsWith(`${legacyConnectionPrefix}/`))
		return "/integrations/instances";
	if (pathname === "/admin/alerts") return "/integrations/alertmanager";
	if (pathname === "/admin/alert-intake-issues")
		return "/integrations/alertmanager/issues";
	if (pathname === "/admin/labels") return "/admin";
	return undefined;
}

/**
 * The workbench uses the browser history API so a reload retains the selected
 * resource. Hash links from the previous connection-only workbench are
 * translated once, then removed rather than becoming a second router.
 */
export function legacyHashTarget(hash: string): string | undefined {
	const value = hash.replace(/^#/, "");
	if (value === "new" || value.startsWith("connection/"))
		return "/integrations/instances";
	return undefined;
}

export function normalizeWorkspacePath(pathname: string): string {
	if (!pathname || pathname === "/") return "/alerts/list";
	return pathname.startsWith("/") ? pathname : `/${pathname}`;
}

export function readWorkspaceRoute(location: Pick<Location, "pathname" | "search"> = window.location): WorkspaceRoute {
	const pathname = normalizeWorkspacePath(location.pathname);
	return { pathname, search: location.search, route: `${pathname}${location.search}` };
}

export function navigateWorkspace(to: string, replace = false): WorkspaceRoute {
	const target = new URL(to, window.location.origin);
	const pathname = normalizeWorkspacePath(target.pathname);
	const destination = consolidatedRouteTarget(pathname) ?? pathname;
	window.history[replace ? "replaceState" : "pushState"](null, "", `${destination}${target.search}`);
	window.dispatchEvent(new PopStateEvent("popstate"));
	return readWorkspaceRoute();
}

export function migrateLegacyHash(): boolean {
	const target = legacyHashTarget(window.location.hash);
	if (!target) return false;
	navigateWorkspace(target, true);
	return true;
}

export function isConnectionRoute(pathname: string): boolean {
	return pathname === legacyConnectionPrefix || pathname.startsWith(`${legacyConnectionPrefix}/`);
}
