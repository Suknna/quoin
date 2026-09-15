import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  listInspectionPlans: vi.fn(), createInspectionPlan: vi.fn(), updateInspectionPlan: vi.fn(),
  listInspectionRuns: vi.fn(), createInspectionRun: vi.fn(), getInspectionRun: vi.fn(), listInspectionReports: vi.fn(),
  getInspectionReport: vi.fn(), cancelInspectionRun: vi.fn(), reanalyzeInspectionRun: vi.fn(), rerunInspection: vi.fn(),
}));
const feedback = vi.hoisted(() => ({
  appendFeedback: vi.fn(), fetchFeedback: vi.fn(),
}));
vi.mock("@/features/inspection/api", async (original) => ({ ...await original<typeof import("@/features/inspection/api")>(), ...api }));
vi.mock("@/features/feedback/api", async (original) => ({ ...await original<typeof import("@/features/feedback/api")>(), ...feedback }));
import { WorkspaceShell } from "@/app/WorkspaceShell";
import { ReportBody, RunDetail, useInspectionsModule } from "./index";

function baseProps(route: string) {
  return { user: { id: "u", username: "u", displayName: "U", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 }, route, navigate: vi.fn(), suspended: false, openEvidence: vi.fn() };
}
const props = baseProps("/inspections");

/** Standalone plans scope an integration directly; no BusinessSystem declaration is involved. */
const integrationPlan = {
  planKey: "prom-up", displayName: "Prometheus 连通巡检", enabled: true, connectionName: "lab-prometheus",
  pluginId: "prometheus", templateId: "prometheus-up", templateVersion: null, params: { expression: "up" },
  scope: { kind: "integration" as const }, cron: "*/5 * * * *", timezone: "Asia/Shanghai",
  rowVersion: 3, createdAt: "2026-09-10T08:00:00Z", updatedAt: "2026-09-10T08:00:00Z",
};
const objectsPlan = {
  ...integrationPlan, planKey: "pod-check", displayName: "指定对象巡检", enabled: true, cron: null,
  scope: { kind: "objects" as const, objects: [{ objectType: "kubernetes_pod", identityKey: "demo/api" }] }, rowVersion: 5,
};
const businessViewPlan = {
  ...integrationPlan, planKey: "bv-payments", displayName: "支付核心视图巡检", enabled: true, cron: null,
  scope: { kind: "businessView" as const, businessViewKey: "payments-core" }, rowVersion: 7,
};
const runSummary = { id: "1", planKey: "prom-up", connectionName: "lab-prometheus", businessSystemKey: "legacy-mall", state: "Completed" as const, rowVersion: 1, triggerKind: "schedule" as const, createdAt: "2026-09-10T08:00:00Z" };

