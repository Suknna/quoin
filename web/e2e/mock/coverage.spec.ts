import { expect, test } from "@playwright/test";

const adminCredentials = { username: "admin", password: "demo-admin-password" };
const nextPassword = "local-preview-password-2026";

type Guard = { forbidden: string[]; pageErrors: string[]; webSockets: string[] };

function protectOffline(page: import("@playwright/test").Page): Guard {
	const guard: Guard = { forbidden: [], pageErrors: [], webSockets: [] };
	page.on("request", (request) => {
		const url = new URL(request.url());
		if (url.origin !== "http://127.0.0.1:4175") guard.forbidden.push(request.url());
	});
	page.on("websocket", (socket) => guard.webSockets.push(socket.url()));
	page.on("pageerror", (error) => guard.pageErrors.push(error.message));
	return guard;
}

async function selectScenario(page: import("@playwright/test").Page, scenario: string) {
	if (await page.getByLabel("场景").count() === 0) await page.getByRole("button", { name: "开发预览 · 模拟数据" }).click();
	await page.getByLabel("场景").selectOption(scenario);
	await page.waitForLoadState("domcontentloaded");
}

async function expectHealthy(page: import("@playwright/test").Page, guard: Guard) {
	await expect(page.getByText("Mock API has no handler", { exact: false })).toHaveCount(0);
	expect(guard.pageErrors).toEqual([]);
	expect(guard.forbidden).toEqual([]);
	// Vite's same-origin HMR socket is allowed; application sockets must not escape it.
	expect(guard.webSockets.filter((url) => new URL(url).host !== "127.0.0.1:4175")).toEqual([]);
}

test("route matrix renders actual data without unhandled APIs or external transports", async ({ page }) => {
	const guard = protectOffline(page);
	await page.goto("/");
	await expect(page.getByRole("button", { name: /演示管理员 admin/ })).toBeVisible();

	const routes = [
		["/alerts/list", "当前告警", "CheckoutLatencyHigh"],
		["/investigations", "调查", "结算延迟调查"],
		["/inspections", "巡检", "选择已发布系统的历史 Run"],
		["/business-systems", "结算系统", "结算健康检查"],
		["/knowledge", "知识库", "结算延迟排查"],
		["/admin", "管理", "关于"],
	] as const;
	for (const [route, , data] of routes) {
		await page.goto(route);
		await expect(page).toHaveURL(new RegExp(`${route}$`));
		await expect(page.getByText(data, { exact: false }).first()).toBeVisible();
		await expectHealthy(page, guard);
	}

	await page.goto("/alerts/list");
	await page.getByText("CheckoutLatencyHigh").first().click();
	await expect(page).toHaveURL(/\/alerts\/list\?.*id=alert-checkout-latency/);
	await expect(page.getByRole("heading", { name: "CheckoutLatencyHigh" })).toBeVisible();
	await expect(page.getByRole("tab", { name: "时间线" })).toBeVisible();
	await page.getByRole("tab", { name: "时间线" }).click();
	await expect(page.getByRole("list", { name: "观察记录时间线" })).toBeVisible();
	await page.getByRole("tab", { name: "AI 分析" }).click();
	await page.getByRole("button", { name: "证据 evidence-latency" }).click();
	await expect(page.getByRole("dialog", { name: "证据阅读" })).toBeVisible();
	await expectHealthy(page, guard);
});

