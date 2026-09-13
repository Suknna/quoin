import "@testing-library/jest-dom/vitest";
import {
	act,
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
	within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const appApiState = vi.hoisted(() => ({
	unauthorized: undefined as (() => void) | undefined,
}));
vi.mock("@/features/evidence/ui", () => ({
	EvidenceReader: () => <p>证据正文</p>,
}));

vi.mock("@/api/workbench", async (importOriginal) => {
	const actual = await importOriginal<typeof import("@/api/workbench")>();
	return {
		...actual,
		setUnauthorizedHandler: vi.fn((handler?: () => void) => {
			appApiState.unauthorized = handler;
		}),
	};
});

import { authUser, modelProviderDetail, otherUser } from "@/api/fixtures";
import { WorkbenchApiError, workbenchApi } from "@/api/workbench";
import { App } from "./App";

beforeEach(() => {
	vi.stubGlobal(
		"ResizeObserver",
		class {
			observe() {}
			unobserve() {}
			disconnect() {}
		},
	);
	vi.stubGlobal(
		"matchMedia",
		vi.fn().mockReturnValue({
			matches: false,
			addEventListener: vi.fn(),
			removeEventListener: vi.fn(),
		}),
	);
	vi.stubGlobal("scrollTo", vi.fn());
	window.history.replaceState(null, "", "/admin/model_provider/new");
	window.location.hash = "";
});
afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe("authentication workflow", () => {
	it("does not disguise unavailable authentication as a logged-out session and retries explicitly", async () => {
		const me = vi
			.spyOn(workbenchApi, "currentUser")
			.mockRejectedValueOnce(new WorkbenchApiError(503, "服务当前不可用"))
			.mockRejectedValueOnce(new WorkbenchApiError(401, "未登录"));
		render(<App />);
		expect(await screen.findByText("暂时无法连接 Quoin")).toBeInTheDocument();
		expect(screen.queryByLabelText("用户名")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "重新连接" }));
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(me).toHaveBeenCalledTimes(2);
	});

	it("uses the password-change stage before requesting protected connections", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue({
			...authUser,
			passwordChangeRequired: true,
		});
		const list = vi.spyOn(workbenchApi, "listConnections");
		render(<App />);
		expect(await screen.findByLabelText("当前临时密码")).toBeInTheDocument();
		expect(list).not.toHaveBeenCalled();
		fireEvent.change(screen.getByLabelText("当前临时密码"), {
			target: { value: "current password long enough" },
		});
		fireEvent.change(screen.getByLabelText("新密码"), {
			target: { value: "new password long enough" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "different password long enough" },
		});
		const change = vi.spyOn(workbenchApi, "changePassword");
		fireEvent.submit(
			screen.getByRole("button", { name: "保存并进入工作台" }).closest("form")!,
		);
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"两次输入的新密码不一致",
		);
		expect(change).not.toHaveBeenCalled();
	});

	it("keeps an operator read-only and does not make mutation requests", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(otherUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		const create = vi.spyOn(workbenchApi, "createConnection");
		render(<App />);
		// Operators have no administration action surface, even when entering an admin URL directly.
		expect(await screen.findByRole("alert")).toHaveTextContent("仅向管理员开放");
		expect(screen.queryByRole("button", { name: "新建" })).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "创建连接" })).not.toBeInTheDocument();
		expect(create).not.toHaveBeenCalled();
	});

	it("opens the non-dismissible reauthentication dialog after a protected 401", async () => {
		// Capture the callback App registers instead of calling API internals directly.
		appApiState.unauthorized = undefined;
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		render(<App />);
		await screen.findByRole("button", { name: "创建模型提供方" });

		act(() => appApiState.unauthorized?.());

		expect(await screen.findByRole("dialog")).toHaveTextContent("会话已失效");
		expect(screen.getByRole("dialog")).toHaveTextContent("请重新登录后继续");
	});

	it("clears a suspended credential draft while retaining harmless fields", async () => {
		appApiState.unauthorized = undefined;
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		render(<App />);
		const name = await screen.findByLabelText("名称");
		const baseUrl = screen.getByLabelText("Base URL");
		const apiKey = screen.getByLabelText("API Key");
		fireEvent.change(name, { target: { value: "main" } });
		fireEvent.change(baseUrl, { target: { value: "https://provider.example" } });
		fireEvent.change(apiKey, { target: { value: "top secret" } });

		act(() => appApiState.unauthorized?.());

		await screen.findByRole("dialog");
		expect(name).toHaveValue("main");
		expect(baseUrl).toHaveValue("https://provider.example");
		expect(apiKey).toHaveValue("");
	});

	it("preserves a suspended draft when the same user signs in again", async () => {
		appApiState.unauthorized = undefined;
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		vi.spyOn(workbenchApi, "login").mockResolvedValue(authUser);
		render(<App />);
		const name = await screen.findByLabelText("名称");
		fireEvent.change(name, { target: { value: "same-user-draft" } });
		act(() => appApiState.unauthorized?.());
		await screen.findByRole("dialog");
		const dialog = screen.getByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("用户名"), {
			target: { value: "admin" },
		});
		fireEvent.change(within(dialog).getByLabelText("密码"), {
			target: { value: "a password long enough" },
		});
		fireEvent.click(within(dialog).getByRole("button", { name: "登录" }));
		await screen.findByLabelText("名称");
		expect(await screen.findByLabelText("名称")).toHaveValue("same-user-draft");
	});

	it("remounts and clears a suspended draft when a different user signs in", async () => {
		appApiState.unauthorized = undefined;
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		vi.spyOn(workbenchApi, "login").mockResolvedValue(otherUser);
		render(<App />);
		fireEvent.change(await screen.findByLabelText("名称"), {
			target: { value: "old-user-draft" },
		});
		act(() => appApiState.unauthorized?.());
		const dialog = await screen.findByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("用户名"), {
			target: { value: "operator" },
		});
		fireEvent.change(within(dialog).getByLabelText("密码"), {
			target: { value: "a password long enough" },
		});
		fireEvent.click(within(dialog).getByRole("button", { name: "登录" }));
		await waitFor(() => expect(screen.getByLabelText("名称")).toHaveValue(""));
	});

	it("allows manual model IDs when discovery is unavailable and clears stale discovery", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		const discover = vi
			.spyOn(workbenchApi, "discoverProviderModels")
			.mockResolvedValueOnce({
				available: false,
				items: [],
				detail: "未返回模型",
			})
			.mockImplementationOnce(
				() =>
					new Promise((resolve) =>
						setTimeout(
							() =>
								resolve({ available: true, items: [{ id: "stale-model" }] }),
							20,
						),
					),
			);
		render(<App />);
		await screen.findByLabelText("Base URL");
		fireEvent.change(screen.getByLabelText("Base URL"), {
			target: { value: "https://provider.invalid" },
		});
		fireEvent.change(screen.getByLabelText("API Key"), {
			target: { value: "secret" },
		});
		fireEvent.click(screen.getByRole("button", { name: "发现模型" }));
		expect(await screen.findByRole("status")).toHaveTextContent(
			"可以直接手工填写模型 ID",
		);
		fireEvent.change(screen.getByLabelText("对话模型 ID"), {
			target: { value: "manual-chat" },
		});
		expect(screen.getByLabelText("对话模型 ID")).toHaveValue("manual-chat");
		fireEvent.click(screen.getByRole("button", { name: "发现模型" }));
		fireEvent.change(screen.getByLabelText("Base URL"), {
			target: { value: "https://changed.invalid" },
		});
		await new Promise((resolve) => setTimeout(resolve, 30));
		expect(screen.queryByText("stale-model")).not.toBeInTheDocument();
		expect(discover).toHaveBeenCalledTimes(2);
	});

	it("clears a model API key when the session is suspended", async () => {
		appApiState.unauthorized = undefined;
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		render(<App />);
		const apiKey = await screen.findByLabelText("API Key");
		fireEvent.change(apiKey, { target: { value: "provider-secret" } });
		act(() => appApiState.unauthorized?.());
		await screen.findByRole("dialog");
		expect(apiKey).toHaveValue("");
	});

	it("keeps the source module and connection draft mounted behind evidence", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		render(<App />);
		const name = await screen.findByLabelText("名称");
		fireEvent.change(name, { target: { value: "preserved-draft" } });
		await screen.findByLabelText("API Key");
		window.history.pushState(
			null,
			"",
			"/evidence/e-1?from=%2Fadmin%2Fmodel_provider%2Fnew",
		);
		window.dispatchEvent(new PopStateEvent("popstate"));
		const dialog = await screen.findByRole("dialog", { name: "证据阅读" });
		expect(dialog).toHaveTextContent("证据正文");
		expect(within(dialog).getAllByRole("button")).toHaveLength(1);
		fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
		const restoredName = await screen.findByLabelText("名称");
		expect(restoredName).toHaveValue("preserved-draft");
		expect(screen.getByLabelText("API Key")).toBeInTheDocument();
	});

	it("opens the expired-session dialog when a focus reconciliation returns 401", async () => {
		vi.spyOn(workbenchApi, "currentUser")
			.mockResolvedValueOnce(authUser)
			.mockRejectedValueOnce(new WorkbenchApiError(401, "expired"));
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		render(<App />);
		await screen.findByLabelText("名称");
		window.dispatchEvent(new Event("focus"));
		expect(await screen.findByRole("dialog")).toHaveTextContent("会话已失效");
	});

	it("enables a model provider using only the newest matching passed probe result", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([
			modelProviderDetail,
		]);
		vi.spyOn(workbenchApi, "fetchConnection").mockResolvedValue(
			modelProviderDetail,
		);
		vi.spyOn(workbenchApi, "listProbeResults").mockResolvedValue([
			{
				id: "old",
				attemptId: "1",
				connectionType: "model_provider",
				connectionRevisionId: "11",
				credentialGenerationId: "12",
				rootBindingRevision: 1,
				actionSetId: "model",
				actionSetVersion: 1,
				probeContractDigest: "x",
				outcome: "passed",
				resultDigest: "x",
				startedAt: "2026-01-01T00:00:00Z",
				finishedAt: "2026-01-01T00:00:00Z",
				details: {},
			},
			{
				id: "new",
				attemptId: "2",
				connectionType: "model_provider",
				connectionRevisionId: "11",
				credentialGenerationId: "12",
				rootBindingRevision: 1,
				actionSetId: "model",
				actionSetVersion: 1,
				probeContractDigest: "x",
				outcome: "passed",
				resultDigest: "x",
				startedAt: "2026-01-02T00:00:00Z",
				finishedAt: "2026-01-02T00:00:00Z",
				details: {},
			},
			{
				id: "wrong",
				attemptId: "3",
				connectionType: "model_provider",
				connectionRevisionId: "other",
				credentialGenerationId: "12",
				rootBindingRevision: 1,
				actionSetId: "model",
				actionSetVersion: 1,
				probeContractDigest: "x",
				outcome: "passed",
				resultDigest: "x",
				startedAt: "2026-01-03T00:00:00Z",
				finishedAt: "2026-01-03T00:00:00Z",
				details: {},
			},
		]);
		vi.spyOn(workbenchApi, "listRevisions").mockResolvedValue([]);
		vi.spyOn(workbenchApi, "listCredentialGenerations").mockResolvedValue([]);
		const enable = vi
			.spyOn(workbenchApi, "enableConnection")
			.mockResolvedValue({ ...modelProviderDetail, enabled: true });
			window.history.replaceState(null, "", "/admin/model_provider/models%2Fmain");
			render(<App />);
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "启用连接" })).toBeEnabled(),
		);
		fireEvent.click(screen.getByRole("button", { name: "启用连接" }));
		fireEvent.click(await screen.findByRole("button", { name: "确认启用" }));
		await waitFor(() =>
			expect(enable).toHaveBeenCalledWith("models/main", 3, "new"),
		);
	});

		it("returns to login after logout receives an already-expired 401", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		vi.spyOn(workbenchApi, "logout").mockRejectedValue(
			new WorkbenchApiError(401, "expired"),
		);
		render(<App />);
		const accountMenu = await screen.findByRole("button", { name: /Admin/ });
		fireEvent.pointerDown(accountMenu);
		fireEvent.click(accountMenu);
		fireEvent.click(await screen.findByText("退出登录"));
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
	});

	it("keeps the workbench open and reports a failed 503 logout", async () => {
		vi.spyOn(workbenchApi, "currentUser").mockResolvedValue(authUser);
		vi.spyOn(workbenchApi, "maintenance").mockResolvedValue(null);
		vi.spyOn(workbenchApi, "listConnections").mockResolvedValue([]);
		vi.spyOn(workbenchApi, "logout").mockRejectedValue(
			new WorkbenchApiError(503, "退出服务不可用"),
		);
		render(<App />);
		const accountMenu = await screen.findByRole("button", { name: /Admin/ });
		fireEvent.pointerDown(accountMenu);
		fireEvent.click(accountMenu);
		fireEvent.click(await screen.findByText("退出登录"));
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"退出服务不可用",
		);
		expect(screen.queryByLabelText("用户名")).not.toBeInTheDocument();
	});
});
