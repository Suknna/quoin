import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Users } from "./Users";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const jsonResponse = (body: unknown, ok = true, status = 200) => ({ ok, status, json: async () => body });

const adminRow = { id: "a1", username: "root", displayName: "Root", role: "admin", enabled: true, initialized: true, authRevision: 1, rowVersion: 3, passwordChangeRequired: false, lastLoginAt: null };
const operatorRow = { id: "u1", username: "op", displayName: "Operator", role: "operator", enabled: true, initialized: false, authRevision: 1, rowVersion: 7, passwordChangeRequired: false, lastLoginAt: null };

/** Routes by method+path; values that are already response-like pass through untouched. */
function stubUserFetch(handlers: { items?: unknown[]; create?: unknown; update?: unknown; contacts?: unknown } = {}) {
	const respond = (value: unknown) =>
		typeof value === "object" && value !== null && "ok" in value ? value : jsonResponse(value);
	const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
		const url = new URL(String(input), "https://quoin.invalid");
		if (url.pathname === "/api/v1/admin/users" && init?.method === "POST") return respond(handlers.create ?? operatorRow);
		if (url.pathname === "/api/v1/admin/users/u1/contacts") return respond(handlers.contacts ?? operatorRow);
		if (url.pathname === "/api/v1/admin/users/u1") return respond(handlers.update ?? operatorRow);
		if (url.pathname === "/api/v1/admin/users") return respond({ items: handlers.items ?? [adminRow, operatorRow] });
		throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`);
	});
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

describe("Users", () => {
	it("creates operators with contacts and never sends a role choice", async () => {
		const fetchMock = stubUserFetch({ items: [] });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
		expect(screen.queryByText("admin")).not.toBeInTheDocument();

		fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "op2" } });
		fireEvent.change(screen.getByLabelText("显示名"), { target: { value: "Operator Two" } });
		fireEvent.change(screen.getByLabelText("临时密码"), { target: { value: "temp password long enough" } });
		const create = screen.getByRole("button", { name: "创建操作员" });
		expect(create).toBeDisabled();

		fireEvent.change(screen.getByLabelText("邮箱收码目标"), { target: { value: "op2@example.com" } });
		expect(create).toBeEnabled();
		fireEvent.click(create);

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/users");
		expect(fetchMock.mock.calls[1][1]?.method).toBe("POST");
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({
			clientCommandId: expect.any(String),
			username: "op2",
			displayName: "Operator Two",
			password: "temp password long enough",
			contacts: [{ channel: "email", target: "op2@example.com" }],
		});
	});

	it("closes the create dialog with feedback once the operator exists", async () => {
		const fetchMock = stubUserFetch({ items: [operatorRow] });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "op2" } });
		fireEvent.change(screen.getByLabelText("显示名"), { target: { value: "Operator Two" } });
		fireEvent.change(screen.getByLabelText("临时密码"), { target: { value: "temp password long enough" } });
		fireEvent.change(screen.getByLabelText("邮箱收码目标"), { target: { value: "op2@example.com" } });
		fireEvent.click(screen.getByRole("button", { name: "创建操作员" }));

		// Success closes the dialog instead of leaving an emptied form open,
		// and the one-shot password note survives the close.
		expect(await screen.findByRole("status")).toHaveTextContent("操作员已创建");
		expect(screen.queryByRole("dialog", { name: "新建操作员" })).not.toBeInTheDocument();
		expect(screen.queryByLabelText("临时密码")).not.toBeInTheDocument();
		// The refreshed table shows the new row: mount load, POST, reload.
		expect(await screen.findByText("Operator")).toBeInTheDocument();
		expect(fetchMock).toHaveBeenCalledTimes(3);
		// Opening the create dialog again starts from a clean slate.
		fireEvent.click(screen.getByRole("button", { name: "新建操作员" }));
		expect(screen.getByLabelText("用户名")).toHaveValue("");
		expect(screen.queryByRole("status")).not.toBeInTheDocument();
	});

	it("keeps creation failures in the dialog without closing it", async () => {
		const fetchMock = stubUserFetch({ create: jsonResponse({ message: "用户名已存在。" }, false, 422) });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "op" } });
		fireEvent.change(screen.getByLabelText("显示名"), { target: { value: "Dup" } });
		fireEvent.change(screen.getByLabelText("临时密码"), { target: { value: "temp password long enough" } });
		fireEvent.change(screen.getByLabelText("邮箱收码目标"), { target: { value: "dup@example.com" } });
		fireEvent.click(screen.getByRole("button", { name: "创建操作员" }));

		// While the modal is open Radix aria-hides the page outside it, so the
		// error (rendered outside the dialog) is found by text, not by role.
		expect(await screen.findByText(/用户名已存在/)).toBeInTheDocument();
		// The dialog stays mounted so the admin can correct and retry.
		expect(screen.getByRole("dialog", { name: "新建操作员" })).toBeInTheDocument();
		// Mount load plus the rejected POST; no reload happened.
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it("shows initialization status and refuses to disable, re-channel, or reset the unique admin", async () => {
		stubUserFetch();
		render(<Users suspended={false} />);
		const adminRowElement = (await screen.findByText("Root")).closest("tr") as HTMLTableRowElement;
		expect(within(adminRowElement).getByText("管理员")).toBeInTheDocument();
		expect(within(adminRowElement).getByText("已初始化")).toBeInTheDocument();
		expect(within(adminRowElement).queryByRole("button", { name: "停用" })).not.toBeInTheDocument();
		expect(within(adminRowElement).queryByRole("button", { name: "配置渠道" })).not.toBeInTheDocument();
		// The backend rejects admin password reset (account self-service or CLI
		// only); the row must not offer an inevitably rejected action.
		expect(within(adminRowElement).queryByRole("button", { name: "重置密码" })).not.toBeInTheDocument();
		expect(within(adminRowElement).getByText(/设置 → 个人资料/)).toBeInTheDocument();

		const operatorRowElement = screen.getByText("Operator").closest("tr") as HTMLTableRowElement;
		expect(within(operatorRowElement).getByText("操作员")).toBeInTheDocument();
		expect(within(operatorRowElement).getByText("未初始化")).toBeInTheDocument();
		expect(within(operatorRowElement).getByRole("button", { name: "停用" })).toBeInTheDocument();
		expect(within(operatorRowElement).getByRole("button", { name: "配置渠道" })).toBeInTheDocument();
		expect(within(operatorRowElement).getByRole("button", { name: "重置密码" })).toBeInTheDocument();
	});

	it("requires at least one channel and replaces targets through the contacts command", async () => {
		const fetchMock = stubUserFetch();
		render(<Users suspended={false} />);
		const operatorRowElement = (await screen.findByText("Operator")).closest("tr") as HTMLTableRowElement;
		fireEvent.click(within(operatorRowElement).getByRole("button", { name: "配置渠道" }));

		const save = screen.getByRole("button", { name: "保存渠道" });
		expect(save).toBeDisabled();

		fireEvent.change(screen.getByLabelText("邮箱收码目标"), { target: { value: "new@example.com" } });
		expect(save).toBeEnabled();
		fireEvent.click(save);

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/users/u1/contacts");
		expect(fetchMock.mock.calls[1][1]?.method).toBe("PUT");
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toMatchObject({
			clientCommandId: expect.any(String),
			expectedRowVersion: 7,
			contacts: [{ channel: "email", target: "new@example.com" }],
		});
	});

	it("sends the enabled toggle with the row-version fence and no role field", async () => {
		const fetchMock = stubUserFetch();
		render(<Users suspended={false} />);
		const operatorRowElement = (await screen.findByText("Operator")).closest("tr") as HTMLTableRowElement;
		fireEvent.click(within(operatorRowElement).getByRole("button", { name: "停用" }));

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(fetchMock.mock.calls[1][0]).toBe("/api/v1/admin/users/u1");
		const body = JSON.parse(String(fetchMock.mock.calls[1][1]?.body));
		expect(body).toMatchObject({ enabled: false, expectedRowVersion: 7, clientCommandId: expect.any(String) });
		expect(body.role).toBeUndefined();
	});
});
