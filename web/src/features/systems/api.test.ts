import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createBusinessView, getBusinessView, listBusinessViews, updateBusinessView, type BusinessView } from "./api";

const checkoutView: BusinessView = {
  viewKey: "checkout",
  displayName: "结算",
  description: "结算业务范围说明",
  scope: { connectionName: "thanos-primary", labelConditions: { service: "checkout" } },
  rowVersion: 3,
  createdAt: "2026-09-13T01:00:00.000Z",
  updatedAt: "2026-09-13T02:00:00.000Z",
};

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(async () => Response.json({ items: [] })));
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubFetchOnce(response: Response) {
  (fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce(response);
}

describe("business views API client", () => {
  it("lists views from the collection endpoint", async () => {
    stubFetchOnce(Response.json({ items: [checkoutView] }));
    await expect(listBusinessViews()).resolves.toEqual([checkoutView]);
    const [url] = (fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/api/v1/business-views");
  });

  it("reads a single view by encoded key", async () => {
    stubFetchOnce(Response.json(checkoutView));
    await expect(getBusinessView("checkout")).resolves.toMatchObject({ viewKey: "checkout", rowVersion: 3 });
    const [url] = (fetch as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(String(url)).toBe("/api/v1/business-views/checkout");
  });

  it("creates a view with an idempotent client command id and full draft payload", async () => {
    stubFetchOnce(Response.json(checkoutView));
    const saved = await createBusinessView({
      viewKey: "checkout",
      displayName: "结算",
      description: "",
      scope: { labelConditions: { service: "checkout" } },
    });
    expect(saved).toMatchObject({ viewKey: "checkout" });
    const [url, init] = (fetch as ReturnType<typeof vi.fn>).mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/business-views");
    expect(init.method).toBe("POST");
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body.clientCommandId).toBeTruthy();
    expect(body).toMatchObject({ viewKey: "checkout", displayName: "结算", scope: { labelConditions: { service: "checkout" } } });
  });

  it("updates a view with optimistic row-version fencing", async () => {
    stubFetchOnce(Response.json({ ...checkoutView, rowVersion: 4 }));
    const saved = await updateBusinessView(
      "checkout",
      { displayName: "结算系统", description: "", scope: { labelConditions: {} } },
      3,
    );
    expect(saved).toMatchObject({ rowVersion: 4 });
    const [url, init] = (fetch as ReturnType<typeof vi.fn>).mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/business-views/checkout");
    expect(init.method).toBe("PUT");
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(body.clientCommandId).toBeTruthy();
    expect(body.expectedRowVersion).toBe(3);
    expect(body).toMatchObject({ displayName: "结算系统" });
  });

  it("surfaces the server problem message instead of a generic failure", async () => {
    stubFetchOnce(Response.json({ message: "视图标识已存在。", code: "conflict" }, { status: 409 }));
    await expect(createBusinessView({ viewKey: "checkout", displayName: "结算", description: "", scope: { labelConditions: {} } })).rejects.toThrow("视图标识已存在。");
  });
});
