import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PluginConfiguration } from "./plugin-config";

const navigate = vi.fn();

function PluginView({ platform }: { platform: "browser" }) {
	return (
		<PluginConfiguration
			platform={platform}
			navigate={navigate}
			suspended={false}
		/>
	);
}

afterEach(() => {
	cleanup();
	navigate.mockClear();
	vi.restoreAllMocks();
});

/** Routes every request the slice can issue; unexpected URLs fail loudly.
 * String patterns match by prefix so query strings are covered; order specific
 * patterns before generic ones. An optional third tuple entry constrains the
 * HTTP method for URLs that carry both reads and writes. */
function mockFetch(routes: Array<[string | RegExp, unknown] | [string | RegExp, unknown, string]>) {
	return vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
		const url = String(input);
		const method = ((init as RequestInit | undefined)?.method ?? "GET").toUpperCase();
		for (const [pattern, body, routeMethod] of routes) {
			if (routeMethod && routeMethod.toUpperCase() !== method) continue;
			const matched =
				typeof pattern === "string"
					? url === pattern || url.startsWith(pattern)
					: pattern.test(url);
			if (matched) return Response.json(body as Record<string, unknown>);
		}
		return Response.json(
			{ message: `unexpected request: ${url}` },
			{ status: 500 },
		);
	});
}

