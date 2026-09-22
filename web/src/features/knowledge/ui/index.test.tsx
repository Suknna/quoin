import "@testing-library/jest-dom/vitest";
import {
	act,
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type {
	CandidateDetail,
	CandidateSummary,
	ImportBatchDetail,
	ImportBatchSummary,
	KnowledgeDetail,
	KnowledgeVersionDetail,
	KnowledgeVersionSummary,
} from "@/features/knowledge/api";
import { useKnowledgeModule } from "./index";

const user = {
	id: "1",
	username: "a",
	displayName: "A",
	role: "admin",
	passwordChangeRequired: false,
	authRevision: 1,
	enabled: true,
	initialized: true,
	lastLoginAt: null,
	rowVersion: 1,
} as const;

/** Renders the module view the way the workspace does, exposing the navigate spy. */
function renderView({
	route = "/knowledge",
	suspended = false,
}: {
	route?: string;
	suspended?: boolean;
} = {}) {
	const navigate = vi.fn();
	function Harness({
		suspended: nowSuspended = suspended,
		route: nowRoute = route,
	}: {
		suspended?: boolean;
		route?: string;
	} = {}) {
		const view = useKnowledgeModule({
			user,
			route: nowRoute,
			suspended: nowSuspended,
			navigate,
			openEvidence: vi.fn(),
		});
		return (
			<>
				{view.list}
				{view.content}
			</>
		);
	}
	const view = render(<Harness />);
	return {
		navigate,
		unmount: () => view.unmount(),
		rerender: (next: { route?: string; suspended?: boolean }) =>
			view.rerender(<Harness {...next} />),
	};
}

const candidate = (id: string, draftTitle?: string): CandidateSummary => ({
	id,
	sourceType: "source_material",
	sourceId: "s1",
	state: "AwaitingConfirmation",
	rowVersion: 1,
	generation: 1,
	draftRevision: 1,
	draftTitle,
});
const batch = (
	id: string,
	state: ImportBatchSummary["state"] = "AwaitingConfirmation",
): ImportBatchSummary => ({
	id,
	state,
	rowVersion: 1,
	generation: 1,
	createdAt: "2026-09-01T00:00:00.000Z",
});
const knowledgeDetail = (
	over: Partial<KnowledgeDetail> = {},
): KnowledgeDetail => ({
	id: "k1",
	title: "结算延迟排查",
	currentVersionId: "v1",
	currentVersionSeq: 1,
	eligible: true,
	rowVersion: 1,
	versionCount: 1,
	...over,
});
const versionDetail = (
	over: Partial<KnowledgeVersionDetail> = {},
): KnowledgeVersionDetail => ({
	id: "v1",
	versionSeq: 1,
	title: "结算延迟排查",
	body: "先检查 payment 服务和连接池。",
	sourceCandidateId: "c1",
	createdAt: "2026-09-01T00:00:00.000Z",
	eligible: true,
	retrievalStateRowVersion: 1,
	embeddingState: "ready",
	...over,
});
const versionSummary = (
	over: Partial<KnowledgeVersionSummary> = {},
): KnowledgeVersionSummary => ({
	id: "v1",
	versionSeq: 1,
	title: "结算延迟排查",
	sourceCandidateId: "c1",
	embeddingState: "ready",
	createdAt: "2026-09-01T00:00:00.000Z",
	eligible: true,
	retrievalStateRowVersion: 1,
	...over,
});
const candidateDetail = (
	over: Partial<CandidateDetail> = {},
): CandidateDetail => ({
	id: "c1",
	sourceType: "source_material",
	sourceId: "s1",
	state: "AwaitingConfirmation",
	rowVersion: 1,
	generation: 1,
	draftRevision: 1,
	draftTitle: "候选",
	draftBody: "正文",
	originalSuggestion: {
		v: 1,
		source: { type: "source_material", id: "s1" },
		title: "原始标题",
		body: "原始正文",
	},
	...over,
});

/** A promise the test settles by hand, to model slow server responses. */
function deferred<T>() {
	let resolve!: (value: T) => void;
	let reject!: (reason?: unknown) => void;
	const promise = new Promise<T>((settle, fail) => {
		resolve = settle;
		reject = fail;
	});
	return { promise, resolve, reject };
}

vi.mock("@/features/knowledge/api", async (importOriginal) => {
	const actual =
		await importOriginal<typeof import("@/features/knowledge/api")>();
	return {
		...actual,
		api: {
			browse: vi.fn().mockResolvedValue({
				mode: "browse",
				items: [],
				nextCursor: undefined,
			}),
			search: vi.fn(),
			getCandidate: vi.fn(),
			editDraft: vi.fn(),
			confirm: vi.fn(),
			exclude: vi.fn(),
			getKnowledge: vi.fn(),
			listVersions: vi.fn().mockResolvedValue({ items: [] }),
			getVersion: vi.fn(),
			createRevision: vi.fn(),
			stopReuse: vi.fn(),
			startImport: vi.fn(),
			getImportBatch: vi.fn(),
			confirmBatch: vi.fn(),
			cancelBatch: vi.fn(),
			listCandidates: vi.fn().mockResolvedValue({ items: [] }),
			listImportBatches: vi.fn().mockResolvedValue({ items: [] }),
		},
	};
});

beforeEach(async () => {
	const { api } = await import("@/features/knowledge/api");
	vi.mocked(api.browse).mockResolvedValue({
		mode: "browse",
		items: [],
		nextCursor: undefined,
	});
	vi.mocked(api.search).mockResolvedValue({
		mode: "query",
		exactTextMatches: [],
		semanticMatches: [],
	});
	vi.mocked(api.listCandidates).mockResolvedValue({ items: [] });
	vi.mocked(api.listImportBatches).mockResolvedValue({ items: [] });
	vi.mocked(api.listVersions).mockResolvedValue({ items: [] });
});
afterEach(() => {
	cleanup();
	vi.clearAllMocks();
});

describe("knowledge section navigation", () => {
	it("renders the three sections and navigates between them", async () => {
		const { navigate } = renderView();
		expect(
			await screen.findByRole("button", { name: "知识库" }),
		).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "待确认" }));
		expect(navigate).toHaveBeenCalledWith("/knowledge/candidates");
		fireEvent.click(screen.getByRole("button", { name: "导入批次" }));
		expect(navigate).toHaveBeenCalledWith("/knowledge/imports");
	});
});