test("login and first-password scenarios submit errors and successful auth", async ({ page }) => {
	const guard = protectOffline(page);
	await page.goto("/");
	await selectScenario(page, "login");
	await page.getByLabel("用户名").fill(adminCredentials.username);
	await page.getByLabel("密码").fill("wrong-password-value");
	await page.getByRole("button", { name: "登录" }).click();
	await expect(page.getByText("演示账号或密码不正确")).toBeVisible();
	await page.getByLabel("密码").fill(adminCredentials.password);
	await page.getByRole("button", { name: "登录" }).click();
	await expect(page.getByRole("button", { name: /演示管理员 admin/ })).toBeVisible();

	await selectScenario(page, "first-password");
	await expect(page.getByRole("heading", { name: "先设置你自己的密码" })).toBeVisible();
	await page.getByLabel("当前临时密码").fill("wrong-current-password");
	await page.getByRole("textbox", { name: "新密码", exact: true }).fill(nextPassword);
	await page.getByLabel("再次输入新密码").fill(nextPassword);
	await page.getByRole("button", { name: "保存并进入工作台" }).click();
	await expect(page.getByText("当前演示密码不正确")).toBeVisible();
	await expect(page.getByRole("heading", { name: "先设置你自己的密码" })).toBeVisible();
	await page.getByLabel("当前临时密码").fill("demo-operator-password");
	await page.getByRole("textbox", { name: "新密码", exact: true }).fill(nextPassword);
	await page.getByLabel("再次输入新密码").fill(nextPassword);
	await page.getByRole("button", { name: "保存并进入工作台" }).click();
	await expect(page.getByRole("heading", { name: "先设置你自己的密码" })).toHaveCount(0);
	await expect(page.getByRole("button", { name: /演示操作员 operator/ })).toBeVisible();
	await expectHealthy(page, guard);
});

test("mandatory mutation persists, refetches, and scenario reset restores fixtures", async ({ page }) => {
	const guard = protectOffline(page);
	await page.goto("/investigations/new");
	await page.getByPlaceholder("描述需要调查的问题…").fill("E2E mandatory local mutation");
	await page.getByRole("button", { name: "创建并发送" }).click();
	await expect(page.getByRole("heading", { name: "E2E mandatory local mutation" })).toBeVisible();
	// The detail performs a fresh GET after its POST-created projection is mounted.
	// Do not reload the page here: scenario bootstrap intentionally resets fixtures on remount.
	await expect(page.getByRole("heading", { name: "E2E mandatory local mutation" })).toBeVisible();
	await page.getByRole("button", { name: "开发预览 · 模拟数据" }).click();
	await page.getByRole("button", { name: "重置模拟数据" }).click();
	await page.waitForLoadState("domcontentloaded");
	await page.goto("/investigations");
	await expect(page.getByText("E2E mandatory local mutation")).toHaveCount(0);
	await expect(page.getByText("结算延迟调查")).toBeVisible();
	await expectHealthy(page, guard);
});

test("operator controls are absent and conflict reports a failed write", async ({ page }) => {
	const guard = protectOffline(page);
	await page.goto("/");
	await selectScenario(page, "operator");
	await expect(page.getByRole("button", { name: "管理", exact: true })).toHaveCount(0);
	await page.goto("/admin");
	await expect(page.getByText("仅向管理员开放")).toBeVisible();

	await selectScenario(page, "conflict");
	await page.goto("/knowledge/candidates/candidate-1");
	await expect(page.getByRole("heading", { name: "编辑知识候选" })).toBeVisible();
	await page.getByLabel("标题").fill("conflict title");
	await page.getByRole("button", { name: "保存草稿" }).click();
	await expect(page.getByText("候选状态已变化，请刷新后重试。")).toBeVisible();
	await expectHealthy(page, guard);
});

test("MSW serves local EventSource and blocks business WebSocket escape", async ({ page }) => {
	const guard = protectOffline(page);
	await page.goto("/alerts/list");
	await expect(page.getByText("CheckoutLatencyHigh")).toBeVisible();
	const event = await page.evaluate(() => new Promise<string>((resolve, reject) => {
		const source = new EventSource("/api/v1/alerts/events?after=10");
		const timeout = window.setTimeout(() => { source.close(); reject(new Error("mock EventSource timed out")); }, 5_000);
		source.addEventListener("change", (message) => { window.clearTimeout(timeout); source.close(); resolve((message as MessageEvent<string>).data); });
		source.onerror = () => { window.clearTimeout(timeout); source.close(); reject(new Error("mock EventSource failed")); };
	}));
	expect(event).toContain("alert-checkout-latency");
	const webSocketBlocked = await page.evaluate(() => { try { new WebSocket("wss://business.example.invalid/stream"); return false; } catch (error) { return error instanceof DOMException && error.name === "SecurityError"; } });
	expect(webSocketBlocked).toBe(true);
	await selectScenario(page, "unavailable");
	await expect(page.getByText("暂时无法连接 Quoin")).toBeVisible();
	await selectScenario(page, "expired");
	await expect(page.getByRole("heading", { name: "登录工作台" })).toBeVisible();
	await expectHealthy(page, guard);
});
