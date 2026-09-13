import "@testing-library/jest-dom/vitest";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useAdministrationModule } from "./index";
function View({ role = "operator", route = "/administration/users" }: { role?: "admin" | "operator"; route?: string }) { const view = useAdministrationModule({ user: { id: "1", username: "a", displayName: "A", role, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }, route, navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }); return <>{view.list}{view.content}</>; }
describe("administration module", () => {
 it("keeps administration inaccessible to operators without rendering management links", () => { render(<View />); expect(screen.getByRole("alert")).toHaveTextContent("仅向管理员开放"); expect(screen.queryByRole("button", { name: "用户" })).not.toBeInTheDocument(); expect(screen.queryByRole("button", { name: "关于" })).not.toBeInTheDocument(); });
 it("keeps Settings limited to retained pages and one model-provider entry", () => { render(<View role="admin" route="/admin/users" />); expect(screen.getAllByRole("button", { name: "模型提供方" }).length).toBeGreaterThan(0); for (const name of ["连接", "告警源", "告警接入问题", "标签契约"]) expect(screen.queryByRole("button", { name })).not.toBeInTheDocument(); expect(screen.getByRole("button", { name: "运行时" })).toBeInTheDocument(); });
 it("gates direct Journey links and marks their navigation action unavailable", () => { render(<View role="admin" route="/admin/journeys" />); expect(screen.getByText("浏览器巡检开发中")).toBeInTheDocument(); expect(screen.getByText(/已存储的目录、身份和巡检数据不会被删除或修改/)).toBeInTheDocument(); expect(screen.getAllByRole("button", { name: "Journey（开发中）" }).every((button) => button.hasAttribute("disabled"))).toBe(true); });
 it("renders an unknown-page alert instead of unrelated Journeys content", () => { render(<View role="admin" route="/admin/runtimes" />); expect(screen.getByText("未找到此管理页面。")).toBeInTheDocument(); expect(screen.queryByRole("heading", { name: "Journey" })).not.toBeInTheDocument(); });
});