describe("knowledge home (browse + search)", () => {
	it("renders browse rows and opens the detail drawer", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.browse).mockResolvedValue({
			mode: "browse",
			items: [
				{
					id: "k1",
					title: "结算延迟排查",
					currentVersionId: "v1",
					currentVersionSeq: 2,
					eligible: true,
					rowVersion: 1,
				},
			],
			nextCursor: undefined,
		});
		const { navigate } = renderView();
		fireEvent.click(await screen.findByText("结算延迟排查"));
		expect(navigate).toHaveBeenCalledWith("/knowledge?item=k1");
	});

	it("shows the empty state with guidance", async () => {
		renderView();
		expect(await screen.findByText("知识库还是空的")).toBeInTheDocument();
	});

	it("shows exact and semantic groups, annotating knowledge that hit both channels once", async () => {
		const { api } = await import("@/features/knowledge/api");
		const k = (id: string, title: string) => ({
			id,
			title,
			currentVersionId: "v",
			currentVersionSeq: 1,
			eligible: true,
			rowVersion: 1,
		});
		vi.mocked(api.search).mockResolvedValue({
			mode: "query",
			exactTextMatches: [{ knowledge: k("1", "双命中"), score: 1 }],
			semanticMatches: [
				{ knowledge: k("1", "双命中"), score: 0.81, indexState: "ready" },
				{ knowledge: k("2", "仅语义"), score: 0.7, indexState: "stale" },
			],
		});
		renderView();
		fireEvent.change(await screen.findByLabelText("检索知识"), {
			target: { value: "cpu" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索" }));
		expect(await screen.findByText("全文匹配")).toBeInTheDocument();
		expect(screen.getByText("语义相似")).toBeInTheDocument();
		// 双命中只在全文组出现一次,并标注语义依据;语义组只剩“仅语义”。
		expect(screen.getAllByText("双命中")).toHaveLength(1);
		expect(screen.getByText(/语义相似 0\.81/)).toBeInTheDocument();
		expect(screen.getByText("仅语义")).toBeInTheDocument();
		expect(screen.getByText(/语义索引已换代/)).toBeInTheDocument();
		expect(api.search).toHaveBeenCalledWith("cpu", undefined);
	});

	it("repeats an unchanged search instead of retaining stale eligibility", async () => {
		const { api } = await import("@/features/knowledge/api");
		renderView();
		fireEvent.change(await screen.findByLabelText("检索知识"), {
			target: { value: "backup" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索" }));
		await waitFor(() => expect(api.search).toHaveBeenCalledTimes(1));
		fireEvent.click(screen.getByRole("button", { name: "搜索" }));
		await waitFor(() => expect(api.search).toHaveBeenCalledTimes(2));
	});

	it("clears search back to the browse table", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.search).mockResolvedValue({
			mode: "query",
			exactTextMatches: [],
			semanticMatches: [],
		});
		renderView();
		fireEvent.change(await screen.findByLabelText("检索知识"), {
			target: { value: "cpu" },
		});
		fireEvent.click(screen.getByRole("button", { name: "搜索" }));
		expect(await screen.findByText(/没有匹配的知识/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "清空搜索" }));
		expect(await screen.findByText("知识库还是空的")).toBeInTheDocument();
	});
});

describe("candidates page", () => {
	it("defaults to the awaiting filter, pages by cursor, and navigates to the editor", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.listCandidates)
			.mockResolvedValueOnce({
				items: [candidate("c1", "候选一")],
				nextCursor: "cand-2",
			})
			.mockResolvedValueOnce({ items: [candidate("c2", "候选二")] });
		const { navigate } = renderView({ route: "/knowledge/candidates" });
		expect(await screen.findByText("候选一")).toBeInTheDocument();
		expect(vi.mocked(api.listCandidates).mock.calls[0]?.[0]).toEqual({
			state: "AwaitingConfirmation",
			sourceType: undefined,
		});
		expect(vi.mocked(api.listCandidates).mock.calls[0]?.[1]).toBeUndefined();
		fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
		expect(await screen.findByText("候选二")).toBeInTheDocument();
		expect(vi.mocked(api.listCandidates).mock.calls[1]?.[1]).toBe("cand-2");
		fireEvent.click(screen.getByText("候选二"));
		expect(navigate).toHaveBeenCalledWith("/knowledge/candidates/c2");
	});

	it("re-reads the first page when the state filter changes", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.listCandidates).mockResolvedValue({ items: [] });
		renderView({ route: "/knowledge/candidates" });
		await screen.findByText("没有待确认的候选");
		fireEvent.click(screen.getByRole("combobox", { name: "按状态过滤" }));
		fireEvent.click(await screen.findByRole("option", { name: "全部状态" }));
		await waitFor(() => {
			expect(vi.mocked(api.listCandidates).mock.calls.at(-1)?.[0]).toEqual({
				state: undefined,
				sourceType: undefined,
			});
		});
	});

	it("keeps loaded rows when a load-more fails and retries the same cursor", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.listCandidates)
			.mockResolvedValueOnce({
				items: [candidate("c1", "候选一")],
				nextCursor: "cand-2",
			})
			.mockRejectedValueOnce("network down")
			.mockResolvedValueOnce({ items: [candidate("c2", "候选二")] });
		renderView({ route: "/knowledge/candidates" });
		expect(await screen.findByText("候选一")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
		expect(await screen.findByText("无法读取知识候选。")).toBeInTheDocument();
		expect(screen.getByText("候选一")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "重试" }));
		expect(await screen.findByText("候选二")).toBeInTheDocument();
		expect(vi.mocked(api.listCandidates).mock.calls[2]?.[1]).toBe("cand-2");
	});

	it("deduplicates appended pages by id so cursor overlap renders one row", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.listCandidates)
			.mockResolvedValueOnce({
				items: [candidate("c1", "候选一")],
				nextCursor: "cand-2",
			})
			.mockResolvedValueOnce({
				items: [candidate("c1", "候选一"), candidate("c2", "候选二")],
			});
		renderView({ route: "/knowledge/candidates" });
		expect(await screen.findByText("候选一")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
		expect(await screen.findByText("候选二")).toBeInTheDocument();
		expect(screen.getAllByText("候选一")).toHaveLength(1);
	});

	it("drops in-flight reads after unmount so a fresh mount starts clean", async () => {
		const { api } = await import("@/features/knowledge/api");
		const staleRead = deferred<{ items: CandidateSummary[] }>();
		vi.mocked(api.listCandidates).mockReturnValueOnce(staleRead.promise);
		const first = renderView({ route: "/knowledge/candidates" });
		first.unmount();
		await act(async () => {
			staleRead.resolve({ items: [candidate("c9", "卸载后迟到候选")] });
		});
		vi.mocked(api.listCandidates).mockResolvedValue({
			items: [candidate("c1", "新挂载候选")],
		});
		renderView({ route: "/knowledge/candidates" });
		expect(await screen.findByText("新挂载候选")).toBeInTheDocument();
		expect(screen.queryByText("卸载后迟到候选")).not.toBeInTheDocument();
	});

	it("pauses reads while suspended, drops late responses, and re-reads on resume", async () => {
		const { api } = await import("@/features/knowledge/api");
		const suspendedRead = deferred<{ items: CandidateSummary[] }>();
		vi.mocked(api.listCandidates).mockReturnValueOnce(suspendedRead.promise);
		const { rerender } = renderView({ route: "/knowledge/candidates" });
		rerender({ suspended: true });
		await act(async () => {
			suspendedRead.resolve({ items: [candidate("c9", "挂起期间迟到候选")] });
		});
		expect(screen.queryByText("挂起期间迟到候选")).not.toBeInTheDocument();
		const freshRead = deferred<{ items: CandidateSummary[] }>();
		vi.mocked(api.listCandidates).mockReturnValueOnce(freshRead.promise);
		rerender({ suspended: false });
		await act(async () => {
			freshRead.resolve({ items: [candidate("c1", "恢复后候选")] });
		});
		expect(await screen.findByText("恢复后候选")).toBeInTheDocument();
	});

	it("does not start reads while the workbench is suspended", async () => {
		const { api } = await import("@/features/knowledge/api");
		renderView({ route: "/knowledge/candidates", suspended: true });
		await act(async () => {});
		expect(api.listCandidates).not.toHaveBeenCalled();
	});
});

