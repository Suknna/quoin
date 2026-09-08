import { describe, expect, it, afterEach, vi } from "vitest";
import { legacyHashTarget, migrateLegacyHash, navigateWorkspace, readWorkspaceRoute } from "./router";

afterEach(() => {
	window.history.replaceState(null, "", "/");
	vi.restoreAllMocks();
});

describe("workspace history router", () => {
	it("keeps a deep path and its query string", () => {
		window.history.replaceState(null, "", "/admin/connections/main?tab=probes");
		expect(readWorkspaceRoute()).toEqual({ pathname: "/admin/connections/main", search: "?tab=probes", route: "/admin/connections/main?tab=probes" });
	});

	it("navigates using history rather than a hash", () => {
		navigateWorkspace("/knowledge/articles/123?revision=2");
		expect(window.location.pathname).toBe("/knowledge/articles/123");
		expect(window.location.search).toBe("?revision=2");
		expect(window.location.hash).toBe("");
	});

	it("migrates only recognized legacy connection hashes", () => {
		expect(legacyHashTarget("#connection/models%2Fmain")).toBe("/admin/connections/models%2Fmain");
		expect(legacyHashTarget("#new")).toBe("/admin/connections/new");
		expect(legacyHashTarget("#alerts")).toBeUndefined();
		window.location.hash = "connection/main";
		expect(migrateLegacyHash()).toBe(true);
		expect(window.location.pathname).toBe("/admin/connections/main");
		expect(window.location.hash).toBe("");
	});
});
