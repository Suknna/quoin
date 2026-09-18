export interface WorkspaceRoute {
	pathname: string;
	search: string;
	/** A normalized path used by module contracts. */
	route: string;
}

const legacyConnectionPrefix = "/admin/connections";
const integrationsBase = "/settings/platform/integrations";

/**
 * Consolidation keeps saved links useful without leaving a second management
 * surface alive. Settings owns personal and platform configuration; the
 * platform-access group (指标接入、Alertmanager) lives under it and keeps
 * room to grow.
 */
export function consolidatedRouteTarget(pathname: string): string | undefined {
	if (
		pathname === legacyConnectionPrefix ||
		pathname.startsWith(`${legacyConnectionPrefix}/`)
	)
		return `${integrationsBase}/instances`;
	if (pathname === "/admin/alerts") return `${integrationsBase}/alertmanager`;
	if (pathname === "/admin/alert-intake-issues")
		return `${integrationsBase}/alertmanager/issues`;
	if (
		pathname === "/admin/model_provider" ||
		pathname.startsWith("/admin/model_provider/")
	)
		return `/settings/platform/model-providers${pathname.slice("/admin/model_provider".length)}`;
	if (pathname === "/admin" || pathname === "/admin/users")
		return "/settings/platform/users";
	if (pathname === "/admin/about") return "/settings/platform/about";
	if (pathname === "/admin/backups") return "/settings/platform/backups";
	if (pathname === "/admin/audit") return "/settings/platform/audit";
	if (pathname === "/admin/runtime") return "/settings/platform/runtime";
	if (pathname === "/admin/labels") return "/settings/platform/users";
	if (pathname === "/integrations" || pathname.startsWith("/integrations/"))
		return `${integrationsBase}${pathname.slice("/integrations".length)}`;
	if (
		pathname === "/account" ||
		pathname === "/account/profile" ||
		pathname === "/account/contacts"
	)
		return "/settings/profile";
	if (pathname === "/account/security") return "/settings/security";
	if (pathname.startsWith("/account/")) return "/settings/profile";
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
		return `${integrationsBase}/instances`;
	return undefined;
}

export function normalizeWorkspacePath(pathname: string): string {
	if (!pathname || pathname === "/") return "/alerts/list";
	return pathname.startsWith("/") ? pathname : `/${pathname}`;
}

export function readWorkspaceRoute(
	location: Pick<Location, "pathname" | "search"> = window.location,
): WorkspaceRoute {
	const pathname = normalizeWorkspacePath(location.pathname);
	return {
		pathname,
		search: location.search,
		route: `${pathname}${location.search}`,
	};
}

export function navigateWorkspace(to: string, replace = false): WorkspaceRoute {
	const target = new URL(to, window.location.origin);
	const pathname = normalizeWorkspacePath(target.pathname);
	const destination = consolidatedRouteTarget(pathname) ?? pathname;
	window.history[replace ? "replaceState" : "pushState"](
		null,
		"",
		`${destination}${target.search}`,
	);
	window.dispatchEvent(new PopStateEvent("popstate"));
	return readWorkspaceRoute();
}

export function migrateLegacyHash(): boolean {
	const target = legacyHashTarget(window.location.hash);
	if (!target) return false;
	navigateWorkspace(target, true);
	return true;
}
