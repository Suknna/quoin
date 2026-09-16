import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkbenchApiError } from "@/api/workbench";

const flowApi = vi.hoisted(() => ({
	readDelivery: vi.fn(),
	saveDelivery: vi.fn(),
}));

vi.mock("@/features/authentication/api", async (importOriginal) => {
	const actual =
		await importOriginal<typeof import("@/features/authentication/api")>();
	return { ...actual, authFlowApi: flowApi };
});

import { DeliveryPane } from "./DeliveryPane";

const emptyDelivery = {
	configuration: {},
	rowVersion: 3,
	source: "administrator",
	configured: false,
};

const savedDelivery = {
	configuration: {},
	rowVersion: 4,
	source: "administrator",
	configured: true,
};

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
	flowApi.readDelivery.mockResolvedValue(emptyDelivery);
	flowApi.saveDelivery.mockResolvedValue(savedDelivery);
});

afterEach(() => {
	cleanup();
	// reset (not clear) so leftover once-queues never leak across tests.
	vi.resetAllMocks();
	vi.unstubAllGlobals();
});

async function choose(name: RegExp) {
	fireEvent.click(await screen.findByRole("button", { name }));
}

async function pickSelect(label: string, option: string) {
	fireEvent.click(screen.getByRole("combobox", { name: label }));
	fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("DeliveryPane", () => {
	it("saves an SMTP channel from a provider preset with the auth-code secret", async () => {
		const onDone = vi.fn();
		render(<DeliveryPane onDone={onDone} onFlowGone={vi.fn()} />);
		await choose(/SMTP 邮箱/);
		fireEvent.change(await screen.findByLabelText("发件邮箱"), {
			target: { value: "root@163.com" },
		});
		await pickSelect("服务商", "163 邮箱");
		fireEvent.change(screen.getByLabelText("授权码"), {
			target: { value: "auth-code" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		expect(flowApi.saveDelivery).toHaveBeenCalledWith({
			configuration: {
				email: {
					kind: "smtp",
					// The preset carries the correct TLS endpoint for 授权码 auth.
					host: "smtp.163.com",
					port: 465,
					from: "root@163.com",
					// The username defaults to the sender address.
					username: "root@163.com",
					passwordRef: "smtp_password",
					tlsMode: "implicit",
				},
			},
			secrets: { smtp_password: "auth-code" },
			expectedRowVersion: 3,
		});
		// Success hands control back to the flow (contact step next).
		expect(onDone).toHaveBeenCalledTimes(1);
	});

	it("keeps a stored SMTP password reference and allows a username override", async () => {
		flowApi.readDelivery.mockResolvedValue({
			configuration: {
				email: {
					kind: "smtp",
					host: "smtp.163.com",
					port: 465,
					from: "root@163.com",
					username: "root@163.com",
					passwordRef: "legacy_ref",
					tlsMode: "implicit",
				},
			},
			rowVersion: 7,
			source: "administrator",
			configured: true,
		});
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/SMTP 邮箱/);
		expect(
			screen.getByRole("combobox", { name: "服务商" }),
		).toHaveTextContent("163 邮箱");
		expect(screen.getByLabelText("授权码")).toHaveValue("");
		fireEvent.click(screen.getByRole("button", { name: "高级设置" }));
		fireEvent.change(await screen.findByLabelText("SMTP 用户名（可选）"), {
			target: { value: "custom-user" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		expect(update.configuration.email).toEqual({
			kind: "smtp",
			host: "smtp.163.com",
			port: 465,
			from: "root@163.com",
			username: "custom-user",
			// The stored reference name survives; its value is never readable.
			passwordRef: "legacy_ref",
			tlsMode: "implicit",
		});
		// A blank 授权码 keeps the stored secret — nothing leaks into the payload.
		expect(update.secrets).toEqual({});
	});

	it("preserves both loaded channels and the webhook secret references", async () => {
		flowApi.readDelivery.mockResolvedValue({
			configuration: {
				email: {
					kind: "smtp",
					host: "smtp.qq.com",
					port: 465,
					from: "a@qq.com",
					username: "a@qq.com",
					passwordRef: "smtp_password",
					tlsMode: "implicit",
				},
				sms: {
					kind: "webhook",
					url: "https://gw.invalid/sms",
					encoding: "form",
					headers: { "X-Source": "quoin" },
					secretHeaders: { "X-Api-Key": "sms_key" },
					fields: { mobile: "{recipient}" },
					successField: "ok",
					successValue: "true",
					allowPrivateCIDRs: ["172.16.0.0/12"],
					rootCaPem: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
				},
			},
			rowVersion: 5,
			source: "administrator",
			configured: true,
		});
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/出站 Webhook/);
		// SMS is the default recipient channel for a fresh webhook session.
		expect(
			screen.getByRole("combobox", { name: "接收渠道" }),
		).toHaveTextContent("短信验证码");
		expect(screen.getByLabelText("Webhook 地址")).toHaveValue(
			"https://gw.invalid/sms",
		);
		expect(
			screen.getByRole("combobox", { name: "鉴权方式" }),
		).toHaveTextContent("API Key 请求头");
		// The simple API Key input and the plain header row share the label;
		// the auth field renders first.
		expect(screen.getAllByLabelText("请求头名称")[0]).toHaveValue("X-Api-Key");
		expect(screen.getByLabelText("API Key")).toHaveValue("");
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		// The untouched SMTP channel round-trips unchanged.
		expect(update.configuration.email).toEqual({
			kind: "smtp",
			host: "smtp.qq.com",
			port: 465,
			from: "a@qq.com",
			username: "a@qq.com",
			passwordRef: "smtp_password",
			tlsMode: "implicit",
		});
		expect(update.configuration.sms).toEqual({
			kind: "webhook",
			url: "https://gw.invalid/sms",
			encoding: "form",
			fields: { mobile: "{recipient}" },
			headers: { "X-Source": "quoin" },
			// The reference NAME is preserved; the stored value stays server-side.
			secretHeaders: { "X-Api-Key": "sms_key" },
			successField: "ok",
			successValue: "true",
			allowPrivateCIDRs: ["172.16.0.0/12"],
			rootCaPem: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
		});
		expect(update.secrets).toEqual({});
		expect(JSON.stringify(update)).not.toContain("sms_key_value");
	});

	it("configures a fresh webhook with bearer auth and key/value rows", async () => {
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/出站 Webhook/);
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "https://hook.invalid/alert" },
		});
		await pickSelect("鉴权方式", "Bearer 令牌");
		fireEvent.change(screen.getByLabelText("访问令牌"), {
			target: { value: "tok-1" },
		});
		// Mapping and header rows live in the collapsed advanced section.
		fireEvent.click(screen.getByRole("button", { name: "高级设置" }));
		fireEvent.click(screen.getByRole("button", { name: "添加字段" }));
		fireEvent.change(screen.getAllByLabelText("字段路径")[0], {
			target: { value: "mobile" },
		});
		fireEvent.change(screen.getAllByLabelText("模板")[0], {
			target: { value: "{recipient}" },
		});
		fireEvent.click(screen.getByRole("button", { name: "添加请求头" }));
		fireEvent.change(screen.getAllByLabelText("请求头名称")[0], {
			target: { value: "X-Source" },
		});
		fireEvent.change(screen.getAllByLabelText("请求头值")[0], {
			target: { value: "quoin" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		expect(update.configuration.sms).toEqual({
			kind: "webhook",
			url: "https://hook.invalid/alert",
			encoding: "json",
			fields: { mobile: "{recipient}" },
			headers: { "X-Source": "quoin" },
			secretHeaders: { Authorization: "webhook_bearer" },
		});
		// The typed token is composed into the Authorization value, write-only.
		expect(update.secrets).toEqual({ webhook_bearer: "Bearer tok-1" });
	});

	it("renders deployment-owned settings read-only without raw JSON", async () => {
		flowApi.readDelivery.mockResolvedValue({
			configuration: {
				email: { kind: "smtp", host: "smtp.example.com", port: 587 },
			},
			rowVersion: 2,
			source: "deployment",
			configured: true,
		});
		const onDone = vi.fn();
		render(<DeliveryPane onDone={onDone} onFlowGone={vi.fn()} />);
		expect(
			await screen.findByText(/投递配置由部署文件管理，此处只读/),
		).toBeInTheDocument();
		expect(screen.getByText("邮箱：SMTP（smtp.example.com:587）")).toBeInTheDocument();
		expect(screen.getByText("短信：未配置")).toBeInTheDocument();
		expect(screen.queryByText(/passwordRef/)).not.toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "保存投递设置" }),
		).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "返回继续初始化" }));
		expect(onDone).toHaveBeenCalledTimes(1);
	});

	it("offers a retry after a failed load and blocks saving", async () => {
		flowApi.readDelivery.mockRejectedValueOnce(
			new WorkbenchApiError(500, "数据库不可用"),
		);
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		expect(await screen.findByRole("alert")).toHaveTextContent("数据库不可用");
		expect(
			screen.queryByRole("button", { name: "保存投递设置" }),
		).not.toBeInTheDocument();
		flowApi.readDelivery.mockResolvedValueOnce(emptyDelivery);
		fireEvent.click(screen.getByRole("button", { name: "重试" }));
		expect(
			await screen.findByRole("button", { name: /SMTP 邮箱/ }),
		).toBeInTheDocument();
	});

	it("reports an invalid webhook address without saving", async () => {
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/SMTP 邮箱/);
		await screen.findByLabelText("发件邮箱");
		// Back returns to the chooser without discarding the pane.
		fireEvent.click(screen.getByRole("button", { name: "返回选择" }));
		expect(
			screen.getByRole("button", { name: /出站 Webhook/ }),
		).toBeInTheDocument();
		await choose(/出站 Webhook/);
		// jsdom enforces the browser's required checks, so the reachable
		// validation path is the explicit HTTPS-only rule.
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "http://gw.invalid/sms" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		expect(await screen.findByRole("alert")).toHaveTextContent(
			"Webhook 地址需要以 https:// 开头",
		);
		expect(flowApi.saveDelivery).not.toHaveBeenCalled();
	});

	it("preserves the untouched email channel when saving the SMS webhook", async () => {
		// Visiting the SMTP form and leaving must not repoint the email slot:
		// saving one focused form may never drop the other channel.
		const emailChannel = {
			kind: "smtp",
			host: "smtp.qq.com",
			port: 465,
			from: "a@qq.com",
			username: "a@qq.com",
			passwordRef: "smtp_password",
			tlsMode: "implicit",
		};
		flowApi.readDelivery.mockResolvedValue({
			configuration: { email: emailChannel },
			rowVersion: 9,
			source: "administrator",
			configured: true,
		});
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/SMTP 邮箱/);
		await screen.findByLabelText("发件邮箱");
		fireEvent.click(screen.getByRole("button", { name: "返回选择" }));
		await choose(/出站 Webhook/);
		expect(
			screen.getByRole("combobox", { name: "接收渠道" }),
		).toHaveTextContent("短信验证码");
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "https://gw.invalid/sms" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		expect(update.configuration.email).toEqual(emailChannel);
		expect(update.configuration.sms).toEqual({
			kind: "webhook",
			url: "https://gw.invalid/sms",
			encoding: "json",
		});
		expect(update.secrets).toEqual({});
		expect(flowApi.saveDelivery.mock.calls[0][0].expectedRowVersion).toBe(9);
	});

	it("scopes fresh webhook secret references per channel", async () => {
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/出站 Webhook/);
		await pickSelect("接收渠道", "邮箱验证码");
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "https://mail.invalid/hook" },
		});
		await pickSelect("鉴权方式", "Bearer 令牌");
		fireEvent.change(screen.getByLabelText("访问令牌"), {
			target: { value: "tok-e" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		expect(update.configuration.email).toEqual({
			kind: "webhook",
			url: "https://mail.invalid/hook",
			encoding: "json",
			secretHeaders: { Authorization: "webhook_email_bearer" },
		});
		// Distinct from the SMS-scoped default: two webhooks never share one
		// stored credential by accident, and no value is ever readable back.
		expect(update.secrets).toEqual({ webhook_email_bearer: "Bearer tok-e" });
	});

	it("keeps explicit custom secret headers even with a single row", async () => {
		// Regression: a lone custom row used to be re-derived as the simple
		// API Key mode, ignoring its typed secret value.
		render(<DeliveryPane onDone={vi.fn()} onFlowGone={vi.fn()} />);
		await choose(/出站 Webhook/);
		fireEvent.change(await screen.findByLabelText("Webhook 地址"), {
			target: { value: "https://gw.invalid/sms" },
		});
		await pickSelect("鉴权方式", "自定义秘密请求头");
		fireEvent.click(screen.getByRole("button", { name: "添加秘密请求头" }));
		fireEvent.change(screen.getAllByLabelText("请求头名称")[0], {
			target: { value: "X-Sign" },
		});
		fireEvent.change(screen.getAllByLabelText("秘密引用名")[0], {
			target: { value: "sms_sign" },
		});
		fireEvent.change(screen.getByLabelText("秘密值 sms_sign"), {
			target: { value: "sv" },
		});
		fireEvent.click(screen.getByRole("button", { name: "保存投递设置" }));
		await waitFor(() => expect(flowApi.saveDelivery).toHaveBeenCalledTimes(1));
		const update = flowApi.saveDelivery.mock.calls[0][0];
		expect(update.configuration.sms.secretHeaders).toEqual({
			"X-Sign": "sms_sign",
		});
		expect(update.secrets).toEqual({ sms_sign: "sv" });
	});
});
