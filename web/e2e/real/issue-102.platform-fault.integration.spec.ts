import { spawnSync } from "node:child_process";
import { expect, test } from "@playwright/test";

const adminUsername = process.env.QUOIN_E2E_ADMIN_USERNAME;
const adminPassword = process.env.QUOIN_E2E_ADMIN_INITIAL_PASSWORD;
const adminFinalPassword = process.env.QUOIN_E2E_ADMIN_FINAL_PASSWORD;

if (!adminUsername || !adminPassword || !adminFinalPassword) {
	throw new Error(
		"#102 real E2E requires generated credentials.",
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

/** Production Plinth adapter lifecycle over its deployment mTLS identity; no seed or fake health projection. */
test("A controlled Plinth disconnect/reconnect becomes a platform-only Operator alert", async ({
	browser,
	baseURL,
}) => {
	const adminContext = await browser.newContext({ ignoreHTTPSErrors: true });
	await activateAdmin(adminContext, baseURL!);
	// Plinth connects automatically through its deployment CA-signed mTLS
	// client identity (ADR-0009): there is no registration flow to drive.
	// A failed prior run can leave the intentionally controlled container down;
	// restore it before asserting this run's independent lifecycle transition.
	compose(["start", "plinth"]);
	await expect
		.poll(
			async () =>
				(
					(await adminContext.request.get(`${baseURL}/api/v1/admin/about`)).json() as {
						components: Array<{ slot: string; connected: boolean }>;
					}
				).components.some((slot) => slot.slot === "plinth" && slot.connected),
			{ timeout: 30_000 },
		)
		.toBe(true);

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
	for (const path of ["/api/v1/admin/about"]) {
		const denied = await operatorContext.request.get(`${baseURL}${path}`);
		expect(denied.status(), path).toBe(403);
	}

	compose(["start", "plinth"]);
	await expect
		.poll(
			async () =>
				(
					(await adminContext.request.get(`${baseURL}/api/v1/admin/about`)).json() as {
						components: Array<{ slot: string; connected: boolean }>;
					}
				).components.some((slot) => slot.slot === "plinth" && slot.connected),
			{ timeout: 20_000 },
		)
		.toBe(true);
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
