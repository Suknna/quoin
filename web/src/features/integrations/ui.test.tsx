import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import { alertmanagerReceiverYaml } from "./api";
import { useIntegrationsModule } from "./ui";

async function pickBearerAuth() {
	// Radix Select opens from the keyboard in jsdom; pointer events need APIs
	// the environment does not implement.
	fireEvent.keyDown(screen.getByRole("combobox", { name: "认证方式" }), {
		key: "ArrowDown",
	});
	fireEvent.click(await screen.findByRole("option", { name: "Bearer Token" }));
}

const props: WorkspaceModuleProps = {
	user: {
		id: "admin-1",
		username: "admin",
		displayName: "Admin",
		role: "admin",
		passwordChangeRequired: false,
		authRevision: 1,
		enabled: true,
		initialized: true,
		lastLoginAt: null,
		rowVersion: 1,
	},
	route: "/settings/platform/integrations",
	navigate: vi.fn(),
	suspended: false,
	openEvidence: vi.fn(),
};

function IntegrationView({
	route = "/settings/platform/integrations",
}: {
	route?: string;
}) {
	const view = useIntegrationsModule({ ...props, route });
	return <>{view.content}</>;
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	(props.navigate as ReturnType<typeof vi.fn>).mockClear();
});

describe("integration workbench", () => {
	it("renders the server plugin catalog without exposing unknown plugin capabilities", async () => {
		const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
			Response.json({
				items: [
					{
						id: "prometheus",
						displayName: "实验指标",
						description: "自动观测监控目标",
						enabled: true,
						version: "1",
						capabilities: ["discover", "tools"],
					},
					{
						id: "unknown",
						displayName: "未知插件",
						description: "服务端新增的未知插件",
						enabled: false,
						version: "1",
						capabilities: ["tools"],
					},
				],
			}),
		);
		render(<IntegrationView />);
		expect(
			await screen.findByRole("button", { name: "配置 实验指标" }),
		).toBeEnabled();
		expect(fetchMock).toHaveBeenCalledWith(
			"/api/v1/integrations/plugins",
			expect.anything(),
		);
		expect(screen.queryByText("未知插件")).not.toBeInTheDocument();
		expect(screen.queryByText("Thanos")).not.toBeInTheDocument();
	});

	it("filters the catalog by platform name", async () => {
		vi.spyOn(globalThis, "fetch").mockResolvedValue(
			Response.json({
				items: [
					{
						id: "thanos",
						displayName: "Thanos",
						description: "指标",
						enabled: true,
						version: "1",
						capabilities: ["discover"],
					},
					{
						id: "alertmanager",
						displayName: "Alertmanager",
						description: "告警",
						enabled: true,
						version: "1",
						capabilities: [],
					},
				],
			}),
		);
		render(<IntegrationView />);
		await screen.findByText("Thanos");
		fireEvent.change(screen.getByRole("textbox", { name: "搜索支持的平台" }), {
			target: { value: "Thanos" },
		});
		expect(screen.getByText("Thanos")).toBeInTheDocument();
		expect(screen.queryByText("Alertmanager")).not.toBeInTheDocument();
	});

	it("shows denied content instead of a management view to an operator", () => {
		vi.spyOn(globalThis, "fetch");
		function OperatorView() {
			const view = useIntegrationsModule({
				...props,
				user: { ...props.user, role: "operator" },
			});
			return <>{view.content}</>;
		}
		render(<OperatorView />);
		expect(screen.getByRole("alert")).toHaveTextContent(
			"接入管理仅向管理员开放",
		);
	});

	it("generates a valid Alertmanager receiver YAML snippet", () => {
		const yaml = alertmanagerReceiverYaml(
			"https://quoin.example.test/api/v1/alert-receiver",
			"secret-token",
		);
		expect(yaml).toContain("webhook_configs:");
		expect(yaml).toContain("send_resolved: true");
		expect(yaml).toContain("type: Bearer");
		expect(yaml).toContain('credentials: "secret-token"');
	});

	it("shows alert intake issues in the Alertmanager operations route", async () => {
		vi.spyOn(globalThis, "fetch").mockResolvedValue(
			Response.json({
				items: [
					{
						id: "issue-1",
						kind: "identity_conflict",
						issueKey: "receiver",
						occurrenceCount: 2,
						rowVersion: 1,
					},
				],
			}),
		);
		render(
			<IntegrationView route="/settings/platform/integrations/alertmanager/issues" />,
		);
		expect(
			await screen.findByRole("heading", { name: "告警接入问题" }),
		).toBeInTheDocument();
		expect(screen.getByText(/identity_conflict/)).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "确认" })).toBeEnabled();
	});

	it("submits the selected Prometheus authentication fields and clears its secret inputs", async () => {
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/connections"))
					return Response.json(
						{
							id: "42",
							name: "prom-main",
							type: "prometheus",
							enabled: false,
							rowVersion: 1,
							config: {
								type: "prometheus",
								baseUrl: "https://metrics.example",
								authType: "bearer",
							},
						},
						{ status: 201 },
					);
				if (url.endsWith("/probe"))
					return Response.json(
						{ id: "probe-1", state: "Succeeded" },
						{ status: 201 },
					);
				if (url.endsWith("/probe-attempts/probe-1"))
					return Response.json({
						id: "probe-1",
						state: "Succeeded",
						endedAt: "2026-09-10T00:00:00Z",
					});
				if (url.includes("/probe-results"))
					return Response.json({
						items: [
							{ id: "result-1", attemptId: "probe-1", outcome: "passed" },
						],
					});
				if (url.endsWith("/enable"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: true,
						rowVersion: 2,
						config: {
							type: "prometheus",
							baseUrl: "https://metrics.example",
							authType: "bearer",
						},
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/prometheus" />,
		);
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "prom-main" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://metrics.example" },
		});
		await pickBearerAuth();
		fireEvent.change(screen.getByLabelText("Bearer Token"), {
			target: { value: "never-return-this" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations/prometheus/prom-main",
			),
		);
		const payload = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/api/v1/connections"),
				)?.[1]?.body,
			),
		);
		expect(payload.connection).toMatchObject({
			type: "prometheus",
			authType: "bearer",
			bearerToken: "never-return-this",
		});
		const enablePayload = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/enable"),
				)?.[1]?.body,
			),
		);
		expect(enablePayload).toMatchObject({ qualifiedProbeResultId: "result-1" });
		expect(screen.getByLabelText("Bearer Token")).toHaveValue("");
		fetchMock.mockRestore();
	});

	it("keeps the created metrics connection after a failed probe and retries without creating it again", async () => {
		let probes = 0;
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/connections"))
					return Response.json(
						{
							id: "42",
							name: "prom-main",
							type: "prometheus",
							enabled: false,
							rowVersion: 1,
							config: {
								type: "prometheus",
								baseUrl: "https://metrics.example",
								authType: "none",
							},
						},
						{ status: 201 },
					);
				if (url.endsWith("/probe"))
					return Response.json({ id: `probe-${++probes}` }, { status: 202 });
				if (url.includes("probe-attempts/probe-1"))
					return Response.json({ state: "Failed" });
				if (url.includes("probe-attempts/probe-2"))
					return Response.json({ state: "Succeeded" });
				if (url.includes("/probe-results"))
					return Response.json({
						items: [
							{ id: "result-2", attemptId: "probe-2", outcome: "passed" },
						],
					});
				if (url.endsWith("/enable"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: true,
						rowVersion: 2,
						config: {
							type: "prometheus",
							baseUrl: "https://metrics.example",
							authType: "none",
						},
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/prometheus" />,
		);
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "prom-main" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://metrics.example" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		await waitFor(() =>
			expect(screen.getByText(/已创建的接入保持停用/)).toBeInTheDocument(),
		);
		expect(
			fetchMock.mock.calls.filter(([url]) =>
				String(url).endsWith("/api/v1/connections"),
			),
		).toHaveLength(1);
		fireEvent.click(screen.getByRole("button", { name: "重新验证并启用" }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations/prometheus/prom-main",
			),
		);
		expect(
			fetchMock.mock.calls.filter(([url]) =>
				String(url).endsWith("/api/v1/connections"),
			),
		).toHaveLength(1);
	});

	it("surfaces the typed probe failure diagnostic instead of a bare failure", async () => {
		const diagnostic = "查询请求失败: credential unavailable: connection material denied";
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/connections"))
					return Response.json(
						{
							id: "43",
							name: "prom-diag",
							type: "prometheus",
							enabled: false,
							rowVersion: 1,
							config: {
								type: "prometheus",
								baseUrl: "https://metrics.example",
								authType: "none",
							},
						},
						{ status: 201 },
					);
				if (url.endsWith("/probe"))
					return Response.json({ id: "probe-diag" }, { status: 202 });
				if (url.includes("probe-attempts/probe-diag"))
					return Response.json({
						state: "Failed",
						endedAt: "2026-09-21T14:33:29.839Z",
						terminationReason: "invalid_response",
					});
				if (url.includes("/probe-results"))
					return Response.json({
						items: [
							{
								id: "result-diag",
								attemptId: "probe-diag",
								outcome: "failed",
								details: { error: diagnostic },
							},
						],
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/prometheus" />,
		);
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "prom-diag" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://metrics.example" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		await waitFor(() =>
			expect(screen.getByText(/连通性验证未通过/)).toBeInTheDocument(),
		);
		// The failure alert carries the typed diagnostic, and the result card
		// repeats it for copy-paste instead of showing only "failed".
		expect(screen.getAllByText(new RegExp(diagnostic)).length).toBeGreaterThan(
			0,
		);
		expect(
			fetchMock.mock.calls.some(([url]) =>
				String(url).includes("/probe-results"),
			),
		).toBe(true);
	});

	it("shows rotated metrics as requiring revalidation and offers recovery", async () => {
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/connections/prom-main"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: false,
						revalidationRequired: true,
						rowVersion: 4,
						config: {
							type: "prometheus",
							baseUrl: "https://metrics.example",
							authType: "none",
						},
					});
				if (url.endsWith("/probe"))
					return Response.json({ id: "probe-fresh" }, { status: 202 });
				if (url.includes("probe-attempts/probe-fresh"))
					return Response.json({ state: "Succeeded" });
				if (url.includes("/probe-results"))
					return Response.json({
						items: [
							{
								id: "result-fresh",
								attemptId: "probe-fresh",
								outcome: "passed",
							},
						],
					});
				if (url.endsWith("/enable"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: true,
						revalidationRequired: false,
						rowVersion: 5,
						config: {
							type: "prometheus",
							baseUrl: "https://metrics.example",
							authType: "none",
						},
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/instances?platform=prometheus&instance=prom-main" />,
		);
		await waitFor(() =>
			expect(screen.getAllByText("需要重新验证").length).toBeGreaterThan(0),
		);
		const recovery = screen.getByRole("button", { name: "重新验证并启用" });
		expect(recovery).toBeEnabled();
		fireEvent.click(recovery);
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(([url]) => String(url).endsWith("/enable")),
			).toBe(true),
		);
		const enablePayload = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/enable"),
				)?.[1]?.body,
			),
		);
		expect(enablePayload).toMatchObject({
			qualifiedProbeResultId: "result-fresh",
		});
	});

	it("rotates bearer credentials with the exact backend payload and clears the secret", async () => {
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/connections/prom-main"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: true,
						rowVersion: 3,
						config: {
							type: "prometheus",
							baseUrl: "https://metrics.example",
							authType: "none",
							tlsCaPem: "-----BEGIN CERTIFICATE-----\nold-ca",
							tlsServerName: "metrics.internal",
							tlsSkipVerify: true,
						},
					});
				if (url.endsWith("/rotate"))
					return Response.json({
						id: "42",
						name: "prom-main",
						type: "prometheus",
						enabled: true,
						rowVersion: 4,
						config: {
							type: "prometheus",
							baseUrl: "https://replacement.example",
							authType: "bearer",
						},
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/prometheus/prom-main/rotate" />,
		);
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "保存新版本" })).toBeEnabled(),
		);
		expect(screen.getByLabelText("自定义 CA（可选）")).toHaveValue(
			"-----BEGIN CERTIFICATE-----\nold-ca",
		);
		expect(screen.getByLabelText("TLS Server Name（可选）")).toHaveValue(
			"metrics.internal",
		);
		expect(
			screen.getByLabelText("跳过 TLS 证书校验（仅限已知受控环境）"),
		).toBeChecked();
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://replacement.example" },
		});
		await pickBearerAuth();
		fireEvent.change(screen.getByLabelText("Bearer Token"), {
			target: { value: "rotation-secret" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存新版本" }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations/prometheus/prom-main",
			),
		);
		const payload = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/rotate"),
				)?.[1]?.body,
			),
		);
		expect(payload).toMatchObject({
			expectedRowVersion: 3,
			connection: {
				type: "prometheus",
				baseUrl: "https://replacement.example",
				authType: "bearer",
				bearerToken: "rotation-secret",
				tlsCaPem: "-----BEGIN CERTIFICATE-----\nold-ca",
				tlsServerName: "metrics.internal",
				tlsSkipVerify: true,
			},
		});
		expect(screen.getByLabelText("Bearer Token")).toHaveValue("");
		expect(
			fetchMock.mock.calls.filter(([url]) =>
				String(url).endsWith("/api/v1/connections"),
			),
		).toHaveLength(0);
	});

	it("opens metrics instance management by stable name, not the numeric DB id", async () => {
		// Regression: the server keys every connection read by `name`, while the
		// list projection also carries a numeric `id`. Navigating with the id
		// produced /integrations/prometheus/1 and a 404 "目标连接不存在".
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.startsWith("/api/v1/alert-sources"))
					return Response.json({ items: [] });
				if (url.startsWith("/api/v1/connections"))
					return Response.json({
						items: [
							{
								id: "1",
								name: "mall-prometheus",
								type: "prometheus",
								enabled: false,
								rowVersion: 7,
								config: {
									type: "prometheus",
									baseUrl: "http://10.43.100.205:9090",
									authType: "none",
								},
							},
						],
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/instances" />,
		);
		expect(await screen.findByText("mall-prometheus")).toBeInTheDocument();
		expect(screen.getByText("已停用")).toBeInTheDocument();
		fireEvent.click(
			screen.getByRole("button", { name: /mall-prometheus/ }),
		);
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations/instances?platform=prometheus&instance=mall-prometheus",
			),
		);
		expect(props.navigate).not.toHaveBeenCalledWith(
			"/settings/platform/integrations/prometheus/1",
		);
		fetchMock.mockRestore();
	});

	it("creates an Alertmanager source and opens its in-memory reveal dialog", async () => {
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/alert-sources"))
					return Response.json({
						revealHandle: "one-time-handle",
						revealAvailable: true,
					});
				if (url.endsWith("/api/v1/alert-sources/credentials/reveal"))
					return Response.json({ bearerToken: "secret-token" });
				if (url.endsWith("/api/v1/alert-sources/receiver-config"))
					return Response.json({
						publicReceiverUrl:
							"https://quoin.example.test/api/v1/alert-receiver",
					});
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/alertmanager" />,
		);
		fireEvent.change(screen.getByLabelText("来源键"), {
			target: { value: "production" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建并显示一次凭据" }));
		await waitFor(() => expect(screen.getByRole("dialog")).toBeInTheDocument());
		expect(screen.getByDisplayValue("secret-token")).toBeInTheDocument();
		expect(
			screen.getByDisplayValue(
				"https://quoin.example.test/api/v1/alert-receiver",
			),
		).toBeInTheDocument();
		fetchMock.mockRestore();
	});

	// 回归：receiver-config 503（部署未配置 stelePublicURL）时，创建请求根本
	// 不应发出，失败必须以常驻内联错误显示，而不是只靠会自动消失的 toast——
	// 否则表单静默回到初始状态，看起来像“什么都没发生”。
	it("keeps a persistent inline error and skips creation when the receiver endpoint is unavailable", async () => {
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/alert-sources/receiver-config"))
					return Response.json(
						{ title: "Service Unavailable", status: 503, detail: "告警接收地址尚未配置" },
						{ status: 503 },
					);
				return Response.json(
					{ message: "unexpected request" },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/alertmanager" />,
		);
		fireEvent.change(screen.getByLabelText("来源键"), {
			target: { value: "mall-shop-alertmanager" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建并显示一次凭据" }));
		const alert = await screen.findByRole("alert");
		expect(alert).toHaveTextContent("告警接收地址尚未配置");
		expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
		// 表单回到可用状态，创建 POST 从未发出（凭据不会被白白消耗）。
		expect(
			screen.getByRole("button", { name: "创建并显示一次凭据" }),
		).toBeEnabled();
		expect(
			fetchMock.mock.calls.some(
				([input, init]) =>
					String(input).endsWith("/api/v1/alert-sources") &&
					(init as RequestInit | undefined)?.method === "POST",
			),
		).toBe(false);
		fetchMock.mockRestore();
	});

	// 回归：抽屉内的启用/停用/轮换只刷新抽屉自身，背后的实例列表行徽标
	// 停留在旧状态，且关抽屉不会重挂载列表（同 query 页内路由）。
	it("refreshes the instances list behind the drawer after enabling a connection", async () => {
		let enabled = false;
		let listLoads = 0;
		const projection = () => ({
			id: "1",
			name: "mall-prometheus",
			type: "prometheus",
			enabled,
			rowVersion: enabled ? 8 : 7,
			config: { type: "prometheus", baseUrl: "http://10.43.100.205:9090", authType: "none" },
		});
		const fetchMock = vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input, init) => {
				const url = String(input);
				if (url.startsWith("/api/v1/alert-sources"))
					return Response.json({ items: [] });
				if (url === "/api/v1/connections?limit=100") {
					listLoads += 1;
					return Response.json({ items: [projection()] });
				}
				if (url === "/api/v1/connections/mall-prometheus")
					return Response.json(projection());
				if (
					url === "/api/v1/connections/mall-prometheus/probe" &&
					init?.method === "POST"
				)
					return Response.json({ id: "attempt-1" });
				if (url === "/api/v1/connections/mall-prometheus/probe-attempts/attempt-1")
					return Response.json({
						state: "Succeeded",
						endedAt: "2026-09-21T10:00:00Z",
					});
				if (url === "/api/v1/connections/mall-prometheus/probe-results?limit=50")
					return Response.json({
						items: [
							{ id: "result-1", attemptId: "attempt-1", outcome: "passed" },
						],
					});
				if (
					url === "/api/v1/connections/mall-prometheus/enable" &&
					init?.method === "POST"
				) {
					enabled = true;
					return Response.json(projection());
				}
				return Response.json(
					{ message: `unexpected request ${url}` },
					{ status: 500 },
				);
			});
		render(
			<IntegrationView route="/settings/platform/integrations/instances?platform=prometheus&instance=mall-prometheus" />,
		);
		// 列表行徽标与抽屉徽标初始一致为“已停用”。
		expect(await screen.findAllByText("已停用")).toHaveLength(2);
		fireEvent.click(screen.getByRole("button", { name: "验证并启用" }));
		// 启用成功后两处都必须翻转为“已启用”：抽屉自刷新，列表经失效重拉。
		await waitFor(() => expect(screen.getAllByText("已启用")).toHaveLength(2));
		expect(screen.queryByText("已停用")).not.toBeInTheDocument();
		expect(listLoads).toBeGreaterThanOrEqual(2);
		fetchMock.mockRestore();
	});
});
