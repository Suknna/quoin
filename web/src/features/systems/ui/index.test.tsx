import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkspaceModuleProps } from "@/app/module-contract";
import type { BusinessView } from "../api";
import { useSystemsModule } from "./index";

const checkoutView: BusinessView = {
  viewKey: "checkout",
  displayName: "结算",
  description: "结算业务范围说明",
  scope: { connectionName: "thanos-primary", labelConditions: { service: "checkout" } },
  rowVersion: 3,
  createdAt: "2026-09-13T01:00:00.000Z",
  updatedAt: "2026-09-13T02:00:00.000Z",
};

const connection = { id: "conn-1", name: "thanos-primary", type: "thanos", enabled: true, config: {}, rowVersion: 1 };

const navigate = vi.fn();
let posted: Array<{ url: string; init: RequestInit }> = [];

function stubApi(overrides: Record<string, (init?: RequestInit) => Response> = {}) {
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (method !== "GET") posted.push({ url, init: init ?? {} });
    const handler = overrides[`${method} ${url}`];
    if (handler) return handler(init);
    if (method === "GET" && url === "/api/v1/business-views") return Response.json({ items: [checkoutView] });
    if (method === "GET" && url === "/api/v1/business-views/checkout") return Response.json(checkoutView);
    if (method === "GET" && url === "/api/v1/connections?limit=100") return Response.json({ items: [connection] });
    throw new Error(`Unexpected API request: ${method} ${url}`);
  }));
}

function ModuleHarness({ route, suspended = false }: { route: string; suspended?: boolean }) {
  const props: WorkspaceModuleProps = { user: { role: "admin" } as WorkspaceModuleProps["user"], route, navigate, suspended, openEvidence: vi.fn() };
  const view = useSystemsModule(props);
  return (
    <div>
      <aside aria-label="模块列表">{view.list}</aside>
      <main aria-label="模块内容">{view.content}</main>
    </div>
  );
}

function requestBody(call: number): Record<string, unknown> {
  return JSON.parse(String(posted[call].init.body)) as Record<string, unknown>;
}