function InspectionView({ route = "/inspections", navigate }: { route?: string; navigate?: (route: string) => void }) {
  const moduleProps = { ...props, route, navigate: navigate ?? props.navigate };
  const view = useInspectionsModule(moduleProps);
  return <>{view.list}{view.actions}{view.content}</>;
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  api.listInspectionPlans.mockResolvedValue([]);
  api.listInspectionRuns.mockResolvedValue({ items: [] });
});
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
  const detail = { id: "6", planKey: "prom-up", connectionName: "lab-prometheus", state: "Completed" as const, rowVersion: 1, triggerKind: "manual" as const, createdAt: "2026-09-10T10:00:00Z", reportCount: 1, analysisActive: false, checks: [] };
  function renderRunDetail(onOpenRun = vi.fn()) {
    api.getInspectionRun.mockResolvedValue({ ...detail });
    api.listInspectionReports.mockResolvedValue([{ version: 1, modelId: "fixture-chat", createdAt: "2026-09-10T10:01:00Z" }]);
    api.getInspectionReport.mockResolvedValue({ id: "42", runId: "6", version: 1, evidenceDigest: "digest", evidenceIds: [], modelId: "fixture-chat", content: "# 报告\n\n".repeat(200), createdAt: "2026-09-10T10:01:00Z" });
    feedback.fetchFeedback.mockResolvedValue({ items: [] });
    render(<RunDetail runId="6" props={props} onBack={vi.fn()} onOpenRun={onOpenRun} />);
  }

  it("keeps long report content in a bounded native scroll container", async () => {
    renderRunDetail();
    expect(await screen.findByTestId("report-content")).toHaveClass("max-h-[28rem]", "overflow-y-auto");
    expect(screen.getByRole("button", { name: "已采纳" })).toBeInTheDocument();
  });

  it("uses the immutable report ID rather than a run/version composite", async () => {
    renderRunDetail();
    await waitFor(() => expect(feedback.fetchFeedback).toHaveBeenCalledWith({ type: "inspection_report", id: "42" }));
  });

  it("reuses frozen evidence for reanalysis and deep-links the recollection run", async () => {
    const onOpenRun = vi.fn();
    renderRunDetail(onOpenRun);
    api.reanalyzeInspectionRun.mockResolvedValue({ id: "att-1", type: "inspection_analysis", state: "Queued", rowVersion: 1, createdAt: "2026-09-10T10:02:00Z" });
    api.rerunInspection.mockResolvedValue({ ...detail, id: "run-10" });
    fireEvent.click(await screen.findByRole("button", { name: "重新分析现有证据" }));
    await waitFor(() => expect(api.reanalyzeInspectionRun).toHaveBeenCalledWith("6"));
    fireEvent.click(await screen.findByRole("button", { name: "重新采证（新 Run）" }));
    await waitFor(() => expect(onOpenRun).toHaveBeenCalledWith("run-10"));
  });

  it("disables cancellation once the run is terminal even while its analysis is active", async () => {
    api.getInspectionRun.mockResolvedValue({ id: "run-9", planKey: "prom-up", connectionName: "lab-prometheus", state: "Completed", rowVersion: 2, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: true, checks: [] });
    const { unmount } = render(<RunDetail runId="run-9" props={props} onBack={vi.fn()} onOpenRun={vi.fn()} />);
    // Terminal run: cancel stays visible (the analysis still shows here) but can no longer be fired.
    expect(await screen.findByRole("button", { name: "取消" })).toBeDisabled();
    unmount();
    api.getInspectionRun.mockResolvedValue({ id: "run-9", planKey: "prom-up", connectionName: "lab-prometheus", state: "Running", rowVersion: 2, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    render(<RunDetail runId="run-9" props={props} onBack={vi.fn()} onOpenRun={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("button", { name: "取消" })).toBeEnabled());
  });
});

