import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkbenchApiError } from "@/api/workbench";
import type { UserSummary } from "@/api/generated/types";
import type { AuthFlow } from "./api";

const flowApi = vi.hoisted(() => ({
	start: vi.fn(),
	resume: vi.fn(),
	setPassword: vi.fn(),
	addContact: vi.fn(),
	sendChallenge: vi.fn(),
	verify: vi.fn(),
	complete: vi.fn(),
	readDelivery: vi.fn(),
	saveDelivery: vi.fn(),
}));

vi.mock("@/features/authentication/api", async (importOriginal) => {
	const actual =
		await importOriginal<typeof import("@/features/authentication/api")>();
	return { ...actual, authFlowApi: flowApi };
});

import { AuthScreen } from "./AuthScreen";

const flowUser = { id: "1", username: "admin", displayName: "Admin" };
const unverifiedEmail = {
	id: "c1",
	channel: "email",
	maskedTarget: "a***@quoin.dev",
	verified: false,
} as const;
const verifiedEmail = { ...unverifiedEmail, verified: true };
const sessionUser: UserSummary = {
	id: "1",
	username: "admin",
	displayName: "Admin",
	role: "admin",
	enabled: true,
	initialized: true,
	passwordChangeRequired: false,
	authRevision: 1,
	rowVersion: 1,
	lastLoginAt: null,
};
const noFlow = () => new WorkbenchApiError(404, "没有进行中的认证流程");
const emptyDelivery = {
	configuration: {},
	rowVersion: 0,
	source: "",
	configured: false,
};

/** Builds a flow projection; factorVerified defaults to the pending false. */
function flowOf(overrides: Partial<AuthFlow>): AuthFlow {
	return {
		type: "admin_initialize",
		user: flowUser,
		expiresAt: "2026-09-15T10:00:00Z",
		contacts: [],
		factorVerified: false,
		...overrides,
	};
}

beforeEach(() => {
	vi.stubGlobal(
		"ResizeObserver",
		class {
			observe() {}
			unobserve() {}
			disconnect() {}
		},
	);
	// jsdom lacks scrollIntoView, which the Radix select popup calls on open.
	Element.prototype.scrollIntoView ??= vi.fn();
	if (!document.elementFromPoint) {
		document.elementFromPoint = () => null;
	}
	flowApi.resume.mockRejectedValue(noFlow());
	flowApi.readDelivery.mockResolvedValue(emptyDelivery);
});

afterEach(() => {
	cleanup();
	// reset (not clear) so leftover once-queues never leak across tests.
	vi.resetAllMocks();
	vi.unstubAllGlobals();
});

async function submitCredentials() {
	fireEvent.change(await screen.findByLabelText("用户名"), {
		target: { value: "admin" },
	});
	fireEvent.change(screen.getByLabelText("密码"), {
		target: { value: "a password long enough" },
	});
	fireEvent.click(screen.getByRole("button", { name: "登录" }));
}

async function sendAndEnterCode(code = "012345") {
	fireEvent.click(screen.getByRole("button", { name: "发送验证码" }));
	await waitFor(() =>
		expect(flowApi.sendChallenge).toHaveBeenCalledWith("c1"),
	);
	fireEvent.change(screen.getByLabelText("验证码"), {
		target: { value: code },
	});
	fireEvent.click(screen.getByRole("button", { name: "验证并继续" }));
	await waitFor(() => expect(flowApi.verify).toHaveBeenCalledWith(code));
}

