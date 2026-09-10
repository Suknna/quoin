import { expect, test } from "@playwright/test";

const adminUsername = process.env.QUOIN_E2E_ADMIN_USERNAME;
const adminPassword = process.env.QUOIN_E2E_ADMIN_PASSWORD;
const operatorUsername = `operator96${Date.now()}`;
const operatorPassword = "Operator 96 passphrase 2026!";
const adminFinalPassword = process.env.QUOIN_E2E_ADMIN_FINAL_PASSWORD;
const operatorFinalPassword = "Operator 96 final passphrase 2027!";

if (!adminUsername || !adminPassword || !adminFinalPassword) {
	throw new Error(
		"#96 real E2E requires generated Admin credentials from scripts/e2e-real/up.sh.",
	);
}

async function loginAndSetPassword(
	page: import("@playwright/test").Page,
	username: string,
	temporary: string,
	final: string,
) {
	await page.goto("/");
	await page.getByLabel("用户名").fill(username);
	await page.getByLabel("密码").fill(temporary);
	await page.getByRole("button", { name: "登录" }).click();
	const changePassword = page.getByRole("heading", {
		name: "先设置你自己的密码",
	});
	await page.waitForTimeout(300);
	if (await changePassword.isVisible()) {
		await page.getByLabel("当前临时密码").fill(temporary);
		await page
			.getByRole("textbox", { name: "新密码", exact: true })
			.fill(final);
		await page.getByLabel("再次输入新密码").fill(final);
		await page.getByRole("button", { name: "保存并进入工作台" }).click();
		await expect(changePassword).toHaveCount(0);
		return;
	}
	// A retained disposable runtime already has its initial Admin finalized.
	await page.getByLabel("用户名").fill(username);
	await page.getByLabel("密码").fill(final);
	await page.getByRole("button", { name: "登录" }).click();
	await expect(page.getByLabel("用户名")).toHaveCount(0);
}

/**
 * #96's supported non-legacy browser acceptance: management is created via the
 * actual screen; upstream delivery goes through public gateway -> Stele ->
 * Quoin; the Operator session proves server-side management denial while
 * retaining alert and business-context reads. No fixture writes application DB.
 */
