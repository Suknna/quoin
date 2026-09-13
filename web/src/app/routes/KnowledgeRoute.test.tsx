import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import KnowledgeRoute from "./KnowledgeRoute";

const user = { id: "1", username: "admin", displayName: "Admin", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 };

beforeEach(() => {
	vi.stubGlobal("matchMedia", vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
	vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("KnowledgeRoute", () => {
	it("gates direct workflow links behind the development-state view", () => {
		render(<KnowledgeRoute props={{ user, route: "/knowledge/candidates/c-1", navigate: vi.fn(), suspended: false, openEvidence: vi.fn() }} onLogout={vi.fn()} />);
		expect(screen.getByText("知识库开发中")).toBeInTheDocument();
		expect(screen.getByText(/已存储的数据不会被删除或修改/)).toBeInTheDocument();
		expect(screen.queryByText("编辑知识候选")).not.toBeInTheDocument();
	});
});
