import { spawnSync } from "node:child_process";
import { expect, test } from "@playwright/test";

const adminUsername = process.env.QUOIN_E2E_ADMIN_USERNAME;
const adminPassword = process.env.QUOIN_E2E_ADMIN_PASSWORD;
const adminFinalPassword = process.env.QUOIN_E2E_ADMIN_FINAL_PASSWORD;
const registerPlinth = process.env.QUOIN_E2E_REGISTER_PLINTH_HELPER;

if (
	!adminUsername ||
	!adminPassword ||
	!adminFinalPassword ||
	!registerPlinth
) {
	throw new Error(
		"#102 real E2E requires generated credentials and the attached-stdin Plinth helper.",
	);
}

async function activateAdmin(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
) {
	// A fresh harness still has the CLI-issued temporary password; a retained
	// harness has the final password. Neither case depends on another test running.
	const response = await context.request.post(`${baseURL}/api/v1/auth/login`, {
		headers: { Origin: baseURL },
		data: { username: adminUsername, password: adminFinalPassword },
	});
	if (response.status() === 200) return;
	expect(response.status()).toBe(401);
	const login = await context.request.post(`${baseURL}/api/v1/auth/login`, {
		headers: { Origin: baseURL },
		data: { username: adminUsername, password: adminPassword },
	});
	expect(login.status()).toBe(200);
	const change = await context.request.put(`${baseURL}/api/v1/auth/password`, {
		headers: { Origin: baseURL },
		data: { currentPassword: adminPassword, newPassword: adminFinalPassword },
	});
	expect(change.status()).toBe(204);
}

async function activateOperator(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
	username: string,
	temporary: string,
	final: string,
) {
	// This creates the exact browser-session transition through the supported
	// auth API. It avoids treating a transient frontend rendering fault as an
	// authorization result; the subsequent page navigation remains a real URL test.
	const loginResponse = await context.request.post(
		`${baseURL}/api/v1/auth/login`,
		{ headers: { Origin: baseURL }, data: { username, password: temporary } },
	);
	expect(loginResponse.status()).toBe(200);
	const changeResponse = await context.request.put(
		`${baseURL}/api/v1/auth/password`,
		{
			headers: { Origin: baseURL },
			data: { currentPassword: temporary, newPassword: final },
		},
	);
	expect(changeResponse.status()).toBe(204);
}

function compose(args: string[]) {
	const repoRoot = new URL("../../../", import.meta.url).pathname;
	const result = spawnSync(
		"docker",
		[
			"compose",
			"--project-directory",
			repoRoot,
			"--env-file",
			"/dev/null",
			"-f",
			`${repoRoot}deploy/e2e-real.compose.yaml`,
			...args,
		],
		{
			cwd: repoRoot,
			encoding: "utf8",
			env: {
				...process.env,
				QUOIN_E2E_RUNTIME:
					process.env.QUOIN_E2E_RUNTIME ?? `${repoRoot}.artifacts/e2e-102`,
				QUOIN_E2E_PORT: process.env.QUOIN_E2E_PORT ?? "8444",
				QUOIN_E2E_PROJECT: process.env.QUOIN_E2E_PROJECT ?? "quoin-e2e-102",
			},
		},
	);
	if (result.status !== 0)
		throw new Error(
			`controlled Plinth lifecycle command failed: ${result.stderr}`,
		);
}

