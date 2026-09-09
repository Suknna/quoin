import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
import { useAlertsModule } from "./index";

function View({ route, navigate = vi.fn() }: { route: string; navigate?: (route: string) => void }) {
  const view = useAlertsModule({
    user: { id: "1", username: "admin", displayName: "Admin", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 },
    route,
    navigate,
    suspended: false,
    openEvidence: vi.fn(),
  });
  return <>{view.content}</>;
}

describe("alerts module", () => {
  it("renders the history view when the route includes its query string", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ items: [] }) }));
    render(<View route="/alerts/list?view=history" />);
    expect(await screen.findByRole("heading", { name: "告警历史" })).toBeInTheDocument();
  });

  it("starts analysis only after its tab opens and does not create when reading fails", async () => {
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/observations")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/alerts/alert-1/analyses")) return Promise.resolve({ ok: false, json: async () => ({}) });
      if (input.includes("/alerts/alert-1")) return Promise.resolve({ ok: true, json: async () => ({ id: "alert-1", state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: "Example" } }) });
      return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<View route="/alerts/list?id=alert-1" />);
    expect(await screen.findByRole("heading", { name: "Example" })).toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("/analyses"))).toBe(false);
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" }));
    fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await screen.findByRole("alert");
    expect(fetchMock.mock.calls.filter(([url, init]) => String(url).endsWith("/analyses") && (init as RequestInit | undefined)?.method === "POST")).toHaveLength(0);
  });

  it("remounts analysis for a newly selected occurrence", async () => {
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      const id = input.includes("alert-2") ? "alert-2" : "alert-1";
      if (input.includes("/observations")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/attempts")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.endsWith("/analyses")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: `analysis-${id}`, state: "Succeeded", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z" }] }) });
      if (input.includes("/analyses/")) return Promise.resolve({ ok: true, json: async () => ({ id: `analysis-${id}`, state: "Succeeded", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z", attemptCount: 1, output: { id: "output", modelId: "demo", content: id, evidenceIds: [], createdAt: "2026-01-01T00:00:00Z" } }) });
      return Promise.resolve({ ok: true, json: async () => ({ id, state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: id } }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    const view = render(<View route="/alerts/list?id=alert-1" />);
    await screen.findByRole("heading", { name: "alert-1" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" }));
    fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await screen.findByText("初步分析");
    view.rerender(<View route="/alerts/list?id=alert-2" />);
    expect(await screen.findByRole("heading", { name: "alert-2" })).toBeInTheDocument();
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" }));
    fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => String(url).includes("alert-2/analyses"))).toBe(true));
  });

  it("polls an active analysis until its terminal output is available", async () => {
    let reads = 0;
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/observations") || input.includes("/attempts")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.endsWith("/analyses")) { reads += 1; return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "analysis-1", state: reads === 1 ? "Running" : "Succeeded", rowVersion: reads, createdAt: "2026-01-01T00:00:00Z" }] }) }); }
      if (input.includes("/analyses/")) return Promise.resolve({ ok: true, json: async () => ({ id: "analysis-1", state: reads === 1 ? "Running" : "Succeeded", rowVersion: reads, createdAt: "2026-01-01T00:00:00Z", attemptCount: 1, output: reads === 1 ? undefined : { id: "output", modelId: "demo", content: "finished", evidenceIds: [], createdAt: "2026-01-01T00:00:00Z" } }) });
      return Promise.resolve({ ok: true, json: async () => ({ id: "alert-1", state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: "Example" } }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<View route="/alerts/list?id=alert-1" />);
    await screen.findByRole("heading", { name: "Example" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" })); fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await screen.findByText("分析正在执行，关闭详情不会取消任务。");
    await waitFor(() => expect(reads).toBeGreaterThan(1), { timeout: 4_000 });
    expect(await screen.findByText("finished")).toBeInTheDocument();
  });

  it("selects the newest failed analysis and retries that analysis", async () => {
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/observations") || input.includes("/attempts")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.endsWith("/analyses")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "old-success", state: "Succeeded", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z" }, { id: "new-failure", state: "Failed", rowVersion: 2, createdAt: "2026-01-02T00:00:00Z" }] }) });
      if (input.includes("/retry")) return Promise.resolve({ ok: true, json: async () => ({ id: "new-failure", state: "Running", rowVersion: 3, createdAt: "2026-01-02T00:00:00Z", attemptCount: 2 }) });
      if (input.includes("/analyses/")) return Promise.resolve({ ok: true, json: async () => ({ id: input.includes("new-failure") ? "new-failure" : "old-success", state: input.includes("new-failure") ? "Failed" : "Succeeded", rowVersion: 2, createdAt: "2026-01-02T00:00:00Z", attemptCount: 1 }) });
      return Promise.resolve({ ok: true, json: async () => ({ id: "alert-1", state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: "Example" } }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<View route="/alerts/list?id=alert-1" />);
    await screen.findByRole("heading", { name: "Example" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" })); fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await screen.findByRole("button", { name: "重试分析" });
    fireEvent.click(screen.getByRole("button", { name: "重试分析" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([url, init]) => String(url).includes("new-failure/retry") && (init as RequestInit).method === "POST")).toBe(true));
  });

  it("closing and reopening does not cancel or duplicate an active analysis", async () => {
    const navigate = vi.fn();
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.includes("/observations") || input.includes("/attempts")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
      if (input.endsWith("/analyses")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "active", state: "Running", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z" }] }) });
      if (input.includes("/analyses/")) return Promise.resolve({ ok: true, json: async () => ({ id: "active", state: "Running", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z", attemptCount: 1 }) });
      return Promise.resolve({ ok: true, json: async () => ({ id: "alert-1", state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: "Example" } }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    const result = render(<View route="/alerts/list?id=alert-1" navigate={navigate} />);
    await screen.findByRole("heading", { name: "Example" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" })); fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await screen.findByText("分析正在执行，关闭详情不会取消任务。");
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(navigate).toHaveBeenCalledWith("/alerts/list?view=current");
    result.rerender(<View route="/alerts/list?view=current" navigate={navigate} />);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    result.rerender(<View route="/alerts/list?id=alert-1" navigate={navigate} />);
    await screen.findByRole("heading", { name: "Example" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" })); fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url, init]) => String(url).endsWith("/analyses") && (init as RequestInit | undefined)?.method === "POST")).toHaveLength(0));
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes("/cancel"))).toBe(false);
  });
});
