import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); });
import { useAlertsModule } from "./index";

function View({ route, navigate = vi.fn(), openEvidence = vi.fn() }: { route: string; navigate?: (route: string) => void; openEvidence?: (id: string) => void }) {
  const view = useAlertsModule({
    user: { id: "1", username: "admin", displayName: "Admin", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 },
    route,
    navigate,
    suspended: false,
    openEvidence,
  });
  return <>{view.content}</>;
}

describe("alerts module", () => {
  it("shows platform lifecycle facts without upstream observations or business analysis", async () => {
    const fetchMock = vi.fn().mockImplementation((input: string) => Promise.resolve({ ok: true, json: async () => input === "/api/v1/alerts/platform:1" ? { id: "platform:1", source: "platform", component: "plinth", reason: "runtime_control_stream_disconnected", state: "Resolved", rowVersion: 2, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:02:00Z", resolvedAt: "2026-01-01T00:02:00Z", labels: { alertname: "Plinth disconnected" }, annotations: { summary: "运行通道断开" } } : { snapshotSeq: 2, items: [] } }));
    vi.stubGlobal("fetch", fetchMock);
    render(<View route="/alerts/list?id=platform:1" />);
    await screen.findByRole("heading", { name: "Plinth disconnected" });
    expect(screen.getByText("发生时间")).toBeInTheDocument();
    expect(screen.getByText("恢复时间")).toBeInTheDocument();
    expect(screen.getByText("此平台故障没有业务采集声明，不支持初步分析。不会启动业务采集。")).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "AI 分析" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "时间线" })).not.toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([url]) => /observations|analyses/.test(String(url)))).toBe(false);
  });

  it("renders the history view when the route includes its query string", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ items: [] }) }));
    render(<View route="/alerts/list?view=history" />);
    expect(await screen.findByRole("heading", { name: "告警历史" })).toBeInTheDocument();
  });

  it("keeps live updates on by default without normal refresh controls", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ snapshotSeq: 1, items: [] }) }));
    render(<View route="/alerts/list" />);

    await screen.findByRole("heading", { name: "当前告警" });
    expect(screen.queryByRole("button", { name: "刷新" })).not.toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "自动刷新" })).not.toBeInTheDocument();
  });

  it("re-reads an open detail when the shared SSE boundary reports a newer version", async () => {
    const sources: { listeners: Map<string, ((event: Event) => void)[]>; emit: (type: string, data: string) => void }[] = [];
    vi.stubGlobal("EventSource", class {
      static readonly CONNECTING = 0;
      static readonly CLOSED = 2;
      readyState = 0;
      onerror: ((event: Event) => void) | null = null;
      listeners = new Map<string, ((event: Event) => void)[]>();
      constructor(url: string) { void url; sources.push(this); }
      addEventListener(type: string, listener: (event: Event) => void) { this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]); }
      close() { this.readyState = 2; }
      emit(type: string, data: string) { for (const listener of this.listeners.get(type) ?? []) listener(new MessageEvent(type, { data })); }
    });
    let detailVersion = 1;
    const fetchMock = vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ snapshotSeq: 5, items: [] }) });
      if (input === "/api/v1/alerts/platform:1") return Promise.resolve({ ok: true, json: async () => ({ id: "platform:1", source: "platform", component: "plinth", state: detailVersion === 1 ? "Firing" : "Resolved", rowVersion: detailVersion, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:02:00Z", resolvedAt: detailVersion === 1 ? undefined : "2026-01-01T00:02:00Z", labels: { alertname: "Plinth disconnected" } }) });
      return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<View route="/alerts/list?id=platform:1" />);

    await screen.findByRole("heading", { name: "Plinth disconnected" });
    await waitFor(() => expect(sources).toHaveLength(1));
    detailVersion = 2;
    sources[0].emit("change", JSON.stringify({ seq: "6", type: "state_changed", occurrenceId: "platform:1", rowVersion: 2 }));

    await screen.findByText("Resolved");
    // Both list and open-detail projections reconcile through the same stream;
    // the detail must therefore issue its own authoritative re-read as well.
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => url === "/api/v1/alerts/platform:1").length).toBeGreaterThan(1));
    expect(sources).toHaveLength(1);
  });

  it("re-snapshots both tab counts after a live occurrence enters then leaves the current list", async () => {
    const sources: { listeners: Map<string, ((event: Event) => void)[]>; emit: (type: string, data: string) => void }[] = [];
    vi.stubGlobal("EventSource", class {
      static readonly CONNECTING = 0;
      static readonly CLOSED = 2;
      readyState = 0;
      onerror: ((event: Event) => void) | null = null;
      listeners = new Map<string, ((event: Event) => void)[]>();
      constructor(url: string) { void url; sources.push(this); }
      addEventListener(type: string, listener: (event: Event) => void) { this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]); }
      close() { this.readyState = 2; }
      emit(type: string, data: string) { for (const listener of this.listeners.get(type) ?? []) listener(new MessageEvent(type, { data })); }
    });
    let state: "Firing" | "Resolved" = "Firing";
    const occurrence = () => ({ id: "platform:1", source: "platform", component: "plinth", state, rowVersion: state === "Firing" ? 1 : 2, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:02:00Z", resolvedAt: state === "Resolved" ? "2026-01-01T00:02:00Z" : undefined, labels: { alertname: "Plinth disconnected" } });
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: string) => {
      if (input.includes("state=Firing")) return Promise.resolve({ ok: true, json: async () => ({ snapshotSeq: 5, items: state === "Firing" ? [occurrence()] : [] }) });
      if (input.includes("state=Resolved")) return Promise.resolve({ ok: true, json: async () => ({ snapshotSeq: 5, items: state === "Resolved" ? [occurrence()] : [] }) });
      if (input === "/api/v1/alerts/platform:1") return Promise.resolve({ ok: true, json: async () => occurrence() });
      return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
    }));
    render(<View route="/alerts/list" />);

    await screen.findByText("Plinth disconnected");
    expect(screen.getByRole("tab", { name: "当前告警1" })).toBeInTheDocument();
    state = "Resolved";
    sources[0].emit("change", JSON.stringify({ seq: "6", type: "state_changed", occurrenceId: "platform:1", rowVersion: 2 }));

    await waitFor(() => expect(screen.queryByText("Plinth disconnected")).not.toBeInTheDocument());
    await screen.findByRole("tab", { name: "当前告警0" });
    expect(screen.getByRole("tab", { name: "历史告警1" })).toBeInTheDocument();
  });

  it("retries a failed initial list fetch through EntityList while keeping SSE enabled", async () => {
    const eventSources: unknown[] = [];
    vi.stubGlobal("EventSource", class {
      static readonly CONNECTING = 0;
      static readonly CLOSED = 2;
      readyState = 0;
      onerror: ((event: Event) => void) | null = null;
      constructor(url: string) { void url; eventSources.push(this); }
      addEventListener(type: string, listener: (event: Event) => void) { void type; void listener; /* Test double only needs a live source boundary. */ }
      close() { this.readyState = 2; }
    });
    let firingListReads = 0;
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: string) => {
      if (input.includes("/api/v1/alerts?state=Firing")) {
        firingListReads += 1;
        return Promise.resolve({ ok: firingListReads > 1, json: async () => ({ snapshotSeq: 2, items: [] }) });
      }
      return Promise.resolve({ ok: true, json: async () => ({ snapshotSeq: 2, items: [] }) });
    }));
    render(<View route="/alerts/list" />);

    const readsBeforeRetry = firingListReads;
    fireEvent.click(await screen.findByRole("button", { name: "重试" }));
    await waitFor(() => expect(firingListReads).toBeGreaterThan(readsBeforeRetry));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(screen.getByText("当前没有告警")).toBeInTheDocument();
    expect(eventSources).toHaveLength(1);
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

	it("uses semantic drawer primitives for overview, timeline, evidence, and collapsed execution records", async () => {
		const openEvidence = vi.fn();
		const fetchMock = vi.fn().mockImplementation((input: string) => {
			if (input.includes("/api/v1/alerts?")) return Promise.resolve({ ok: true, json: async () => ({ items: [] }) });
			if (input.includes("/observations")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "observation-1", observedState: "firing", effect: "initial_firing", startsAt: "2026-01-01T00:00:00Z", receivedAt: "2026-01-01T00:00:00Z", committedAt: "2026-01-01T00:01:00Z" }] }) });
			if (input.includes("/attempts")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "attempt-1", type: "initial", state: "Succeeded", terminationReason: "completed" }] }) });
			if (input.endsWith("/analyses")) return Promise.resolve({ ok: true, json: async () => ({ items: [{ id: "analysis-1", state: "Succeeded", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z" }] }) });
			if (input.includes("/analyses/")) return Promise.resolve({ ok: true, json: async () => ({ id: "analysis-1", state: "Succeeded", rowVersion: 1, createdAt: "2026-01-01T00:00:00Z", attemptCount: 1, output: { id: "output", modelId: "demo", content: "确认结论", evidenceIds: ["evidence-1"], createdAt: "2026-01-01T00:00:00Z" } }) });
			return Promise.resolve({ ok: true, json: async () => ({ id: "alert-1", state: "Firing", rowVersion: 1, firstSeenAt: "2026-01-01T00:00:00Z", lastStateChangeAt: "2026-01-01T00:00:00Z", labels: { alertname: "Example", instance: "node-1", severity: "critical" }, annotations: { description: "Readable description", runbook: "https://runbook.invalid/long-path" } }) });
		});
		vi.stubGlobal("fetch", fetchMock);
		render(<View route="/alerts/list?id=alert-1" openEvidence={openEvidence} />);
		await screen.findByRole("heading", { name: "Example" });
		expect(screen.getByText("node-1")).toHaveClass("font-mono");
		expect(screen.getByText("critical")).toHaveClass("bg-destructive");
		expect(screen.getByText("Firing")).toHaveClass("bg-destructive");
		fireEvent.mouseDown(screen.getByRole("tab", { name: "时间线" }));
		fireEvent.click(screen.getByRole("tab", { name: "时间线" }));
		expect(await screen.findByRole("list", { name: "观察记录时间线" })).toBeInTheDocument();
		expect(screen.getByText("firing")).toBeInTheDocument();
		fireEvent.mouseDown(screen.getByRole("tab", { name: "AI 分析" }));
		fireEvent.click(screen.getByRole("tab", { name: "AI 分析" }));
		await screen.findByText("确认结论");
		const evidence = screen.getByRole("button", { name: "证据 evidence-1 查看证据" });
		expect(evidence).toHaveClass("w-full");
		fireEvent.click(evidence);
		expect(openEvidence).toHaveBeenCalledWith("evidence-1");
		const executionRecords = screen.getByRole("button", { name: "执行记录" });
		expect(executionRecords).toHaveAttribute("aria-expanded", "false");
		expect(screen.queryByText("initial")).not.toBeInTheDocument();
		fireEvent.click(executionRecords);
		expect(executionRecords).toHaveAttribute("aria-expanded", "true");
		expect(await screen.findByText("initial")).toBeInTheDocument();
		expect(screen.getByText("Succeeded")).toBeInTheDocument();
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