describe("browser plugin configuration", () => {
	it("renders no entry while the browser plugin is disabled", async () => {
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: false, version: "1", capabilities: ["tools"] }] }],
		]);
		render(<PluginView platform="browser" />);
		expect(await screen.findByText("浏览器接入未启用")).toBeInTheDocument();
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).includes("/business-systems"),
			),
		).toBe(false);
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).includes("/journey-catalog"),
			),
		).toBe(false);
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).includes("/browser-identities"),
			),
		).toBe(false);
		expect(
			screen.queryByRole("button", { name: "开始人工登录" }),
		).not.toBeInTheDocument();
	});

	it("shows the real Journey catalog and saved identities without offering business-bound creation", async () => {
		mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {
				"login-check": { purpose: "authentication_probe", version: 2, summary: "登录探测", params_schema: { properties: { baseUrl: { type: "string" } } } },
				"health-check": { purpose: "health", version: 1, summary: "健康检查" },
			} } }],
			["/api/v1/browser-identities", { items: [] }],
			["/api/v1/business-systems", { items: [
				{ key: "checkout", displayName: "Checkout", enabled: true, rowVersion: 1, browserIdentityState: "Ready" },
				{ key: "other", displayName: "Other", enabled: true, rowVersion: 1, browserIdentityState: "none" },
			] }],
		]);
		render(<PluginView platform="browser" />);
		expect(await screen.findByText("login-check")).toBeInTheDocument();
		expect(screen.queryByText("health-check")).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: /Checkout/ })).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: /Other/ }),
		).not.toBeInTheDocument();
		// Creation exists, but only as a business-independent identity; it stays
		// disabled until the required fields are filled.
		expect(screen.getByRole("button", { name: "创建身份" })).toBeDisabled();
	});

	it("renders legacy business-referenced identities read-only with the migration state and no browser-login writes", async () => {
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {} } }],
			["/api/v1/browser-identities", { items: [] }],
			["/api/v1/business-systems/checkout/browser-identity", {
				id: "identity-1",
				state: "Ready",
				rowVersion: 3,
				currentRevision: { id: "rev-1", revision: 2, name: "结算台登录", startUrl: "https://target.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: {} }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
				currentProfile: { id: "profile-1", generation: 1, chromiumRevision: "140", publishedAt: "2026-09-13T00:00:00Z" },
				lastProbe: null,
				currentOperation: null,
			}],
			["/api/v1/business-systems", { items: [{ key: "checkout", displayName: "Checkout", enabled: true, rowVersion: 1, browserIdentityState: "Ready" }] }],
		]);
		render(<PluginView platform="browser" />);
		fireEvent.click(await screen.findByRole("button", { name: /Checkout/ }));
		expect(await screen.findByText("结算台登录")).toBeInTheDocument();
		// The historical binding stays readable but carries an explicit
		// migration state: no login/publish/cancel writes from here.
		expect(screen.getByText(/仅可查看/)).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "开始人工登录" }),
		).not.toBeInTheDocument();
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).includes("/browser-login"),
			),
		).toBe(false);
	});

	it("edits a standalone identity revision with row-version fencing through the shared catalog form", async () => {
		// id and identityKey are deliberately distinct: every identity-scoped
		// route must use the stable identityKey, never the numeric id.
		const identity = {
			id: "42",
			identityKey: "mall-login",
			state: "AuthenticationRequired",
			rowVersion: 3,
			currentRevision: { id: "rev-1", revision: 1, name: "运营台", startUrl: "https://ops.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: {} }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
			currentProfile: null,
			lastProbe: null,
			currentOperation: null,
		};
		const updated = {
			...identity,
			state: "Ready",
			rowVersion: 4,
			currentRevision: { ...identity.currentRevision, id: "rev-2", revision: 2, name: "运营台-v2" },
		};
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {
				"login-check": { purpose: "authentication_probe", version: 2, summary: "登录探测", params_schema: { properties: { loginPath: { type: "string", title: "登录路径" } } } },
			} } }],
			["/api/v1/business-systems", { items: [] }],
			["/api/v1/browser-identities/mall-login", updated, "PUT"],
			["/api/v1/browser-identities/mall-login", identity, "GET"],
			["/api/v1/browser-identities", { items: [identity] }, "GET"],
		]);
		render(<PluginView platform="browser" />);
		fireEvent.click(await screen.findByRole("button", { name: /运营台/ }));
		fireEvent.click(
			await screen.findByRole("button", { name: "编辑修订" }),
		);
		// The edit form starts from the frozen revision values; the create form
		// coexists on the page, so queries are scoped to the identity panel.
		const panel = within(
			await screen.findByRole("region", { name: "浏览器身份：mall-login" }),
		);
		expect(panel.getByLabelText("名称")).toHaveValue("运营台");
		expect(panel.getByLabelText("起始 URL")).toHaveValue("https://ops.example");
		fireEvent.change(panel.getByLabelText("名称"), { target: { value: "运营台-v2" } });
		fireEvent.click(panel.getByRole("button", { name: "保存新修订" }));
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(
					([url, init]) =>
						String(url) === "/api/v1/browser-identities/mall-login" &&
						(init as RequestInit | undefined)?.method === "PUT",
				),
			).toBe(true),
		);
		const putPayload = JSON.parse(
			String(
				fetchMock.mock.calls.find(
					([url, init]) =>
						String(url) === "/api/v1/browser-identities/mall-login" &&
						(init as RequestInit | undefined)?.method === "PUT",
				)?.[1]?.body,
			),
		);
		expect(putPayload).toMatchObject({
			name: "运营台-v2",
			startUrl: "https://ops.example",
			authenticationProbe: {
				journeyId: "login-check",
				journeyVersion: 2,
				params: { loginPath: "" },
			},
			expectedRowVersion: 3,
		});
		// The saved revision is displayed in the panel and propagated to the list.
		expect(await panel.findByText("运营台-v2")).toBeInTheDocument();
		expect(screen.getAllByText("运营台-v2").length).toBeGreaterThan(1);
	});

	it("creates a standalone identity from the frozen catalog journey without any business binding", async () => {
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {
				"login-check": { purpose: "authentication_probe", version: 2, summary: "登录探测", params_schema: { properties: { loginPath: { type: "string", title: "登录路径" } } } },
			} } }],
			["/api/v1/business-systems", { items: [] }],
			["/api/v1/browser-identities/mall-login", {
				id: "42",
				identityKey: "mall-login",
				state: "AuthenticationRequired",
				rowVersion: 1,
				currentRevision: { id: "rev-1", revision: 1, name: "运营台", startUrl: "https://ops.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: { loginPath: "/login" } }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
				currentProfile: null,
				lastProbe: null,
				currentOperation: null,
			}, "GET"],
			["/api/v1/browser-identities", {
				id: "42",
				identityKey: "mall-login",
				state: "AuthenticationRequired",
				rowVersion: 1,
				currentRevision: { id: "rev-1", revision: 1, name: "运营台", startUrl: "https://ops.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: { loginPath: "/login" } }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
				currentProfile: null,
				lastProbe: null,
				currentOperation: null,
			}, "POST"],
			["/api/v1/browser-identities", { items: [] }, "GET"],
		]);
		render(<PluginView platform="browser" />);
		await screen.findByText("创建身份");
		fireEvent.change(screen.getByLabelText("名称"), { target: { value: "运营台" } });
		fireEvent.change(screen.getByLabelText("起始 URL"), { target: { value: "https://ops.example" } });
		fireEvent.change(screen.getByLabelText("登录路径"), { target: { value: "/login" } });
		fireEvent.click(screen.getByRole("button", { name: "创建身份" }));
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(
					([url, init]) =>
						String(url) === "/api/v1/browser-identities" &&
						(init as RequestInit | undefined)?.method === "POST",
				),
			).toBe(true),
		);
		const created = JSON.parse(
			String(
				fetchMock.mock.calls.find(
					([url, init]) =>
						String(url) === "/api/v1/browser-identities" &&
						(init as RequestInit | undefined)?.method === "POST",
				)?.[1]?.body,
			),
		);
		expect(created).toMatchObject({
			name: "运营台",
			startUrl: "https://ops.example",
			authenticationProbe: {
				journeyId: "login-check",
				journeyVersion: 2,
				params: { loginPath: "/login" },
			},
		});
		// The created identity is selected and read back through its stable
		// identityKey — never through the numeric id.
		expect(await screen.findByRole("region", { name: "浏览器身份：mall-login" })).toBeInTheDocument();
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url) === "/api/v1/browser-identities/mall-login",
			),
		).toBe(true);
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url) === "/api/v1/browser-identities/42",
			),
		).toBe(false);
	});

	it("starts and cancels a manual login for a standalone identity on the identity-scoped operations path", async () => {
		const identity = {
			id: "42",
			identityKey: "mall-login",
			state: "AuthenticationRequired",
			rowVersion: 3,
			currentRevision: { id: "rev-1", revision: 1, name: "运营台", startUrl: "https://ops.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: {} }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
			currentProfile: null,
			lastProbe: null,
			currentOperation: null,
		};
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {} } }],
			["/api/v1/business-systems", { items: [] }],
			["/api/v1/browser-identities/mall-login/operations/op-1", { id: "op-1", identityId: "42", identityRevisionId: "rev-1", kind: "manual_login", state: "Running", rowVersion: 2, requestedAt: "2026-09-13T00:00:00Z", canAttach: false, canPublish: false, canCancel: true }, "GET"],
			["/api/v1/browser-identities/mall-login", identity, "GET"],
			["/api/v1/browser-identities/mall-login/operations/op-1/cancel", { id: "op-1", identityId: "42", identityRevisionId: "rev-1", kind: "manual_login", state: "Cancelled", rowVersion: 3, requestedAt: "2026-09-13T00:00:00Z", canAttach: false, canPublish: false, canCancel: false }],
			["/api/v1/browser-identities/mall-login/operations", { id: "op-1", identityId: "42", identityRevisionId: "rev-1", kind: "manual_login", state: "Running", rowVersion: 1, requestedAt: "2026-09-13T00:00:00Z", canAttach: false, canPublish: false, canCancel: true }, "POST"],
			["/api/v1/browser-identities", { items: [identity] }, "GET"],
		]);
		render(<PluginView platform="browser" />);
		fireEvent.click(await screen.findByRole("button", { name: /运营台/ }));
		expect(await screen.findByRole("button", { name: "开始人工登录" })).toBeEnabled();
		fireEvent.click(screen.getByRole("button", { name: "开始人工登录" }));
		await waitFor(() =>
			expect(screen.getAllByText(/Running/).length).toBeGreaterThan(0),
		);
		fireEvent.click(screen.getByRole("button", { name: "取消" }));
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(([url]) =>
					String(url).endsWith(
						"/api/v1/browser-identities/mall-login/operations/op-1/cancel",
					),
				),
			).toBe(true),
		);
		const startPayload = JSON.parse(
			String(
				fetchMock.mock.calls.find(
					([url, init]) =>
						String(url) ===
							"/api/v1/browser-identities/mall-login/operations" &&
						(init as RequestInit).method === "POST",
				)?.[1]?.body,
			),
		);
		expect(startPayload).toMatchObject({ expectedRowVersion: 3 });
		// A cancelled operation is terminal: the panel offers login again.
		expect(
			await screen.findByRole("button", { name: "开始人工登录" }),
		).toBeEnabled();
		expect(screen.queryByRole("button", { name: "取消" })).not.toBeInTheDocument();
	});

	it("fails loudly when a standalone identity has no identityKey instead of falling back to the numeric id", async () => {
		const fetchMock = mockFetch([
			["/api/v1/integrations/plugins", { items: [{ id: "browser", displayName: "受控浏览器", description: "可选浏览器", enabled: true, version: "1", capabilities: ["tools"] }] }],
			["/api/v1/journey-catalog", { version: "1", digest: "digest-1", catalogJson: { journeys: {} } }],
			["/api/v1/business-systems", { items: [] }],
			["/api/v1/browser-identities", { items: [{
				id: "42",
				state: "Ready",
				rowVersion: 1,
				currentRevision: { id: "rev-1", revision: 1, name: "缺 key", startUrl: "https://ops.example", authenticationProbe: { journeyId: "login-check", journeyVersion: 2, params: {} }, catalogDigest: "digest-1", catalogVersion: "1", createdAt: "2026-09-13T00:00:00Z" },
				currentProfile: null,
				lastProbe: null,
				currentOperation: null,
			}] }],
		]);
		render(<PluginView platform="browser" />);
		expect(await screen.findByText(/缺少 identityKey/)).toBeInTheDocument();
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).startsWith("/api/v1/browser-identities/42"),
			),
		).toBe(false);
	});
});
