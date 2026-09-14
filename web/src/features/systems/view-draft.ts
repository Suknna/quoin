// One draft, two editing views. The form and the YAML text always project the
// same ViewDraft, and saving always goes through toPayload() -> the single API
// write path. Unknown YAML fields are rejected instead of being silently lost.

import { parse as parseYaml, stringify as stringifyYaml } from "yaml";
import type { BusinessViewScope } from "./api";

export interface ViewDraft {
  viewKey: string;
  displayName: string;
  description: string;
  connectionName?: string;
  labelConditions: Record<string, string>;
}

/** Stable user-readable keys follow the deployment-wide DNS-like convention; the server stays authoritative. */
export const viewKeyPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function emptyDraft(): ViewDraft {
  return { viewKey: "", displayName: "", description: "", labelConditions: {} };
}

export function draftOf(view: { viewKey: string; displayName: string; description: string; scope: BusinessViewScope }): ViewDraft {
  return {
    viewKey: view.viewKey,
    displayName: view.displayName,
    description: view.description,
    ...(view.scope.connectionName ? { connectionName: view.scope.connectionName } : {}),
    labelConditions: { ...view.scope.labelConditions },
  };
}

/** Local checks only; the backend remains authoritative and may still reject. */
export function draftProblems(draft: ViewDraft): string[] {
  const problems: string[] = [];
  if (!viewKeyPattern.test(draft.viewKey)) problems.push("视图标识需为小写字母、数字或连字符，并以字母或数字开头和结尾。");
  if (!draft.displayName.trim()) problems.push("显示名称不能为空。");
  for (const [key, value] of Object.entries(draft.labelConditions)) {
    if (!key.trim()) problems.push("标签条件的键不能为空。");
    if (!value.trim()) problems.push("标签条件的值不能为空。");
  }
  return problems;
}

export function toPayload(draft: ViewDraft): {
  viewKey: string;
  displayName: string;
  description: string;
  scope: BusinessViewScope;
} {
  return {
    viewKey: draft.viewKey,
    displayName: draft.displayName,
    description: draft.description,
    scope: {
      ...(draft.connectionName ? { connectionName: draft.connectionName } : {}),
      labelConditions: { ...draft.labelConditions },
    },
  };
}

const isObject = (value: unknown): value is Record<string, unknown> => Boolean(value) && typeof value === "object" && !Array.isArray(value);

/** Rejects keys that the draft model does not understand so form round-trips never drop data silently. */
function rejectUnknown(value: Record<string, unknown>, allowed: string[], where: string) {
  const unknown = Object.keys(value).filter((key) => !allowed.includes(key));
  if (unknown.length) throw new Error(`${where} 包含不支持的字段：${unknown.join("、")}。`);
}

export function draftYaml(draft: ViewDraft): string {
  return stringifyYaml({
    apiVersion: "quoin/v1",
    kind: "BusinessView",
    metadata: { name: draft.viewKey, displayName: draft.displayName, ...(draft.description ? { description: draft.description } : {}) },
    spec: { scope: { ...(draft.connectionName ? { connectionName: draft.connectionName } : {}), ...(Object.keys(draft.labelConditions).length ? { labelConditions: draft.labelConditions } : {}) } },
  });
}

export function parseViewYaml(yaml: string): ViewDraft {
  const root = parseYaml(yaml);
  if (!isObject(root)) throw new Error("业务视图必须是一个 YAML 映射。");
  if (root.apiVersion !== "quoin/v1" || root.kind !== "BusinessView") throw new Error("业务视图必须包含 apiVersion: quoin/v1 和 kind: BusinessView。");
  rejectUnknown(root, ["apiVersion", "kind", "metadata", "spec"], "文档");
  const metadata = root.metadata;
  if (!isObject(metadata)) throw new Error("metadata 必须是映射。");
  rejectUnknown(metadata, ["name", "displayName", "description"], "metadata");
  const viewKey = metadata.name;
  if (typeof viewKey !== "string" || !viewKeyPattern.test(viewKey)) throw new Error("metadata.name (viewKey) 需为小写字母、数字或连字符。");
  if (typeof metadata.displayName !== "string" || !metadata.displayName.trim()) throw new Error("metadata.displayName 不能为空。");
  if (metadata.description !== undefined && (typeof metadata.description !== "string")) throw new Error("metadata.description 必须是字符串。");
  let scope: BusinessViewScope = { labelConditions: {} };
  if (root.spec !== undefined) {
    if (!isObject(root.spec)) throw new Error("spec 必须是映射。");
    rejectUnknown(root.spec, ["scope"], "spec");
    if (root.spec.scope !== undefined) {
      if (!isObject(root.spec.scope)) throw new Error("spec.scope 必须是映射。");
      rejectUnknown(root.spec.scope, ["connectionName", "labelConditions"], "spec.scope");
      const { connectionName, labelConditions } = root.spec.scope;
      if (connectionName !== undefined && (typeof connectionName !== "string" || !connectionName.trim())) throw new Error("spec.scope.connectionName 必须是非空字符串。");
      if (labelConditions !== undefined) {
        if (!isObject(labelConditions)) throw new Error("标签条件必须是非空字符串映射。");
        if (Object.values(labelConditions).some((item) => typeof item !== "string" || !item)) throw new Error("标签条件必须是非空字符串映射。");
        scope = {
          ...(typeof connectionName === "string" && connectionName ? { connectionName } : {}),
          labelConditions: labelConditions as Record<string, string>,
        };
      } else if (typeof connectionName === "string" && connectionName) {
        scope = { connectionName, labelConditions: {} };
      }
    }
  }
  return {
    viewKey,
    displayName: metadata.displayName,
    description: typeof metadata.description === "string" ? metadata.description : "",
    ...(scope.connectionName ? { connectionName: scope.connectionName } : {}),
    labelConditions: { ...scope.labelConditions },
  };
}
