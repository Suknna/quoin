import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { CandidateSummary, ImportBatchSummary } from "@/features/knowledge/api";
import { useKnowledgeModule } from "./index";

const user = { id: "1", username: "a", displayName: "A", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 } as const;

/** Renders the module view the way the workspace does, exposing the navigate spy. */
function renderView({ route = "/knowledge", suspended = false }: { route?: string; suspended?: boolean } = {}) {
  const navigate = vi.fn();
  function Harness({ suspended: nowSuspended = suspended, route: nowRoute = route }: { suspended?: boolean; route?: string } = {}) {
    const view = useKnowledgeModule({ user, route: nowRoute, suspended: nowSuspended, navigate, openEvidence: vi.fn() });
    return <>{view.list}{view.content}</>;
  }
  const view = render(<Harness />);
  return {
    navigate,
    unmount: () => view.unmount(),
    rerender: (next: { route?: string; suspended?: boolean }) => view.rerender(<Harness {...next} />),
  };
}

const candidate = (id: string, draftTitle?: string): CandidateSummary => ({ id, sourceType: "source_material", sourceId: "s1", state: "AwaitingConfirmation", rowVersion: 1, generation: 1, draftRevision: 1, draftTitle });
const batch = (id: string, state: ImportBatchSummary["state"] = "AwaitingConfirmation"): ImportBatchSummary => ({ id, state, rowVersion: 1, generation: 1, createdAt: "2026-09-01T00:00:00Z" });

