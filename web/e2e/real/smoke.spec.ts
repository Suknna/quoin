import { expect, test } from "@playwright/test";

test("real deployment serves the login shell", async ({ page }) => {
	await page.goto("/");
	await expect(page.getByRole("heading", { name: "登录工作台" })).toBeVisible();
});
