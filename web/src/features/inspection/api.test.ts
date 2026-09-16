// Wire-contract regression tests: request/response fixtures mirror the exact
// JSON the Go server emits (internal/quoin/inspection), so a contract drift
// fails here before it reaches a real deployment. Huma marshals the handler's
// `Body` field as the entire response body — there is no {"body": ...} wrapper.
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  createInspectionPlan, createInspectionRun, getInspectionReport, listInspectionPlans,
  listInspectionReports, listInspectionRuns, reanalyzeInspectionRun, updateInspectionPlan,
} from "./api";

/** A recorded fetch that answers every inspection call with the server's real JSON. */
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

// Field-for-field projection of inspection.RunSummary (nullable pointers with
// omitempty are simply absent from the wire).
const realRunSummary = {
  id: "1", planKey: "basic-mall-prometheus", state: "Completed", rowVersion: 4,
  triggerKind: "manual", evidenceAt: "2026-09-13T08:00:00Z", createdAt: "2026-09-13T07:59:41Z",
  connectionName: "mall-prometheus",
};
// RunDetail = RunSummary + frozen checks/analysis projection.
const realRunDetail = {
  ...realRunSummary,
  checks: [{ checkKey: "promql_instant", status: "ok", evidenceId: "evidence-17" }],
  reportCount: 1, analysisActive: false,
};
// PluginInspectionPlan as persisted (nullable templateVersion/cron arrive as null).
const realPlan = {
  planKey: "basic-mall-prometheus", displayName: "Mall Prometheus 基础巡检", enabled: true,
  connectionName: "mall-prometheus", pluginId: "prometheus", templateId: "prometheus-basic",
  templateVersion: null, params: { expression: "up" }, scope: { kind: "integration" },
  cron: null, timezone: "Asia/Shanghai", rowVersion: 2,
  createdAt: "2026-09-13T07:00:00Z", updatedAt: "2026-09-13T07:00:00Z",
};

afterEach(() => vi.unstubAllGlobals());

