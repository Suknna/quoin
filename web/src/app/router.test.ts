import { afterEach, describe, expect, it, vi } from "vitest";
import {
	consolidatedRouteTarget,
	legacyHashTarget,
	migrateLegacyHash,
	navigateWorkspace,
	readWorkspaceRoute,
} from "./router";

afterEach(() => {
	window.history.replaceState(null, "", "/");
	vi.restoreAllMocks();
});

describe("workspace history router", () => {
	it("keeps a deep path and its query string", () => {
		window.history.replaceState(
			null,
			"",
			"/admin/model_provider/main?tab=probes",
		);
		expect(readWorkspaceRoute()).toEqual({
			pathname: "/admin/model_provider/main",
			search: "?tab=probes",
			route: "/admin/model_provider/main?tab=probes",
		});
	});

	it("navigates using history rather than a hash", () => {
		navigateWorkspace("/knowledge/articles/123?revision=2");
		expect(window.location.pathname).toBe("/knowledge/articles/123");
		expect(window.location.search).toBe("?revision=2");
		expect(window.location.hash).toBe("");
	});

	it("redirects retired administration management routes to their consolidated owners", () => {
		expect(consolidatedRouteTarget("/admin/connections/main")).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(consolidatedRouteTarget("/admin/alerts")).toBe(
			"/settings/platform/integrations/alertmanager",
		);
		expect(consolidatedRouteTarget("/admin/alert-intake-issues")).toBe(
			"/settings/platform/integrations/alertmanager/issues",
		);
		expect(consolidatedRouteTarget("/admin/labels")).toBe(
			"/settings/platform/users",
		);
		navigateWorkspace("/admin/connections/main?tab=probes");
		expect(window.location.pathname).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(window.location.search).toBe("?tab=probes");
	});

	it("redirects retired /admin module routes into the platform settings group", () => {
		expect(consolidatedRouteTarget("/admin")).toBe("/settings/platform/users");
		expect(consolidatedRouteTarget("/admin/users")).toBe(
			"/settings/platform/users",
		);
		expect(consolidatedRouteTarget("/admin/about")).toBe(
			"/settings/platform/about",
		);
		expect(consolidatedRouteTarget("/admin/backups")).toBe(
			"/settings/platform/backups",
		);
		expect(consolidatedRouteTarget("/admin/audit")).toBe(
			"/settings/platform/audit",
		);
		expect(consolidatedRouteTarget("/admin/model_provider")).toBe(
			"/settings/platform/model-providers",
		);
		expect(consolidatedRouteTarget("/admin/model_provider/models%2Fmain")).toBe(
			"/settings/platform/model-providers/models%2Fmain",
		);
		expect(consolidatedRouteTarget("/settings/platform/users")).toBeUndefined();
		navigateWorkspace("/admin/model_provider/new");
		expect(window.location.pathname).toBe(
			"/settings/platform/model-providers/new",
		);
	});

	it("redirects retired integrations routes into the platform access group", () => {
		expect(consolidatedRouteTarget("/integrations")).toBe(
			"/settings/platform/integrations",
		);
		expect(consolidatedRouteTarget("/integrations/instances")).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(consolidatedRouteTarget("/integrations/alertmanager/issues")).toBe(
			"/settings/platform/integrations/alertmanager/issues",
		);
		expect(
			consolidatedRouteTarget("/settings/platform/integrations"),
		).toBeUndefined();
		navigateWorkspace("/integrations/instances");
		expect(window.location.pathname).toBe(
			"/settings/platform/integrations/instances",
		);
	});

	it("keeps the static alertmanager issues page on its own route instead of an instance drawer", () => {
		// 验收回归 fix16：直达 /alertmanager/issues 曾被实例归并规则当成实例名
		// "issues"，重定向到 instances 抽屉并报“该实例不存在”。
		expect(
			consolidatedRouteTarget("/settings/platform/integrations/alertmanager/issues"),
		).toBeUndefined();
		window.history.replaceState(
			null,
			"",
			"/settings/platform/integrations/alertmanager/issues",
		);
		navigateWorkspace("/settings/platform/integrations/alertmanager/issues");
		expect(window.location.pathname).toBe(
			"/settings/platform/integrations/alertmanager/issues",
		);
		expect(window.location.search).toBe("");
	});

	it("still folds deep-linked instance names into the instances drawer", () => {
		expect(
			consolidatedRouteTarget("/settings/platform/integrations/alertmanager/prod-cluster"),
		).toBe(
			"/settings/platform/integrations/instances?platform=alertmanager&instance=prod-cluster",
		);
		expect(
			consolidatedRouteTarget(
				"/settings/platform/integrations/prometheus/prom-main",
			),
		).toBe(
			"/settings/platform/integrations/instances?platform=prometheus&instance=prom-main",
		);
		expect(
			consolidatedRouteTarget("/settings/platform/integrations/thanos/edge-01"),
		).toBe(
			"/settings/platform/integrations/instances?platform=thanos&instance=edge-01",
		);
	});

	it("redirects retired account routes into the settings module", () => {
		expect(consolidatedRouteTarget("/account")).toBe("/settings/profile");
		expect(consolidatedRouteTarget("/account/profile")).toBe(
			"/settings/profile",
		);
		expect(consolidatedRouteTarget("/account/contacts")).toBe(
			"/settings/profile",
		);
		expect(consolidatedRouteTarget("/account/security")).toBe(
			"/settings/security",
		);
		expect(consolidatedRouteTarget("/account/audit")).toBe("/settings/profile");
		expect(consolidatedRouteTarget("/settings/profile")).toBeUndefined();
		navigateWorkspace("/account/security");
		expect(window.location.pathname).toBe("/settings/security");
	});

	it("migrates only recognized legacy connection hashes to integrations", () => {
		expect(legacyHashTarget("#connection/models%2Fmain")).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(legacyHashTarget("#new")).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(legacyHashTarget("#alerts")).toBeUndefined();
		window.location.hash = "connection/main";
		expect(migrateLegacyHash()).toBe(true);
		expect(window.location.pathname).toBe(
			"/settings/platform/integrations/instances",
		);
		expect(window.location.hash).toBe("");
	});
});
