import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  listBusinessSystems: vi.fn(), getBusinessSystem: vi.fn(), listInspectionRuns: vi.fn(), createInspectionRun: vi.fn(),
  getInspectionRun: vi.fn(), listInspectionReports: vi.fn(), getInspectionReport: vi.fn(), cancelInspectionRun: vi.fn(), reanalyzeInspectionRun: vi.fn(), rerunInspection: vi.fn(),
}));
const feedback = vi.hoisted(() => ({
  appendFeedback: vi.fn(), fetchFeedback: vi.fn(),
}));
vi.mock("@/features/inspection/api", async (original) => ({ ...await original<typeof import("@/features/inspection/api")>(), ...api }));
vi.mock("@/features/feedback/api", async (original) => ({ ...await original<typeof import("@/features/feedback/api")>(), ...feedback }));
import { initialInspectionPlan, ReportBody, RunDetail, useInspectionsModule } from "./index";

const props = { user: { id: "u", username: "u", displayName: "U", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }, route: "/inspections", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() };
const browserPlan = { planKey: "browser", displayName: "Browser", checks: [{ checkKey: "login", displayName: "Login", analysisQuestion: "Logged in?", kind: "browser" as const }] };
const promqlPlan = { planKey: "metrics", displayName: "Metrics", checks: [{ checkKey: "up", displayName: "Up", analysisQuestion: "Up?", kind: "promql" as const }] };

function InspectionView() { const view = useInspectionsModule(props); return <>{view.list}{view.actions}{view.content}</>; }

beforeEach(() => { Element.prototype.scrollIntoView = vi.fn(); });
afterEach(() => { cleanup(); vi.clearAllMocks(); vi.unstubAllGlobals(); });

describe("inspection report body", () => {
  it("renders Markdown headings, tables, and frozen evidence links", () => {
    const openEvidence = vi.fn();
    render(<ReportBody content={"# 巡检结论\n\n| 检查 | 状态 |\n| --- | --- |\n| 连通性 | 正常 |\n\n结论见 #e-1。"} evidenceIds={["e-1"]} openEvidence={openEvidence} />);
    expect(screen.getByRole("heading", { name: "巡检结论" })).toBeInTheDocument();
    expect(screen.getByRole("table")).toHaveTextContent("连通性");
    fireEvent.click(screen.getByRole("button", { name: "#e-1" }));
    expect(openEvidence).toHaveBeenCalledWith("e-1");
  });
});

describe("inspection report feedback", () => {
  it("keeps long report content in a bounded native scroll container", async () => {
    api.getInspectionRun.mockResolvedValue({ id: "6", planKey: "mall-health", state: "Completed", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 1, analysisActive: false, checks: [] });
    api.listInspectionReports.mockResolvedValue([{ version: 1, modelId: "fixture-chat", createdAt: "2026-09-10T10:01:00Z" }]);
    api.getInspectionReport.mockResolvedValue({ id: "42", runId: "6", version: 1, evidenceDigest: "digest", evidenceIds: [], modelId: "fixture-chat", content: "# 报告\n\n".repeat(200), createdAt: "2026-09-10T10:01:00Z" });
    feedback.fetchFeedback.mockResolvedValue({ items: [] });
    render(<RunDetail runId="6" props={props} browserPlan={false} onBack={vi.fn()} onOpenRun={vi.fn()} />);
    expect(await screen.findByTestId("report-content")).toHaveClass("max-h-[28rem]", "overflow-y-auto");
    expect(screen.getByRole("button", { name: "已采纳" })).toBeInTheDocument();
  });

  it("uses the immutable report ID rather than a run/version composite", async () => {
    api.getInspectionRun.mockResolvedValue({ id: "6", planKey: "mall-health", state: "Completed", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 1, analysisActive: false, checks: [] });
    api.listInspectionReports.mockResolvedValue([{ version: 1, modelId: "fixture-chat", createdAt: "2026-09-10T10:01:00Z" }]);
    api.getInspectionReport.mockResolvedValue({ id: "42", runId: "6", version: 1, evidenceDigest: "digest", evidenceIds: [], modelId: "fixture-chat", content: "# 报告", createdAt: "2026-09-10T10:01:00Z" });
    feedback.fetchFeedback.mockResolvedValue({ items: [] });
    render(<RunDetail runId="6" props={props} browserPlan={false} onBack={vi.fn()} onOpenRun={vi.fn()} />);
    await waitFor(() => expect(feedback.fetchFeedback).toHaveBeenCalledWith({ type: "inspection_report", id: "42" }));
  });
});

describe("browser inspection gate", () => {
  it("prefers the first non-browser plan and still opens the chooser when a browser plan comes first", async () => {
    api.listBusinessSystems.mockResolvedValue([{ key: "payments", enabled: true, currentConfigVersionId: "v1" }]);
    api.getBusinessSystem.mockResolvedValue({ key: "payments", plans: [browserPlan, promqlPlan] });
    api.listInspectionRuns.mockResolvedValue([]);
    render(<InspectionView />);
    await waitFor(() => expect(screen.getByRole("button", { name: "创建巡检" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "创建巡检" }));
    expect(await screen.findByText("Metrics")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("combobox"));
    expect(await screen.findByRole("option", { name: "Browser（浏览器巡检开发中）" })).toHaveAttribute("aria-disabled", "true");
  });

  it("shows an explicit development state when every plan is browser-backed", async () => {
    api.listBusinessSystems.mockResolvedValue([{ key: "payments", enabled: true, currentConfigVersionId: "v1" }]);
    api.getBusinessSystem.mockResolvedValue({ key: "payments", plans: [browserPlan] });
    api.listInspectionRuns.mockResolvedValue([]);
    render(<InspectionView />);
    await waitFor(() => expect(screen.getByRole("button", { name: "创建巡检" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "创建巡检" }));
    expect(await screen.findByText(/当前没有可启动的 PromQL 巡检计划/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "开始巡检" })).toBeDisabled();
  });

  it("selects a non-browser plan deterministically", () => {
    expect(initialInspectionPlan([browserPlan, promqlPlan])).toBe("metrics");
  });

  it("refreshes and selects a newly created run, with distinguishable sidebar metadata", async () => {
    api.listBusinessSystems.mockResolvedValue([{ key: "mall", displayName: "Mall", enabled: true, currentConfigVersionId: "v1" }]);
    api.getBusinessSystem.mockResolvedValue({ key: "mall", plans: [promqlPlan] });
    api.listInspectionRuns.mockResolvedValueOnce([{ id: "1", planKey: "mall-health", state: "Completed", triggerKind: "schedule", createdAt: "2026-09-10T08:00:00Z", rowVersion: 1 }]).mockResolvedValue([{ id: "6", planKey: "mall-health", state: "Completed", triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", rowVersion: 1 }, { id: "1", planKey: "mall-health", state: "Completed", triggerKind: "schedule", createdAt: "2026-09-10T08:00:00Z", rowVersion: 1 }]);
    api.createInspectionRun.mockResolvedValue({ id: "6" });
    api.getInspectionRun.mockResolvedValue({ id: "6", planKey: "mall-health", state: "Completed", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    render(<InspectionView />);
    await waitFor(() => expect(screen.getByRole("button", { name: /mall-health · Run 1/ })).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "创建巡检" }));
    fireEvent.click(screen.getByRole("button", { name: "开始巡检" }));
    await waitFor(() => expect(api.listInspectionRuns).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("button", { name: /mall-health · Run 6/ })).toHaveTextContent("手动");
    expect(screen.getByRole("button", { name: /mall-health · Run 1/ })).toHaveTextContent("定时");
  });
});
