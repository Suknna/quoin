import { expect, test } from "@playwright/test";

function guard(page: import("@playwright/test").Page) {
	const errors: string[] = [];
	const external: string[] = [];
	page.on("pageerror", (error) => errors.push(error.message));
	page.on("request", (request) => { if (new URL(request.url()).origin !== "http://127.0.0.1:4175") external.push(request.url()); });
	return { errors, external };
}

async function expectClean(page: import("@playwright/test").Page, state: ReturnType<typeof guard>) {
	await expect(page.getByText("Mock API has no handler", { exact: false })).toHaveCount(0);
	expect(state.errors).toEqual([]);
	expect(state.external).toEqual([]);
}

test("secondary and detail routes render their declared projections without fallback APIs", async ({ page }) => {
	const state = guard(page);
	const routes = [
		["/alerts/list?view=history", "CatalogErrors"],
		["/alerts/list?id=alert-checkout-latency", "CheckoutLatencyHigh"],
		["/postmortems", "能力建设中"],
		["/investigations/investigation-checkout", "结算延迟调查"],
		["/inspections/inspection-run-1", "checkout-health"],
		["/knowledge/items/knowledge-1", "结算延迟排查"],
		["/knowledge/candidates/candidate-1", "编辑知识候选"],
		["/knowledge/imports/new", "导入原文"],
		["/knowledge/imports/import-1", "导入的结算故障排查步骤"],
		["/admin/users", "用户"],
		["/admin/connections", "添加连接"],
		["/admin/alerts", "告警源与凭据"],
		["/admin/alert-intake-issues", "delivery_truncated"],
		["/admin/runtimes", "运行时"],
		["/admin/backups", "备份与保留"],
		["/admin/maintenance", "维护"],
		["/admin/audit", "审计"],
		["/admin/labels", "标签契约"],
		["/admin/journeys", "Journey"],
		["/account", "个人资料"],
		["/account/security", "修改密码"],
		["/account/audit", "审计事件"],
	] as const;
	for (const [route, evidence] of routes) {
		await page.goto(route);
		await expect(page).toHaveURL(new RegExp(route.replace(/[?]/g, "\\?")));
		await expect(page.getByText(evidence, { exact: false }).first()).toBeVisible();
		await expectClean(page, state);
	}
});
