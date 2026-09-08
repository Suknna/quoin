import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  upload: vi.fn(), publish: vi.fn(), getConfig: vi.fn(), listVerifications: vi.fn(), runVerification: vi.fn(), getIdentity: vi.fn(), getCatalog: vi.fn(),
}));
vi.mock("../../../src/features/admin/business-systems/api", async (original) => ({
  ...(await original<typeof import("../../../src/features/admin/business-systems/api")>()),
  uploadBusinessSystemConfig: api.upload,
  publishBusinessSystemConfig: api.publish,
  getConfigVersion: api.getConfig,
  listVerificationRuns: api.listVerifications,
  runVerification: api.runVerification,
  getBrowserIdentity: api.getIdentity,
  getJourneyCatalog: api.getCatalog,
}));
import { BrowserIdentityPanel, configureBrowserIdentity, UploadConfig, Version } from "./index";

const props = { user: { id: "u", username: "u", displayName: "U", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }, route: "/systems", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() };
const system = { key: "payments", displayName: "Payments", enabled: false, rowVersion: 7, currentConfigVersionId: "published", browserIdentityState: "none" as const, configVersionCount: 2, discoveries: [], plans: [] };
const version = { id: "draft", versionSeq: 2, state: "draft" as const, createdAt: "2026-01-01T00:00:00Z", digest: "abcdef0123456789", parserVersion: "v1", schemaVersion: "v1", systemKey: "payments", displayName: "Payments", enabled: true, labelContractVersionId: "contract", journeyCatalogDigest: "catalog", journeyCatalogVersion: "1" };

afterEach(cleanup);

beforeEach(() => {
  vi.restoreAllMocks();
  api.getConfig.mockResolvedValue({ ...version, yamlBody: "enabled: true\n", timezone: "UTC", resourceRefreshIntervalSeconds: 60, discoveries: [], plans: [] });
  api.listVerifications.mockResolvedValue([]);
  api.getCatalog.mockResolvedValue({ version: "catalog-1", digest: "a".repeat(64), catalogJson: { journeys: { "auth.probe": { purpose: "authentication_probe", version: 4, summary: "登录状态", params_schema: { properties: { tenant: { type: "string", title: "租户" }, retries: { type: "integer", title: "重试次数", default: 2 } } } } } } });
});

describe("system configuration workflows", () => {
  it("retains the selected YAML file and renders backend path errors after an upload failure", async () => {
    api.upload.mockRejectedValue({ message: "YAML 无效", fieldErrors: [{ path: "plans[0].checks[0]", reason: "缺少 query", remediation: "填写 PromQL" }] });
    render(<UploadConfig suspended={false} onUploaded={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "上传 YAML" }));
    const file = new File(["enabled: true"], "payments.yaml", { type: "text/yaml" });
    fireEvent.change(screen.getByLabelText("YAML 文件"), { target: { files: [file] } });
    fireEvent.click(screen.getByRole("button", { name: "创建草稿" }));
    expect(await screen.findByText("plans[0].checks[0]", { exact: false })).toBeInTheDocument();
    expect(screen.getByText("payments.yaml")).toBeInTheDocument();
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