describe("candidate editor", () => {
	it("loads the draft, saves edits, and confirms into knowledge", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
		vi.mocked(api.editDraft).mockResolvedValue(candidate("c1"));
		vi.mocked(api.confirm).mockResolvedValue({
			...candidate("c1"),
			state: "Confirmed",
			confirmedKnowledgeId: "k9",
		});
		const { navigate } = renderView({ route: "/knowledge/candidates/c1" });
		expect(await screen.findByText("编辑知识候选")).toBeInTheDocument();
		fireEvent.change(screen.getByLabelText("标题"), {
			target: { value: "新标题" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		await waitFor(() =>
			expect(api.editDraft).toHaveBeenCalledWith("c1", 1, {
				title: "新标题",
				body: "正文",
			}),
		);
		fireEvent.click(screen.getByRole("button", { name: "确认知识" }));
		fireEvent.click(await screen.findByRole("button", { name: "确认" }));
		await waitFor(() =>
			expect(navigate).toHaveBeenCalledWith("/knowledge/items/k9"),
		);
	});

	it("keeps user input on revision conflict", async () => {
		const { api, CommandConflictError } = await import(
			"@/features/knowledge/api"
		);
		vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
		vi.mocked(api.editDraft).mockRejectedValue(
			new CommandConflictError({
				code: "row_version_conflict",
				currentRevision: 3,
			}),
		);
		renderView({ route: "/knowledge/candidates/c1" });
		expect(await screen.findByText("编辑知识候选")).toBeInTheDocument();
		fireEvent.change(screen.getByLabelText("标题"), {
			target: { value: "我的修改" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		expect(await screen.findByText(/草稿已被更新/)).toBeInTheDocument();
		expect(screen.getByLabelText("标题")).toHaveValue("我的修改");
	});

	it("validates scope JSON before saving", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
		renderView({ route: "/knowledge/candidates/c1" });
		expect(await screen.findByText("编辑知识候选")).toBeInTheDocument();
		fireEvent.change(screen.getByLabelText(/适用范围/), {
			target: { value: "not json" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存草稿" }));
		expect(await screen.findByText(/JSON 对象/)).toBeInTheDocument();
		expect(api.editDraft).not.toHaveBeenCalled();
	});

	it("shows the read-only original suggestion on demand", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
		renderView({ route: "/knowledge/candidates/c1" });
		expect(await screen.findByText("编辑知识候选")).toBeInTheDocument();
		expect(screen.queryByText("原始正文")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "查看 AI 原始建议" }));
		expect(await screen.findByText("原始正文")).toBeInTheDocument();
	});

	it("excludes the candidate after confirmation", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
		vi.mocked(api.exclude).mockResolvedValue({
			...candidate("c1"),
			state: "Excluded",
		});
		const { navigate } = renderView({ route: "/knowledge/candidates/c1" });
		expect(await screen.findByText("编辑知识候选")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "排除" }));
		fireEvent.click(await screen.findByRole("button", { name: "排除" }));
		await waitFor(() => expect(api.exclude).toHaveBeenCalledWith("c1", 1));
		expect(navigate).toHaveBeenCalledWith("/knowledge/candidates");
	});

	// 2026-09-21 实机验收:取消批次的候选在状态模型中仍是待确认,但批次围栏
	// 已冻结它——编辑层必须呈现只读并说明原因,而不是渲染注定 409 的操作。
	it("renders a cancelled-batch awaiting candidate read-only with the batch freeze explained", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getCandidate).mockResolvedValue(
			candidateDetail({
				id: "c6",
				draftTitle: "验收临时文档",
				batchState: "Cancelled",
			}),
		);
		renderView({ route: "/knowledge/candidates/c6" });
		expect(await screen.findByText("查看知识候选")).toBeInTheDocument();
		expect(
			screen.getByText(/所属导入批次已取消,此候选已冻结/),
		).toBeInTheDocument();
		// 冻结候选不渲染任何写操作,输入只读。
		expect(
			screen.queryByRole("button", { name: "保存草稿" }),
		).not.toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "确认知识" }),
		).not.toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "排除" }),
		).not.toBeInTheDocument();
		expect(screen.getByLabelText("标题")).toBeDisabled();
		expect(screen.getByLabelText("正文")).toBeDisabled();
		// 原始建议仍可对照阅读。
		fireEvent.click(screen.getByRole("button", { name: "查看 AI 原始建议" }));
		expect(await screen.findByText("原始正文")).toBeInTheDocument();
	});
});

