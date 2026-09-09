import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { workbenchApi } from "@/api/workbench";
import { useAccountModule } from "./index";

vi.mock("@/api/workbench", () => ({
	newClientCommandId: () => "command-id",
	notifyUnauthorized: vi.fn(),
	workbenchApi: { changePassword: vi.fn(), currentUser: vi.fn() },
}));

const user = { id: "1", username: "alice", displayName: "Alice", role: "operator" as const, passwordChangeRequired: true, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 };
function View({ route = "/account" }: { route?: string }) { const view = useAccountModule({ user, route, navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }); return <>{view.list}{view.content}</>; }

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("account module", () => {
	it("renders a read-only profile with actual role label and no duplicate logout", () => {
		render(<View />);
		expect(screen.getByDisplayValue("alice")).toHaveAttribute("readonly");
		expect(screen.getByText("操作员")).toBeInTheDocument();
		expect(screen.queryByText("退出登录")).not.toBeInTheDocument();
	});

	it("validates confirmation, changes the password, refreshes authoritative user state, and clears secrets", async () => {
		const changed = { ...user, passwordChangeRequired: false, authRevision: 2 };
		vi.mocked(workbenchApi.changePassword).mockResolvedValue();
		vi.mocked(workbenchApi.currentUser).mockResolvedValue(changed);
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ items: [] }) }));
		render(<View route="/account/security" />);
		fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "current password long enough" } });
		fireEvent.change(screen.getByLabelText("新密码"), { target: { value: "new password long enough" } });
		fireEvent.change(screen.getByLabelText("再次输入新密码"), { target: { value: "different password long enough" } });
		fireEvent.click(screen.getByRole("button", { name: "更新密码" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("两次输入的新密码不一致");
		expect(workbenchApi.changePassword).not.toHaveBeenCalled();

		fireEvent.change(screen.getByLabelText("再次输入新密码"), { target: { value: "new password long enough" } });
		fireEvent.click(screen.getByRole("button", { name: "更新密码" }));
		await waitFor(() => expect(workbenchApi.changePassword).toHaveBeenCalledWith({ currentPassword: "current password long enough", newPassword: "new password long enough" }));
		expect(workbenchApi.currentUser).toHaveBeenCalled();
		expect(screen.getByLabelText("当前密码")).toHaveValue("");
		expect(screen.getByLabelText("新密码")).toHaveValue("");
		expect(screen.getByLabelText("再次输入新密码")).toHaveValue("");
	});

	it("paginates sessions and asks for confirmation before revoking the current session", async () => {
		const fetch = vi.fn()
			.mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ items: [{ id: "current", clientLabel: "Chrome", createdAt: "2026-01-01T00:00:00Z", lastActiveAt: "2026-01-02T00:00:00Z", idleExpiresAt: "2026-01-03T00:00:00Z", absoluteExpiresAt: "2026-02-01T00:00:00Z", current: true }], nextCursor: "next" }) })
			.mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ items: [{ id: "other", clientLabel: "Firefox", createdAt: "2026-01-01T00:00:00Z", lastActiveAt: "2026-01-02T00:00:00Z", idleExpiresAt: "2026-01-03T00:00:00Z", absoluteExpiresAt: "2026-02-01T00:00:00Z", current: false }] }) });
		vi.stubGlobal("fetch", fetch);
		render(<View route="/account/security" />);
		expect(await screen.findByText("Chrome")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
		expect(await screen.findByText("Firefox")).toBeInTheDocument();
		expect(fetch.mock.calls[1][0]).toContain("cursor=next");
		fireEvent.click(screen.getAllByRole("button", { name: "撤销" })[0]);
		expect(await screen.findByText("撤销此会话？")).toBeInTheDocument();
		expect(screen.getByText("这是当前设备。确认后将清除本设备认证并重新加载登录页面。")).toBeInTheDocument();
		expect(fetch).toHaveBeenCalledTimes(2);
	});
});
