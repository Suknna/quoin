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
import { Users } from "./Users";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const jsonResponse = (body: unknown, ok = true, status = 200) => ({
	ok,
	status,
	json: async () => body,
});

const adminRow = {
	id: "a1",
	username: "root",
	displayName: "Root",
	role: "admin",
	enabled: true,
	initialized: true,
	authRevision: 1,
	rowVersion: 3,
	passwordChangeRequired: false,
	lastLoginAt: null,
};
const operatorRow = {
	id: "u1",
	username: "op",
	displayName: "Operator",
	role: "operator",
	enabled: true,
	initialized: false,
	authRevision: 1,
	rowVersion: 7,
	passwordChangeRequired: false,
	lastLoginAt: null,
};

/** Routes by method+path; values that are already response-like pass through untouched. */
function stubUserFetch(
	handlers: {
		items?: unknown[];
		create?: unknown;
		update?: unknown;
		contacts?: unknown;
	} = {},
) {
	const respond = (value: unknown) =>
		typeof value === "object" && value !== null && "ok" in value
			? value
			: jsonResponse(value);
	const fetchMock = vi.fn(
		async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = new URL(String(input), "https://quoin.invalid");
			if (url.pathname === "/api/v1/admin/users" && init?.method === "POST")
				return respond(handlers.create ?? operatorRow);
			if (url.pathname === "/api/v1/admin/users/u1/contacts")
				return respond(handlers.contacts ?? operatorRow);
			if (url.pathname === "/api/v1/admin/users/u1")
				return respond(handlers.update ?? operatorRow);
			if (url.pathname === "/api/v1/admin/users")
				return respond({ items: handlers.items ?? [adminRow, operatorRow] });
			throw new Error(
				`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`,
			);
		},
	);
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