describe("import batches", () => {
	it("lists batches with the state filter and opens the detail", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.listImportBatches).mockResolvedValue({
			items: [batch("b1")],
		});
		const { navigate } = renderView({ route: "/knowledge/imports" });
		// 面板导航里也有“待确认”入口;取表格行内的批次状态。
		const table = await screen.findByRole("table");
		const cell = await within(table).findByText("待确认");
		const row = cell.closest("tr");
		expect(row).toBeTruthy();
		expect(vi.mocked(api.listImportBatches).mock.calls[0]?.[0]).toEqual({
			state: undefined,
		});
		fireEvent.click(row!);
		expect(navigate).toHaveBeenCalledWith("/knowledge/imports/b1");
	});

	it("starts an import and routes to the batch detail", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.startImport).mockResolvedValue({
			...batch("b9"),
			candidates: [],
		} as ImportBatchDetail);
		const { navigate } = renderView({ route: "/knowledge/imports/new" });
		fireEvent.change(await screen.findByLabelText("原文"), {
			target: { value: "一段运维手册" },
		});
		fireEvent.click(screen.getByRole("button", { name: "开始导入" }));
		await waitFor(() =>
			expect(api.startImport).toHaveBeenCalledWith("一段运维手册"),
		);
		expect(navigate).toHaveBeenCalledWith("/knowledge/imports/b9");
	});

	it("confirms the selected candidates transactionally", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getImportBatch).mockResolvedValue({
			...batch("b1"),
			candidates: [
				candidate("c1", "候选一"),
				{ ...candidate("c2", "候选二"), state: "Confirmed" },
			],
		} as ImportBatchDetail);
		vi.mocked(api.confirmBatch).mockResolvedValue({
			...batch("b1", "Completed"),
			candidates: [],
		} as ImportBatchDetail);
		renderView({ route: "/knowledge/imports/b1" });
		expect(await screen.findByText("候选一")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: /确认所选/ }));
		await waitFor(() =>
			expect(api.confirmBatch).toHaveBeenCalledWith("b1", [
				{ candidateId: "c1", expectedRevision: 1 },
			]),
		);
	});

	// 2026-09-21 实机验收:取消批次后待确认候选仍以可勾选/可编辑呈现。
	// 批次终态是围栏:候选不预选、复选框禁用、入口降为只读"查看",并说明原因。
	it("freezes awaiting candidates of a cancelled batch instead of offering doomed operations", async () => {
		const { api } = await import("@/features/knowledge/api");
		vi.mocked(api.getImportBatch).mockResolvedValue({
			...batch("b2", "Cancelled"),
			candidates: [
				{ ...candidate("c6", "验收临时一"), batchState: "Cancelled" },
				{ ...candidate("c7", "验收临时二"), batchState: "Cancelled" },
			],
		} as ImportBatchDetail);
		renderView({ route: "/knowledge/imports/b2" });
		expect(await screen.findByText("验收临时一")).toBeInTheDocument();
		// 已取消:勾选框全部禁用且不预选。
		const first = screen.getByRole("checkbox", {
			name: "选择 验收临时一",
		}) as HTMLInputElement;
		const second = screen.getByRole("checkbox", {
			name: "选择 验收临时二",
		}) as HTMLInputElement;
		expect(first).toBeDisabled();
		expect(second).toBeDisabled();
		expect(first).not.toBeChecked();
		expect(second).not.toBeChecked();
		// 不可能操作不再出现:无整批确认/取消入口。
		expect(
			screen.queryByRole("button", { name: /确认所选/ }),
		).not.toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "取消批次" }),
		).not.toBeInTheDocument();
		// 入口如实降为只读导航并解释冻结原因。
		expect(screen.getByText(/批次已取消:本批候选已冻结/)).toBeInTheDocument();
		expect(screen.getAllByRole("button", { name: "查看" }).length).toBe(2);
		expect(
			screen.queryByRole("button", { name: "编辑" }),
		).not.toBeInTheDocument();
	});

	it("stops import polling while suspended", async () => {
		vi.useFakeTimers();
		try {
			const { api } = await import("@/features/knowledge/api");
			vi.mocked(api.getImportBatch).mockResolvedValue({
				...batch("b1", "Processing"),
				candidates: [],
			} as ImportBatchDetail);
			renderView({ route: "/knowledge/imports/b1", suspended: true });
			await act(async () => {});
			await act(async () => {
				vi.advanceTimersByTime(6000);
			});
			expect(api.getImportBatch).toHaveBeenCalledTimes(1);
		} finally {
			vi.useRealTimers();
		}
	});
});

