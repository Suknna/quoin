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
		files: [
			"src/**/*.{ts,tsx}",
			"templates/**/*.{ts,tsx}",
			"workbench/**/*.{ts,tsx}",
		],
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
		files: ["workbench/**/*.{ts,tsx}"],
		rules: {
			// The standalone entrypoint intentionally defines its local screen components together.
			"react-refresh/only-export-components": "off",
			"react-hooks/exhaustive-deps": "off",
		},
	},
	{
		// Preserve upstream mixed exports for exact template comparison.
		files: [
			"templates/components/ui/button.tsx",
			"templates/components/ui/badge.tsx",
			"templates/components/ui/tabs.tsx",
			"templates/components/ui/sidebar.tsx",
		],
		rules: { "react-refresh/only-export-components": "off" },
	},
	{
		// The upstream randomized skeleton is unused by sidebar-09.
		files: ["templates/components/ui/sidebar.tsx"],
		rules: { "react-hooks/purity": "off" },
	},
);
