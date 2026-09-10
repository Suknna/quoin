import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.QUOIN_REAL_E2E_BASE_URL;
if (!baseURL) throw new Error("QUOIN_REAL_E2E_BASE_URL is required for real-system Playwright tests.");

/**
 * Explicit browser acceptance for a separately provisioned real deployment.
 * The supported #96/#102 command selects only current issue scenarios; legacy
 * files remain opt-in and can never make supported acceptance look green.
 */
export default defineConfig({
	testDir: "./e2e/real",
	testMatch: process.env.QUOIN_REAL_E2E_LEGACY === "1" ? undefined : "**/issue-{96,97,102}*.integration.spec.ts",
	timeout: 60_000,
	expect: { timeout: 15_000 },
	forbidOnly: Boolean(process.env.CI),
	// Registration reveal tokens are intentionally one-time secrets; do not
	// preserve traces that could contain them, even for a failed local run.
	use: { baseURL, trace: "off", screenshot: "off", video: "off", ignoreHTTPSErrors: true },
	workers: 1,
	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