beforeEach(() => {
  navigate.mockReset();
  posted = [];
  HTMLElement.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("business views module", () => {
  it("renders the view list, explains optional organization, and routes selection", async () => {
    stubApi();
    render(<ModuleHarness route="/business-views" />);
    const list = within(screen.getByLabelText("模块列表"));
    expect(await list.findByText("结算")).toBeInTheDocument();
    expect(list.getByText("checkout")).toBeInTheDocument();
    expect(list.getByText("thanos-primary")).toBeInTheDocument();
    const main = within(screen.getByLabelText("模块内容"));
    expect(main.getByText("选择一个业务视图")).toBeInTheDocument();
    expect(main.getByText(/可选的组织方式/)).toBeInTheDocument();
    fireEvent.click(list.getByRole("button", { name: /结算/ }));
    expect(navigate).toHaveBeenCalledWith("/business-views?view=checkout");
    fireEvent.click(list.getByRole("button", { name: "新建" }));
    expect(navigate).toHaveBeenCalledWith("/business-views/new");
  });

  it("shows view scope, label conditions and routes to the editor", async () => {
    stubApi();
    render(<ModuleHarness route="/business-views?view=checkout" />);
    const main = within(screen.getByLabelText("模块内容"));
    expect(await main.findByRole("heading", { name: "结算" })).toBeInTheDocument();
    expect(main.getByText("结算业务范围说明")).toBeInTheDocument();
    expect(main.getByText("thanos-primary")).toBeInTheDocument();
    expect(main.getByText("service")).toBeInTheDocument();
    expect(main.getByText("checkout")).toBeInTheDocument();
    expect(main.getByText("更新时间")).toBeInTheDocument();
    fireEvent.click(main.getByRole("button", { name: "编辑" }));
    expect(navigate).toHaveBeenCalledWith("/business-views?view=checkout&edit=1");
  });

  it("offers an inspection entry carrying the view scope for preselection", async () => {
    stubApi();
    render(<ModuleHarness route="/business-views?view=checkout" />);
    const main = within(screen.getByLabelText("模块内容"));
    fireEvent.click(await main.findByRole("button", { name: /按此视图巡检/ }));
    expect(navigate).toHaveBeenCalledWith("/inspections?businessViewKey=checkout&connectionName=thanos-primary");
  });

  it("omits the inspection connection parameter when the view scopes all sources", async () => {
    stubApi({ "GET /api/v1/business-views/checkout": () => Response.json({ ...checkoutView, scope: { labelConditions: { service: "checkout" } } }) });
    render(<ModuleHarness route="/business-views?view=checkout" />);
    const main = within(screen.getByLabelText("模块内容"));
    fireEvent.click(await main.findByRole("button", { name: /按此视图巡检/ }));
    expect(navigate).toHaveBeenCalledWith("/inspections?businessViewKey=checkout");
  });

  it("creates a view through the shared draft and routes to its detail", async () => {
    stubApi({ "POST /api/v1/business-views": () => Response.json({ ...checkoutView, rowVersion: 1 }) });
    render(<ModuleHarness route="/business-views/new" />);
    fireEvent.change(await screen.findByLabelText("视图标识"), { target: { value: "checkout" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "结算" } });
    fireEvent.change(screen.getByLabelText("业务说明"), { target: { value: "结算业务范围说明" } });
    fireEvent.click(screen.getByRole("button", { name: "添加条件" }));
    fireEvent.change(screen.getByLabelText("标签"), { target: { value: "service" } });
    fireEvent.change(screen.getByLabelText("值"), { target: { value: "checkout" } });
    fireEvent.click(screen.getByLabelText("来源接入"));
    fireEvent.click(await screen.findByRole("option", { name: "thanos-primary" }));
    fireEvent.click(screen.getByRole("button", { name: "保存业务视图" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0].url).toBe("/api/v1/business-views");
    expect(posted[0].init.method).toBe("POST");
    const body = requestBody(0);
    expect(body.clientCommandId).toBeTruthy();
    expect(body).toMatchObject({
      viewKey: "checkout",
      displayName: "结算",
      description: "结算业务范围说明",
      scope: { connectionName: "thanos-primary", labelConditions: { service: "checkout" } },
    });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/business-views?view=checkout"));
  });

  it("keeps the form and YAML on one draft through one save path", async () => {
    stubApi({ "POST /api/v1/business-views": () => Response.json({ ...checkoutView, rowVersion: 1 }) });
    render(<ModuleHarness route="/business-views/new" />);
    fireEvent.change(await screen.findByLabelText("显示名称"), { target: { value: "结算" } });
    // Radix Tabs triggers activate on mousedown, not click.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "YAML" }));
    const yaml = screen.getByLabelText("业务视图 YAML") as HTMLTextAreaElement;
    await waitFor(() => expect(yaml.value).toContain("kind: BusinessView"));
    expect(yaml.value).toContain("displayName: 结算");
    fireEvent.change(yaml, {
      target: { value: "apiVersion: quoin/v1\nkind: BusinessView\nmetadata:\n  name: checkout\n  displayName: 结算系统\nspec:\n  scope:\n    labelConditions:\n      env: prod\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "保存业务视图" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    const scope = requestBody(0).scope as Record<string, unknown>;
    expect(scope.labelConditions).toEqual({ env: "prod" });
    expect("connectionName" in scope).toBe(false);
    expect(requestBody(0)).toMatchObject({ viewKey: "checkout", displayName: "结算系统" });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "表单" }));
    expect((screen.getByLabelText("显示名称") as HTMLInputElement).value).toBe("结算系统");
  });

  it("updates an existing view with optimistic row-version fencing and a frozen key", async () => {
    stubApi({ "PUT /api/v1/business-views/checkout": () => Response.json({ ...checkoutView, rowVersion: 4 }) });
    render(<ModuleHarness route="/business-views?view=checkout&edit=1" />);
    const key = (await screen.findByLabelText("视图标识")) as HTMLInputElement;
    expect(key.value).toBe("checkout");
    expect(key).toBeDisabled();
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "结算系统" } });
    fireEvent.click(screen.getByRole("button", { name: "保存业务视图" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0].url).toBe("/api/v1/business-views/checkout");
    expect(posted[0].init.method).toBe("PUT");
    expect(requestBody(0).expectedRowVersion).toBe(3);
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/business-views?view=checkout"));
  });

  it("surfaces save conflicts without destroying the draft", async () => {
    stubApi({ "PUT /api/v1/business-views/checkout": () => Response.json({ message: "视图已被其他人更新，请刷新后重试。" }, { status: 409 }) });
    render(<ModuleHarness route="/business-views?view=checkout&edit=1" />);
    fireEvent.change(await screen.findByLabelText("显示名称"), { target: { value: "结算系统" } });
    fireEvent.click(screen.getByRole("button", { name: "保存业务视图" }));
    expect(await screen.findByText("视图已被其他人更新，请刷新后重试。")).toBeInTheDocument();
    expect((screen.getByLabelText("显示名称") as HTMLInputElement).value).toBe("结算系统");
    expect(navigate).not.toHaveBeenCalled();
  });

  it("blocks saving an invalid view key and explains the rule", async () => {
    stubApi();
    render(<ModuleHarness route="/business-views/new" />);
    fireEvent.change(await screen.findByLabelText("视图标识"), { target: { value: "Bad Key" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "结算" } });
    expect(screen.getByRole("button", { name: "保存业务视图" })).toBeDisabled();
    expect(screen.getByText(/小写字母、数字或连字符/)).toBeInTheDocument();
    expect(posted).toHaveLength(0);
  });

  it("stays usable when the metrics connection list is unavailable", async () => {
    stubApi({
      "GET /api/v1/connections?limit=100": () => Response.json({ message: "无法读取连接。" }, { status: 503 }),
      "POST /api/v1/business-views": (init) => {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        return Response.json({ ...checkoutView, viewKey: body.viewKey, displayName: body.displayName, description: body.description, scope: body.scope, rowVersion: 1 });
      },
    });
    render(<ModuleHarness route="/business-views/new" />);
    fireEvent.change(await screen.findByLabelText("视图标识"), { target: { value: "all" } });
    fireEvent.change(screen.getByLabelText("显示名称"), { target: { value: "全部来源视图" } });
    await waitFor(() => expect(screen.getByText(/候选来源为全部接入/)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "保存业务视图" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(requestBody(0).scope).toEqual({ labelConditions: {} });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/business-views?view=all"));
  });

  it("disables saving while the workspace is suspended", async () => {
    stubApi();
    render(<ModuleHarness route="/business-views/new" suspended />);
    await screen.findByLabelText("视图标识");
    expect(screen.getByRole("button", { name: "保存业务视图" })).toBeDisabled();
  });
});
