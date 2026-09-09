import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
	{ ignores: ["dist", "node_modules", "playwright-report", "test-results"] },
	js.configs.recommended,
	...tseslint.configs.recommended,
	{
		files: ["src/**/*.{ts,tsx}"],
		languageOptions: {
			ecmaVersion: 2023,
			globals: globals.browser,
		},
		plugins: {
			"react-hooks": reactHooks,
			"react-refresh": reactRefresh,
		},
		rules: {
			...reactHooks.configs.recommended.rules,
			"react-refresh/only-export-components": [
				"warn",
				{ allowConstantExport: true },
			],
		},
	},
	{
		// The app shell owns screen composition and upstream UI files retain mixed exports.
		files: [
			"src/app/**/*.{ts,tsx}",
			"src/components/ui/{button,badge,tabs,sidebar}.tsx",
		],
		rules: {
			"react-refresh/only-export-components": "off",
			"react-hooks/exhaustive-deps": "off",
		},
	},
	{
		// The upstream randomized skeleton is unused by the application sidebar.
		files: ["src/components/ui/sidebar.tsx"],
		rules: { "react-hooks/purity": "off" },
	},
);
