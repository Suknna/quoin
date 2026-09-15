import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { UserSummary } from "@/api/generated/types";
import { completeContactChange, ContactChange } from "./ContactChange";

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

const jsonResponse = (body: unknown, ok = true, status = 200) => ({ ok, status, json: async () => body });
const noContentResponse = () => ({
	ok: true,
	status: 204,
	json: async () => {
		throw new Error("204 must not be parsed");
	},
});

const admin: UserSummary = { id: "1", username: "root", displayName: "Root", role: "admin", passwordChangeRequired: false, authRevision: 1, enabled: true, initialized: true, lastLoginAt: null, rowVersion: 1 };
const operator: UserSummary = { ...admin, id: "2", username: "op", displayName: "Op", role: "operator", initialized: false };

const startedFlow = {
	type: "contact_change",
	contacts: [{ id: "c-old", channel: "email", maskedTarget: "r***@example.com", verified: true }],
	candidate: null,
	passwordSet: false,
	expiresAt: "2026-09-15T10:00:00Z",
};

/** Routes the flow's POST/GET requests; every call is recorded for assertions. */
function stubFlowFetch(handlers: {
	start?: unknown;
	contacts?: unknown;
	challenge?: unknown;
	verify?: unknown;
	complete?: unknown;
	read?: unknown;
} = {}) {
	const respond = (value: unknown) =>
		typeof value === "object" && value !== null && "ok" in value ? value : jsonResponse(value);
	const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
		const url = new URL(String(input), "https://quoin.invalid");
		if (url.pathname === "/api/v1/auth/contact-change/complete") return respond(handlers.complete ?? noContentResponse());
		if (url.pathname === "/api/v1/auth/contact-change") return respond(handlers.start ?? startedFlow);
		if (url.pathname === "/api/v1/auth/flow" && (init?.method ?? "GET") === "GET") return respond(handlers.read ?? startedFlow);
		if (url.pathname === "/api/v1/auth/flow/contacts") return respond(handlers.contacts ?? { id: "cand-1", channel: "email", maskedTarget: "n***@example.com", verified: false });
		if (url.pathname === "/api/v1/auth/flow/challenge") return respond(handlers.challenge ?? { id: "cand-1", channel: "email", maskedTarget: "n***@example.com", verified: false });
		if (url.pathname === "/api/v1/auth/flow/verify") return respond(handlers.verify ?? { completed: false });
		throw new Error(`unexpected request: ${init?.method ?? "GET"} ${url.pathname}`);
	});
	vi.stubGlobal("fetch", fetchMock);
	return fetchMock;
}

/** Drives the flow up to the verify stage with the default email channel. */
async function walkToVerify() {
	const reload = vi.fn();
	vi.stubGlobal("location", { ...window.location, reload });
	const fetchMock = stubFlowFetch({ read: { ...startedFlow, passwordSet: true, candidate: { id: "cand-1", channel: "email", maskedTarget: "n***@example.com", verified: true } } });
	render(<ContactChange user={admin} suspended={false} />);
	fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "current password long" } });
	fireEvent.click(screen.getByRole("button", { name: "开始更换" }));
	fireEvent.change(await screen.findByLabelText("邮箱目标"), { target: { value: "new@example.com" } });
	fireEvent.click(screen.getByRole("button", { name: "暂存新渠道" }));
	fireEvent.click(await screen.findByRole("button", { name: "发送验证码" }));
	fireEvent.change(await screen.findByLabelText("验证码"), { target: { value: "012345" } });
	return { fetchMock, reload };
}

