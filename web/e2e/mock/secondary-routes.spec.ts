import { expect, test } from "@playwright/test";

function guard(page: import("@playwright/test").Page) {
	const errors: string[] = [];
	const external: string[] = [];
	page.on("pageerror", (error) => errors.push(error.message));
	page.on("request", (request) => {
		if (new URL(request.url()).origin !== "http://127.0.0.1:4175")
			external.push(request.url());
	});
	return { errors, external };
}

async function expectClean(
	page: import("@playwright/test").Page,
	state: ReturnType<typeof guard>,
) {
	await expect(
		page.getByText("Mock API has no handler", { exact: false }),
	).toHaveCount(0);
	expect(state.errors).toEqual([]);
	expect(state.external).toEqual([]);
}

test("secondary and detail routes render their declared projections without fallback APIs", async ({
	page,
}) => {
	const state = guard(page);
	const routes = [
		["/alerts/list?view=history", "CatalogErrors"],
		["/alerts/list?id=alert-checkout-latency", "CheckoutLatencyHigh"],
		["/postmortems", "能力建设中"],
		["/investigations/investigation-checkout", "结算延迟调查"],
		["/inspections/runs/inspection-run-1", "checkout-health"],
		["/knowledge/items/knowledge-1", "结算延迟排查"],
		["/knowledge/candidates/candidate-1", "编辑知识候选"],
		["/knowledge/imports/new", "导入原文"],
		["/knowledge/imports/import-1", "导入的结算故障排查步骤"],
		["/settings/profile", "个人资料"],
		["/settings/security", "修改密码"],
		["/settings/platform/users", "用户"],
		["/settings/platform/backups", "备份与保留"],
		["/settings/platform/audit", "审计"],
		["/settings/platform/about", "关于平台"],
		["/settings/platform/runtime", "运行时"],
		["/settings/platform/model-providers", "模型提供方"],
		["/settings/platform/integrations", "接入管理"],
	] as const;
	for (const [route, evidence] of routes) {
		await page.goto(route);
		await expect(page).toHaveURL(new RegExp(route.replace(/[?]/g, "\\?")));
		await expect(
			page.getByText(evidence, { exact: false }).first(),
		).toBeVisible();
		await expectClean(page, state);
	}
});