test("Admin creates Alertmanager, Stele persists its alert, and Operator is restricted", async ({
	browser,
	baseURL,
	request,
}) => {
	const adminContext = await browser.newContext({ ignoreHTTPSErrors: true });
	const adminPage = await adminContext.newPage();
	await loginAndSetPassword(
		adminPage,
		adminUsername!,
		adminPassword!,
		adminFinalPassword,
	);

	const sourceKey = `e2e-alertmanager-${Date.now()}`;
	await adminPage.goto("/integrations/alertmanager");
	await expect(
		adminPage.getByRole("heading", { name: "配置 Alertmanager" }),
	).toBeVisible();
	await adminPage.locator("#alertmanager-key").fill(sourceKey);
	await adminPage.getByRole("button", { name: "创建并显示一次凭据" }).click();
	await expect(
		adminPage.getByRole("dialog", { name: "一次性接收凭据" }),
	).toBeVisible();
	const receiverURL = await adminPage
		.getByRole("dialog")
		.locator("input[readonly]")
		.first()
		.inputValue();
	const bearer = await adminPage
		.getByRole("dialog")
		.locator("input[readonly]")
		.nth(1)
		.inputValue();
	expect(receiverURL).toBe(`${baseURL}/stele/alerts`);
	expect(bearer).not.toEqual("");
	// Stele refreshes the authoritative credential snapshot asynchronously after
	// Quoin commits its source command. Wait for its production cache instead of
	// treating a transient 401 as a bad one-time credential.
	await expect
		.poll(
			async () =>
				(
					await request.post(receiverURL, {
						headers: {
							Authorization: `Bearer ${bearer}`,
							"Content-Type": "application/json",
						},
						data: { status: "firing", alerts: [] },
						ignoreHTTPSErrors: true,
					})
				).status(),
			{ timeout: 15_000 },
		)
		.toBe(204);
	await adminPage.getByRole("button", { name: "我已安全保存" }).click();

	const payload = {
		status: "firing",
		alerts: [
			{
				status: "firing",
				labels: {
					alertname: "QuoinE2EAlert",
					acceptance_source: sourceKey,
					service: "checkout",
					severity: "critical",
				},
				annotations: { summary: "#96 public Stele acceptance" },
				startsAt: "2026-09-10T00:00:00Z",
				// Quoin validates this real Alertmanager wire fingerprint against labels;
				// omit it here so the production parser computes the canonical value.
				fingerprint: "",
			},
		],
	};
	const delivery = await request.post(receiverURL, {
		// Delivery is authenticated by Stele's source token, never the logged-in
		// browser session. Its URL may be the public local gateway on a different
		// request context, so preserve the token's Bearer scheme explicitly.
		headers: {
			Authorization: `Bearer ${bearer}`,
			"Content-Type": "application/json",
		},
		data: payload,
		ignoreHTTPSErrors: true,
	});
	expect(delivery.status()).toBe(204);

	const invalidDelivery = await request.post(receiverURL, {
		headers: { Authorization: "Bearer invalid-source-credential" },
		data: payload,
		ignoreHTTPSErrors: true,
	});
	expect(invalidDelivery.status()).toBe(401);
	const repeatedDelivery = await request.post(receiverURL, {
		headers: { Authorization: `Bearer ${bearer}` },
		data: payload,
		ignoreHTTPSErrors: true,
	});
	expect(repeatedDelivery.status()).toBe(204);
	const persistedAlerts = await adminContext.request.get(
		`${baseURL}/api/v1/alerts`,
	);
	expect(persistedAlerts.status()).toBe(200);
	const persistedBody = (await persistedAlerts.json()) as {
		items: Array<{ labels?: Record<string, string> }>;
	};
	// A unique fixture label isolates this run from retained historical alerts.
	expect(persistedBody.items.filter((item) => item.labels?.acceptance_source === sourceKey)).toHaveLength(1);

	await expect
		.poll(
			async () => {
				const response = await adminContext.request.get(
					`${baseURL}/api/v1/alerts`,
				);
				const body = (await response.json()) as {
					items?: Array<{ labels?: Record<string, string> }>;
				};
				return (
					body.items?.some(
						(item) => item.labels?.alertname === "QuoinE2EAlert",
					) ?? false
				);
			},
			{ timeout: 15_000 },
		)
		.toBe(true);

	const createOperator = await adminContext.request.post(
		`${baseURL}/api/v1/admin/users`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `create-operator-${Date.now()}`,
				username: operatorUsername,
				displayName: "Operator 96",
				role: "operator",
				password: operatorPassword,
			},
		},
	);
	expect(createOperator.status()).toBe(201);

	const operatorContext = await browser.newContext({ ignoreHTTPSErrors: true });
	// Use the supported auth boundary to establish a separate Operator session;
	// the browser then proves the actual protected alert route and direct API
	// denials without relying on timing-sensitive first-login rendering.
	const operatorLogin = await operatorContext.request.post(
		`${baseURL}/api/v1/auth/login`,
		{
			headers: { Origin: baseURL },
			data: { username: operatorUsername, password: operatorPassword },
		},
	);
	expect(operatorLogin.status()).toBe(200);
	const operatorPasswordChange = await operatorContext.request.put(
		`${baseURL}/api/v1/auth/password`,
		{
			headers: { Origin: baseURL },
			data: {
				currentPassword: operatorPassword,
				newPassword: operatorFinalPassword,
			},
		},
	);
	expect(operatorPasswordChange.status()).toBe(204);
	const operatorPage = await operatorContext.newPage();
	await operatorPage.goto("/alerts/list");
	await expect(
		operatorPage.getByText("QuoinE2EAlert", { exact: true }).last(),
	).toBeVisible();

	for (const path of [
		"/api/v1/alert-sources",
		"/api/v1/business-systems",
		"/api/v1/label-contracts",
		"/api/v1/runtime",
	]) {
		const denied = await operatorContext.request.get(`${baseURL}${path}`);
		expect(denied.status(), path).toBe(403);
	}
	const context = await operatorContext.request.get(
		`${baseURL}/api/v1/business-context`,
	);
	expect(context.status()).toBe(200);
	const contextBody = (await context.json()) as { items?: unknown[] };
	expect(Array.isArray(contextBody.items)).toBe(true);

	await operatorContext.close();
	await adminContext.close();
});
