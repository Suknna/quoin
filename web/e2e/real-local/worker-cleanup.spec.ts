import { expect, test } from "@playwright/test";

test("dev:real removes only Quoin's stale mock worker before API bootstrap", async ({ page }) => {
	await page.goto("/");
	await page.evaluate(async () => {
		await navigator.serviceWorker.register("/mockServiceWorker.js");
		await navigator.serviceWorker.ready;
	});
	await page.reload();
	await expect(page.getByRole("button", { name: /Stub Admin admin/ })).toBeVisible();
	const registrations = await page.evaluate(async () => (await navigator.serviceWorker.getRegistrations()).map((entry) => entry.active?.scriptURL ?? entry.scope));
	expect(registrations.filter((url) => url.endsWith("/mockServiceWorker.js"))).toEqual([]);
});
