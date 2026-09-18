import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const calls: unknown[] = [];
const useKnowledgeModule = (props: unknown) => {
	calls.push(props);
	return {
		title: "知识",
		list: null,
		content: <div>知识工作台</div>,
	};
};
vi.mock("@/features/knowledge/ui", () => ({
	useKnowledgeModule: (props: unknown) => useKnowledgeModule(props),
}));

import KnowledgeRoute from "./KnowledgeRoute";

const user = {
	id: "1",
	username: "admin",
	displayName: "Admin",
	role: "admin" as const,
	passwordChangeRequired: false,
	authRevision: 1,
	enabled: true,
	initialized: true,
	lastLoginAt: null,
	rowVersion: 1,
};

beforeEach(() => {
	vi.stubGlobal(
		"matchMedia",
		vi.fn().mockReturnValue({
			matches: false,
			addEventListener: vi.fn(),
			removeEventListener: vi.fn(),
		}),
	);
	vi.stubGlobal(
		"ResizeObserver",
		class {
			observe() {}
			unobserve() {}
			disconnect() {}
		},
	);
});
afterEach(() => {
	cleanup();
	vi.unstubAllGlobals();
});

describe("KnowledgeRoute", () => {
	it("mounts the real knowledge workbench module instead of the placeholder", () => {
		const props = {
			user,
			route: "/knowledge/candidates/c-1",
			navigate: vi.fn(),
			suspended: false,
			openEvidence: vi.fn(),
		};
		render(<KnowledgeRoute props={props} onLogout={vi.fn()} />);
		expect(calls[0]).toBe(props);
		expect(screen.getByText("知识工作台")).toBeInTheDocument();
	});
});