describe("AuthScreen", () => {
	it("shows the login form when no flow is active", async () => {
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(screen.getByRole("status")).toHaveTextContent("正在恢复认证状态…");
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(flowApi.resume).toHaveBeenCalledTimes(1);
	});

	it("resumes a running login flow after a refresh instead of the login form", async () => {
		flowApi.resume.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		expect(screen.queryByLabelText("用户名")).not.toBeInTheDocument();
		// No code is requested behind the user's back; sending is explicit.
		expect(flowApi.sendChallenge).not.toHaveBeenCalled();
	});

	it("sends a stale login row straight to OTP, never back into a password step", async () => {
		// Old pending login rows may still carry passwordSet=false; temp
		// credentials now classify as initialization flows, so a login flow
		// must always land on the challenge, or it would loop on a password
		// the server no longer accepts for this class.
		flowApi.resume.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail], passwordSet: false }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		expect(screen.queryByLabelText("新密码")).not.toBeInTheDocument();
	});

	it("completes a login challenge into a workbench session", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		flowApi.verify.mockResolvedValue({ completed: true, user: sessionUser });
		const authenticated = vi.fn();
		render(<AuthScreen onAuthenticated={authenticated} />);
		await submitCredentials();
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		await sendAndEnterCode();
		await waitFor(() => expect(authenticated).toHaveBeenCalledWith(sessionUser));
		// A completed login never touches the initialization finish endpoint.
		expect(flowApi.complete).not.toHaveBeenCalled();
	});

	it("renders invalid credentials generically and keeps the form mounted", async () => {
		flowApi.start.mockRejectedValue(
			new WorkbenchApiError(401, "用户名或密码不正确。"),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"用户名或密码不正确",
		);
		expect(screen.getByLabelText("用户名")).toHaveValue("admin");
		expect(screen.getByLabelText("密码")).toHaveValue("");
	});

	it("re-challenges an already verified contact when initialization restarts", async () => {
		// A restarted/recovered initialization keeps the account-level
		// verified contact, but completion needs a factor verified INSIDE
		// this flow: the step router must follow factorVerified, not
		// contact.verified, or completion would 422 forever.
		flowApi.resume
			.mockResolvedValueOnce(
				flowOf({ contacts: [verifiedEmail], passwordSet: true }),
			)
			.mockResolvedValueOnce(
				flowOf({ contacts: [verifiedEmail], passwordSet: true, factorVerified: true }),
			);
		flowApi.verify.mockResolvedValue({ completed: false });
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		// The verified contact does NOT skip the challenge; no finish yet.
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		expect(
			screen.queryByRole("button", { name: "完成初始化" }),
		).not.toBeInTheDocument();
		expect(screen.queryByLabelText("邮箱地址")).not.toBeInTheDocument();
		await sendAndEnterCode();
		// Only after the per-flow verify does the finish step appear.
		fireEvent.click(await screen.findByRole("button", { name: "完成初始化" }));
		await waitFor(() => expect(flowApi.complete).toHaveBeenCalledTimes(1));
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(screen.getByRole("status")).toHaveTextContent("初始化完成");
	});

	it("keeps a delivery-settings entry after a refresh at the OTP step", async () => {
		// The post-password delivery overlay is transient state; a refresh at
		// contact/OTP must still reach the missing channel configuration.
		flowApi.resume.mockResolvedValue(
			flowOf({ contacts: [unverifiedEmail], passwordSet: true }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		fireEvent.click(
			screen.getByRole("button", { name: "验证码投递设置" }),
		);
		expect(await screen.findByLabelText("SMTP 服务器")).toBeInTheDocument();
		// Returning lands back on the same flow-derived OTP step, unharmed.
		fireEvent.click(screen.getByRole("button", { name: "返回继续初始化" }));
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		expect(flowApi.resume).toHaveBeenCalledTimes(1);
		await sendAndEnterCode();
		expect(flowApi.verify).toHaveBeenCalledWith("012345");
	});

	it("walks admin initialization: password, contact, verification, completion", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ contacts: [], passwordSet: false }),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(flowOf({ passwordSet: true }))
			.mockResolvedValueOnce(
				flowOf({ contacts: [unverifiedEmail], passwordSet: true }),
			)
			.mockResolvedValueOnce(
				flowOf({ contacts: [unverifiedEmail], passwordSet: true, factorVerified: true }),
			);
		flowApi.verify.mockResolvedValue({ completed: false });
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();

		// Password step: a mismatch never reaches the API.
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "different new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"两次输入的新密码不一致",
		);
		expect(flowApi.setPassword).not.toHaveBeenCalled();

		fireEvent.change(screen.getByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		expect(flowApi.setPassword).toHaveBeenCalledWith("brand new password");

		// Delivery settings open before the contact step; the admin may skip them.
		await screen.findByText("验证码投递设置");
		fireEvent.click(screen.getByRole("button", { name: "返回继续初始化" }));

		// Contact step opens for an admin flow without any contact.
		await screen.findByLabelText("邮箱地址");
		fireEvent.change(screen.getByLabelText("邮箱地址"), {
			target: { value: "root@quoin.dev" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存联系方式" }));
		expect(flowApi.addContact).toHaveBeenCalledWith("email", "root@quoin.dev");

		// Verification step; init verification only marks the flow factor.
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		await sendAndEnterCode("654321");

		// Finish step completes the flow and returns to the login form — no session.
		fireEvent.click(await screen.findByRole("button", { name: "完成初始化" }));
		await waitFor(() => expect(flowApi.complete).toHaveBeenCalledTimes(1));
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(screen.getByRole("status")).toHaveTextContent("初始化完成");
	});

	it("walks operator initialization without offering a contact form", async () => {
		const operator = { id: "2", username: "operator", displayName: "Operator" };
		flowApi.start.mockResolvedValue(
			flowOf({
				type: "operator_initialize",
				user: operator,
				contacts: [unverifiedEmail],
				passwordSet: false,
			}),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(
				flowOf({
					type: "operator_initialize",
					user: operator,
					contacts: [unverifiedEmail],
					passwordSet: true,
				}),
			)
			.mockResolvedValueOnce(
				flowOf({
					type: "operator_initialize",
					user: operator,
					contacts: [verifiedEmail],
					passwordSet: true,
					factorVerified: true,
				}),
			);
		flowApi.verify.mockResolvedValue({ completed: false });
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		// The admin-assigned target receives the challenge; no contact form ever
		// appears, and operators get no delivery-settings entry.
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		expect(screen.queryByLabelText("邮箱地址")).not.toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "验证码投递设置" }),
		).not.toBeInTheDocument();
		await sendAndEnterCode();
		fireEvent.click(await screen.findByRole("button", { name: "完成初始化" }));
		expect(await screen.findByRole("status")).toHaveTextContent("初始化完成");
	});

	it("saves SMTP delivery settings during admin initialization", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ contacts: [], passwordSet: false }),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(flowOf({ passwordSet: true }));
		flowApi.saveDelivery.mockResolvedValue({
			configuration: {
				email: {
					kind: "smtp",
					host: "smtp.example.com",
					port: 587,
					from: "quoin@example.com",
					username: "quoin",
					passwordRef: "smtp_password",
					tlsMode: "starttls",
				},
			},
			rowVersion: 1,
			source: "administrator",
			configured: true,
		});
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		// The default password is replaced before delivery settings open.
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		await screen.findByText("验证码投递设置");
		fireEvent.change(await screen.findByLabelText("SMTP 服务器"), {
			target: { value: "smtp.example.com" },
		});
		fireEvent.change(screen.getByLabelText("端口"), {
			target: { value: "587" },
		});
		fireEvent.change(screen.getByLabelText("发件地址"), {
			target: { value: "quoin@example.com" },
		});
		fireEvent.change(screen.getByLabelText("SMTP 用户名（可选）"), {
			target: { value: "quoin" },
		});
		fireEvent.change(screen.getByLabelText("SMTP 密码（可选）"), {
			target: { value: "mail-secret" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		expect(flowApi.saveDelivery).toHaveBeenCalledWith({
			configuration: {
				email: {
					kind: "smtp",
					host: "smtp.example.com",
					port: 587,
					from: "quoin@example.com",
					username: "quoin",
					// The reference is written; the value only travels write-only.
					passwordRef: "smtp_password",
					tlsMode: "starttls",
				},
			},
			secrets: { smtp_password: "mail-secret" },
			expectedRowVersion: 0,
		});
		expect(await screen.findByRole("status")).toHaveTextContent(
			"投递设置已保存",
		);
		// The typed secret is dropped from memory after a successful save.
		expect(screen.getByLabelText("SMTP 密码（可选）")).toHaveValue("");
		fireEvent.click(screen.getByRole("button", { name: "返回继续初始化" }));
		expect(await screen.findByLabelText("邮箱地址")).toBeInTheDocument();
	});

	it("saves webhook HTTPS private CIDRs together with the private CA", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ contacts: [], passwordSet: false }),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(flowOf({ passwordSet: true }));
		flowApi.saveDelivery.mockResolvedValue({
			configuration: {
				email: {
					kind: "webhook",
					url: "https://gateway.quoin.demo.invalid/sms",
					allowPrivateCIDRs: ["172.16.0.0/12"],
					rootCaPem: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
				},
			},
			rowVersion: 1,
			source: "administrator",
			configured: true,
		});
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		await screen.findByText("验证码投递设置");
		// The email channel switches to the webhook kind, which exposes the
		// same private-HTTPS controls as the SMTP section.
		fireEvent.click(screen.getByRole("combobox", { name: "邮箱渠道" }));
		fireEvent.click(await screen.findByRole("option", { name: "出站 Webhook" }));
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "https://gateway.quoin.demo.invalid/sms" },
		});
		fireEvent.change(
			screen.getByLabelText("允许的私网 CIDR（高级，可选）"),
			{ target: { value: "172.16.0.0/12" } },
		);
		fireEvent.change(
			screen.getByLabelText("私有根 CA 证书 PEM（高级，可选）"),
			{ target: { value: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----" } },
		);
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		expect(flowApi.saveDelivery).toHaveBeenCalledWith({
			configuration: {
				email: {
					kind: "webhook",
					url: "https://gateway.quoin.demo.invalid/sms",
					allowPrivateCIDRs: ["172.16.0.0/12"],
					rootCaPem: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
				},
			},
			secrets: {},
			expectedRowVersion: 0,
		});
	});

	it("shows deployment-owned delivery settings read-only", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ contacts: [], passwordSet: false }),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(flowOf({ passwordSet: true }));
		flowApi.readDelivery.mockResolvedValue({
			configuration: {
				email: { kind: "smtp", host: "smtp.example.com", port: 587 },
			},
			rowVersion: 2,
			source: "deployment",
			configured: true,
		});
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		// The password step completes first; the delivery pane then opens read-only.
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		// Wait for the read-only branch that replaces the editable form once
		// the deployment-owned settings load.
		expect(
			await screen.findByText(/投递配置由部署文件管理，此处只读/),
		).toBeInTheDocument();
		expect(screen.getByText("验证码投递设置")).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "保存投递设置" }),
		).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "返回继续初始化" }));
		expect(await screen.findByLabelText("邮箱地址")).toBeInTheDocument();
	});

	it("reports an invalid code generically and never auto-resends", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		flowApi.verify.mockRejectedValue(
			new WorkbenchApiError(422, "验证码不正确或已失效。", "invalid_code"),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		await sendAndEnterCode();
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"验证码不正确或已失效",
		);
		// A spent code is a plain failure: no automatic resend happens.
		expect(flowApi.sendChallenge).toHaveBeenCalledTimes(1);
		// Resending stays governed by the normal cooldown.
		expect(screen.getByRole("button", { name: /重新发送（\d+s）/ })).toBeDisabled();
		// The user may retry a fresh code without resending first.
		fireEvent.change(screen.getByLabelText("验证码"), {
			target: { value: "543210" },
		});
		fireEvent.click(screen.getByRole("button", { name: "验证并继续" }));
		await waitFor(() => expect(flowApi.verify).toHaveBeenCalledWith("543210"));
		expect(flowApi.sendChallenge).toHaveBeenCalledTimes(1);
	});

	it("returns to the login form when the flow cookie is gone", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		flowApi.sendChallenge.mockRejectedValue(noFlow());
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		fireEvent.click(screen.getByRole("button", { name: "发送验证码" }));
		expect(await screen.findByRole("status")).toHaveTextContent(
			"认证流程已失效",
		);
		expect(screen.getByLabelText("用户名")).toBeInTheDocument();
	});
});
