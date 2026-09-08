export interface WorkspaceRoute {
	pathname: string;
	search: string;
	/** A normalized path used by module contracts. */
	route: string;
}

const connectionPrefix = "/admin/connections";

/**
 * The workbench uses the browser history API so a reload retains the selected
 * resource. Hash links from the previous connection-only workbench are
 * translated once, then removed rather than becoming a second router.
 */
export function legacyHashTarget(hash: string): string | undefined {
	const value = hash.replace(/^#/, "");
	if (value === "new") return `${connectionPrefix}/new`;
	if (value.startsWith("connection/")) {
		return `${connectionPrefix}/${value.slice("connection/".length)}`;
	}
	return undefined;
}

export function normalizeWorkspacePath(pathname: string): string {
	if (!pathname || pathname === "/") return "/alerts";
	return pathname.startsWith("/") ? pathname : `/${pathname}`;
}

export function readWorkspaceRoute(location: Pick<Location, "pathname" | "search"> = window.location): WorkspaceRoute {
	const pathname = normalizeWorkspacePath(location.pathname);
	return { pathname, search: location.search, route: `${pathname}${location.search}` };
}

export function navigateWorkspace(to: string, replace = false): WorkspaceRoute {
	const target = new URL(to, window.location.origin);
	const pathname = normalizeWorkspacePath(target.pathname);
	window.history[replace ? "replaceState" : "pushState"](null, "", `${pathname}${target.search}`);
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
	return pathname === connectionPrefix || pathname.startsWith(`${connectionPrefix}/`);
}
