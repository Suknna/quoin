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
}));

vi.mock("@/features/authentication/api", async (importOriginal) => {
	const actual =
		await importOriginal<typeof import("@/features/authentication/api")>();
	return { ...actual, authFlowApi: flowApi };
});

// DeliveryPane is an independent component with its own dedicated test suite;
// this stub keeps the focus on how AuthScreen mounts the pane and reacts to
// its onDone/onFlowGone callbacks.
vi.mock("./DeliveryPane", async () => {
	const { createElement } = await import("react");
	function DeliveryPaneStub({
		onDone,
		onFlowGone,
	}: {
		onDone: () => void;
		onFlowGone: () => void;
	}) {
		return createElement(
			"div",
			{ "data-testid": "delivery-pane-stub" },
			createElement(
				"button",
				{ type: "button", onClick: onDone },
				"模拟返回初始化",
			),
			createElement(
				"button",
				{ type: "button", onClick: onFlowGone },
				"模拟投递流程失效",
			),
		);
	}
	return { DeliveryPane: DeliveryPaneStub };
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
const smsContact = {
	id: "c2",
	channel: "sms",
	maskedTarget: "1****5678",
	verified: false,
} as const;
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
	flowApi.resume.mockRejectedValue(noFlow());
});

afterEach(() => {
	cleanup();
	// reset (not clear) so leftover once-queues never leak across tests.
	vi.resetAllMocks();
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

/**
 * The challenge step now opens at a receiving-contact chooser; pick the email
 * contact, send the challenge and enter the code.
 */
async function sendAndEnterCode(code = "012345") {
	fireEvent.click(await screen.findByRole("button", { name: /邮箱验证码/ }));
	fireEvent.click(await screen.findByRole("button", { name: "发送验证码" }));
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
		// The two-column login screen owns the brand panel.
		expect(
			screen.getByRole("complementary", { name: "关于 Quoin" }),
		).toBeInTheDocument();
		expect(flowApi.resume).toHaveBeenCalledTimes(1);
	});

	it("resumes a running login flow after a refresh into the OTP chooser", async () => {
		flowApi.resume.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(await screen.findByRole("heading", { name: "二次验证" })).toBeInTheDocument();
		// The chooser name joins the channel label and masked target; the exact
		// spacing between the inline spans is irrelevant to a11y matching.
		expect(
			screen.getByRole("button", { name: /邮箱验证码\s*a\*\*\*@quoin\.dev/ }),
		).toBeInTheDocument();
		// The unbound channel stays visible but disabled instead of vanishing.
		expect(screen.getByRole("button", { name: /短信验证码\s*未绑定/ })).toBeDisabled();
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

	it("offers the way back to login when a resumed flow has no contacts", async () => {
		// Defensive against a flow projection without contacts: the challenge
		// cannot be sent, so the only escape hatch is a fresh login.
		flowApi.resume.mockResolvedValue(
			flowOf({ type: "login", contacts: [] }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(
			await screen.findByText("没有可用的验证方式，请重新登录。"),
		).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "返回登录" }));
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(screen.getByRole("status")).toHaveTextContent("认证流程已失效");
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
		// After success the workbench takes over as a separate screen: the
		// flow shell carries no brand complementary panel.
		expect(
			screen.queryByRole("complementary", { name: "关于 Quoin" }),
		).not.toBeInTheDocument();
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

	it("sends the challenge to the contact chosen in the OTP chooser", async () => {
		// Both channels are offered with their masked targets, and the
		// challenge only goes out for the one the user actually picks.
		flowApi.resume.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail, smsContact] }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		expect(
			await screen.findByRole("button", { name: /邮箱验证码\s*a\*\*\*@quoin\.dev/ }),
		).toBeInTheDocument();
		const smsChoice = screen.getByRole("button", { name: /短信验证码\s*1\*\*\*\*5678/ });
		expect(smsChoice).toBeInTheDocument();
		fireEvent.click(smsChoice);
		fireEvent.click(await screen.findByRole("button", { name: "发送验证码" }));
		await waitFor(() => expect(flowApi.sendChallenge).toHaveBeenCalledWith("c2"));
		expect(flowApi.sendChallenge).not.toHaveBeenCalledWith("c1");
	});

	it("keeps the sent challenge and cooldown when re-selecting the same contact", async () => {
		// Going back to the chooser and picking the same target again is pure
		// navigation: it must not discard the already sent challenge, so the
		// code entry stays available and resending stays cooldown-gated — no
		// second send, which would only burn the server rate limit.
		flowApi.start.mockResolvedValue(
			flowOf({ type: "login", contacts: [verifiedEmail] }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		fireEvent.click(screen.getByRole("button", { name: /邮箱验证码/ }));
		fireEvent.click(await screen.findByRole("button", { name: "发送验证码" }));
		await waitFor(() => expect(flowApi.sendChallenge).toHaveBeenCalledTimes(1));
		// Back to the chooser, then re-select the same email target.
		fireEvent.click(screen.getByRole("button", { name: "选择其他方式" }));
		fireEvent.click(await screen.findByRole("button", { name: /邮箱验证码/ }));
		// The sent challenge survived the round-trip: the code field is still
		// offered and the resend button remains disabled by the cooldown.
		expect(screen.getByLabelText("验证码")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: /重新发送（\d+s）/ })).toBeDisabled();
		expect(flowApi.sendChallenge).toHaveBeenCalledTimes(1);
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
		expect(await screen.findByTestId("delivery-pane-stub")).toBeInTheDocument();
		// Returning lands back on the same flow-derived OTP step, unharmed.
		fireEvent.click(screen.getByRole("button", { name: "模拟返回初始化" }));
		expect(await screen.findByText(/a\*\*\*@quoin\.dev/)).toBeInTheDocument();
		expect(flowApi.resume).toHaveBeenCalledTimes(1);
		await sendAndEnterCode();
		expect(flowApi.verify).toHaveBeenCalledWith("012345");
	});

	it("resets to the login form when the delivery pane reports the flow gone", async () => {
		flowApi.resume.mockResolvedValue(
			flowOf({ contacts: [unverifiedEmail], passwordSet: true }),
		);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await screen.findByText(/a\*\*\*@quoin\.dev/);
		fireEvent.click(screen.getByRole("button", { name: "验证码投递设置" }));
		fireEvent.click(
			await screen.findByRole("button", { name: "模拟投递流程失效" }),
		);
		expect(await screen.findByLabelText("用户名")).toBeInTheDocument();
		expect(screen.getByRole("status")).toHaveTextContent("认证流程已失效");
		expect(screen.queryByTestId("delivery-pane-stub")).not.toBeInTheDocument();
	});

	it("walks admin initialization: password, delivery skip, contact choice, verification, completion", async () => {
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
		expect(await screen.findByTestId("delivery-pane-stub")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "模拟返回初始化" }));

		// The contact step starts at a channel chooser, then the email form.
		fireEvent.click(await screen.findByRole("button", { name: "邮箱验证码" }));
		fireEvent.change(await screen.findByLabelText("邮箱地址"), {
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

	it("registers an SMS contact through the contact channel chooser", async () => {
		flowApi.start.mockResolvedValue(
			flowOf({ contacts: [], passwordSet: false }),
		);
		flowApi.resume
			.mockRejectedValueOnce(noFlow()) // the mount-time resume finds no flow
			.mockResolvedValueOnce(flowOf({ passwordSet: true }))
			.mockResolvedValueOnce(
				flowOf({ contacts: [smsContact], passwordSet: true }),
			);
		render(<AuthScreen onAuthenticated={vi.fn()} />);
		await submitCredentials();
		fireEvent.change(await screen.findByLabelText("新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.change(screen.getByLabelText("再次输入新密码"), {
			target: { value: "brand new password" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存并继续" }));
		fireEvent.click(await screen.findByRole("button", { name: "模拟返回初始化" }));
		// The SMS choice swaps the form to a phone number, and the channel
		// reaches addContact so the challenge can go out over SMS.
		fireEvent.click(await screen.findByRole("button", { name: "短信验证码" }));
		fireEvent.change(await screen.findByLabelText("手机号"), {
			target: { value: "13800138000" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存联系方式" }));
		expect(flowApi.addContact).toHaveBeenCalledWith("sms", "13800138000");
		// The registered SMS target then shows up in the OTP chooser.
		expect(
			await screen.findByRole("button", { name: /短信验证码\s*1\*\*\*\*5678/ }),
		).toBeInTheDocument();
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
		fireEvent.click(screen.getByRole("button", { name: /邮箱验证码/ }));
		fireEvent.click(await screen.findByRole("button", { name: "发送验证码" }));
		expect(await screen.findByRole("status")).toHaveTextContent(
			"认证流程已失效",
		);
		expect(screen.getByLabelText("用户名")).toBeInTheDocument();
	});
});