describe("plan workspace", () => {
  it("shows a loading state instead of a premature empty list while reads hang", async () => {
    // The reads never settle (backend starvation symptom): the UI must not
    // claim "还没有巡检计划" / "没有巡检记录" — a hang is not an authoritative empty range.
    let resolvePlans: ((plans: unknown[]) => void) = () => {};
    api.listInspectionPlans.mockReturnValue(new Promise((resolve) => { resolvePlans = resolve; }));
    api.listInspectionRuns.mockReturnValue(new Promise(() => {}));
    render(<InspectionView />);
    expect(await screen.findByRole("status", { name: "正在读取巡检计划" })).toBeInTheDocument();
    expect(screen.queryByText("还没有巡检计划")).not.toBeInTheDocument();
    expect(screen.queryByText("没有巡检记录")).not.toBeInTheDocument();
    resolvePlans([integrationPlan]);
    expect(await screen.findByText("Prometheus 连通巡检")).toBeInTheDocument();
    expect(screen.queryByText("还没有巡检计划")).not.toBeInTheDocument();
  });

  it("lists real plans and a single server page of runs without business systems", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    api.listInspectionRuns.mockResolvedValue({ items: [runSummary] });
    render(<InspectionView />);
    expect(await screen.findByText("Prometheus 连通巡检")).toBeInTheDocument();
    // One page request, no business-system precondition and no full cursor walk.
    expect(api.listInspectionRuns).toHaveBeenCalledTimes(1);
    expect(api.listInspectionRuns).toHaveBeenCalledWith({});
    expect(screen.getByRole("button", { name: /Prometheus 连通巡检 · Run 1/ })).toHaveTextContent("lab-prometheus");
  });

  it("filters runs by a selected plan server-side", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    render(<InspectionView />);
    await screen.findByText("Prometheus 连通巡检");
    fireEvent.click(screen.getByRole("combobox", { name: "按计划筛选" }));
    fireEvent.click(await screen.findByRole("option", { name: "Prometheus 连通巡检" }));
    await waitFor(() => expect(api.listInspectionRuns).toHaveBeenLastCalledWith({ planKey: "prom-up" }));
  });

  it("opens the run workspace for deep-linked run routes", async () => {
    api.getInspectionRun.mockResolvedValue({ id: "run-9", planKey: "prom-up", connectionName: "lab-prometheus", state: "Running", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    render(<InspectionView route="/inspections/runs/run-9" />);
    expect(await screen.findByText(/Run run-9/)).toBeInTheDocument();
    expect(api.getInspectionRun).toHaveBeenCalledWith("run-9");
  });

  it("starts a run from a real plan and deep-links the created run", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    api.createInspectionRun.mockResolvedValue({ id: "run-6", planKey: "prom-up", state: "Queued", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    const navigate = vi.fn();
    render(<InspectionView navigate={navigate} />);
    await screen.findByText("Prometheus 连通巡检");
    fireEvent.click(screen.getByRole("button", { name: "运行巡检" }));
    fireEvent.click(await screen.findByRole("button", { name: "开始巡检" }));
    await waitFor(() => expect(api.createInspectionRun).toHaveBeenCalledWith("prom-up"));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/inspections/runs/run-6"));
  });

  it("preselects the linked connection's enabled plan when arriving from integration details", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan, objectsPlan]);
    api.createInspectionRun.mockResolvedValue({ id: "run-7", planKey: "prom-up", state: "Queued", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    render(<InspectionView route="/inspections?connectionName=lab-prometheus" />);
    // The chooser opens by itself with the connection's plan preselected; keys are never synthesized.
    const chooser = await screen.findByRole("dialog", { name: "运行巡检" });
    expect(within(chooser).getByRole("combobox")).toHaveTextContent("Prometheus 连通巡检");
    fireEvent.click(within(chooser).getByRole("button", { name: "开始巡检" }));
    await waitFor(() => expect(api.createInspectionRun).toHaveBeenCalledWith("prom-up"));
  });

  it("runs the business-view scoped plan when arriving with a businessViewKey hint", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan, businessViewPlan]);
    api.createInspectionRun.mockResolvedValue({ id: "run-8", planKey: "bv-payments", state: "Queued", rowVersion: 1, triggerKind: "manual", createdAt: "2026-09-10T10:00:00Z", reportCount: 0, analysisActive: false, checks: [] });
    render(<InspectionView route="/inspections?businessViewKey=payments-core&connectionName=lab-prometheus" />);
    // Only the matching business-view plan qualifies; the whole-integration plan of the same
    // connection must never be executed for a business-view entry.
    const chooser = await screen.findByRole("dialog", { name: "运行巡检" });
    expect(within(chooser).getByRole("combobox")).toHaveTextContent("支付核心视图巡检");
    fireEvent.click(within(chooser).getByRole("button", { name: "开始巡检" }));
    await waitFor(() => expect(api.createInspectionRun).toHaveBeenCalledTimes(1));
    expect(api.createInspectionRun).toHaveBeenCalledWith("bv-payments");
  });

  it("opens a prefilled business-view plan editor instead of running a wider plan", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    render(<InspectionView route="/inspections?businessViewKey=payments-core&connectionName=lab-prometheus" />);
    // No business-view plan exists: the editor opens prefilled; the integration-scope
    // plan of the same connection must not be started for this entry.
    const editor = await screen.findByRole("dialog", { name: "新建巡检计划" });
    expect(within(editor).getByRole("combobox", { name: "巡检范围" })).toHaveTextContent("业务视图");
    expect(within(editor).getByLabelText("业务视图 Key")).toHaveValue("payments-core");
    expect(within(editor).getByLabelText("接入连接名")).toHaveValue("lab-prometheus");
    expect(screen.queryByRole("dialog", { name: "运行巡检" })).not.toBeInTheDocument();
    expect(api.createInspectionRun).not.toHaveBeenCalled();
  });

  it("offers a prefilled plan editor when the linked connection has no enabled plan", async () => {
    api.listInspectionPlans.mockResolvedValue([{ ...integrationPlan, enabled: false }]);
    render(<InspectionView route="/inspections?connectionName=lab-prometheus" />);
    expect(await screen.findByText(/还没有可立即运行的巡检计划/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "新建计划" }));
    expect(await screen.findByLabelText("接入连接名")).toHaveValue("lab-prometheus");
  });

  it("creates a plan with YAML params and integration scope", async () => {
    api.createInspectionPlan.mockResolvedValue(integrationPlan);
    render(<InspectionView />);
    fireEvent.click(await screen.findByRole("button", { name: "新建计划" }));
    fireEvent.change(await screen.findByLabelText("计划 Key"), { target: { value: "prom-up" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "Prometheus 连通巡检" } });
    fireEvent.change(screen.getByLabelText("接入连接名"), { target: { value: "lab-prometheus" } });
    fireEvent.change(screen.getByLabelText("插件 ID"), { target: { value: "prometheus" } });
    fireEvent.change(screen.getByLabelText("模板 ID"), { target: { value: "prometheus-up" } });
    fireEvent.change(screen.getByLabelText("采集参数（YAML）"), { target: { value: "expression: up\n" } });
    fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
    await waitFor(() => expect(api.createInspectionPlan).toHaveBeenCalledWith({
      planKey: "prom-up", displayName: "Prometheus 连通巡检", enabled: true, connectionName: "lab-prometheus",
      pluginId: "prometheus", templateId: "prometheus-up", templateVersion: null, params: { expression: "up" },
      scope: { kind: "integration" }, cron: null, timezone: expect.any(String),
    }));
    await waitFor(() => expect(api.listInspectionPlans).toHaveBeenCalledTimes(2));
  });

  it("creates an objects-scoped plan from explicit object rows", async () => {
    api.createInspectionPlan.mockResolvedValue(objectsPlan);
    render(<InspectionView />);
    fireEvent.click(await screen.findByRole("button", { name: "新建计划" }));
    fireEvent.change(await screen.findByLabelText("计划 Key"), { target: { value: "pod-check" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "指定对象巡检" } });
    fireEvent.change(screen.getByLabelText("接入连接名"), { target: { value: "lab-prometheus" } });
    fireEvent.change(screen.getByLabelText("插件 ID"), { target: { value: "prometheus" } });
    fireEvent.change(screen.getByLabelText("模板 ID"), { target: { value: "pod-check" } });
    fireEvent.click(screen.getByRole("combobox", { name: "巡检范围" }));
    fireEvent.click(await screen.findByRole("option", { name: "指定对象" }));
    fireEvent.change(await screen.findByLabelText("对象类型"), { target: { value: "kubernetes_pod" } });
    fireEvent.change(screen.getByLabelText("对象身份键"), { target: { value: "demo/api" } });
    fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
    await waitFor(() => expect(api.createInspectionPlan).toHaveBeenCalledWith(expect.objectContaining({
      scope: { kind: "objects", objects: [{ objectType: "kubernetes_pod", identityKey: "demo/api" }] },
    })));
  });

  it("creates a business-view scoped plan", async () => {
    api.createInspectionPlan.mockResolvedValue(integrationPlan);
    render(<InspectionView />);
    fireEvent.click(await screen.findByRole("button", { name: "新建计划" }));
    fireEvent.change(await screen.findByLabelText("计划 Key"), { target: { value: "bv-check" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "业务视图巡检" } });
    fireEvent.change(screen.getByLabelText("接入连接名"), { target: { value: "lab-prometheus" } });
    fireEvent.change(screen.getByLabelText("插件 ID"), { target: { value: "prometheus" } });
    fireEvent.change(screen.getByLabelText("模板 ID"), { target: { value: "bv-check" } });
    fireEvent.click(screen.getByRole("combobox", { name: "巡检范围" }));
    fireEvent.click(await screen.findByRole("option", { name: "业务视图" }));
    fireEvent.change(await screen.findByLabelText("业务视图 Key"), { target: { value: "payments-core" } });
    fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
    await waitFor(() => expect(api.createInspectionPlan).toHaveBeenCalledWith(expect.objectContaining({
      scope: { kind: "businessView", businessViewKey: "payments-core" },
    })));
  });

  it("updates an existing plan with its optimistic row version", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    api.updateInspectionPlan.mockResolvedValue({ ...integrationPlan, displayName: "改名巡检", rowVersion: 4 });
    render(<InspectionView />);
    fireEvent.click(await screen.findByRole("button", { name: "编辑 Prometheus 连通巡检" }));
    fireEvent.change(await screen.findByLabelText("显示名称"), { target: { value: "改名巡检" } });
    fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
    await waitFor(() => expect(api.updateInspectionPlan).toHaveBeenCalledWith("prom-up", expect.objectContaining({
      expectedRowVersion: 3, displayName: "改名巡检", scope: { kind: "integration" }, params: { expression: "up" }, cron: "*/5 * * * *",
    })));
  });

  it("keeps invalid plan input on the form with a readable reason", async () => {
    render(<InspectionView />);
    fireEvent.click(await screen.findByRole("button", { name: "新建计划" }));
    fireEvent.click(screen.getByRole("button", { name: "保存计划" }));
    expect(await screen.findByText(/计划 Key、显示名称、接入连接名、插件 ID 和模板 ID 必须填写/)).toBeInTheDocument();
    expect(api.createInspectionPlan).not.toHaveBeenCalled();
  });

  it("surfaces the server reason when run creation conflicts with an active run", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    api.createInspectionRun.mockRejectedValue(new Error("该计划已有执行中的 Run。"));
    render(<InspectionView />);
    await screen.findByText("Prometheus 连通巡检");
    fireEvent.click(screen.getByRole("button", { name: "运行巡检" }));
    fireEvent.click(await screen.findByRole("button", { name: "开始巡检" }));
    // The server reason surfaces both in the chooser and behind it, without discarding the dialog.
    const reasons = await screen.findAllByText("该计划已有执行中的 Run。");
    expect(reasons.length).toBeGreaterThan(0);
  });
});

