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
		const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
			String(input).includes("/plugin-events/deadletters") ? Response.json({ count: 0, items: [] }) : Response.json({
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
		vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
			String(input).includes("/plugin-events/deadletters") ? Response.json({ count: 0, items: [] }) : Response.json({
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

	it("shows the bounded subscriber deadletter and explicitly confirms replay", async () => {
		let replayed = false;
		const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
			const url = String(input);
			if (url.endsWith("/api/v1/integrations/plugins")) return Response.json({ items: [] });
			if (url.endsWith("/api/v1/integrations/plugin-events/deadletters")) {
				return Response.json(replayed ? { count: 0, items: [] } : { count: 1, items: [{ deliveryId: 7, eventId: 8, subscriberId: "hooky", attempts: 5, lastError: "handler_failed" }] });
			}
			if (url.endsWith("/api/v1/integrations/plugin-events/deadletters/7/replay") && init?.method === "POST") {
				replayed = true;
				return new Response(null, { status: 204 });
			}
			return Response.json({ message: "unexpected request" }, { status: 500 });
		});
		render(<IntegrationView />);
		expect(await screen.findByText("插件事件死信 · 1 条")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "查看详情" }));
		expect(screen.getByText(/事件 #8 · 订阅者 hooky/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "重放" }));
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		await waitFor(() => expect(replayed).toBe(true));
		await waitFor(() => expect(screen.queryByText("插件事件死信 · 1 条")).not.toBeInTheDocument());
		expect(fetchMock).toHaveBeenCalledWith("/api/v1/integrations/plugin-events/deadletters/7/replay", expect.objectContaining({ method: "POST" }));
		const command = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/deadletters/7/replay"))?.[1];
		expect(JSON.parse(String(command?.body)).clientCommandId).toMatch(/^[A-Za-z0-9_-]{8,128}$/);
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

	describe("generic event-source plugins", () => {
		// #110: 任何带 event_source 能力的已启用目录插件（非品牌专用表单）都走
		// 通用来源表单；这里用一枚合成插件 ID 验证目录驱动的路由与生命周期。
		const SYNTHETIC_ID = "synthetic-hook";
		const SYNTHETIC_PLUGIN_ID = "synthetic-plugin";
		const syntheticPlugin = {
			id: SYNTHETIC_PLUGIN_ID,
			sourceKind: SYNTHETIC_ID,
			displayName: "合成事件源",
			description: "用于验收的通用事件接入插件",
			enabled: true,
			version: "1",
			capabilities: ["event_source", "alert_normalizer"],
		};

		function mockCatalog(items: unknown[]) {
			return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
				const url = String(input);
				if (url.endsWith("/api/v1/integrations/plugins"))
					return Response.json({ items });
				return Response.json({ message: "unexpected request" }, { status: 500 });
			});
		}

		it("routes a catalog event-source plugin to the generic creation form with its display name", async () => {
			mockCatalog([
				syntheticPlugin,
				{
					id: "prometheus",
					displayName: "Prometheus",
					description: "指标",
					enabled: true,
					version: "1",
					capabilities: ["discover", "tools"],
				},
			]);
			render(
				<IntegrationView route={`/settings/platform/integrations/${SYNTHETIC_PLUGIN_ID}`} />,
			);
			expect(
				await screen.findByRole("heading", { name: "配置 合成事件源" }),
			).toBeInTheDocument();
			expect(screen.getByLabelText("来源键")).toBeEnabled();
		});

		it("creates a generic source with the plugin kind as protocol and reveals the kind receiver URL without YAML", async () => {
			const fetchMock = vi
				.spyOn(globalThis, "fetch")
				.mockImplementation(async (input, init) => {
					const url = String(input);
					if (url.endsWith("/api/v1/integrations/plugins"))
						return Response.json({ items: [syntheticPlugin] });
					if (url.endsWith(`/api/v1/alert-sources/receiver-config?kind=${SYNTHETIC_ID}`))
						return Response.json({
							publicReceiverUrl:
								`https://quoin.example.test/stele/webhook/${SYNTHETIC_ID}`,
						});
					if (
						url === "/api/v1/alert-sources" &&
						init?.method === "POST"
					)
						return Response.json(
							{ revealHandle: "one-time-handle", revealAvailable: true },
							{ status: 201 },
						);
					if (url.endsWith("/api/v1/alert-sources/credentials/reveal"))
						return Response.json({
							credentialId: "9",
							bearerToken: "secret-token",
						});
					return Response.json(
						{ message: "unexpected request" },
						{ status: 500 },
					);
				});
			render(
				<IntegrationView route={`/settings/platform/integrations/${SYNTHETIC_PLUGIN_ID}`} />,
			);
			await screen.findByRole("heading", { name: "配置 合成事件源" });
			fireEvent.change(screen.getByLabelText("来源键"), {
				target: { value: "edge-site" },
			});
			fireEvent.click(
				screen.getByRole("button", { name: "创建并显示一次凭据" }),
			);
			await waitFor(() =>
				expect(screen.getByRole("dialog")).toBeInTheDocument(),
			);
			expect(screen.getByDisplayValue("secret-token")).toBeInTheDocument();
			expect(
				screen.getByDisplayValue(
					`https://quoin.example.test/stele/webhook/${SYNTHETIC_ID}`,
				),
			).toBeInTheDocument();
			// receiver YAML 是 Alertmanager 专用产物；通用来源不出现。
			expect(
				screen.queryByText("Alertmanager receiver YAML"),
			).not.toBeInTheDocument();
			const payload = JSON.parse(
				String(
					fetchMock.mock.calls.find(
						([url, init]) =>
							String(url) === "/api/v1/alert-sources" &&
							(init as RequestInit | undefined)?.method === "POST",
					)?.[1]?.body,
				),
			);
			expect(payload).toMatchObject({
				key: "edge-site",
				protocol: SYNTHETIC_ID,
			});
			fetchMock.mockRestore();
		});

		it("renders the shared not-found view for unsupported or disabled source kinds", async () => {
			mockCatalog([
				syntheticPlugin,
				{
					id: "disabled-hook",
					sourceKind: "disabled-kind",
					displayName: "停用事件源",
					description: "未启用",
					enabled: false,
					version: "1",
					capabilities: ["event_source", "alert_normalizer"],
				},
				{
					id: "events-without-alerts",
					sourceKind: "non-alert-events",
					displayName: "非告警事件源",
					description: "缺少告警归一化能力",
					enabled: true,
					version: "1",
					capabilities: ["event_source"],
				},
				{
					id: "tool-only",
					displayName: "仅工具插件",
					description: "无入站能力",
					enabled: true,
					version: "1",
					capabilities: ["tools"],
				},
			]);
			for (const kind of ["no-such-kind", "disabled-hook", "events-without-alerts", "tool-only"]) {
				render(
					<IntegrationView route={`/settings/platform/integrations/${kind}`} />,
				);
				expect(
					await screen.findByText("找不到此页面"),
				).toBeInTheDocument();
				cleanup();
			}
		});

		it("lists generic source instances by kind and opens the shared drawer", async () => {
			vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
				const url = String(input);
				if (url.startsWith("/api/v1/alert-sources"))
					return Response.json({
						items: [
							{
								key: "edge-site",
								protocol: SYNTHETIC_ID,
								enabled: true,
								rowVersion: 3,
								createdAt: "2026-09-28T00:00:00Z",
							},
						],
					});
				if (url.startsWith("/api/v1/connections"))
					return Response.json({ items: [] });
				return Response.json({ message: "unexpected request" }, { status: 500 });
			});
			render(
				<IntegrationView route="/settings/platform/integrations/instances" />,
			);
			expect(await screen.findByText("edge-site")).toBeInTheDocument();
			expect(screen.getByText(/synthetic-hook/)).toBeInTheDocument();
			fireEvent.click(screen.getByRole("button", { name: /edge-site/ }));
			await waitFor(() =>
				expect(props.navigate).toHaveBeenCalledWith(
					`/settings/platform/integrations?platform=${SYNTHETIC_ID}&instance=edge-site`,
				),
			);
		});

		it("manages a generic source instance status and credential generations in the drawer", async () => {
			vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
				const url = String(input);
				if (
					url ===
					`/api/v1/alert-sources/edge-site/credentials?limit=100`
				)
					return Response.json({
						items: [
							{
								id: "7",
								rowVersion: 2,
								state: "PendingRetirement",
								createdAt: "2026-09-28T00:00:00Z",
							},
						],
					});
				if (url === "/api/v1/alert-sources/edge-site")
					return Response.json({
						key: "edge-site",
						protocol: SYNTHETIC_ID,
						enabled: true,
						rowVersion: 3,
						createdAt: "2026-09-28T00:00:00Z",
					});
				return Response.json({ message: `unexpected request ${url}` }, { status: 500 });
			});
			render(
				<IntegrationView route="/settings/platform/integrations?platform=synthetic-hook&instance=edge-site" />,
			);
			expect(await screen.findByText("已启用")).toBeInTheDocument();
			expect(screen.getByText("7")).toBeInTheDocument();
			expect(screen.getByText("待退休")).toBeInTheDocument();
			expect(screen.getByRole("button", { name: "轮换凭据" })).toBeEnabled();
			expect(screen.getByRole("button", { name: "退休" })).toBeEnabled();
		});
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
				"/settings/platform/integrations?platform=prometheus&instance=mall-prometheus",
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

	// 回归：详情视图的启用/停用/轮换只刷新详情自身；列表与详情是同一抽屉的
	// 两个互斥视图，返回列表时重挂载并重新拉取，徽标不会停留在旧状态。
	it("refreshes the connection status in the drawer detail after enabling", async () => {
		let enabled = false;
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
				if (url === "/api/v1/connections?limit=100")
					return Response.json({ items: [projection()] });
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
			<IntegrationView route="/settings/platform/integrations?platform=prometheus&instance=mall-prometheus" />,
		);
		// 详情视图初始为“已停用”，启用成功后抽屉内翻转为“已启用”。
		expect(await screen.findByText("已停用")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "验证并启用" }));
		await waitFor(() =>
			expect(screen.getByText("已启用")).toBeInTheDocument(),
		);
		expect(screen.queryByText("已停用")).not.toBeInTheDocument();
		fetchMock.mockRestore();
	});
});

