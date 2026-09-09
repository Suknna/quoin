import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.QUOIN_REAL_E2E_BASE_URL;
if (!baseURL) throw new Error("QUOIN_REAL_E2E_BASE_URL is required for real-system Playwright tests.");

/** Explicit smoke tests for a separately provisioned real environment. */
export default defineConfig({
	testDir: "./e2e/real",
	// Legacy journeys are retained for their real-stack intent. Their former Docker
	// fixture is intentionally not recreated; opt in only with a compatible stack.
	testIgnore: process.env.QUOIN_REAL_E2E_LEGACY === "1" ? undefined : "**/legacy/**",
	timeout: 45_000,
	expect: { timeout: 10_000 },
	forbidOnly: Boolean(process.env.CI),
	use: { baseURL, trace: "retain-on-failure" },
	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