describe("knowledge item sheet", () => {
	function mockItemApis() {
		return import("@/features/knowledge/api").then(({ api }) => {
			vi.mocked(api.getKnowledge).mockResolvedValue(knowledgeDetail());
			vi.mocked(api.listVersions).mockResolvedValue({
				items: [versionSummary()],
			});
			vi.mocked(api.getVersion).mockResolvedValue(
				versionDetail({
					scope: { service: "checkout" },
					conditions: { severity: "critical" },
				}),
			);
			vi.mocked(api.getCandidate).mockResolvedValue(candidateDetail());
			return api;
		});
	}

	it("renders the current version with scope and version history", async () => {
		await mockItemApis();
		renderView({ route: "/knowledge?item=k1" });
		expect(
			await screen.findByText("先检查 payment 服务和连接池。"),
		).toBeInTheDocument();
		expect(screen.getByText("适用范围")).toBeInTheDocument();
		expect(screen.getByText("适用条件")).toBeInTheDocument();
		expect(screen.getByText("版本历史")).toBeInTheDocument();
	});

	it("previews a historical version and returns to current", async () => {
		const api = await mockItemApis();
		vi.mocked(api.getVersion).mockImplementation(
			async (_id: string, versionId: string) =>
				versionId === "v0"
					? versionDetail({
							id: "v0",
							versionSeq: 0,
							title: "旧版",
							body: "旧版正文",
						})
					: versionDetail({ scope: { service: "checkout" } }),
		);
		vi.mocked(api.listVersions).mockResolvedValue({
			items: [
				versionSummary(),
				versionSummary({
					id: "v0",
					versionSeq: 0,
					title: "旧版",
					eligible: false,
				}),
			],
		});
		renderView({ route: "/knowledge?item=k1" });
		expect(
			await screen.findByText("先检查 payment 服务和连接池。"),
		).toBeInTheDocument();
		fireEvent.click(screen.getByText(/v0 · 旧版/));
		expect(await screen.findByText("旧版正文")).toBeInTheDocument();
		expect(screen.getByText(/正在预览历史版本/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "回到当前版本" }));
		expect(
			await screen.findByText("先检查 payment 服务和连接池。"),
		).toBeInTheDocument();
	});

	it("creates a revision from the current version", async () => {
		const api = await mockItemApis();
		vi.mocked(api.createRevision).mockResolvedValue(candidate("c7"));
		const { navigate } = renderView({ route: "/knowledge?item=k1" });
		fireEvent.click(await screen.findByRole("button", { name: "创建修订" }));
		await waitFor(() =>
			expect(api.createRevision).toHaveBeenCalledWith("k1", "v1", 1),
		);
		expect(navigate).toHaveBeenCalledWith("/knowledge/candidates/c7");
	});

	it("stops reuse after an explicit confirmation", async () => {
		const api = await mockItemApis();
		vi.mocked(api.stopReuse).mockResolvedValue(undefined);
		renderView({ route: "/knowledge?item=k1" });
		fireEvent.click(await screen.findByRole("button", { name: "停止复用" }));
		fireEvent.click(await screen.findByRole("button", { name: "停止复用" }));
		await waitFor(() =>
			expect(api.stopReuse).toHaveBeenCalledWith("k1", "v1", 1),
		);
	});
});