describe("generic HTTP connection plugins (#110)", () => {
	// 合成插件目录 fixture：三个稳定身份互不相同 —— 插件 ID ≠ sourceKind（如
	// 同时具备）≠ connectionKind。前端不做品牌分支；目录声明什么就用什么。
	const httpPlugin = {
		id: "synthetic-plugin",
		connectionKind: "synthetic-http",
		connectionAuthModes: ["none", "basic", "bearer"],
		connectionProbePath: "/health",
		displayName: "合成 HTTP 平台",
		description: "用于验收的受控 HTTP 连接插件",
		enabled: true,
		version: "1",
		capabilities: ["http_connection"],
	};

	/** Fetch mock with explicit routes; unmatched requests fail loudly. */
	function mockFetch(
		routes: (url: string, init?: RequestInit) => Response | undefined,
	) {
		return vi
			.spyOn(globalThis, "fetch")
			.mockImplementation(async (input, init) => {
				const url = String(input);
				const matched = routes(url, init as RequestInit | undefined);
				if (matched) return matched;
				return Response.json(
					{ message: `unexpected request ${url}` },
					{ status: 500 },
				);
			});
	}

	async function pickAuthOption(label: string) {
		// Radix Select opens from the keyboard in jsdom; pointer events need APIs
		// the environment does not implement.
		fireEvent.keyDown(screen.getByRole("combobox", { name: "认证方式" }), {
			key: "ArrowDown",
		});
		fireEvent.click(await screen.findByRole("option", { name: label }));
	}

	const connectionProjection = (
		overrides: Record<string, unknown> = {},
	) => ({
		id: "9",
		name: "edge-http",
		type: "synthetic-http",
		enabled: false,
		rowVersion: 1,
		config: {
			type: "synthetic-http",
			baseUrl: "https://edge.example",
			authType: "none",
		},
		...overrides,
	});

	it("offers configuration for an enabled plugin whose connection kind differs from its id", async () => {
		mockFetch((url) =>
			url.endsWith("/api/v1/integrations/plugins")
				? Response.json({ items: [httpPlugin] })
				: undefined,
		);
		render(<IntegrationView />);
		expect(await screen.findByText("HTTP 连接")).toBeInTheDocument();
		expect(
			screen.getByRole("button", { name: "配置 合成 HTTP 平台" }),
		).toBeEnabled();
	});

	it("creates, probes and enables a synthetic HTTP connection with the plugin-declared contract", async () => {
		const fetchMock = mockFetch((url, init) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({ items: [httpPlugin] });
			if (url === "/api/v1/connections" && init?.method === "POST")
				return Response.json(connectionProjection(), { status: 201 });
			if (url === "/api/v1/connections?limit=100")
				return Response.json({
					items: [
						connectionProjection({ enabled: true, rowVersion: 2 }),
					],
				});
			if (url.endsWith("/connections/edge-http/probe"))
				return Response.json({ id: "probe-9" }, { status: 202 });
			if (url.endsWith("/connections/edge-http/probe-attempts/probe-9"))
				return Response.json({
					state: "Succeeded",
					endedAt: "2026-09-28T00:00:00Z",
				});
			if (url.includes("/connections/edge-http/probe-results"))
				return Response.json({
					items: [
						{ id: "result-9", attemptId: "probe-9", outcome: "passed" },
					],
				});
			if (url.endsWith("/connections/edge-http/enable"))
				return Response.json(
					connectionProjection({ enabled: true, rowVersion: 2 }),
				);
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/synthetic-plugin" />,
		);
		await screen.findByRole("heading", { name: "配置 合成 HTTP 平台" });
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "edge-http" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://edge.example" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		// 启用成功后实例列表刷新出现该连接。
		expect(await screen.findByText("edge-http")).toBeInTheDocument();
		expect(screen.getByText("已启用")).toBeInTheDocument();
		const createCall = fetchMock.mock.calls.find(
			([url, init]) =>
				String(url) === "/api/v1/connections" &&
				(init as RequestInit | undefined)?.method === "POST",
		);
		const createBody = JSON.parse(String(createCall?.[1]?.body));
		expect(createBody).toMatchObject({ name: "edge-http" });
		expect(createBody.connection).toMatchObject({
			type: "synthetic-http",
			baseUrl: "https://edge.example",
			authType: "none",
		});
		// 无认证不得携带任何凭据字段。
		expect(createBody.connection).not.toHaveProperty("username");
		expect(createBody.connection).not.toHaveProperty("password");
		expect(createBody.connection).not.toHaveProperty("bearerToken");
		const enableCall = fetchMock.mock.calls.find(([url]) =>
			String(url).endsWith("/connections/edge-http/enable"),
		);
		expect(enableCall).toBeTruthy();
		expect(JSON.parse(String(enableCall?.[1]?.body))).toMatchObject({
			expectedRowVersion: 1,
			qualifiedProbeResultId: "result-9",
		});
	});

	it("offers only the plugin-declared auth modes and submits basic credentials", async () => {
		const fetchMock = mockFetch((url, init) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({
					items: [{ ...httpPlugin, connectionAuthModes: ["basic", "bearer"] }],
				});
			if (url === "/api/v1/connections" && init?.method === "POST")
				return Response.json(
					connectionProjection({
						config: {
							type: "synthetic-http",
							baseUrl: "https://edge.example",
							authType: "basic",
							username: "edge-user",
						},
					}),
					{ status: 201 },
				);
			if (url.endsWith("/connections/edge-http/probe"))
				return Response.json({ id: "probe-basic" }, { status: 202 });
			if (url.includes("probe-attempts/probe-basic"))
				return Response.json({ state: "Failed" });
			if (url.includes("/connections/edge-http/probe-results"))
				return Response.json({
					items: [
						{
							id: "result-basic",
							attemptId: "probe-basic",
							outcome: "failed",
							details: { reason: "invalid_response" },
						},
					],
				});
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/synthetic-plugin" />,
		);
		await screen.findByRole("heading", { name: "配置 合成 HTTP 平台" });
		fireEvent.keyDown(screen.getByRole("combobox", { name: "认证方式" }), {
			key: "ArrowDown",
		});
		expect(
			await screen.findByRole("option", { name: "HTTP Basic" }),
		).toBeInTheDocument();
		expect(
			screen.queryByRole("option", { name: "无认证" }),
		).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("option", { name: "HTTP Basic" }));
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "edge-http" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://edge.example" },
		});
		fireEvent.change(screen.getByLabelText("用户名"), {
			target: { value: "edge-user" },
		});
		fireEvent.change(screen.getByLabelText("密码"), {
			target: { value: "edge-pass" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		await waitFor(() =>
			expect(screen.getByText(/连通性验证未通过/)).toBeInTheDocument(),
		);
		const createCall = fetchMock.mock.calls.find(
			([url, init]) =>
				String(url) === "/api/v1/connections" &&
				(init as RequestInit | undefined)?.method === "POST",
		);
		const body = JSON.parse(String(createCall?.[1]?.body));
		expect(body.connection).toMatchObject({
			type: "synthetic-http",
			authType: "basic",
			username: "edge-user",
			password: "edge-pass",
		});
		expect(body.connection).not.toHaveProperty("bearerToken");
	});

	it("never echoes submitted credentials in lifecycle error messages", async () => {
		mockFetch((url, init) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({ items: [httpPlugin] });
			if (url === "/api/v1/connections" && init?.method === "POST")
				return Response.json(
					connectionProjection({
						config: {
							type: "synthetic-http",
							baseUrl: "https://edge.example",
							authType: "bearer",
						},
					}),
					{ status: 201 },
				);
			if (url.endsWith("/connections/edge-http/probe"))
				return Response.json({ id: "probe-secret" }, { status: 202 });
			if (url.includes("probe-attempts/probe-secret"))
				return Response.json({ state: "Failed" });
			if (url.includes("/connections/edge-http/probe-results"))
				return Response.json({
					items: [
						{
							id: "result-secret",
							attemptId: "probe-secret",
							outcome: "failed",
							details: {
								error: "credential unavailable: gateway denied",
							},
						},
					],
				});
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/synthetic-plugin" />,
		);
		await screen.findByRole("heading", { name: "配置 合成 HTTP 平台" });
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "edge-http" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://edge.example" },
		});
		await pickAuthOption("Bearer Token");
		fireEvent.change(screen.getByLabelText("Bearer Token"), {
			target: { value: "super-secret-token-value" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建、验证并启用" }));
		expect(await screen.findByText(/连通性验证未通过/)).toBeInTheDocument();
		// 诊断原文可见，但秘密永不回显。
		expect(document.body.textContent).toContain("gateway denied");
		expect(document.body.textContent).not.toContain(
			"super-secret-token-value",
		);
	});

	it("fails closed for disabled or unknown connection plugins", async () => {
		mockFetch((url) =>
			url.endsWith("/api/v1/integrations/plugins")
				? Response.json({ items: [{ ...httpPlugin, enabled: false }] })
				: undefined,
		);
		for (const route of [
			"/settings/platform/integrations/synthetic-plugin",
			"/settings/platform/integrations/no-such-plugin",
			"/settings/platform/integrations/synthetic-http/edge-http",
		]) {
			render(<IntegrationView route={route} />);
			expect(await screen.findByText("找不到此页面")).toBeInTheDocument();
			cleanup();
		}
	});

	it("keeps webhook-source and HTTP connection affordances side by side", async () => {
		mockFetch((url) =>
			url.endsWith("/api/v1/integrations/plugins")
				? Response.json({
						items: [
							{
								...httpPlugin,
								id: "dual-plugin",
								sourceKind: "dual-hook",
								connectionKind: "dual-http",
								capabilities: [
									"event_source",
									"alert_normalizer",
									"http_connection",
								],
							},
						],
					})
				: undefined,
		);
		render(
			<IntegrationView route="/settings/platform/integrations/dual-plugin" />,
		);
		expect(
			await screen.findByRole("heading", { name: "配置 合成 HTTP 平台" }),
		).toBeInTheDocument();
		expect(
			screen.getByRole("heading", { name: "HTTP 连接" }),
		).toBeInTheDocument();
		expect(
			screen.getByRole("heading", { name: "事件接入" }),
		).toBeInTheDocument();
		expect(screen.getByLabelText("实例名称")).toBeInTheDocument();
		expect(screen.getByLabelText("来源键")).toBeInTheDocument();
	});

	it("creates a connection without a declared probe path but never offers enablement", async () => {
		let probes = 0;
		let enables = 0;
		const fetchMock = mockFetch((url, init) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({
					items: [{ ...httpPlugin, connectionProbePath: undefined }],
				});
			if (url === "/api/v1/connections" && init?.method === "POST")
				return Response.json(connectionProjection(), { status: 201 });
			if (url === "/api/v1/connections?limit=100")
				return Response.json({ items: [connectionProjection()] });
			if (url.endsWith("/probe")) {
				probes += 1;
				return Response.json({ id: "probe-x" }, { status: 202 });
			}
			if (url.endsWith("/enable")) {
				enables += 1;
				return Response.json(connectionProjection());
			}
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/synthetic-plugin" />,
		);
		await screen.findByRole("heading", { name: "配置 合成 HTTP 平台" });
		expect(
			screen.getByText(/未声明探测路径，新实例无法启用/),
		).toBeInTheDocument();
		fireEvent.change(screen.getByLabelText("实例名称"), {
			target: { value: "edge-http" },
		});
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://edge.example" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建（保持停用）" }));
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(
					([url, init]) =>
						String(url) === "/api/v1/connections" &&
						(init as RequestInit | undefined)?.method === "POST",
					),
			).toBe(true),
		);
		expect(probes).toBe(0);
		expect(enables).toBe(0);
	});

	it("lists synthetic HTTP instances beside the other integrations and opens the drawer", async () => {
		const fetchMock = mockFetch((url) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({ items: [httpPlugin] });
			if (url === "/api/v1/connections?limit=100")
				return Response.json({ items: [connectionProjection()] });
			if (url.startsWith("/api/v1/alert-sources"))
				return Response.json({ items: [] });
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/instances" />,
		);
		expect(await screen.findByText("edge-http")).toBeInTheDocument();
		expect(screen.getByText(/synthetic-http/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: /edge-http/ }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations?platform=synthetic-http&instance=edge-http",
			),
		);
		// 列表读取只用了稳定的 name，不使用数字 id。
		expect(
			fetchMock.mock.calls.some(([url]) => String(url).includes("/9")),
		).toBe(false);
	});

	it("manages a synthetic HTTP connection lifecycle in the drawer with confirmed disable", async () => {
		let enabled = true;
		const projection = () =>
			connectionProjection({ enabled, rowVersion: enabled ? 3 : 4 });
		const fetchMock = mockFetch((url) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({ items: [httpPlugin] });
			if (url === "/api/v1/connections/edge-http")
				return Response.json(projection());
			if (url.endsWith("/connections/edge-http/disable")) {
				enabled = false;
				return Response.json(projection());
			}
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations?platform=synthetic-http&instance=edge-http" />,
		);
		expect(await screen.findByText("已启用")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "验证并启用" })).toBeDisabled();
		fireEvent.click(screen.getByRole("button", { name: "停用接入" }));
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.some(([url]) =>
					String(url).endsWith("/connections/edge-http/disable"),
				),
			).toBe(true),
		);
		const disableBody = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/connections/edge-http/disable"),
				)?.[1]?.body,
			),
		);
		expect(disableBody).toMatchObject({ expectedRowVersion: 3 });
		fireEvent.click(screen.getByRole("button", { name: "轮换凭据" }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations/synthetic-http/edge-http",
			),
		);
	});

	it("rotates a synthetic HTTP connection with prefilled non-secrets and clears the new secret", async () => {
		const fetchMock = mockFetch((url) => {
			if (url.endsWith("/api/v1/integrations/plugins"))
				return Response.json({ items: [httpPlugin] });
			if (url === "/api/v1/connections/edge-http")
				return Response.json(
					connectionProjection({
						enabled: true,
						rowVersion: 3,
						config: {
							type: "synthetic-http",
							baseUrl: "https://edge.example",
							authType: "none",
							tlsServerName: "edge.internal",
							tlsSkipVerify: true,
						},
					}),
				);
			if (url.endsWith("/connections/edge-http/rotate"))
				return Response.json(
					connectionProjection({
						enabled: true,
						rowVersion: 4,
						config: {
							type: "synthetic-http",
							baseUrl: "https://replacement.example",
							authType: "bearer",
							tlsServerName: "edge.internal",
							tlsSkipVerify: true,
						},
					}),
				);
			return undefined;
		});
		render(
			<IntegrationView route="/settings/platform/integrations/synthetic-http/edge-http" />,
		);
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "保存新版本" })).toBeEnabled(),
		);
		expect(screen.getByLabelText("端点 URL")).toHaveValue(
			"https://edge.example",
		);
		expect(screen.getByLabelText("TLS Server Name（可选）")).toHaveValue(
			"edge.internal",
		);
		await pickAuthOption("Bearer Token");
		fireEvent.change(screen.getByLabelText("端点 URL"), {
			target: { value: "https://replacement.example" },
		});
		fireEvent.change(screen.getByLabelText("Bearer Token"), {
			target: { value: "rotation-secret" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存新版本" }));
		await waitFor(() =>
			expect(props.navigate).toHaveBeenCalledWith(
				"/settings/platform/integrations?platform=synthetic-http&instance=edge-http",
			),
		);
		const payload = JSON.parse(
			String(
				fetchMock.mock.calls.find(([url]) =>
					String(url).endsWith("/connections/edge-http/rotate"),
				)?.[1]?.body,
			),
		);
		expect(payload).toMatchObject({
			expectedRowVersion: 3,
			connection: {
				type: "synthetic-http",
				baseUrl: "https://replacement.example",
				authType: "bearer",
				bearerToken: "rotation-secret",
				tlsServerName: "edge.internal",
				tlsSkipVerify: true,
			},
		});
		expect(screen.getByLabelText("Bearer Token")).toHaveValue("");
	});
});
