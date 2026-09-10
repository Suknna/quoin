import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  upload: vi.fn(), publish: vi.fn(), getConfig: vi.fn(), listVerifications: vi.fn(), runVerification: vi.fn(), getIdentity: vi.fn(), getCatalog: vi.fn(),
}));
vi.mock("@/features/admin/business-systems/api", async (original) => ({
  ...(await original<typeof import("@/features/admin/business-systems/api")>()),
  uploadBusinessSystemConfig: api.upload,
  publishBusinessSystemConfig: api.publish,
  getConfigVersion: api.getConfig,
  listVerificationRuns: api.listVerifications,
  runVerification: api.runVerification,
  getBrowserIdentity: api.getIdentity,
  getJourneyCatalog: api.getCatalog,
}));
import { BrowserIdentityPanel, configureBrowserIdentity, DeclarationDraft, parseDeclaration, Version } from "./index";

const props = { user: { id: "u", username: "u", displayName: "U", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }, route: "/systems", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() };
const system = { key: "payments", displayName: "Payments", enabled: false, rowVersion: 7, currentConfigVersionId: "published", browserIdentityState: "none" as const, configVersionCount: 2, discoveries: [], plans: [] };
const version = { id: "draft", versionSeq: 2, state: "draft" as const, createdAt: "2026-01-01T00:00:00Z", digest: "abcdef0123456789", parserVersion: "v1", schemaVersion: "v1", systemKey: "payments", displayName: "Payments", enabled: true, labelContractVersionId: "contract", journeyCatalogDigest: "catalog", journeyCatalogVersion: "1" };

afterEach(() => { cleanup(); sessionStorage.clear(); });

beforeEach(() => {
  vi.restoreAllMocks();
  api.getConfig.mockResolvedValue({ ...version, yamlBody: "enabled: true\n", timezone: "UTC", resourceRefreshIntervalSeconds: 60, discoveries: [], plans: [] });
  api.listVerifications.mockResolvedValue([]);
  api.getCatalog.mockResolvedValue({ version: "catalog-1", digest: "a".repeat(64), catalogJson: { journeys: { "auth.probe": { purpose: "authentication_probe", version: 4, summary: "登录状态", params_schema: { properties: { tenant: { type: "string", title: "租户" }, retries: { type: "integer", title: "重试次数", default: 2 } } } } } } });
});

