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
import { AuditPage } from "./index";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const jsonResponse = (body: unknown, ok = true, status = 200) => ({
	ok,
	status,
	json: async () => body,
});

const settings = {
	retentionMonths: 24,
	minRetentionMonths: 6,
	rowVersion: 7,
	updatedAt: "2026-01-01T00:00:00Z",
	updatedBy: "admin-1",
	cleanup: {
		lastRunAt: "2026-09-14T02:00:00Z",
		lastSuccessCutoffAt: "2026-09-13T02:00:00Z",
		lastSuccessDeletedCount: 12,
		lastFailureAt: null,
		lastErrorCode: null,
	},
};

const preview = {
	retentionMonths: 12,
	currentRetentionMonths: 24,
	shortening: true,
	cutoffAt: "2025-09-01T00:00:00Z",
	estimatedExpirableEvents: 3,
	estimatedExpirableCorrelations: 2,
};

const correlationEvents = [
	{
		id: "e2",
		correlationId: "corr-1",
		actorType: "user",
		actorId: "u1",
		action: "user.created",
		outcome: "success",
		phase: "outcome",
		domainRefType: "user",
		domainRefId: "u9",
		createdAt: "2026-09-01T08:00:00Z",
	},
	{
		id: "e1",
		correlationId: "corr-1",
		actorType: "service",
		actorId: "executor",
		action: "command.attempt",
		outcome: "success",
		phase: "execution",
		attemptId: "att-1",
		createdAt: "2026-09-01T07:59:59Z",
	},
];

const listedEvents = [
	...correlationEvents,
	{
		id: "e0",
		actorType: "user",
		actorId: "u2",
		action: "legacy.action",
		outcome: "rejected",
		createdAt: "2026-08-01T00:00:00Z",
	},
];

/** Routes the page's concurrent initial requests (events + settings) by URL.
 * Extra values that are already response-like (have `ok`) pass through untouched. */
function stubAuditFetch(
	events: (url: URL) => unknown,
	extra: { settings?: unknown; patch?: unknown; preview?: unknown } = {},
) {
	const respond = (value: unknown) =>
		typeof value === "object" && value !== null && "ok" in value
			? value
			: jsonResponse(value);
	const fetchMock = vi.fn(
		async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = new URL(String(input), "https://quoin.invalid");
			if (url.pathname === "/api/v1/audit-events") return respond(events(url));
			if (url.pathname === "/api/v1/admin/audit-settings") {
				if (init?.method === "PATCH") return respond(extra.patch ?? settings);
				return respond(extra.settings ?? settings);
			}
			if (url.pathname === "/api/v1/admin/audit-settings/preview")
				return respond(extra.preview ?? preview);
			throw new Error(`unexpected request: ${url.pathname}`);
		},
	);
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