describe("Users", () => {
	it("creates operators without contacts and never sends a role choice", async () => {
		const fetchMock = stubUserFetch({ items: [] });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
		expect(screen.queryByText("admin")).not.toBeInTheDocument();

		// Contacts are optional display-only data (ADR-0010): the temporary
		// password alone drives the forced change, so the form submits with
		// every contact input blank.
		fireEvent.change(screen.getByLabelText("用户名"), {
			target: { value: "op2" },
		});
		fireEvent.change(screen.getByLabelText("显示名"), {
			target: { value: "Operator Two" },
		});
		fireEvent.change(screen.getByLabelText("临时密码"), {
			target: { value: "temp password long enough" },
		});
		const create = screen.getByRole("button", { name: "创建操作员" });
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
			contacts: [],
		});
	});

	it("attaches the email contact when the admin fills it in", async () => {
		const fetchMock = stubUserFetch({ items: [] });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		fireEvent.change(screen.getByLabelText("用户名"), {
			target: { value: "op3" },
		});
		fireEvent.change(screen.getByLabelText("显示名"), {
			target: { value: "Operator Three" },
		});
		fireEvent.change(screen.getByLabelText("临时密码"), {
			target: { value: "temp password long enough" },
		});
		fireEvent.change(screen.getByLabelText("邮箱联系方式"), {
			target: { value: "op3@example.com" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建操作员" }));

		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({
			clientCommandId: expect.any(String),
			username: "op3",
			displayName: "Operator Three",
			password: "temp password long enough",
			contacts: [{ channel: "email", target: "op3@example.com" }],
		});
	});

	it("closes the create dialog with feedback once the operator exists", async () => {
		const fetchMock = stubUserFetch({ items: [operatorRow] });
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		fireEvent.change(screen.getByLabelText("用户名"), {
			target: { value: "op2" },
		});
		fireEvent.change(screen.getByLabelText("显示名"), {
			target: { value: "Operator Two" },
		});
		fireEvent.change(screen.getByLabelText("临时密码"), {
			target: { value: "temp password long enough" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建操作员" }));

		// Success closes the dialog instead of leaving an emptied form open,
		// and the one-shot password note survives the close.
		expect(await screen.findByRole("status")).toHaveTextContent("操作员已创建");
		expect(
			screen.queryByRole("dialog", { name: "新建操作员" }),
		).not.toBeInTheDocument();
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
		const fetchMock = stubUserFetch({
			create: jsonResponse({ message: "用户名已存在。" }, false, 422),
		});
		render(<Users suspended={false} />);
		fireEvent.click(await screen.findByRole("button", { name: "新建操作员" }));
		fireEvent.change(screen.getByLabelText("用户名"), {
			target: { value: "op" },
		});
		fireEvent.change(screen.getByLabelText("显示名"), {
			target: { value: "Dup" },
		});
		fireEvent.change(screen.getByLabelText("临时密码"), {
			target: { value: "temp password long enough" },
		});
		fireEvent.change(screen.getByLabelText("邮箱联系方式"), {
			target: { value: "dup@example.com" },
		});
		fireEvent.click(screen.getByRole("button", { name: "创建操作员" }));

		// While the modal is open Radix aria-hides the page outside it, so the
		// error (rendered outside the dialog) is found by text, not by role.
		expect(await screen.findByText(/用户名已存在/)).toBeInTheDocument();
		// The dialog stays mounted so the admin can correct and retry.
		expect(
			screen.getByRole("dialog", { name: "新建操作员" }),
		).toBeInTheDocument();
		// Mount load plus the rejected POST; no reload happened.
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it("shows initialization status and gates admin actions behind the detail drawer", async () => {
		stubUserFetch();
		render(<Users suspended={false} />);
		const adminRowElement = (await screen.findByText("Root")).closest(
			"tr",
		) as HTMLTableRowElement;
		expect(within(adminRowElement).getByText("管理员")).toBeInTheDocument();
		expect(within(adminRowElement).getByText(/已初始化/)).toBeInTheDocument();

		// 管理员抽屉：不提供必然被后端拒绝的停用/配置渠道/重置密码。
		fireEvent.click(adminRowElement);
		let sheet = await screen.findByRole("dialog", { name: "Root" });
		expect(
			within(sheet).queryByRole("button", { name: "停用" }),
		).not.toBeInTheDocument();
		expect(
			within(sheet).queryByRole("button", { name: "配置渠道" }),
		).not.toBeInTheDocument();
		// The backend rejects admin password reset (account self-service or CLI
		// only); the drawer must not offer an inevitably rejected action.
		expect(
			within(sheet).queryByRole("button", { name: "重置密码" }),
		).not.toBeInTheDocument();
		expect(
			within(sheet).getByText(/设置 → 账户与安全/),
		).toBeInTheDocument();
		fireEvent.click(within(sheet).getByRole("button", { name: "Close" }));
		await waitFor(() =>
			expect(
				screen.queryByRole("dialog", { name: "Root" }),
			).not.toBeInTheDocument(),
		);

		// 操作员抽屉：停用、配置渠道、重置密码都可用。
		const operatorRowElement = screen
			.getByText("Operator")
			.closest("tr") as HTMLTableRowElement;
		expect(within(operatorRowElement).getByText("操作员")).toBeInTheDocument();
		expect(
			within(operatorRowElement).getByText(/未初始化/),
		).toBeInTheDocument();
		fireEvent.click(operatorRowElement);
		sheet = await screen.findByRole("dialog", { name: "Operator" });
		expect(
			within(sheet).getByRole("button", { name: "停用" }),
		).toBeInTheDocument();
		expect(
			within(sheet).getByRole("button", { name: "配置渠道" }),
		).toBeInTheDocument();
		expect(
			within(sheet).getByRole("button", { name: "重置密码" }),
		).toBeInTheDocument();
	});

	it("shows the user's masked contacts inside the detail drawer", async () => {
		stubUserFetch({
			contacts: {
				items: [
					{
						id: "c-1",
						channel: "email",
						maskedTarget: "o***@example.test",
						verified: true,
					},
				],
			},
		});
		render(<Users suspended={false} />);
		fireEvent.click(
			(await screen.findByText("Operator")).closest("tr") as HTMLTableRowElement,
		);
		const sheet = await screen.findByRole("dialog", { name: "Operator" });
		// 管理员可读任意用户的同一掩码投影；明文不出服务器。
		expect(
			await within(sheet).findByText("o***@example.test"),
		).toBeInTheDocument();
		expect(within(sheet).getByText("已验证")).toBeInTheDocument();
		expect(within(sheet).getByText("邮箱")).toBeInTheDocument();
	});

	it("replaces targets through the contacts command and allows clearing all channels", async () => {
		const fetchMock = stubUserFetch();
		render(<Users suspended={false} />);
		fireEvent.click(
			(await screen.findByText("Operator")).closest("tr") as HTMLTableRowElement,
		);
		const sheet = await screen.findByRole("dialog", { name: "Operator" });
		fireEvent.click(
			within(sheet).getByRole("button", { name: "配置渠道" }),
		);

		// Display-only contacts (ADR-0010): an empty set is a valid save —
		// it retires every channel instead of being blocked.
		const save = screen.getByRole("button", { name: "保存渠道" });
		expect(save).toBeDisabled();
		fireEvent.submit(save.closest("form") as HTMLFormElement);
		expect(
			fetchMock.mock.calls.filter(([, init]) => init?.method === "PUT"),
		).toHaveLength(0);
		fireEvent.click(
			screen.getByRole("checkbox", {
				name: "我确认以本表单替换全部现有联系方式",
			}),
		);
		expect(save).toBeEnabled();
		fireEvent.click(save);
		await waitFor(() =>
			expect(
				fetchMock.mock.calls.filter(([, init]) => init?.method === "PUT"),
			).toHaveLength(1),
		);
		const puts = () =>
			fetchMock.mock.calls.filter(([, init]) => init?.method === "PUT");
		expect(puts()[0][0]).toBe("/api/v1/admin/users/u1/contacts");
		expect(JSON.parse(String(puts()[0][1]?.body))).toMatchObject({
			clientCommandId: expect.any(String),
			expectedRowVersion: 7,
			contacts: [],
		});

		// A filled target rides the same command with its structured channel.
		fireEvent.click(
			within(
				await screen.findByRole("dialog", { name: "Operator" }),
			).getByRole("button", { name: "配置渠道" }),
		);
		fireEvent.change(screen.getByLabelText("邮箱联系方式"), {
			target: { value: "new@example.com" },
		});
		expect(screen.getByRole("button", { name: "保存渠道" })).toBeDisabled();
		fireEvent.click(
			screen.getByRole("checkbox", {
				name: "我确认以本表单替换全部现有联系方式",
			}),
		);
		fireEvent.click(screen.getByRole("button", { name: "保存渠道" }));
		await waitFor(() => expect(puts()).toHaveLength(2));
		expect(JSON.parse(String(puts()[1][1]?.body))).toMatchObject({
			clientCommandId: expect.any(String),
			expectedRowVersion: 7,
			contacts: [{ channel: "email", target: "new@example.com" }],
		});
	});

	it("sends the enabled toggle with the row-version fence and no role field", async () => {
		const fetchMock = stubUserFetch();
		render(<Users suspended={false} />);
		fireEvent.click(
			(await screen.findByText("Operator")).closest("tr") as HTMLTableRowElement,
		);
		fireEvent.click(
			within(
				await screen.findByRole("dialog", { name: "Operator" }),
			).getByRole("button", { name: "停用" }),
		);
		// 停用现在经过确认弹框；确认后才发送更新命令。
		fireEvent.click(await screen.findByRole("button", { name: "确认" }));

		const patchCalls = () =>
			fetchMock.mock.calls.filter(
				([input, init]) =>
					String(input) === "/api/v1/admin/users/u1" &&
					init?.method === "PATCH",
			);
		await waitFor(() => expect(patchCalls()).toHaveLength(1));
		const body = JSON.parse(String(patchCalls()[0][1]?.body));
		expect(body).toMatchObject({
			enabled: false,
			expectedRowVersion: 7,
			clientCommandId: expect.any(String),
		});
		expect(body.role).toBeUndefined();
	});
});