describe("system configuration workflows", () => {
	it("round-trips canonical declaration fields and rejects a nested secret field", () => {
		const declaration = parseDeclaration(`system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: "42"\nenabled: true\ntimezone: Asia/Shanghai\nalert_source_ids: ["7"]\nalert_source_labels: {service: checkout}\nresource_discoveries:\n  - key: pods\n    display_name: Pods\n    selector: up{service="checkout"}\n    identity_labels: [namespace, pod]\ninspection_plans:\n  - key: health\n    display_name: Health\n    checks:\n      - key: up\n        display_name: Up\n        analysis_question: Is it up?\n        kind: promql\n        query: {mode: instant, expression: 'up{service="checkout"}'}\n`);
		expect(declaration).toMatchObject({ system_key: "checkout", display_name: "Checkout", metrics_connection_id: "42", timezone: "Asia/Shanghai" });
		expect(() => parseDeclaration(`system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: "42"\nenabled: true\ntimezone: UTC\nresource_discoveries: []\ninspection_plans:\n  - key: p\n    display_name: P\n    checks:\n      - key: q\n        display_name: Q\n        analysis_question: Q?\n        kind: promql\n        query: {mode: instant, expression: up}\n        password: forbidden\n`)).toThrow(/不支持字段/);
	});
	it("renders the declaration form and applies canonical YAML into its typed fields", async () => {
		vi.stubGlobal("fetch", vi.fn(async (input: string | URL) => {
			const url = String(input);
			if (url.includes("/connections")) return Response.json({ items: [{ id: "42", name: "metrics", type: "prometheus", enabled: true, rowVersion: 1, config: { baseUrl: "https://metrics.example" } }] });
			if (url.includes("/alert-sources")) return Response.json({ items: [] });
			if (url.includes("/label-contracts")) return Response.json({ items: [{ version: 3, state: "active" }] });
			return Response.json({ items: [] });
		}));
		render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
		const yaml = `system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: "42"\nenabled: true\ntimezone: Asia/Shanghai\nresource_discoveries:\n  - key: pods\n    display_name: Pods\n    selector: 'up{service="checkout"}'\n    identity_labels: [namespace, pod]\ninspection_plans:\n  - key: health\n    display_name: Health\n    checks:\n      - key: up\n        display_name: Up\n        analysis_question: Is it up?\n        kind: promql\n        query: {mode: instant, expression: 'up{service="checkout"}'}\n`;
		fireEvent.change(screen.getByLabelText("权威 YAML 声明"), { target: { value: yaml } });
		fireEvent.click(screen.getByRole("button", { name: "应用 YAML 到表单" }));
		await waitFor(() => expect(screen.getByDisplayValue("checkout")).toBeInTheDocument());
		expect(screen.getByDisplayValue("Checkout")).toBeInTheDocument();
		expect(screen.getByLabelText("指标接入")).toHaveTextContent(/metrics/);
		expect(screen.getByText(/resource_discoveries/)).toBeInTheDocument();
	});
		it("accepts character-by-character typed rule editing and compiles it into YAML", async () => {
			render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			await screen.findByLabelText("业务标识");
			fireEvent.change(screen.getByLabelText("业务标识"), { target: { value: "checkout" } });
			fireEvent.change(screen.getByLabelText("业务名称"), { target: { value: "Checkout" } });
			fireEvent.click(screen.getByRole("button", { name: "添加资源规则" }));
			const selector = screen.getByLabelText("资源 1 Selector");
			for (const value of ["u", "up", "up{"]) fireEvent.change(selector, { target: { value } });
			expect(selector).toHaveValue("up{");
			fireEvent.change(screen.getByLabelText("资源 1 Key"), { target: { value: "pods" } });
			fireEvent.change(screen.getByLabelText("资源 1 名称"), { target: { value: "Pods" } });
			fireEvent.change(screen.getByLabelText("资源 1 身份标签"), { target: { value: "namespace, pod" } });
			fireEvent.click(screen.getByRole("button", { name: "添加 PromQL 巡检" }));
			fireEvent.change(screen.getByLabelText("巡检 1-1 Key"), { target: { value: "up" } });
			fireEvent.change(screen.getByLabelText("巡检 1-1 名称"), { target: { value: "Up" } });
			fireEvent.change(screen.getByLabelText("巡检 1-1 问题"), { target: { value: "Is it up?" } });
			fireEvent.change(screen.getByLabelText("巡检 1-1 PromQL"), { target: { value: "up{job=\"checkout\"}" } });
			expect((screen.getByLabelText("权威 YAML 声明") as HTMLTextAreaElement).value).toContain("selector: up{");
		});

		it("disables typed fields while raw YAML is dirty and restores them after apply", async () => {
			render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			const yaml = screen.getByLabelText("权威 YAML 声明");
			fireEvent.change(yaml, { target: { value: "not: a declaration" } });
			expect(screen.getByLabelText("业务标识")).toBeDisabled();
			fireEvent.click(screen.getByRole("button", { name: "应用 YAML 到表单" }));
			expect(await screen.findByText(/包含不支持字段/)).toBeInTheDocument();
			expect(screen.getByLabelText("业务标识")).toBeDisabled();
			fireEvent.change(yaml, { target: { value: "system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: \"42\"\nenabled: true\ntimezone: UTC\nresource_discoveries: []\ninspection_plans: []\n" } });
			fireEvent.click(screen.getByRole("button", { name: "应用 YAML 到表单" }));
			await waitFor(() => expect(screen.getByLabelText("业务标识")).not.toBeDisabled());
		});

		it("restores an incomplete per-new-route draft after unmounting for integration setup", async () => {
			const first = render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			fireEvent.change(screen.getByLabelText("业务标识"), { target: { value: "browser-return97" } });
			await waitFor(() => expect(sessionStorage.getItem("quoin.business-declaration.draft.new")).toContain("browser-return97"));
			first.unmount();
			render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			await waitFor(() => expect(screen.getByLabelText("业务标识")).toHaveValue("browser-return97"));
		});

		it("never persists a browser check with opaque Journey parameters", async () => {
			const browserYaml = "system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: \"42\"\nenabled: true\ntimezone: UTC\nresource_discoveries: []\ninspection_plans:\n  - key: browser\n    display_name: Browser\n    checks:\n      - key: login\n        display_name: Login\n        analysis_question: Logged in?\n        kind: browser\n        journey_id: auth.probe\n        journey_params: {token: never-store}\n";
			render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			fireEvent.change(screen.getByLabelText("权威 YAML 声明"), { target: { value: browserYaml } });
			fireEvent.click(screen.getByRole("button", { name: "应用 YAML 到表单" }));
			await waitFor(() => expect(screen.getByText(/浏览器巡检及其 Journey 参数/)).toBeInTheDocument());
			expect(sessionStorage.getItem("quoin.business-declaration.draft.new")).toBeNull();
		});

		it("sends exactly the current canonical YAML after a form to YAML round trip", async () => {
			api.upload.mockResolvedValue({ systemKey: "checkout" });
			render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
			const expected = "system_key: checkout\ndisplay_name: Checkout\nmetrics_connection_id: \"42\"\nenabled: true\ntimezone: UTC\nresource_discoveries: []\ninspection_plans: []\n";
			fireEvent.change(screen.getByLabelText("权威 YAML 声明"), { target: { value: expected } });
			fireEvent.click(screen.getByRole("button", { name: "应用 YAML 到表单" }));
			await waitFor(() => expect(screen.getByLabelText("业务标识")).toHaveValue("checkout"));
			fireEvent.click(screen.getByRole("button", { name: "创建版本化草稿" }));
			await waitFor(() => expect(api.upload).toHaveBeenCalled());
			expect(await api.upload.mock.calls[0][0].file.text()).toBe(expected);
		});

	  it("sends the current published version ID as the publish fence", async () => {
    api.publish.mockResolvedValue(system);
    render(<Version system={system} version={version} versions={[version]} props={props} onPublished={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "发布" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认发布" }));
    await waitFor(() => expect(api.publish).toHaveBeenCalledWith("payments", "draft", "published"));
  });

  it("keeps the draft view after a publish conflict and tells the operator to refresh", async () => {
    api.publish.mockRejectedValue(new Error("版本已变化"));
    render(<Version system={system} version={version} versions={[version]} props={props} onPublished={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "发布" }));
    fireEvent.click(await screen.findByRole("button", { name: "确认发布" }));
    expect(await screen.findByText(/草稿和输入保持不变/)).toBeInTheDocument();
    expect(screen.getByText("v2 · draft")).toBeInTheDocument();
  });

  it("creates an initial browser identity without a row-version fence and types catalog parameters", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ identity: { id: "identity", rowVersion: 1, state: "AuthenticationRequired", currentRevision: { id: "revision", revision: 1, name: "支付登录", startUrl: "https://payments.example", authenticationProbe: { journeyId: "auth.probe", journeyVersion: 4, params: {} }, catalogDigest: "a".repeat(64), catalogVersion: "catalog-1", createdAt: "2026-01-01T00:00:00Z" }, currentProfile: null, lastProbe: null, currentOperation: null } }) });
    vi.stubGlobal("fetch", fetchMock); vi.stubGlobal("crypto", { randomUUID: () => "command" });
    api.getIdentity.mockRejectedValue(Object.assign(new Error("not found"), { status: 404 }));
    render(<BrowserIdentityPanel systemKey="payments" suspended={false} />);
    expect(await screen.findByText(/尚未配置/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "配置浏览器身份" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "支付登录" } });
    fireEvent.change(screen.getByLabelText("起始 URL"), { target: { value: "https://payments.example" } });
    fireEvent.change(screen.getByLabelText("租户"), { target: { value: "main" } });
    fireEvent.click(screen.getByRole("button", { name: "创建身份" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const body = JSON.parse(fetchMock.mock.calls[0][1].body);
    expect(body).toMatchObject({ name: "支付登录", startUrl: "https://payments.example", authenticationProbe: { journeyId: "auth.probe", journeyVersion: 4, params: { tenant: "main", retries: 2 } } });
    expect(body.expectedRowVersion).toBeUndefined();
  });

  it("sends the browser identity revision fence for a configured identity", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ identity: { id: "identity", rowVersion: 8 } }) });
    vi.stubGlobal("fetch", fetchMock); vi.stubGlobal("crypto", { randomUUID: () => "command" });
    await configureBrowserIdentity("payments", { name: "Login", startUrl: "https://payments.example", expectedRowVersion: 7, authenticationProbe: { journeyId: "auth.probe", journeyVersion: 4, params: {} } });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).expectedRowVersion).toBe(7);
  });
});
