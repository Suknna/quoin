import "@testing-library/jest-dom/vitest";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { isRuntimeSlotView } from "@/features/settings/platform/runtimes/api";
import { Runtimes } from "./Runtimes";

const runtime = {
	slot: "plinth",
	state: "registered",
	currentGeneration: 2,
	pendingGeneration: 3,
	retiringGeneration: 1,
	retirementState: "PendingRetirement",
	rowVersion: 8,
	connected: true,
};
describe("Runtimes", () => {
	afterEach(() => cleanup());
	it("confirms replacement registration and uses the expected row-version fence", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValue({ ok: true, json: async () => ({ plinth: runtime }) });
		vi.stubGlobal("fetch", fetchMock);
		render(<Runtimes suspended={false} />);
		await screen.findByRole("button", { name: "准备替代注册" });
		fireEvent.click(screen.getByRole("button", { name: "准备替代注册" }));
		expect(
			screen.getByRole("heading", { name: "替代 plinth 的注册凭据？" }),
		).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
		expect(fetchMock.mock.calls[1][0]).toBe(
			"/api/v1/runtime-slots/plinth/registration/prepare",
		);
		expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({
			expectedRowVersion: 8,
		});
	});
	it("labels an unregistered slot as first registration", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValue({
				ok: true,
				json: async () => ({ plinth: { ...runtime, state: "unregistered" } }),
			});
		vi.stubGlobal("fetch", fetchMock);
		render(<Runtimes suspended={false} />);
		expect(
			await screen.findByRole("button", { name: "准备首次注册" }),
		).toBeInTheDocument();
	});
	it("renders exactly the plinth slot", async () => {
		// 受控浏览器退役: the server projects only the plinth slot.
		const fetchMock = vi
			.fn()
			.mockResolvedValue({
				ok: true,
				json: async () => ({
					plinth: { ...runtime, slot: "plinth", state: "unregistered" },
				}),
			});
		vi.stubGlobal("fetch", fetchMock);
		const view = render(<Runtimes suspended={false} />);
		await waitFor(() =>
			expect(view.container.querySelectorAll("tbody tr")).toHaveLength(1),
		);
		expect(view.container.textContent).not.toContain("lintel");
		expect(
			view.container.querySelectorAll("tbody tr")[0].textContent,
		).toContain("plinth");
	});
	it("drops a malformed slot projection instead of rendering an unusable registration row", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValue({
				ok: true,
				json: async () => ({ plinth: { slot: "plinth" } }),
			});
		vi.stubGlobal("fetch", fetchMock);
		const view = render(<Runtimes suspended={false} />);
		await waitFor(() =>
			expect(view.container.querySelectorAll("tbody tr")).toHaveLength(0),
		);
	});
	it("guards slot projections before registration actions", () => {
		const valid = {
			slot: "lintel",
			state: "unregistered",
			currentGeneration: 0,
			rowVersion: 1,
			connected: false,
		};
		expect(isRuntimeSlotView(valid)).toBe(true);
		for (const malformed of [
			undefined,
			null,
			{},
			{ slot: "lintel" },
			{ ...valid, rowVersion: 0 },
			{ ...valid, state: "unknown" },
			{ ...valid, slot: "gateway" },
		]) {
			expect(isRuntimeSlotView(malformed)).toBe(false);
		}
	});
	it("shows the complete registration JSON accepted by the Plinth CLI", async () => {
		const revealed = {
			slot: "plinth",
			generation: 7,
			registrationToken: "one-time-registration-secret",
		};
		vi.stubGlobal(
			"fetch",
			vi
				.fn()
				.mockImplementation((path: string) =>
					Promise.resolve({
						ok: true,
						json: async () =>
							path === "/api/v1/runtime"
								? { plinth: runtime }
								: path.includes("prepare")
									? {
											registrationTokenAvailable: true,
											registrationTokenHandle: "x".repeat(32),
										}
									: revealed,
					}),
				),
		);
		render(<Runtimes suspended={false} />);
		fireEvent.click(
			await screen.findByRole("button", { name: "准备替代注册" }),
		);
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		const payload = await screen.findByRole("textbox", {
			name: "一次性注册凭据 JSON",
		});
		expect(JSON.parse((payload as HTMLTextAreaElement).value)).toEqual({
			slot: "plinth",
			generation: 7,
			token: revealed.registrationToken,
		});
		expect(payload).toHaveAttribute("readonly");
		expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "关闭并清除" }));
		expect(
			screen.queryByRole("textbox", { name: "一次性注册凭据 JSON" }),
		).not.toBeInTheDocument();
	});
	it("clears a revealed secret explicitly", async () => {
		const fetchMock = vi
			.fn()
			.mockImplementation((path: string) =>
				path === "/api/v1/runtime"
					? Promise.resolve({
							ok: true,
							json: async () => ({ plinth: runtime }),
						})
					: path.includes("prepare")
						? Promise.resolve({
								ok: true,
								json: async () => ({
									registrationTokenAvailable: true,
									registrationTokenHandle: "x".repeat(32),
								}),
							})
						: Promise.resolve({
								ok: true,
								json: async () => ({ registrationToken: "secret" }),
							}),
			);
		vi.stubGlobal("fetch", fetchMock);
		render(<Runtimes suspended={false} />);
		fireEvent.click(
			await screen.findByRole("button", { name: "准备替代注册" }),
		);
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		expect(await screen.findByText(/一次性注册令牌/)).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "关闭并清除" }));
		expect(screen.queryByText(/一次性注册令牌/)).not.toBeInTheDocument();
	});
	it("does not restore an asynchronously revealed secret after suspension", async () => {
		let reveal: (response: unknown) => void = () => {};
		const fetchMock = vi.fn().mockImplementation((path: string) =>
			path === "/api/v1/runtime"
				? Promise.resolve({ ok: true, json: async () => ({ plinth: runtime }) })
				: path.includes("prepare")
					? Promise.resolve({
							ok: true,
							json: async () => ({
								registrationTokenAvailable: true,
								registrationTokenHandle: "x".repeat(32),
							}),
						})
					: new Promise((resolve) => {
							reveal = resolve;
						}),
		);
		vi.stubGlobal("fetch", fetchMock);
		const view = render(<Runtimes suspended={false} />);
		await screen.findByRole("button", { name: "准备替代注册" });
		fireEvent.click(screen.getByRole("button", { name: "准备替代注册" }));
		fireEvent.click(screen.getByRole("button", { name: "确认" }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
		view.rerender(<Runtimes suspended />);
		reveal({ ok: true, json: async () => ({ registrationToken: "secret" }) });
		await waitFor(() =>
			expect(screen.queryByText(/一次性注册令牌/)).not.toBeInTheDocument(),
		);
	});
});
