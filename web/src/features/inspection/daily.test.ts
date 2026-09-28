// 每日报告 HTTP wire-contract 回归：fixtures 逐字段镜像 Go 服务端的 JSON
// (internal/quoin/inspection/daily.go + app/inspection/daily.go)。契约漂移在这里
// 先于真实部署失败。Huma 将 handler 的 Body 字段直接序列化为响应体，无包装。
import { afterEach, describe, expect, it, vi } from "vitest";
import {
	backfillDailyReport,
	createDailyReportConfig,
	getDailyReport,
	getDailyReportVersion,
	listDailyReportConfigs,
	listDailyReports,
	rerunDailyReport,
	updateDailyReportConfig,
} from "./daily";

/** A recorded fetch that answers every call with the server's real JSON. */
function stubFetch(routes: Array<{ method: string; path: RegExp; status?: number; body: unknown }>) {
	const calls: Array<{ method: string; url: string; body?: unknown }> = [];
	vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
		const url = String(input);
		const method = init?.method ?? "GET";
		calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
		const route = routes.find((candidate) => candidate.method === method && candidate.path.test(url));
		if (!route) throw new Error(`unstubbed ${method} ${url}`);
		return new Response(JSON.stringify(route.body), { status: route.status ?? 200, headers: { "Content-Type": "application/json" } });
	}));
	return calls;
}

// DailyReportConfig as persisted (field-for-field).
const realConfig = {
	configKey: "ops-daily", displayName: "运维每日报告", enabled: true,
	timezone: "Asia/Shanghai", triggerTime: "08:00", planKeys: ["prom-up"],
	rowVersion: 2, createdAt: "2026-09-27T00:00:00Z", updatedAt: "2026-09-27T00:00:00Z",
};
// DailyReportSummary：Collecting 时 sealedAt 直接缺席（指针 omitempty，非 null）。
const realSummary = {
	id: "5", configKey: "ops-daily", localDate: "2026-09-27", timezone: "Asia/Shanghai",
	windowStartUtc: "2026-09-26T16:00:00Z", windowEndUtc: "2026-09-27T16:00:00Z",
	triggerKind: "schedule", state: "Sealed", sealedAt: "2026-09-28T02:00:00Z",
	latestVersion: 1, createdAt: "2026-09-28T00:00:00Z",
};
const collectingSummary = {
	id: "6", configKey: "ops-daily", localDate: "2026-09-28", timezone: "Asia/Shanghai",
	windowStartUtc: "2026-09-27T16:00:00Z", windowEndUtc: "2026-09-28T16:00:00Z",
	triggerKind: "manual", state: "Collecting",
	latestVersion: 0, createdAt: "2026-09-29T00:00:00Z",
};
// DailyReportDetail = Summary + 冻结身份 + 版本 + 最新封存内容。
const sealedContent = {
	schemaKind: "inspection_daily_report_v1", configKey: "ops-daily", localDate: "2026-09-27",
	timezone: "Asia/Shanghai", windowStartUtc: "2026-09-26T16:00:00Z", windowEndUtc: "2026-09-27T16:00:00Z",
	sealedAt: "2026-09-28T02:00:00Z",
	sources: [
		{ planKey: "prom-up", displayName: "Prom 连通巡检", connectionName: "lab-prometheus", enabled: true, sourceEnabled: true, status: "ok", checks: [{ runId: 7, checkKey: "promql_check", status: "ok", observedAt: "2026-09-27T09:30:00Z" }] },
		{ planKey: "legacy", enabled: false, sourceEnabled: false, missing: true, status: "gap", gapReasons: ["plan_missing"] },
	],
	totals: { checksOk: 1, checksGap: 0, checksError: 0, sourcesGap: 1 },
};
const realDetail = {
	...realSummary,
	configRowVersion: 2, cutoffAt: "2026-09-28T02:00:00Z",
	contributions: sealedContent.sources,
	versions: [{ version: 1, createdAt: "2026-09-28T02:00:00Z" }],
	latest: sealedContent,
};

afterEach(() => vi.unstubAllGlobals());

