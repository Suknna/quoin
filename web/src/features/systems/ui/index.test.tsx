import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ upload: vi.fn() }));
vi.mock("@/features/admin/business-systems/api", async (original) => ({ ...(await original<typeof import("@/features/admin/business-systems/api")>()), uploadBusinessSystemConfig: api.upload }));
import type { WorkspaceModuleProps } from "@/app/module-contract";
import type { BusinessSystemDetail } from "@/features/admin/business-systems/api";
import { DeclarationDraft, parseDeclaration, SystemDetail } from "./index";

const declaration = `apiVersion: quoin/v1
kind: BusinessSystem
metadata:
  name: checkout
  displayName: Checkout
  description: Checkout workloads
spec:
  metrics:
    connectionRef: thanos-primary
    matchLabels: {service: checkout}
    resources:
      - name: pods
        displayName: Checkout pods
        matchLabels: {component: api}
        discoveryMetric: up
        identityLabels: [namespace, pod]
        allowedMetrics: [up, http_requests_total]
  alerts:
    sourceRefs: [demo-alertmanager]
    matchLabels: {severity: critical}
  inspections:
    - name: health
      displayName: Health
      schedule: '*/5 * * * *'
      timezone: Asia/Shanghai
      checks:
        - name: availability
          resourceRef: pods
          expression: up
          question: Is checkout available?
`;

afterEach(() => { vi.useRealTimers(); cleanup(); });
beforeEach(() => {
  HTMLElement.prototype.scrollIntoView = vi.fn();
  api.upload.mockReset();
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL) => {
    const url = String(input);
    if (url.includes("/connections")) return Response.json({ items: [{ id: "connection-1", name: "thanos-primary", type: "thanos", enabled: true, config: {} }] });
    if (url.includes("/alert-sources")) return Response.json({ items: [{ key: "demo-alertmanager", protocol: "alertmanager", enabled: true, rowVersion: 1 }] });
    return Response.json({ items: [] });
  }));
});

const system: BusinessSystemDetail = {
  key: "checkout",
  displayName: "Checkout",
  enabled: true,
  rowVersion: 1,
  configVersionCount: 0,
  discoveries: [],
  plans: [],
  resourceRefreshIntervalSeconds: 300,
  browserIdentityState: "none",
};

const moduleProps: WorkspaceModuleProps = {
  user: {} as WorkspaceModuleProps["user"],
  route: "/business-systems?system=checkout",
  navigate: vi.fn(),
  suspended: false,
  openEvidence: vi.fn(),
};

function apiRequestUrls() {
  return (fetch as ReturnType<typeof vi.fn>).mock.calls.map(([input]) => String(input));
}