describe("inspection HTTP wire contract", () => {
  it("parses the real run list projection without business-system fields", async () => {
    const calls = stubFetch([{ method: "GET", path: /\/api\/v1\/inspections\/runs/, body: { items: [realRunSummary] } }]);
    const page = await listInspectionRuns();
    expect(page.items).toHaveLength(1);
    expect(page.items[0]).toMatchObject({ id: "1", connectionName: "mall-prometheus", planKey: "basic-mall-prometheus" });
    expect(page.items[0].businessSystemKey).toBeUndefined();
    expect(page.nextCursor).toBeUndefined();
    expect(calls[0].url).toContain("/api/v1/inspections/runs?limit=100");
    expect(calls[0].url).not.toContain("planKey");
  });

  it("narrows the list with the server-side planKey filter", async () => {
    const calls = stubFetch([{ method: "GET", path: /\/api\/v1\/inspections\/runs/, body: { items: [] } }]);
    await listInspectionRuns({ planKey: "basic-mall-prometheus" });
    expect(calls[0].url).toContain("planKey=basic-mall-prometheus");
  });

  it("posts exactly the plan identity and parses the created run", async () => {
    const calls = stubFetch([{ method: "POST", path: /\/api\/v1\/inspections\/runs$/, status: 202, body: realRunDetail }]);
    const run = await createInspectionRun("basic-mall-prometheus");
    expect(run.id).toBe("1");
    expect(run.checks[0]).toEqual({ checkKey: "promql_instant", status: "ok", evidenceId: "evidence-17" });
    // The manual trigger freezes scope server-side: only the command id and plan key travel.
    expect(Object.keys(calls[0].body as object).sort()).toEqual(["clientCommandId", "planKey"]);
    expect((calls[0].body as { planKey: string }).planKey).toBe("basic-mall-prometheus");
    expect((calls[0].body as { clientCommandId: string }).clientCommandId).not.toBe("");
  });

  it("creates plans with the full authority projection", async () => {
    const calls = stubFetch([{ method: "POST", path: /\/api\/v1\/inspections\/plans$/, status: 201, body: realPlan }]);
    const plan = await createInspectionPlan({
      planKey: "basic-mall-prometheus", displayName: "Mall Prometheus 基础巡检", enabled: true,
      connectionName: "mall-prometheus", pluginId: "prometheus", templateId: "prometheus-basic",
      templateVersion: null, params: { expression: "up" }, scope: { kind: "integration" },
      cron: null, timezone: "Asia/Shanghai",
    });
    expect(plan.planKey).toBe("basic-mall-prometheus");
    expect(plan.cron).toBeNull();
    const body = calls[0].body as Record<string, unknown>;
    expect(body).toMatchObject({ planKey: "basic-mall-prometheus", templateVersion: null, cron: null, scope: { kind: "integration" }, clientCommandId: expect.any(String) });
  });

  it("updates a plan by key with its optimistic row version", async () => {
    const calls = stubFetch([{ method: "PUT", path: /\/api\/v1\/inspections\/plans\/basic-mall-prometheus$/, body: realPlan }]);
    await updateInspectionPlan("basic-mall-prometheus", {
      displayName: "改名", enabled: true, connectionName: "mall-prometheus", pluginId: "prometheus",
      templateId: "prometheus-basic", templateVersion: null, params: { expression: "up" },
      scope: { kind: "integration" }, cron: null, timezone: "Asia/Shanghai", expectedRowVersion: 2,
    });
    const body = calls[0].body as Record<string, unknown>;
    expect(body.expectedRowVersion).toBe(2);
    expect(body).toHaveProperty("clientCommandId");
    expect(body).not.toHaveProperty("planKey");
  });

  it("reads reports and their immutable feedback locator", async () => {
    stubFetch([
      { method: "GET", path: /\/reports\?limit=100$/, body: { items: [{ version: 1, modelId: "gpt-demo", createdAt: "2026-09-13T08:05:00Z" }] } },
      { method: "GET", path: /\/reports\/1$/, body: { id: "42", runId: "1", version: 1, evidenceDigest: "a".repeat(64), evidenceIds: ["evidence-17"], modelId: "gpt-demo", content: "# 报告", createdAt: "2026-09-13T08:05:00Z" } },
    ]);
    const reports = await listInspectionReports("1");
    expect(reports).toHaveLength(1);
    const report = await getInspectionReport("1", 1);
    // The server's immutable report locator feeds diagnosis feedback, unlike (runId, version).
    expect(report.id).toBe("42");
    expect(report.evidenceIds).toEqual(["evidence-17"]);
  });

  it("lists the first page of plans", async () => {
    stubFetch([{ method: "GET", path: /\/api\/v1\/inspections\/plans\?limit=100$/, body: { items: [realPlan] } }]);
    const plans = await listInspectionPlans();
    expect(plans).toHaveLength(1);
    expect(plans[0].scope).toEqual({ kind: "integration" });
  });

  it("sends the reanalysis override only when provided, keeping the frozen default wire-clean", async () => {
    const attempts = stubFetch([
      { method: "POST", path: /\/api\/v1\/inspections\/runs\/1\/analyze$/, body: { id: "9", type: "inspection_analysis", state: "Queued", rowVersion: 1, createdAt: "2026-09-16T08:00:00Z" } },
    ]);
    // 缺省（沿用 Run 冻结要求）：请求体不携带 reportInstructions。
    await reanalyzeInspectionRun("1");
    expect(attempts[0].body).not.toHaveProperty("reportInstructions");
    // 仅本次覆盖：文本原样上送（域层按原文冻结，不做静默修剪）。
    await reanalyzeInspectionRun("1", "只看异常项");
    expect((attempts[1].body as Record<string, unknown>).reportInstructions).toBe("只看异常项");
    // 仅本次显式清除：空串原样上送，与继承（字段缺省）可区分。
    await reanalyzeInspectionRun("1", "");
    expect(attempts[2].body as Record<string, unknown>).toHaveProperty("reportInstructions", "");
    expect(attempts[0].body).toHaveProperty("clientCommandId");
  });
});
