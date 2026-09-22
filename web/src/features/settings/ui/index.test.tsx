import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { UserSummary } from "@/api/generated/types";
import { workbenchApi } from "@/api/workbench";
import { useSettingsModule } from "./index";

vi.mock("@/api/workbench", () => ({
	newClientCommandId: () => "command-id",
	notifyUnauthorized: vi.fn(),
	request: vi.fn(async (path: string) => {
		const response = await fetch(path);
		if (!response.ok) throw new Error("请求没有完成。");
		return response.json();
	}),
	workbenchApi: { changePassword: vi.fn(), currentUser: vi.fn() },
}));

const admin: UserSummary = {
	id: "1",
	username: "alice",
	displayName: "Alice",
	role: "admin",
	passwordChangeRequired: true,
	authRevision: 1,
	enabled: true,
	initialized: true,
	lastLoginAt: null,
	rowVersion: 1,
};
const operator: UserSummary = { ...admin, id: "2", role: "operator" };

function View({
	user = admin,
	route = "/settings/profile",
}: {
	user?: UserSummary;
	route?: string;
}) {
	const view = useSettingsModule({
		user,
		route,
		navigate: vi.fn(),
		suspended: false,
		openEvidence: vi.fn(),
	});
	return (
		<>
			{view.list}
			{view.content}
		</>
	);
}

function stubFetch(handlers: {
	contacts?: unknown;
	sessions?: unknown[];
	nextCursor?: string;
}) {
	const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
		const url = new URL(String(input), "https://quoin.invalid");
		if (url.pathname === "/api/v1/auth/contacts") {
			return {
				ok: true,
				status: 200,
				json: async () => ({ items: handlers.contacts ?? [] }),
			};
		}
		if (url.pathname === "/api/v1/auth/sessions") {
			return {
				ok: true,
				status: 200,
				json: async () => ({
					items: handlers.sessions ?? [],
					nextCursor: handlers.nextCursor,
				}),
			};
		}
		throw new Error(`unexpected request: ${url.pathname}`);
	});
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe("settings module", () => {
	it("renders a read-only profile with contacts and no duplicate logout", async () => {
		stubFetch({
			contacts: [
				{
					id: "c-1",
					channel: "email",
					maskedTarget: "a***@example.test",
					verified: true,
				},
			],
		});
		render(<View />);
		expect(screen.getByDisplayValue("alice")).toHaveAttribute("readonly");
		expect(screen.getByText("管理员")).toBeInTheDocument();
		expect(screen.queryByText("退出登录")).not.toBeInTheDocument();
		expect(await screen.findByText("a***@example.test")).toBeInTheDocument();
		// The inline self-service change flow retired with OTP (ADR-0010):
		// channels are displayed read-only here and administered in Users.
		expect(
			screen.queryByRole("button", { name: "更换" }),
		).not.toBeInTheDocument();
		expect(
			screen.getByText(/联系方式仅作展示，不用于验证或登录；由管理员在用户管理页维护/),
		).toBeInTheDocument();
		// The unified navigation highlights the active page.
		expect(screen.getByRole("button", { name: "个人资料" })).toHaveAttribute(
			"aria-current",
			"page",
		);
	});

	it("shows operators their channels read-only with an admin-maintenance hint", async () => {
		stubFetch({
			contacts: [
				{
					id: "c-1",
					channel: "email",
					maskedTarget: "o***@example.test",
					verified: true,
				},
			],
		});
		render(<View user={operator} />);
		expect(await screen.findByText("o***@example.test")).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "更换" }),
		).not.toBeInTheDocument();
		expect(screen.getByText(/联系方式仅作展示，不用于验证或登录；由管理员在用户管理页维护/)).toBeInTheDocument();
	});

	it("validates confirmation, changes the password, refreshes authoritative user state, and clears secrets", async () => {
		const changed = {
			...admin,
			passwordChangeRequired: false,
			authRevision: 2,
		};
		vi.mocked(workbenchApi.changePassword).mockResolvedValue();
		vi.mocked(workbenchApi.currentUser).mockResolvedValue(changed);
		stubFetch({});
		render(<View route="/settings/security" />);
		fireEvent.change(screen.getByLabelText("当前密码"), {
			target: { value: "current password long enough" },
		});
		fireEvent.change(screen.getByLabelText("新密码"), {
			target: { value: "new password long enough" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "different password long enough" },
		});
		fireEvent.click(screen.getByRole("button", { name: "更新密码" }));
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"两次输入的新密码不一致",
		);
		expect(workbenchApi.changePassword).not.toHaveBeenCalled();

		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "new password long enough" },
		});
		fireEvent.click(screen.getByRole("button", { name: "更新密码" }));
		await waitFor(() =>
			expect(workbenchApi.changePassword).toHaveBeenCalledWith({
				currentPassword: "current password long enough",
				newPassword: "new password long enough",
			}),
		);
		expect(workbenchApi.currentUser).toHaveBeenCalled();
		expect(screen.getByLabelText("当前密码")).toHaveValue("");
		expect(screen.getByLabelText("新密码")).toHaveValue("");
		expect(screen.getByLabelText("再次输入新密码")).toHaveValue("");
	});

	it("paginates sessions and asks for confirmation before revoking the current session", async () => {
		const session = (id: string, clientLabel: string, current: boolean) => ({
			id,
			clientLabel,
			createdAt: "2026-01-01T00:00:00Z",
			lastActiveAt: "2026-01-02T00:00:00Z",
			idleExpiresAt: "2026-01-03T00:00:00Z",
			absoluteExpiresAt: "2026-02-01T00:00:00Z",
			current,
		});
		const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
			const url = new URL(String(input), "https://quoin.invalid");
			if (url.pathname === "/api/v1/auth/sessions") {
				return {
					ok: true,
					status: 200,
					json: async () => ({
						items: url.search.includes("cursor=")
							? [session("other", "Firefox", false)]
							: [session("current", "Chrome", true)],
						nextCursor: url.search.includes("cursor=") ? undefined : "next",
					}),
				};
			}
			if (url.pathname === "/api/v1/auth/contacts")
				return { ok: true, status: 200, json: async () => ({ items: [] }) };
			return {
				ok: true,
				status: 204,
				json: async () => {
					throw new Error("204 must not be parsed");
				},
			};
		});
		vi.stubGlobal("fetch", fetchMock);
		render(<View route="/settings/security" />);
		expect(await screen.findByText("Chrome")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
		expect(await screen.findByText("Firefox")).toBeInTheDocument();
		expect(fetchMock.mock.calls[1][0]).toContain("cursor=next");
		fireEvent.click(screen.getAllByRole("button", { name: "撤销" })[0]);
		expect(await screen.findByText("撤销此会话？")).toBeInTheDocument();
		expect(
			screen.getByText(
				"这是当前设备。确认后将清除本设备认证并重新加载登录页面。",
			),
		).toBeInTheDocument();
	});
});