/** Real Admin UI registration plus production Plinth adapter lifecycle; no seed or fake health projection. */
test("About registration and a controlled Plinth disconnect/reconnect become a platform-only Operator alert", async ({
	browser,
	baseURL,
}) => {
	const adminContext = await browser.newContext({ ignoreHTTPSErrors: true });
	await activateAdmin(adminContext, baseURL!);
	const adminPage = await adminContext.newPage();
	await adminPage.goto("/admin/about");
	await expect(
		adminPage.getByRole("heading", { name: "关于平台" }),
	).toBeVisible();
	await expect(
		adminPage.getByRole("heading", { name: "运行时注册与轮换" }),
	).toBeVisible();
	await expect(adminPage.getByRole("heading", { name: "维护" })).toBeVisible();

	const runtimeBefore = (await (
		await adminContext.request.get(`${baseURL}/api/v1/runtime`)
	).json()) as { plinth: { state: string; connected: boolean } };
	if (runtimeBefore.plinth.state !== "registered") {
		// Registration is a protected one-time credential action and therefore
		// stays behind the About confirmation dialog before revealing its token.
		await adminPage
			.getByRole("button", { name: "准备首次注册" })
			.first()
			.click();
		await adminPage.getByRole("button", { name: "确认" }).click();
		const registrationToken = await adminPage
			.getByText("一次性注册令牌：")
			.locator("xpath=..")
			.locator("code")
			.textContent();
		expect(registrationToken).toBeTruthy();
		const registered = spawnSync(registerPlinth!, ["--stdin"], {
			input: `${registrationToken}\n`,
			encoding: "utf8",
			env: process.env,
		});
		expect(registered.status, registered.stderr).toBe(0);
	}
	// A failed prior run can leave the intentionally controlled container down;
	// restore it before asserting this run's independent lifecycle transition.
	compose(["start", "plinth"]);
	await expect
		.poll(
			async () =>
				(await adminContext.request.get(`${baseURL}/api/v1/runtime`)).json(),
			{ timeout: 20_000 },
		)
		.toMatchObject({ plinth: { state: "registered", connected: true } });

	const suffix = Date.now();
	const operatorUsername = `operator102${suffix}`;
	const operatorPassword = `Operator 102 temporary ${suffix}!`;
	const operatorFinalPassword = `Operator 102 final ${suffix}!`;
	const createOperator = await adminContext.request.post(
		`${baseURL}/api/v1/admin/users`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `create-operator-102-${suffix}`,
				username: operatorUsername,
				displayName: "Operator 102",
				role: "operator",
				password: operatorPassword,
			},
		},
	);
	expect(createOperator.status()).toBe(201);

	compose(["stop", "plinth"]);
	let platformFault:
		| { id: string; source: string; component?: string }
		| undefined;
	await expect
		.poll(
			async () => {
				const body = (await (
					await adminContext.request.get(
						`${baseURL}/api/v1/alerts?state=Firing`,
					)
				).json()) as {
					items?: Array<{ id: string; source: string; component?: string }>;
				};
				platformFault = body.items?.find(
					(item) => item.source === "platform" && item.component === "plinth",
				);
				return platformFault?.id;
			},
			{ timeout: 20_000 },
		)
		.toMatch(/^platform:/);
	const analysis = await adminContext.request.post(
		`${baseURL}/api/v1/alerts/${platformFault!.id}/analyses`,
		{
			headers: { Origin: baseURL },
			data: { clientCommandId: `platform-analysis-${suffix}` },
		},
	);
	expect(analysis.status()).toBe(422);

	const operatorContext = await browser.newContext({ ignoreHTTPSErrors: true });
	await activateOperator(
		operatorContext,
		baseURL!,
		operatorUsername,
		operatorPassword,
		operatorFinalPassword,
	);
	const operatorPage = await operatorContext.newPage();
	await operatorPage.goto(`/alerts/list?id=${platformFault!.id}`);
	await expect(
		operatorPage.getByText("平台内部 · plinth", { exact: true }),
	).toBeVisible();
	await expect(operatorPage.getByRole("tab", { name: "AI 分析" })).toHaveCount(
		0,
	);
	await operatorPage.goto("/admin/about");
	// The route guard is a client convenience; the direct API checks below are
	// authoritative. A denied URL must never expose About runtime/maintenance controls.
	await expect(
		operatorPage.getByRole("heading", { name: "关于平台" }),
	).toHaveCount(0);
	await expect(
		operatorPage.getByRole("button", { name: /运行时|维护/ }),
	).toHaveCount(0);
	for (const path of [
		"/api/v1/admin/about",
		"/api/v1/runtime",
		"/api/v1/runtime-slots/plinth/registration/prepare",
	]) {
		const denied = path.endsWith("prepare")
			? await operatorContext.request.post(`${baseURL}${path}`, {
					data: { clientCommandId: `deny-${suffix}`, expectedRowVersion: 1 },
				})
			: await operatorContext.request.get(`${baseURL}${path}`);
		expect(denied.status(), path).toBe(403);
	}

	compose(["start", "plinth"]);
	await expect
		.poll(
			async () =>
				(await adminContext.request.get(`${baseURL}/api/v1/runtime`)).json(),
			{ timeout: 20_000 },
		)
		.toMatchObject({ plinth: { connected: true } });
	await expect
		.poll(
			async () => {
				const body = (await (
					await adminContext.request.get(
						`${baseURL}/api/v1/alerts?state=Resolved`,
					)
				).json()) as { items?: Array<{ id: string }> };
				return (
					body.items?.some((item) => item.id === platformFault!.id) ?? false
				);
			},
			{ timeout: 20_000 },
		)
		.toBe(true);
	await operatorPage.goto(`/alerts/list?id=${platformFault!.id}`);
	await expect(
		operatorPage.getByText("Resolved", { exact: true }),
	).toBeVisible();

	await operatorContext.close();
	await adminContext.close();
});
