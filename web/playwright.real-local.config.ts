import { defineConfig, devices } from "@playwright/test";

/** Exercises dev:real against a local API stub, never a deployment. */
export default defineConfig({
	testDir: "./e2e/real-local",
	timeout: 30_000,
	use: { baseURL: "http://127.0.0.1:4177" },
	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
	webServer: [
		{ command: "node e2e/real/api-stub.mjs", url: "http://127.0.0.1:4188/api/v1/auth/me", reuseExistingServer: false },
		{ command: "VITE_QUOIN_MODE=real QUOIN_API_ORIGIN=http://127.0.0.1:4188 QUOIN_DEV_PORT=4177 pnpm dev", url: "http://127.0.0.1:4177", reuseExistingServer: false },
	],
});