describe("workspace shell integration", () => {
  // Integration path: the real shell renders view.actions twice (desktop and mobile
  // headers). Stateful dialogs must live in the single-mount content slot, or two
  // stacked modal dialogs aria-hide each other and every button loses its name.
  function ShellHost({ route }: { route: string }) {
    const view = useInspectionsModule({ ...props, route });
    return <WorkspaceShell user={props.user} route={route} view={view} navigate={props.navigate} onLogout={vi.fn()} />;
  }

  beforeEach(() => {
    vi.stubGlobal("matchMedia", vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
    vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  });

  it("mounts exactly one accessible run chooser although the shell renders header actions twice", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    render(<ShellHost route="/inspections?connectionName=lab-prometheus" />);
    const dialogs = await screen.findAllByRole("dialog", { name: "运行巡检" });
    expect(dialogs).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "开始巡检" })).toHaveLength(1);
  });

  it("opens a single prefilled business-view editor through the shell", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    render(<ShellHost route="/inspections?businessViewKey=payments-core" />);
    const dialogs = await screen.findAllByRole("dialog", { name: "新建巡检计划" });
    expect(dialogs).toHaveLength(1);
    expect(screen.getAllByLabelText("业务视图 Key")).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "保存计划" })).toHaveLength(1);
  });

  it("opens the chooser once from any duplicated header trigger", async () => {
    api.listInspectionPlans.mockResolvedValue([integrationPlan]);
    const { getAllByRole } = render(<ShellHost route="/inspections" />);
    await screen.findByText("Prometheus 连通巡检");
    fireEvent.click(getAllByRole("button", { name: "运行巡检" })[0]);
    const dialogs = await screen.findAllByRole("dialog", { name: "运行巡检" });
    expect(dialogs).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "开始巡检" })).toHaveLength(1);
  });
});
