import { describe, expect, it } from "vitest";
import { draftProblems, draftYaml, emptyDraft, parseViewYaml, toPayload, type ViewDraft } from "./view-draft";

const checkoutDraft: ViewDraft = {
  viewKey: "checkout",
  displayName: "结算",
  description: "结算业务范围说明",
  connectionName: "thanos-primary",
  labelConditions: { service: "checkout", env: "prod" },
  alertSourceKeys: ["am-prod"],
};

describe("business view draft", () => {
  it("round-trips a draft through YAML without losing fields", () => {
    const parsed = parseViewYaml(draftYaml(checkoutDraft));
    expect(parsed).toEqual(checkoutDraft);
  });

  it("omits optional fields from YAML when unset", () => {
    const yaml = draftYaml({ ...emptyDraft(), viewKey: "checkout", displayName: "结算" });
    expect(yaml).not.toContain("connectionName");
    expect(yaml).not.toContain("labelConditions");
    expect(yaml).not.toContain("alertSourceKeys");
    expect(parseViewYaml(yaml)).toEqual({ viewKey: "checkout", displayName: "结算", description: "", labelConditions: {}, alertSourceKeys: [] });
  });

  it("rejects documents that are not quoin/v1 BusinessView", () => {
    expect(() => parseViewYaml("apiVersion: quoin/v1\nkind: BusinessSystem\n")).toThrow("kind: BusinessView");
  });

  it("rejects unknown fields instead of silently dropping them", () => {
    expect(() => parseViewYaml(`${draftYaml(checkoutDraft)}extra: mystery\n`)).toThrow("extra");
    expect(() => parseViewYaml("apiVersion: quoin/v1\nkind: BusinessView\nmetadata: {name: checkout, displayName: 结算}\nspec:\n  scope:\n    extra: mystery\n")).toThrow("extra");
  });

  it("rejects empty display names and non-string label values", () => {
    expect(() => parseViewYaml(draftYaml({ ...checkoutDraft, displayName: " " }))).toThrow("displayName");
    expect(() => parseViewYaml(draftYaml({ ...checkoutDraft, labelConditions: { env: 1 as unknown as string } }))).toThrow("标签条件");
  });

  it("rejects non-string alert source keys", () => {
    expect(() => parseViewYaml(draftYaml({ ...checkoutDraft, alertSourceKeys: ["am-prod", 1 as unknown as string] }))).toThrow("alertSourceKeys");
  });

  it("rejects invalid view keys", () => {
    expect(() => parseViewYaml(draftYaml({ ...checkoutDraft, viewKey: "Bad Key" }))).toThrow("viewKey");
  });

  it("reports draft problems for the form before save", () => {
    expect(draftProblems({ ...emptyDraft(), viewKey: "Bad Key" })).toHaveLength(2);
    expect(draftProblems({ ...checkoutDraft, labelConditions: { service: "" } })).toEqual(["标签条件的值不能为空。"]);
    expect(draftProblems(checkoutDraft)).toEqual([]);
    // 参与告警归属必须有标签条件：空条件绝不构成吞掉一切的兜底匹配。
    expect(draftProblems({ ...checkoutDraft, labelConditions: {}, alertSourceKeys: ["am-prod"] })).toEqual(["参与告警归属的视图必须至少一个标签条件。"]);
    // 不参与告警归属的普通视图不需要标签条件。
    expect(draftProblems({ ...emptyDraft(), viewKey: "plain", displayName: "普通", alertSourceKeys: [] })).toEqual([]);
  });

  it("builds the wire payload with an explicit scope and omits the connection name when unset", () => {
    expect(toPayload(checkoutDraft)).toEqual({
      viewKey: "checkout",
      displayName: "结算",
      description: "结算业务范围说明",
      scope: { connectionName: "thanos-primary", labelConditions: { service: "checkout", env: "prod" }, alertSourceKeys: ["am-prod"] },
    });
    expect(toPayload({ ...emptyDraft(), viewKey: "all", displayName: "全部" }).scope).toEqual({ labelConditions: {} });
  });
});