describe("audit admin screen", () => {
	it("lists events, marks legacy history without correlation, and shows the correlated timeline in order", async () => {
		const fetchMock = stubAuditFetch((url) =>
			url.searchParams.get("correlationId")
				? { items: [...correlationEvents].reverse() }
				: { items: listedEvents },
		);
		render(<AuditPage suspended={false} />);
		expect(await screen.findByText(/user\.created/)).toBeInTheDocument();

		// 无关联的历史事件在详情抽屉中如实标注，且不提供关联链路 Tab。
		fireEvent.click(screen.getByText("legacy.action").closest("tr")!);
		let sheet = await screen.findByRole("dialog");
		expect(within(sheet).getByText(/历史无关联/)).toBeInTheDocument();
		expect(within(sheet).queryByRole("tab")).toBeNull();
		fireEvent.click(within(sheet).getByRole("button", { name: "Close" }));
		await waitFor(() =>
			expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
		);

		// 行 → 详情抽屉 → 「关联链路」Tab，同一切面内按时间正序呈现。
		fireEvent.click(screen.getByText("user.created").closest("tr")!);
		sheet = await screen.findByRole("dialog");
		fireEvent.mouseDown(within(sheet).getByRole("tab", { name: "关联链路" }));
		expect(
			fetchMock.mock.calls.some(([input]) =>
				String(input).includes("correlationId=corr-1"),
			),
		).toBe(true);
		const panel = await within(sheet).findByRole("tabpanel");
		const timeline = await within(panel).findAllByText(
			/user\.created|command\.attempt/,
		);
		expect(timeline[0]).toHaveTextContent("command.attempt");
		expect(timeline[1]).toHaveTextContent("user.created");
		expect(within(panel).getByText("执行尝试")).toBeInTheDocument();

		fireEvent.click(within(sheet).getByRole("button", { name: "Close" }));
		await waitFor(() =>
			expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
		);
	});

	it("passes column-header filters as query parameters and pages by opaque cursor", async () => {
		const recent = {
			id: "e2",
			actorType: "user",
			actorId: "u1",
			action: "user.created",
			outcome: "success",
			createdAt: "2026-09-01T08:00:00Z",
		};
		const older = {
			id: "e9",
			actorType: "user",
			actorId: "u1",
			action: "user.disabled",
			outcome: "success",
			createdAt: "2026-01-01T08:00:00Z",
		};
		const fetchMock = stubAuditFetch((url) =>
			url.searchParams.has("cursor")
				? { items: [older] }
				: { items: [recent], nextCursor: "page-2" },
		);
		render(<AuditPage suspended={false} />);
		expect(await screen.findByText(/user\.created/)).toBeInTheDocument();

		// 时间列表头漏斗 → 范围筛选。
		fireEvent.click(screen.getByRole("button", { name: "筛选时间" }));
		fireEvent.change(await screen.findByLabelText("开始时间"), {
			target: { value: "2026-09-01T00:00" },
		});
		fireEvent.click(screen.getByRole("button", { name: "应用" }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

		// 操作列表头漏斗 → 文本筛选。
		fireEvent.click(screen.getByRole("button", { name: "筛选操作" }));
		fireEvent.change(await screen.findByLabelText("操作"), {
			target: { value: "user.created" },
		});
		fireEvent.click(screen.getByRole("button", { name: "应用" }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));

		// 关联列表头漏斗 → 关联 ID 筛选。
		fireEvent.click(screen.getByRole("button", { name: "筛选关联" }));
		fireEvent.change(await screen.findByLabelText("关联 ID"), {
			target: { value: "corr-9" },
		});
		fireEvent.click(screen.getByRole("button", { name: "应用" }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(5));

		const filtered = new URL(
			String(fetchMock.mock.calls[4][0]),
			"https://quoin.invalid",
		).searchParams;
		expect(new Date(filtered.get("since")!).getTime()).toBe(
			new Date(2026, 8, 1).getTime(),
		);
		expect(filtered.get("action")).toBe("user.created");
		expect(filtered.get("correlationId")).toBe("corr-9");
		expect(filtered.has("until")).toBe(false);
		expect(filtered.has("actorType")).toBe(false);
		expect(filtered.has("outcome")).toBe(false);

		// 游标翻页：下一页用 nextCursor 取数并替换当前页。
		fireEvent.click(screen.getByRole("button", { name: "下一页" }));
		expect(await screen.findByText(/user\.disabled/)).toBeInTheDocument();
		expect(screen.queryByText(/user\.created/)).not.toBeInTheDocument();
		const paged = new URL(
			String(fetchMock.mock.calls[5][0]),
			"https://quoin.invalid",
		).searchParams;
		expect(paged.get("cursor")).toBe("page-2");
		expect(paged.get("action")).toBe("user.created");
	});

	it("filters by outcome from the 结果 column header", async () => {
		const fetchMock = stubAuditFetch(() => ({ items: [] }));
		render(<AuditPage suspended={false} />);
		expect(
			await screen.findByText("没有匹配的审计事件"),
		).toBeInTheDocument();

		fireEvent.click(screen.getByRole("button", { name: "筛选结果" }));
		fireEvent.click(await screen.findByRole("button", { name: "失败" }));
		await waitFor(() => {
			const last = new URL(
				String(fetchMock.mock.calls.at(-1)?.[0]),
				"https://quoin.invalid",
			);
			expect(last.searchParams.get("outcome")).toBe("failure");
		});
	});

	it("previews the retention impact, requires confirmation, and applies the change", async () => {
		const fetchMock = stubAuditFetch(() => ({ items: [] }), {
			patch: { retentionMonths: 12, minRetentionMonths: 6, rowVersion: 8 },
		});
		render(<AuditPage suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "修改保留期" }));

		const dialog = screen.getByRole("dialog");
		const months = within(dialog).getByLabelText("新保留期（自然月）");
		const confirm = within(dialog).getByRole("button", { name: "确认调整" });
		expect(confirm).toBeDisabled();

		fireEvent.change(months, { target: { value: "3" } });
		expect(
			within(dialog).getByRole("button", { name: "预览影响" }),
		).toBeDisabled();

		fireEvent.change(months, { target: { value: "12" } });
		fireEvent.click(within(dialog).getByRole("button", { name: "预览影响" }));
		await waitFor(() =>
			expect(within(dialog).getByText(/清理截止点/)).toBeInTheDocument(),
		);
		expect(within(dialog).getByText(/正在缩短保留期/)).toBeInTheDocument();
		expect(
			within(dialog).getByText(/预计到期事件：\s*3\s*条/),
		).toBeInTheDocument();

		await waitFor(() => expect(confirm).toBeEnabled());
		fireEvent.click(confirm);
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
		const patchCall = fetchMock.mock.calls.find(
			([, init]) => init?.method === "PATCH",
		);
		expect(JSON.parse(String(patchCall?.[1]?.body))).toMatchObject({
			retentionMonths: 12,
			expectedRowVersion: 7,
		});
		await waitFor(() =>
			expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
		);
		expect(document.body.textContent).toContain("保留 12 个月");
	});

	it("reports a settings conflict instead of pretending the change succeeded", async () => {
		stubAuditFetch(() => ({ items: [] }), {
			patch: jsonResponse({ message: "行版本冲突。" }, false, 409),
		});
		render(<AuditPage suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "修改保留期" }));
		const dialog = screen.getByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("新保留期（自然月）"), {
			target: { value: "12" },
		});
		fireEvent.click(within(dialog).getByRole("button", { name: "预览影响" }));
		await waitFor(() =>
			expect(
				within(dialog).getByRole("button", { name: "确认调整" }),
			).toBeEnabled(),
		);
		fireEvent.click(within(dialog).getByRole("button", { name: "确认调整" }));
		expect(
			await within(dialog).findByText(/已被其他管理员修改/),
		).toBeInTheDocument();
		expect(screen.getByRole("dialog")).toBeInTheDocument();
	});

	it("summarises outcomes and opens the event detail drawer with the full record", async () => {
		stubAuditFetch(() => ({
			items: [
				{
					id: "e10",
					correlationId: "corr-1",
					actorType: "user",
					actorId: "u1",
					action: "user.create",
					outcome: "success",
					phase: "outcome",
					domainRefType: "user",
					domainRefId: "u9",
					clientCommandId: "cmd-1",
					requestId: "req-1",
					createdAt: "2026-09-01T08:00:00Z",
				},
			],
		}));
		render(<AuditPage suspended={false} />);
		// Known actions render a human label with the raw name beside it.
		expect(await screen.findByText("创建用户")).toBeInTheDocument();
		expect(screen.getByText(/当前页 1 条/)).toBeInTheDocument();

		fireEvent.click(screen.getByText("创建用户").closest("tr")!);
		const sheet = await screen.findByRole("dialog");
		expect(within(sheet).getByText("访问对象")).toBeInTheDocument();
		expect(within(sheet).getByText("用户 · u9")).toBeInTheDocument();
		expect(within(sheet).getByText("cmd-1")).toBeInTheDocument();
		expect(within(sheet).getByText("req-1")).toBeInTheDocument();

		fireEvent.mouseDown(within(sheet).getByRole("tab", { name: "关联链路" }));
		const panel = await within(sheet).findByRole("tabpanel");
		expect(await within(panel).findByText("corr-1")).toBeInTheDocument();
	});

	it("surfaces server problems instead of rendering invented events", async () => {
		vi.stubGlobal(
			"fetch",
			vi.fn(async () =>
				jsonResponse({ message: "数据库暂时不可用。" }, false, 503),
			),
		);
		render(<AuditPage suspended={false} />);
		const alerts = await screen.findAllByRole("alert");
		const messages = alerts.map((alert) => alert.textContent).join("\n");
		expect(messages).toContain("数据库暂时不可用。");
		expect(messages).toContain("保留设置读取失败");
		expect(screen.queryByText(/user\.created/)).not.toBeInTheDocument();
	});
});
