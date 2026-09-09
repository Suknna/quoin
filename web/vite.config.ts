import { fileURLToPath } from "node:url";
import { defineConfig, mergeConfig } from "vitest/config";
import workbench from "./vite.workbench.config.ts";

// The backend remains the public API authority. Vite forwards every /api path
// (including SSE and the noVNC WebSocket) so local development retains the same
// browser origin and therefore the production Cookie/CSRF boundary.
const apiOrigin = process.env.QUOIN_API_ORIGIN ?? "http://127.0.0.1:8080";

export default mergeConfig(
	workbench,
	defineConfig({
		build: {
			outDir: fileURLToPath(new URL("./dist", import.meta.url)),
			emptyOutDir: true,
			sourcemap: false,
			manifest: true,
		},
		server: {
			proxy: {
				"^/api(?:/|$)": {
					target: apiOrigin,
					changeOrigin: false,
					ws: true,
				},
			},
		},
		test: {
			include: ["**/*.test.{ts,tsx}", "../src/**/*.test.{ts,tsx}"],
			environment: "jsdom",
		},
	}),
);
