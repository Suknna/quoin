import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Security } from "./Security";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const at = "2026-09-21T16:42:42.600220827Z";
const currentSession = {
	id: "9",
	clientLabel: "Chrome on Linux",
	createdAt: at,
	lastActiveAt: at,
	idleExpiresAt: at,
	absoluteExpiresAt: at,
	current: true,
};
const otherSession = {
	id: "8",
	clientLabel: "Firefox on macOS",
	createdAt: at,
	lastActiveAt: at,
	idleExpiresAt: at,
	absoluteExpiresAt: at,
	current: false,
};

/** Stubs the sessions page load; revoke responses are per-test. */
function stubSecurityFetch(revokeResponse?: unknown) {
	const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
		const url = new URL(String(input), "https://quoin.invalid");
		if (
			url.pathname === "/api/v1/auth/sessions" &&
			init?.method !== "POST"
		) {
			return {
				ok: true,
				status: 200,
				json: async () => ({ items: [currentSession, otherSession] }),
			};
		}
		if (url.pathname === "/api/v1/auth/sessions/8/revoke") {
			return (
				revokeResponse ?? {
					ok: true,
					status: 204,
					json: async () => undefined,
				}
			);
		}
		throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`);
	});
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

const rowOf = (label: string) =>
	screen.getByText(label).closest("tr") as HTMLTableRowElement;

describe("Security sessions", () => {
	it("never offers revoking the current session; logout is the only path", async () => {
		stubSecurityFetch();
		render(<Security suspended={false} onUserChanged={vi.fn()} />);

		// HTTP-AUTH-004：revokeOwnSession MUST NOT 撤销当前请求所用会话，
		// 后端确定性拒绝（active_conflict）。界面不得提供必然失败的按钮。
		const currentRow = await waitFor(() => {
			const row = rowOf("Chrome on Linux");
			expect(within(row).getByText("当前")).toBeInTheDocument();
			return row;
		});
		expect(
			within(currentRow).queryByRole("button", { name: "撤销" }),
		).not.toBeInTheDocument();
		expect(
			within(currentRow).getByText(/退出登录/),
		).toBeInTheDocument();

		const otherRow = rowOf("Firefox on macOS");
		expect(
			within(otherRow).getByRole("button", { name: "撤销" }),
		).toBeInTheDocument();
	});

	it("revokes another session after confirmation and closes the dialog", async () => {
		const fetchMock = stubSecurityFetch();
		render(<Security suspended={false} onUserChanged={vi.fn()} />);

		await screen.findByText("Firefox on macOS");
		fireEvent.click(
			within(rowOf("Firefox on macOS")).getByRole("button", { name: "撤销" }),
		);
		const dialog = await screen.findByRole("alertdialog");
		expect(
			within(dialog).getByText("该设备将需要重新登录；其他设备保持不变。"),
		).toBeInTheDocument();
		fireEvent.click(within(dialog).getByRole("button", { name: "确认撤销" }));

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/auth/sessions/8/revoke");
		expect(fetchMock.mock.calls[1][1]?.method).toBe("POST");
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({
			clientCommandId: expect.any(String),
		});
		// The dialog closes only on success, and the list reloads.
		await waitFor(() =>
			expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
		);
		expect(fetchMock).toHaveBeenCalledTimes(3);
	});

	it("keeps a failed revoke inside the dialog with persistent feedback", async () => {
		const fetchMock = stubSecurityFetch({
			ok: false,
			status: 422,
			json: async () => ({
				code: "validation_failed",
				message: "请求字段不满足要求，请检查后重试。",
			}),
		});
		render(<Security suspended={false} onUserChanged={vi.fn()} />);

		await screen.findByText("Firefox on macOS");
		fireEvent.click(
			within(rowOf("Firefox on macOS")).getByRole("button", { name: "撤销" }),
		);
		fireEvent.click(
			within(await screen.findByRole("alertdialog")).getByRole("button", {
				name: "确认撤销",
			}),
		);

		// 回归：失败原因钉在确认框内（toast 一闪即逝曾让用户以为点击无效），
		// 框保持打开、按钮恢复，可重试或取消。
		const dialog = await screen.findByRole("alertdialog");
		await within(dialog).findByText("请求字段不满足要求，请检查后重试。");
		expect(
			within(dialog).getByRole("button", { name: "确认撤销" }),
		).toBeEnabled();
		// Mount load plus the rejected POST; no reload happened.
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});
});
