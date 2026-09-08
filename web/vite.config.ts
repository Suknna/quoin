import { fileURLToPath } from "node:url";
import { defineConfig, mergeConfig } from "vitest/config";
import workbench from "./vite.workbench.config.ts";

export default mergeConfig(
	workbench,
	defineConfig({
		build: {
			outDir: fileURLToPath(
				new URL("../internal/gen/web/dist", import.meta.url),
			),
			emptyOutDir: true,
			sourcemap: false,
			manifest: true,
		},
		test: {
			include: ["**/*.test.{ts,tsx}", "../src/**/*.test.{ts,tsx}"],
			environment: "jsdom",
		},
	}),
);
