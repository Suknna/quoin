import { describe, expect, it, afterEach, vi } from "vitest";
import { consolidatedRouteTarget, legacyHashTarget, migrateLegacyHash, navigateWorkspace, readWorkspaceRoute } from "./router";

afterEach(() => {
	window.history.replaceState(null, "", "/");
	vi.restoreAllMocks();
});

describe("workspace history router", () => {
	it("keeps a deep path and its query string", () => {
		window.history.replaceState(null, "", "/admin/model_provider/main?tab=probes");
		expect(readWorkspaceRoute()).toEqual({ pathname: "/admin/model_provider/main", search: "?tab=probes", route: "/admin/model_provider/main?tab=probes" });
	});

	it("navigates using history rather than a hash", () => {
		navigateWorkspace("/knowledge/articles/123?revision=2");
		expect(window.location.pathname).toBe("/knowledge/articles/123");
		expect(window.location.search).toBe("?revision=2");
		expect(window.location.hash).toBe("");
	});

	it("redirects retired administration management routes to their consolidated owners", () => {
		expect(consolidatedRouteTarget("/admin/connections/main")).toBe("/integrations/instances");
		expect(consolidatedRouteTarget("/admin/alerts")).toBe("/integrations/alertmanager");
		expect(consolidatedRouteTarget("/admin/alert-intake-issues")).toBe("/integrations/alertmanager/issues");
		expect(consolidatedRouteTarget("/admin/labels")).toBe("/admin");
		navigateWorkspace("/admin/connections/main?tab=probes");
		expect(window.location.pathname).toBe("/integrations/instances");
		expect(window.location.search).toBe("?tab=probes");
	});

	it("migrates only recognized legacy connection hashes to integrations", () => {
		expect(legacyHashTarget("#connection/models%2Fmain")).toBe("/integrations/instances");
		expect(legacyHashTarget("#new")).toBe("/integrations/instances");
		expect(legacyHashTarget("#alerts")).toBeUndefined();
		window.location.hash = "connection/main";
		expect(migrateLegacyHash()).toBe(true);
		expect(window.location.pathname).toBe("/integrations/instances");
		expect(window.location.hash).toBe("");
	});
});