/** A promise the test settles by hand, to model slow server responses. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((settle) => { resolve = settle; });
  return { promise, resolve };
}

vi.mock("@/features/knowledge/api", () => ({
  api: { browse: vi.fn().mockResolvedValue({ items: [], nextCursor: undefined }), search: vi.fn(), getCandidate: vi.fn(), editDraft: vi.fn(), confirm: vi.fn(), exclude: vi.fn(), getKnowledge: vi.fn(), listVersions: vi.fn(), getVersion: vi.fn(), createRevision: vi.fn(), stopReuse: vi.fn(), startImport: vi.fn(), getImportBatch: vi.fn(), confirmBatch: vi.fn(), cancelBatch: vi.fn(), listCandidates: vi.fn().mockResolvedValue({ items: [] }), listImportBatches: vi.fn().mockResolvedValue({ items: [] }) },
  candidateSourceLabels: { source_material: "导入原文" }, candidateStateLabels: { AwaitingConfirmation: "待确认" }, batchStateLabels: { Processing: "处理中", AwaitingConfirmation: "待确认", Completed: "已完成", Cancelled: "已取消", Failed: "失败" }, embeddingStateLabels: {}, indexStateLabels: {},
}));
function View({ route = "/knowledge", suspended = false }: { route?: string; suspended?: boolean }) { const view = useKnowledgeModule({ user: { id: "1", username: "a", displayName: "A", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 }, route, suspended, navigate: vi.fn(), openEvidence: vi.fn() }); return <>{view.list}{view.content}</>; }
afterEach(() => { cleanup(); vi.clearAllMocks(); });
describe("knowledge module", () => { it("routes absolute knowledge candidates into the editor", async () => { const { api } = await import("@/features/knowledge/api"); vi.mocked(api.getCandidate).mockResolvedValue({ id: "c1", sourceType: "source_material", sourceId: "s1", state: "AwaitingConfirmation", rowVersion: 1, generation: 1, draftRevision: 1, draftTitle: "候选", draftBody: "正文", originalSuggestion: { v: 1, source: { type: "source_material", id: "s1" }, title: "候选", body: "正文" } }); render(<View route="/knowledge/candidates/c1" />); expect(await screen.findByText("编辑知识候选")).toBeInTheDocument(); }); it("shows the real search distinction", async () => { const { api } = await import("@/features/knowledge/api"); vi.mocked(api.search).mockResolvedValue({ mode: "query", exactTextMatches: [{ knowledge: { id: "1", title: "全文", currentVersionId: "v", currentVersionSeq: 1, eligible: true, rowVersion: 1 }, score: 1 }], semanticMatches: [{ knowledge: { id: "2", title: "语义", currentVersionId: "v2", currentVersionSeq: 1, eligible: true, rowVersion: 1 }, score: 0.7, indexState: "ready" }] }); render(<View />); fireEvent.change(await screen.findByLabelText("检索知识"), { target: { value: "cpu" } }); fireEvent.click(screen.getByRole("button", { name: "搜索" })); expect(await screen.findByText("全文匹配")).toBeInTheDocument(); expect(screen.getByText("语义相似")).toBeInTheDocument(); }); it("stops import polling while suspended", async () => { vi.useFakeTimers(); const { api } = await import("@/features/knowledge/api"); vi.mocked(api.getImportBatch).mockResolvedValue({ id: "b", state: "Processing", rowVersion: 1, generation: 1, createdAt: "now", candidates: [] }); render(<View route="/imports/b" suspended />); await act(async () => {}); await act(async () => { vi.advanceTimersByTime(6000); }); expect(api.getImportBatch).toHaveBeenCalledTimes(1); vi.useRealTimers(); }); });

describe("knowledge sidebar lists (candidates / import batches)", () => {
  it("renders first pages of both lists, reads exactly one cursor-less page per list, and navigates on select", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates).mockResolvedValue({ items: [candidate("c1", "候选一")] });
    vi.mocked(api.listImportBatches).mockResolvedValue({ items: [batch("b1")] });
    const { navigate } = renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    expect(screen.getByText("导入 b1")).toBeInTheDocument();
    expect(api.listCandidates).toHaveBeenCalledTimes(1);
    expect(api.listImportBatches).toHaveBeenCalledTimes(1);
    // First read is a cursor-less first page (CONTEXT 工作台展示约定：首次只读取一页).
    expect(vi.mocked(api.listCandidates).mock.calls[0]?.[1]).toBeUndefined();
    expect(vi.mocked(api.listImportBatches).mock.calls[0]?.[0]).toBeUndefined();
    fireEvent.click(screen.getByText("候选一"));
    expect(navigate).toHaveBeenCalledWith("/knowledge/candidates/c1");
    fireEvent.click(screen.getByText("导入 b1"));
    expect(navigate).toHaveBeenCalledWith("/knowledge/imports/b1");
  });

  it("shows each list's empty state independently", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates).mockResolvedValue({ items: [] });
    vi.mocked(api.listImportBatches).mockResolvedValue({ items: [] });
    renderView();
    expect(await screen.findByText("暂无待确认候选。")).toBeInTheDocument();
    expect(screen.getByText("暂无导入批次。")).toBeInTheDocument();
  });

  it("appends the candidates second page through its own cursor and stops at the end", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates)
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一")], nextCursor: "cand-2" })
      .mockResolvedValueOnce({ items: [candidate("c2", "候选二")] });
    renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "加载更多候选" }));
    expect(await screen.findByText("候选二")).toBeInTheDocument();
    expect(api.listCandidates).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.listCandidates).mock.calls[1]?.[1]).toBe("cand-2");
    // 最后一页没有 nextCursor，按钮消失（CONTEXT：不伪造页码）。
    expect(screen.queryByRole("button", { name: "加载更多候选" })).not.toBeInTheDocument();
  });

  it("pages import batches through their own cursor without disturbing the candidates list", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates).mockResolvedValue({ items: [candidate("c1", "候选一")] });
    vi.mocked(api.listImportBatches)
      .mockResolvedValueOnce({ items: [batch("b1")], nextCursor: "batch-2" })
      .mockResolvedValueOnce({ items: [batch("b2")] });
    renderView();
    expect(await screen.findByText("导入 b1")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "加载更多批次" }));
    expect(await screen.findByText("导入 b2")).toBeInTheDocument();
    expect(api.listImportBatches).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.listImportBatches).mock.calls[1]?.[0]).toBe("batch-2");
    // 批次翻页不会重读或改动候选列表（独立游标）。
    expect(api.listCandidates).toHaveBeenCalledTimes(1);
    expect(screen.getByText("候选一")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "加载更多批次" })).not.toBeInTheDocument();
  });

  it("isolates first-page failures per list and recovers through retry", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates).mockResolvedValue({ items: [candidate("c1", "候选一")] });
    // 非 Error 拒绝值走 messageOf 的 fallback 文案。
    vi.mocked(api.listImportBatches)
      .mockRejectedValueOnce("network down")
      .mockResolvedValueOnce({ items: [batch("b1")] });
    renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    expect(await screen.findByText("无法读取导入批次。")).toBeInTheDocument();
    // 批次错误只属于批次列表。
    expect(screen.queryByText("无法读取待确认知识。")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("导入 b1")).toBeInTheDocument();
    expect(screen.queryByText("无法读取导入批次。")).not.toBeInTheDocument();
  });

  it("keeps loaded rows when a load-more fails and retries the same cursor", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates)
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一")], nextCursor: "cand-2" })
      .mockRejectedValueOnce("network down")
      .mockResolvedValueOnce({ items: [candidate("c2", "候选二")] });
    renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "加载更多候选" }));
    // 追加失败不吞掉已加载的行，也不再吃掉游标。
    expect(await screen.findByText("无法读取待确认知识。")).toBeInTheDocument();
    expect(screen.getByText("候选一")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("候选二")).toBeInTheDocument();
    expect(api.listCandidates).toHaveBeenCalledTimes(3);
    expect(vi.mocked(api.listCandidates).mock.calls[2]?.[1]).toBe("cand-2");
    expect(screen.queryByText("无法读取待确认知识。")).not.toBeInTheDocument();
  });

  it("deduplicates appended pages by id so cursor overlap renders one row", async () => {
    const { api } = await import("@/features/knowledge/api");
    // 游标窗口重叠（服务端在前部插入了新行）会让下一页重复带回已见条目。
    vi.mocked(api.listCandidates)
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一")], nextCursor: "cand-2" })
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一"), candidate("c2", "候选二")] });
    renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "加载更多候选" }));
    expect(await screen.findByText("候选二")).toBeInTheDocument();
    expect(screen.getAllByText("候选一")).toHaveLength(1);
  });

  it("deduplicates duplicate ids within one appended page", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates)
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一")], nextCursor: "cand-2" })
      .mockResolvedValueOnce({ items: [candidate("c2", "候选二"), candidate("c2", "候选二")] });
    renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "加载更多候选" }));
    await waitFor(() => expect(screen.getAllByText("候选二")).toHaveLength(1));
  });

  it("renders the fast batches page while the candidates read is slow, without cross-talk", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listImportBatches).mockResolvedValue({ items: [batch("b1")] });
    const slowCandidates = deferred<{ items: unknown[] }>();
    vi.mocked(api.listCandidates).mockReturnValueOnce(slowCandidates.promise as never);
    renderView();
    expect(await screen.findByText("导入 b1")).toBeInTheDocument();
    // 候选还在读取骨架中；批次列表先行渲染且不被候选请求牵连。
    expect(screen.getByRole("status", { name: "正在读取待确认候选" })).toBeInTheDocument();
    await act(async () => { slowCandidates.resolve({ items: [candidate("c1", "候选一")] }); });
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    expect(screen.getByText("导入 b1")).toBeInTheDocument();
    expect(api.listImportBatches).toHaveBeenCalledTimes(1);
  });

  it("drops in-flight sidebar reads after unmount so a fresh mount starts clean", async () => {
    const { api } = await import("@/features/knowledge/api");
    const staleRead = deferred<{ items: unknown[] }>();
    vi.mocked(api.listCandidates).mockReturnValueOnce(staleRead.promise as never);
    const first = renderView();
    first.unmount();
    await act(async () => { staleRead.resolve({ items: [candidate("c9", "卸载后迟到候选")] }); });
    // 卸载后的迟到响应不进入新挂载实例的状态。
    vi.mocked(api.listCandidates).mockResolvedValue({ items: [candidate("c1", "新挂载候选")] });
    renderView();
    expect(await screen.findByText("新挂载候选")).toBeInTheDocument();
    expect(screen.queryByText("卸载后迟到候选")).not.toBeInTheDocument();
  });

  it("pauses sidebar reads while suspended, drops late responses, and re-reads on resume", async () => {
    const { api } = await import("@/features/knowledge/api");
    const suspendedRead = deferred<{ items: unknown[] }>();
    vi.mocked(api.listCandidates).mockReturnValueOnce(suspendedRead.promise as never);
    const { rerender } = renderView();
    expect(screen.getByRole("status", { name: "正在读取待确认候选" })).toBeInTheDocument();
    rerender({ suspended: true });
    // 挂起把在途请求作废：迟到数据不得进入状态。
    await act(async () => { suspendedRead.resolve({ items: [candidate("c9", "挂起期间迟到候选")] }); });
    expect(screen.queryByText("挂起期间迟到候选")).not.toBeInTheDocument();
    // 恢复后重新读取第一页，而不是沿用挂起前的旧响应。
    const freshRead = deferred<{ items: unknown[] }>();
    vi.mocked(api.listCandidates).mockReturnValueOnce(freshRead.promise as never);
    rerender({ suspended: false });
    await act(async () => { freshRead.resolve({ items: [candidate("c1", "恢复后候选")] }); });
    expect(await screen.findByText("恢复后候选")).toBeInTheDocument();
  });

  it("does not start sidebar reads while the workbench is suspended", async () => {
    const { api } = await import("@/features/knowledge/api");
    renderView({ suspended: true });
    await act(async () => {});
    expect(api.listCandidates).not.toHaveBeenCalled();
    expect(api.listImportBatches).not.toHaveBeenCalled();
  });

  it("hides load-more and retry affordances while suspended, keeping the error facts", async () => {
    const { api } = await import("@/features/knowledge/api");
    vi.mocked(api.listCandidates)
      .mockResolvedValueOnce({ items: [candidate("c1", "候选一")], nextCursor: "cand-2" })
      .mockRejectedValueOnce("network down");
    vi.mocked(api.listImportBatches).mockRejectedValue("network down");
    const { rerender } = renderView();
    expect(await screen.findByText("候选一")).toBeInTheDocument();
    // 批次初载失败（EntityList 内重试）与候选追加失败（ErrorRetry）都给出操作入口。
    fireEvent.click(screen.getByRole("button", { name: "加载更多候选" }));
    expect(await screen.findByText("无法读取待确认知识。")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "重试" }).length).toBeGreaterThan(0);
    rerender({ suspended: true });
    // 挂起=只读：错误事实保留，但加载更多与全部重试入口不再可点。
    expect(screen.queryByRole("button", { name: "加载更多候选" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "重试" })).not.toBeInTheDocument();
    expect(screen.getByText("无法读取待确认知识。")).toBeInTheDocument();
    expect(screen.getByText("无法读取导入批次。")).toBeInTheDocument();
  });
});
