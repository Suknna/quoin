import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceShell } from "./WorkspaceShell";

const user = { id: "1", username: "admin", displayName: "Admin", role: "admin" as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 };
const view = { title: "功能页面", list: <p>功能对象列表</p>, content: <p>页面内容</p> };

beforeEach(() => {
	vi.stubGlobal("matchMedia", vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
	vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function renderShell(route: string, navigate = vi.fn()) {
	render(<WorkspaceShell user={user} route={route} view={view} navigate={navigate} onLogout={vi.fn()} />);
	return navigate;
}

describe("WorkspaceShell navigation", () => {
	it("renders two persistent labelled module buttons without a switch dropdown", () => {
		renderShell("/alerts/list");
		expect(screen.getByRole("button", { name: "运维中心" })).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "AI SRE" })).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "切换模块" })).not.toBeInTheDocument();
		expect(screen.queryByText("ChevronsUpDown")).not.toBeInTheDocument();
	});

	it("navigates persistent module buttons to their approved defaults", () => {
		const navigate = renderShell("/admin");
		fireEvent.click(screen.getByRole("button", { name: "运维中心" }));
		fireEvent.click(screen.getByRole("button", { name: "AI SRE" }));
		expect(navigate).toHaveBeenNthCalledWith(1, "/alerts/list");
		expect(navigate).toHaveBeenNthCalledWith(2, "/investigations");
	});

	it("removes the actionless alert desktop header while keeping its mobile menu", () => {
		renderShell("/alerts/list");
		expect(document.querySelectorAll("header.md\\:flex")).toHaveLength(0);
		expect(screen.getByRole("button", { name: "菜单" })).toBeInTheDocument();
	});

	it("keeps the common desktop header when another page has no actions", () => {
		renderShell("/inspections");
		expect(document.querySelectorAll("header.md\\:flex")).toHaveLength(1);
	});

	it("shows each administrator operations entry once with only the route-matching item active", () => {
		renderShell("/inspections/run-1");
		expect(screen.getAllByText("运维中心")).toHaveLength(4);
		for (const name of ["告警列表", "故障复盘", "巡检", "业务纳管", "接入管理"]) expect(screen.getByRole("button", { name })).toBeInTheDocument();
		expect(screen.getAllByRole("button", { name: "接入管理" })).toHaveLength(1);
		expect(screen.getByRole("button", { name: "巡检" })).toHaveAttribute("aria-current", "page");
		expect(screen.getByRole("button", { name: "告警列表" })).not.toHaveAttribute("aria-current");
		expect(screen.getByText("功能对象列表")).toBeInTheDocument();
	});

	it("hides management operation pages and denies their content for operators", () => {
		const operator = { ...user, role: "operator" as const };
		render(<WorkspaceShell user={operator} route="/alerts/list" view={view} navigate={vi.fn()} onLogout={vi.fn()} />);
		expect(screen.getByRole("button", { name: "告警列表" })).toBeInTheDocument();
		for (const name of ["巡检", "业务纳管", "接入管理"]) expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
		cleanup();
		render(<WorkspaceShell user={operator} route="/integrations" view={view} navigate={vi.fn()} onLogout={vi.fn()} />);
		expect(screen.getByRole("alert")).toHaveTextContent("此页面仅向管理员开放");
	});

	it("keeps AI SRE knowledge navigation and active semantics", () => {
		renderShell("/knowledge/items/k-1");
		expect(screen.getAllByText("AI SRE")).toHaveLength(4);
		expect(screen.getByRole("button", { name: "知识（开发中）" })).toHaveAttribute("aria-current", "page");
		expect(screen.getByRole("button", { name: "对话" })).not.toHaveAttribute("aria-current");
		expect(screen.getByRole("button", { name: "AI SRE" })).toHaveAttribute("data-active", "true");
	});

	it("does not select a module for generic administration or account routes", () => {
		renderShell("/admin");
		expect(screen.getByRole("button", { name: "运维中心" })).toHaveAttribute("data-active", "false");
		expect(screen.getByRole("button", { name: "AI SRE" })).toHaveAttribute("data-active", "false");
	});
});
