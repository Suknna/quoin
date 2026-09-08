import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const templates = fileURLToPath(new URL("./templates", import.meta.url));

export default defineConfig({
	root: templates,
	plugins: [react(), tailwindcss()],
	resolve: { alias: { "@": templates } },
	server: { host: "127.0.0.1", port: 5174, strictPort: true },
	preview: { host: "127.0.0.1", port: 5174, strictPort: true },
	build: {
		outDir: fileURLToPath(
			new URL("../.artifacts/template-baseline/dist", import.meta.url),
		),
		emptyOutDir: true,
		rollupOptions: {
			input: {
				index: `${templates}/index.html`,
				sidebar: `${templates}/sidebar-09/index.html`,
				login: `${templates}/login-02/index.html`,
			},
		},
	},
});
