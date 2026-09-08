import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

const tlsCert = process.env.QUOIN_PREVIEW_TLS_CERT;
const tlsKey = process.env.QUOIN_PREVIEW_TLS_KEY;
if (Boolean(tlsCert) !== Boolean(tlsKey)) throw new Error("Both preview TLS certificate and key are required");

export default defineConfig({
	root: fileURLToPath(new URL("./workbench", import.meta.url)),
	publicDir: fileURLToPath(new URL("./templates/public", import.meta.url)),
	plugins: [react(), tailwindcss()],
	resolve: {
		alias: { "@": fileURLToPath(new URL("./templates", import.meta.url)) },
	},
	build: {
		outDir: fileURLToPath(
			new URL("../.artifacts/new-workbench/dist", import.meta.url),
		),
		emptyOutDir: true,
		manifest: true,
	},
	server: {
		host: "127.0.0.1",
		port: tlsCert ? 18443 : 5175,
    https: tlsCert && tlsKey ? { cert: readFileSync(tlsCert), key: readFileSync(tlsKey) } : undefined,
		strictPort: true,
		proxy: {
			"^/api/": {
				target: "https://quoin-lab.quoin.internal",
				changeOrigin: true,
				// Trust the lab CA through NODE_EXTRA_CA_CERTS; never disable upstream verification.
				secure: true,
			},
		},
	},
	test: { include: ["**/*.test.{ts,tsx}"], environment: "jsdom" },
});
