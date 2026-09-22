import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	clearStaleRouteReloadGuard,
	reloadAfterStaleRoute,
	STALE_ROUTE_RELOAD_KEY,
} from "./route-reload";

// jsdom 的 location.reload 只打印 Not implemented 并正常返回，守卫语义
// 通过 sessionStorage 键与纯 DOM 状态行断言，不依赖真实导航。
beforeEach(() => {
	window.sessionStorage.clear();
	document.body.replaceChildren();
});

afterEach(() => {
	vi.restoreAllMocks();
});

function noteText(): string | null {
	return document.querySelector("p[role='status']")?.textContent ?? null;
}

describe("reloadAfterStaleRoute", () => {
	it("schedules one automatic reload per page session and arms the guard", () => {
		vi.spyOn(console, "error").mockImplementation(() => undefined);
		reloadAfterStaleRoute();
		expect(window.sessionStorage.getItem(STALE_ROUTE_RELOAD_KEY)).toBe("1");
		expect(noteText()).toContain("部署已更新，正在刷新页面");
	});

	it("does not auto-reload a second time in the same page session", () => {
		vi.spyOn(console, "error").mockImplementation(() => undefined);
		reloadAfterStaleRoute();
		reloadAfterStaleRoute();
		expect(noteText()).toContain("请手动刷新");
		expect(window.sessionStorage.getItem(STALE_ROUTE_RELOAD_KEY)).toBe("1");
	});

	it("re-arms the loop after a successful route chunk load", () => {
		vi.spyOn(console, "error").mockImplementation(() => undefined);
		reloadAfterStaleRoute();
		clearStaleRouteReloadGuard();
		expect(window.sessionStorage.getItem(STALE_ROUTE_RELOAD_KEY)).toBeNull();
		reloadAfterStaleRoute();
		expect(noteText()).toContain("部署已更新，正在刷新页面");
	});
});
