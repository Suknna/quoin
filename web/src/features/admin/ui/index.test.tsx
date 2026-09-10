import "@testing-library/jest-dom/vitest";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useAdministrationModule } from "./index";
function View({ role = "operator", route = "/administration/users" }: { role?: "admin" | "operator"; route?: string }) { const view = useAdministrationModule({ user: { id: "1", username: "a", displayName: "A", role, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }, route, navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }); return <>{view.list}{view.content}</>; }
describe("administration module", () => {
 it("keeps administration inaccessible to operators without rendering management links", () => { render(<View />); expect(screen.getByRole("alert")).toHaveTextContent("仅向管理员开放"); expect(screen.queryByRole("button", { name: "用户" })).not.toBeInTheDocument(); expect(screen.queryByRole("button", { name: "关于" })).not.toBeInTheDocument(); });
 it("resolves an absolute administration route and exposes its real alert source form", async () => { vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ items: [] }) })); render(<View role="admin" route="/administration/alerts" />); expect(await screen.findByText("告警源与凭据")).toBeInTheDocument(); expect(screen.getByLabelText("告警源键")).toBeInTheDocument(); expect(screen.getByRole("button", { name: "创建告警源" })).toBeInTheDocument(); });
 it("offers the existing connections module but not separate runtime or maintenance navigation", () => { render(<View role="admin" route="/admin/users" />); expect(screen.getAllByRole("button", { name: "连接" }).length).toBeGreaterThan(0); expect(screen.queryByRole("button", { name: "运行时" })).not.toBeInTheDocument(); expect(screen.queryByRole("button", { name: "维护" })).not.toBeInTheDocument(); });
 it("renders an unknown-page alert instead of unrelated Journeys content", () => { render(<View role="admin" route="/admin/runtimes" />); expect(screen.getByText("未找到此管理页面。")).toBeInTheDocument(); expect(screen.queryByRole("heading", { name: "Journey" })).not.toBeInTheDocument(); });
});