describe("daily report HTTP wire contract", () => {
	it("lists configs and creates one with the command identity", async () => {
		const calls = stubFetch([
			{ method: "GET", path: /\/api\/v1\/inspections\/daily-report-configs$/, body: { items: [realConfig] } },
			{ method: "POST", path: /\/api\/v1\/inspections\/daily-report-configs$/, status: 201, body: realConfig },
		]);
		const configs = await listDailyReportConfigs();
		expect(configs).toHaveLength(1);
		expect(configs[0]).toMatchObject({ configKey: "ops-daily", triggerTime: "08:00", planKeys: ["prom-up"] });
		const created = await createDailyReportConfig({
			configKey: "ops-daily", displayName: "运维每日报告", enabled: true,
			timezone: "Asia/Shanghai", triggerTime: "08:00", planKeys: ["prom-up"],
		});
		expect(created.rowVersion).toBe(2);
		const body = calls[1].body as Record<string, unknown>;
		expect(body).toMatchObject({ configKey: "ops-daily", timezone: "Asia/Shanghai", triggerTime: "08:00" });
		expect(body.clientCommandId).not.toBe("");
	});

	it("updates a config with its optimistic row version and stable identity", async () => {
		const calls = stubFetch([
			{ method: "PUT", path: /\/api\/v1\/inspections\/daily-report-configs\/ops-daily$/, body: realConfig },
		]);
		await updateDailyReportConfig({
			configKey: "ops-daily", displayName: "改名", enabled: false,
			timezone: "UTC", triggerTime: "09:30", planKeys: ["prom-up"], expectedRowVersion: 2,
		});
		expect(calls[0].url).toContain("/api/v1/inspections/daily-report-configs/ops-daily");
		const body = calls[0].body as Record<string, unknown>;
		expect(body.expectedRowVersion).toBe(2);
		expect(body.configKey).toBe("ops-daily");
		expect(body).toHaveProperty("clientCommandId");
	});

	it("lists reports with the server-side config filter and default limit", async () => {
		const calls = stubFetch([
			{ method: "GET", path: /\/api\/v1\/inspections\/daily-reports\?/, body: { items: [realSummary, collectingSummary] } },
		]);
		const reports = await listDailyReports({ configKey: "ops-daily" });
		expect(reports).toHaveLength(2);
		expect(reports[1].sealedAt).toBeUndefined();
		expect(calls[0].url).toContain("configKey=ops-daily");
		expect(calls[0].url).toContain("limit=50");
	});

	it("reads the report detail with contributions, versions and sealed content", async () => {
		stubFetch([
			{ method: "GET", path: /\/api\/v1\/inspections\/daily-reports\/ops-daily\/2026-09-27$/, body: realDetail },
		]);
		const detail = await getDailyReport("ops-daily", "2026-09-27");
		expect(detail.state).toBe("Sealed");
		expect(detail.contributions[1]).toMatchObject({ missing: true, status: "gap" });
		expect(detail.latest?.totals.sourcesGap).toBe(1);
		expect(detail.versions).toEqual([{ version: 1, createdAt: "2026-09-28T02:00:00Z" }]);
	});

	it("reads one immutable version's raw content document", async () => {
		const calls = stubFetch([
			{ method: "GET", path: /\/versions\/1$/, body: { content: JSON.stringify(sealedContent) } },
		]);
		const content = await getDailyReportVersion("ops-daily", "2026-09-27", 1);
		expect(JSON.parse(content).schemaKind).toBe("inspection_daily_report_v1");
		expect(calls[0].url).toContain("/daily-reports/ops-daily/2026-09-27/versions/1");
	});

	it("backfills the requested original date with only the identity payload", async () => {
		const calls = stubFetch([
			{ method: "POST", path: /\/api\/v1\/inspections\/daily-reports\/backfill$/, status: 202, body: collectingSummary },
		]);
		const report = await backfillDailyReport("ops-daily", "2026-09-28");
		expect(report).toMatchObject({ state: "Collecting", triggerKind: "manual" });
		const body = calls[0].body as Record<string, unknown>;
		// 补跑窗口就是请求的原日期：服务端绝不偷换为当前日期。
		expect(Object.keys(body).sort()).toEqual(["clientCommandId", "configKey", "localDate"]);
		expect(body.localDate).toBe("2026-09-28");
	});

	it("reruns from the same frozen window with only a command id", async () => {
		const calls = stubFetch([
			{ method: "POST", path: /\/daily-reports\/ops-daily\/2026-09-27\/rerun$/, status: 202, body: realSummary },
		]);
		const report = await rerunDailyReport("ops-daily", "2026-09-27");
		expect(report.latestVersion).toBe(1);
		const body = calls[0].body as Record<string, unknown>;
		expect(Object.keys(body)).toEqual(["clientCommandId"]);
	});

	it("surfaces the server's ordinary-language failure message", async () => {
		vi.stubGlobal("fetch", vi.fn(async () =>
			new Response(JSON.stringify({ message: "触发时间必须是本地 24 小时制 HH:MM", code: "malformed_trigger_time" }), { status: 422 }),
		));
		await expect(createDailyReportConfig({
			configKey: "x", displayName: "x", enabled: true,
			timezone: "UTC", triggerTime: "99:99", planKeys: ["p"],
		})).rejects.toMatchObject({ message: "触发时间必须是本地 24 小时制 HH:MM", status: 422 });
	});
});