describe("System detail", () => {
  it("tracks a manual refresh through completion, reloads resources, and avoids Kubernetes APIs", async () => {
    vi.useFakeTimers();
    let resourceReads = 0;
    vi.stubGlobal("fetch", vi.fn(async (input: string | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/config?limit=100")) return Response.json({ items: [] });
      if (url.includes("/resources?current=true&limit=100")) { resourceReads++; return Response.json({ items: resourceReads === 1 ? [] : [{ id: "resource-1", discoveryKey: "pods", identityLabels: { namespace: "default" }, current: true, stale: false, lastSuccessfulRefreshAt: "2026-09-11T10:00:00.000Z" }] }); }
      if (url.endsWith("/resources:refresh") && init?.method === "POST") return Response.json({ id: "refresh-1", businessSystemId: "checkout", configVersionId: "config-1", labelContractVersionId: "labels-1", triggerKind: "manual", state: "Queued", rowVersion: 1, createdAt: "2026-09-11T10:00:00.000Z" });
      if (url.endsWith("/resource-refresh-runs/refresh-1")) return Response.json({ id: "refresh-1", businessSystemId: "checkout", configVersionId: "config-1", labelContractVersionId: "labels-1", triggerKind: "manual", state: "Completed", rowVersion: 2, resultDetail: "匹配到 1 个资源。", createdAt: "2026-09-11T10:00:00.000Z" });
      throw new Error(`Unexpected API request: ${url}`);
    }));

    render(<SystemDetail system={system} props={moduleProps} onChanged={vi.fn()} />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    expect(screen.getByText("尚未发现资源")).toBeInTheDocument();
    expect(screen.queryByText("绑定 Kubernetes 连接")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "绑定" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "解除" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "刷新资源" }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("资源刷新：Queued")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "正在刷新…" })).toBeDisabled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
    expect(screen.getByText("资源刷新：Completed")).toBeInTheDocument();
    expect(screen.getByText("匹配到 1 个资源。")).toBeInTheDocument();
    expect(screen.getByText("pods")).toBeInTheDocument();
    expect(screen.getByText("最近成功刷新")).toBeInTheDocument();
    expect(apiRequestUrls().some((url) => url.includes("/kubernetes-connections"))).toBe(false);
    vi.useRealTimers();
  });

  it("shows the failed refresh result detail and explains a completed no-match refresh", async () => {
    vi.useFakeTimers();
    let runReads = 0;
    vi.stubGlobal("fetch", vi.fn(async (input: string | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/config?limit=100") || url.includes("/resources?current=true&limit=100")) return Response.json({ items: [] });
      if (url.endsWith("/resources:refresh") && init?.method === "POST") return Response.json({ id: "refresh-2", businessSystemId: "checkout", configVersionId: "config-1", labelContractVersionId: "labels-1", triggerKind: "manual", state: "Running", rowVersion: 1, createdAt: "2026-09-11T10:00:00.000Z" });
      if (url.endsWith("/resource-refresh-runs/refresh-2")) { runReads++; return Response.json({ id: "refresh-2", businessSystemId: "checkout", configVersionId: "config-1", labelContractVersionId: "labels-1", triggerKind: "manual", state: runReads === 1 ? "Failed" : "Completed", rowVersion: 2, resultDetail: runReads === 1 ? "指标连接不可用。" : "未匹配资源。", createdAt: "2026-09-11T10:00:00.000Z" }); }
      throw new Error(`Unexpected API request: ${url}`);
    }));

    render(<SystemDetail system={system} props={moduleProps} onChanged={vi.fn()} />);
    await act(async () => { await vi.runOnlyPendingTimersAsync(); });
    fireEvent.click(screen.getByRole("button", { name: "刷新资源" }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("资源刷新：Running")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
    expect(screen.getByText("资源刷新：Failed")).toBeInTheDocument();
    expect(screen.getByText("指标连接不可用。")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "刷新资源" }));
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("资源刷新：Running")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1500); });
    expect(screen.getByText("资源刷新：Completed")).toBeInTheDocument();
    expect(screen.getByText("未匹配资源。")).toBeInTheDocument();
    expect(screen.getByText("本次刷新未匹配到声明的资源；请检查发现指标和标签选择器。")).toBeInTheDocument();
    expect(apiRequestUrls().some((url) => url.includes("/kubernetes-connections"))).toBe(false);
    vi.useRealTimers();
  });
});

describe("Kubernetes-style business system declaration", () => {
  it("parses explicit resources, selector scopes, and metric allowlists", () => {
    const parsed = parseDeclaration(declaration);
    expect(parsed.metadata).toMatchObject({ name: "checkout", displayName: "Checkout" });
    expect(parsed.spec.metrics.resources[0]).toMatchObject({ name: "pods", identityLabels: ["namespace", "pod"], allowedMetrics: ["up", "http_requests_total"] });
    expect(parsed.spec.inspections[0].checks[0]).toMatchObject({ resourceRef: "pods", expression: "up" });
  });

  it("allows applying and submitting new YAML without Label Contracts", async () => {
    api.upload.mockResolvedValue({ systemKey: "checkout" });
    render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
    await screen.findByLabelText("权威 YAML 声明");
    fireEvent.change(screen.getByLabelText("权威 YAML 声明"), { target: { value: declaration } });
    fireEvent.click(screen.getByRole("button", { name: "应用 YAML" }));
    expect(await screen.findByLabelText("编译预览")).toHaveTextContent(/允许指标：up, http_requests_total/);
    fireEvent.click(screen.getByRole("button", { name: "创建版本化草稿" }));
    await waitFor(() => expect(api.upload).toHaveBeenCalledTimes(1));
    const input = api.upload.mock.calls[0][0];
    expect(await input.file.text()).toBe(declaration);
    expect(input).not.toHaveProperty("targetLabelContractVersion");
  });

  it("generates a new template from a selectable connection name", async () => {
    render(<DeclarationDraft suspended={false} navigate={vi.fn()} onUploaded={vi.fn()} />);
    await screen.findByLabelText("从指标连接生成模板");
    fireEvent.click(screen.getByLabelText("从指标连接生成模板"));
    fireEvent.click(await screen.findByText("thanos-primary"));
    await waitFor(() => expect((screen.getByLabelText("权威 YAML 声明") as HTMLTextAreaElement).value).toContain("connectionRef: thanos-primary"));
  });
});
