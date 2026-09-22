import "@testing-library/jest-dom/vitest";
import { describe, expect, it, vi } from "vitest";
import { importRouteHost } from "./route-import";

vi.mock("./route-reload", () => ({
	clearStaleRouteReloadGuard: vi.fn(),
	reloadAfterStaleRoute: vi.fn(),
}));

import {
	clearStaleRouteReloadGuard,
	reloadAfterStaleRoute,
} from "./route-reload";

const hostModule = { default: () => null };

describe("importRouteHost", () => {
	it("recovers through a guarded full-page reload when the chunk import fails, then rethrows", async () => {
		const failure = new TypeError(
			"Failed to fetch dynamically imported module",
		);
		await expect(
			importRouteHost(() => Promise.reject(failure)),
		).rejects.toBe(failure);
		expect(reloadAfterStaleRoute).toHaveBeenCalledTimes(1);
		expect(clearStaleRouteReloadGuard).not.toHaveBeenCalled();
	});

	it("passes the module through and re-arms the guard on success", async () => {
		vi.mocked(clearStaleRouteReloadGuard).mockClear();
		vi.mocked(reloadAfterStaleRoute).mockClear();
		await expect(
			importRouteHost(() => Promise.resolve(hostModule)),
		).resolves.toBe(hostModule);
		expect(clearStaleRouteReloadGuard).toHaveBeenCalledTimes(1);
		expect(reloadAfterStaleRoute).not.toHaveBeenCalled();
	});
});
