import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

const apiOrigin = process.env.QUOIN_API_ORIGIN ?? "http://127.0.0.1:8080";
const tlsCert = process.env.QUOIN_PREVIEW_TLS_CERT;
const tlsKey = process.env.QUOIN_PREVIEW_TLS_KEY;

// A certificate is optional for ordinary local development, but must be supplied as a pair
// so TLS previews retain upstream verification rather than silently degrading security.
if (Boolean(tlsCert) !== Boolean(tlsKey)) {
	throw new Error("Both preview TLS certificate and key are required");
}

export default defineConfig({
	plugins: [
		react(),
		tailwindcss(),
		// The MSW worker is served only by Vite development mode. It never enters
		// public/ or the production bundle.
		{
			name: "quoin-dev-mock-worker",
			configureServer(server) {
				server.middlewares.use("/mockServiceWorker.js", (_request, response) => {
					response.setHeader("Content-Type", "application/javascript; charset=utf-8");
					response.end(readFileSync(fileURLToPath(new URL("./src/mocks/mockServiceWorker.js", import.meta.url))));
				});
			},
		},
	],
	resolve: {
		alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
	},
	server: {
		host: "127.0.0.1",
		port: tlsCert ? 18443 : Number(process.env.QUOIN_DEV_PORT ?? 5175),
		https: tlsCert && tlsKey ? { cert: readFileSync(tlsCert), key: readFileSync(tlsKey) } : undefined,
		strictPort: true,
			// Mock mode has no proxy: unsupported API requests fail visibly in MSW.
			// Real mode preserves the production same-origin API/SSE/WebSocket boundary.
			proxy: process.env.VITE_QUOIN_MODE === "real"
				? { "^/api(?:/|$)": { target: apiOrigin, changeOrigin: false, ws: true } }
				: undefined,
	},
	build: {
		outDir: fileURLToPath(new URL("./dist", import.meta.url)),
		emptyOutDir: true,
		sourcemap: false,
		manifest: true,
	},
	test: {
		include: ["src/**/*.test.{ts,tsx}"],
		environment: "jsdom",
	},
});