describe("ContactChange", () => {
	it("shows operators a read-only notice without any change controls", () => {
		render(<ContactChange user={operator} suspended={false} />);
		expect(screen.getByText(/收码渠道由管理员指定/)).toBeInTheDocument();
		expect(screen.queryByLabelText("当前密码")).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "开始更换" })).not.toBeInTheDocument();
	});

	it("walks start -> stage -> send -> verify -> confirm -> finish and reloads to login", async () => {
		const { fetchMock, reload } = await walkToVerify();

		// The old channel stays visible (masked) while the candidate is pending.
		expect(screen.getByText(/r\*\*\*@example\.com/)).toBeInTheDocument();

		// 200 with completed:false still proves contact_change verification.
		fireEvent.click(screen.getByRole("button", { name: "验证" }));
		const confirmed = await screen.findByLabelText("再次输入当前密码");
		expect(screen.getByText(/已验证候选渠道/)).toBeInTheDocument();
		expect(screen.getByText(/n\*\*\*@example\.com/)).toBeInTheDocument();

		fireEvent.change(confirmed, { target: { value: "current password long" } });
		fireEvent.click(screen.getByRole("button", { name: "确认更换" }));

		await waitFor(() => expect(reload).toHaveBeenCalled());
		const bodies = fetchMock.mock.calls.map(([, init]) => (init?.body ? JSON.parse(String(init.body)) : null));
		expect(bodies[0]).toEqual({ currentPassword: "current password long" });
		expect(bodies[1]).toEqual({ channel: "email", target: "new@example.com" });
		expect(bodies[2]).toEqual({ contactId: "cand-1" });
		expect(bodies[3]).toEqual({ code: "012345" });
		expect(bodies[4]).toBeNull(); // flow re-read after verification
		expect(bodies[5]).toEqual({ currentPassword: "current password long" });
		expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
			"/api/v1/auth/contact-change",
			"/api/v1/auth/flow/contacts",
			"/api/v1/auth/flow/challenge",
			"/api/v1/auth/flow/verify",
			"/api/v1/auth/flow",
			"/api/v1/auth/contact-change/complete",
		]);
	});

	it("keeps the old channel intact and says so when a stage fails", async () => {
		stubFlowFetch({ contacts: jsonResponse({ message: "收码目标格式不正确。" }, false, 422) });
		render(<ContactChange user={admin} suspended={false} />);
		fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "current password long" } });
		fireEvent.click(screen.getByRole("button", { name: "开始更换" }));
		expect(await screen.findByText(/当前渠道/)).toBeInTheDocument();

		fireEvent.change(screen.getByLabelText("邮箱目标"), { target: { value: "not-an-email" } });
		fireEvent.click(screen.getByRole("button", { name: "暂存新渠道" }));

		const alert = await screen.findByRole("alert");
		expect(alert).toHaveTextContent("收码目标格式不正确。");
		expect(alert).toHaveTextContent("原收码渠道保持不变");
		expect(screen.getByText(/r\*\*\*@example\.com/)).toBeInTheDocument();
		expect(screen.queryByText("候选渠道：")).not.toBeInTheDocument();
	});

	it("clears the password from memory when leaving the start stage and never persists it", async () => {
		const setItem = vi.fn();
		vi.stubGlobal("localStorage", { setItem, getItem: vi.fn(), removeItem: vi.fn(), clear: vi.fn() });
		stubFlowFetch();
		render(<ContactChange user={admin} suspended={false} />);
		const password = screen.getByLabelText("当前密码");
		fireEvent.change(password, { target: { value: "current password long" } });
		fireEvent.click(screen.getByRole("button", { name: "开始更换" }));
		expect(await screen.findByLabelText("邮箱目标")).toBeInTheDocument();
		expect(screen.queryByLabelText("当前密码")).not.toBeInTheDocument();
		expect(setItem).not.toHaveBeenCalled();

		fireEvent.click(screen.getByRole("button", { name: "放弃本次变更" }));
		expect(screen.getByLabelText("当前密码")).toHaveValue("");
	});

	it("completes through the wire API without parsing the 204 body", async () => {
		const fetchMock = vi.fn().mockResolvedValue(noContentResponse());
		vi.stubGlobal("fetch", fetchMock);
		await expect(completeContactChange("current password long")).resolves.toBeUndefined();
		expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/contact-change/complete");
		expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ currentPassword: "current password long" });
	});
});
