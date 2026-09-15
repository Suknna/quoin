import { afterEach, describe, expect, it, vi } from "vitest";
import {
	auditEventsQuery,
	getAuditSettings,
	listAuditEvents,
	localInputToTimestamp,
	phaseLabel,
	previewAuditSettings,
	updateAuditSettings,
} from "./api";

afterEach(() => {
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const jsonResponse = (body: unknown, ok = true, status = 200) => ({
	ok,
	status,
	json: async () => body,
});

describe("auditEventsQuery", () => {
	it("omits blank criteria so the server never filters on empty strings", () => {
		const params = new URLSearchParams(
			auditEventsQuery({ correlationId: "corr-1", action: "", outcome: undefined }),
		);
		expect(params.get("limit")).toBe("50");
		expect(params.get("correlationId")).toBe("corr-1");
		expect(params.has("action")).toBe(false);
		expect(params.has("outcome")).toBe(false);
		expect(params.has("cursor")).toBe(false);
	});

	it("carries since/until bounds and the opaque paging cursor", () => {
		const params = new URLSearchParams(
			auditEventsQuery(
				{ since: "2026-03-01T00:00:00Z", until: "2026-09-01T00:00:00Z" },
				"cursor-9",
			),
		);
		expect(params.get("since")).toBe("2026-03-01T00:00:00Z");
		expect(params.get("until")).toBe("2026-09-01T00:00:00Z");
		expect(params.get("cursor")).toBe("cursor-9");
	});
});

describe("localInputToTimestamp", () => {
	it("converts wall-clock input to a UTC instant and rejects invalid values", () => {
		expect(localInputToTimestamp("")).toBeUndefined();
		expect(localInputToTimestamp("not-a-date")).toBeUndefined();
		const converted = localInputToTimestamp("2026-01-02T03:04");
		expect(converted).toBeDefined();
		expect(new Date(converted!)).toEqual(new Date(2026, 0, 2, 3, 4));
	});
});

describe("phaseLabel", () => {
	it("maps known phases and passes unknown phases through unchanged", () => {
		expect(phaseLabel("access")).toBe("访问");
		expect(phaseLabel("admission")).toBe("准入");
		expect(phaseLabel("execution")).toBe("执行尝试");
		expect(phaseLabel("outcome")).toBe("结果");
		expect(phaseLabel("custom_phase")).toBe("custom_phase");
		expect(phaseLabel(undefined)).toBe("事件");
	});
});

describe("request helpers", () => {
	it("issues a GET against audit-events and surfaces problem messages on failure", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(
				jsonResponse({ items: [{ id: "e1" }], nextCursor: "n1" }),
			)
			.mockResolvedValueOnce(
				jsonResponse({ message: "仅管理员可读审计。" }, false, 403),
			);
		vi.stubGlobal("fetch", fetchMock);

		const page = await listAuditEvents({ outcome: "failure" }, "c2");
		expect(page).toEqual({ items: [{ id: "e1" }], nextCursor: "n1" });
		const [url, init] = fetchMock.mock.calls[0];
		expect(url).toBe("/api/v1/audit-events?limit=50&outcome=failure&cursor=c2");
		expect(init).toMatchObject({ credentials: "include" });

		await expect(listAuditEvents({})).rejects.toThrow("仅管理员可读审计。");
	});

	it("reads retention settings from the admin endpoint", async () => {
		const fetchMock = vi.fn().mockResolvedValue(
			jsonResponse({ retentionMonths: 6, minRetentionMonths: 6, rowVersion: 1 }),
		);
		vi.stubGlobal("fetch", fetchMock);
		const settings = await getAuditSettings();
		expect(settings.retentionMonths).toBe(6);
		expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/admin/audit-settings");
	});

	it("previews and applies retention changes with the agreed payloads", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(
				jsonResponse({
					retentionMonths: 12,
					currentRetentionMonths: 24,
					shortening: true,
					cutoffAt: "2025-09-01T00:00:00Z",
					estimatedExpirableEvents: 3,
					estimatedExpirableCorrelations: 2,
				}),
			)
			.mockResolvedValueOnce(
				jsonResponse({
					retentionMonths: 12,
					minRetentionMonths: 6,
					rowVersion: 8,
				}),
			);
		vi.stubGlobal("fetch", fetchMock);

		const preview = await previewAuditSettings(12);
		expect(preview.shortening).toBe(true);
		expect(fetchMock.mock.calls[0][0]).toBe(
			"/api/v1/admin/audit-settings/preview",
		);
		expect(fetchMock.mock.calls[0][1]?.method).toBe("POST");
		expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
			retentionMonths: 12,
		});

		const saved = await updateAuditSettings(12, 7, "command-1");
		expect(saved.rowVersion).toBe(8);
		expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/audit-settings");
		expect(fetchMock.mock.calls[1][1]?.method).toBe("PATCH");
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({
			retentionMonths: 12,
			expectedRowVersion: 7,
			clientCommandId: "command-1",
		});
	});
});
