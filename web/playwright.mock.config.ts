import { defineConfig, devices } from "@playwright/test";

/** Offline UI acceptance: Vite plus MSW only, never Docker or a real service. */
export default defineConfig({
	testDir: "./e2e/mock",
	timeout: 30_000,
	expect: { timeout: 8_000 },
	forbidOnly: Boolean(process.env.CI),
	workers: 1,
	use: {
		baseURL: "http://127.0.0.1:4175",
		trace: "retain-on-failure",
	},
	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
	webServer: {
		command: "QUOIN_DEV_PORT=4175 pnpm dev",
		url: "http://127.0.0.1:4175",
		reuseExistingServer: !process.env.CI,
		timeout: 60_000,
	},
});
