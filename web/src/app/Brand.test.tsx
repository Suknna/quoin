/// <reference types="node" />
import { existsSync } from "node:fs";
import { join } from "node:path";
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { BRAND_ASSET_FILES, BrandLockup, BrandMark } from "./Brand";

afterEach(cleanup);

describe("brand assets", () => {
	it("keeps every referenced runtime asset present in web/public/brand", () => {
		// Vitest runs with the web workspace as its working directory.
		const publicBrand = join(process.cwd(), "public", "brand");
		for (const file of BRAND_ASSET_FILES) {
			expect(existsSync(join(publicBrand, file)), file).toBe(true);
		}
	});

	it("exposes the adaptive login wordmark under a stable accessible name", () => {
		render(<BrandLockup />);
		const lockup = screen.getByRole("img", { name: "Quoin" });
		const sources = [...lockup.querySelectorAll("img")].map((img) =>
			img.getAttribute("src"),
		);
		expect(sources).toEqual([
			"/brand/lockup-light.png",
			"/brand/lockup-dark.png",
		]);
		for (const img of lockup.querySelectorAll("img")) {
			// Only the wrapper carries the name; both variants stay decorative.
			expect(img).toHaveAttribute("alt", "");
		}
	});

	it("renders the fixed dark-surface wordmark as a single named image", () => {
		render(<BrandLockup background="dark" alt="quoin" />);
		expect(screen.getByRole("img", { name: "quoin" })).toHaveAttribute(
			"src",
			"/brand/lockup-dark.png",
		);
		expect(screen.queryByRole("img", { name: "Quoin" })).not.toBeInTheDocument();
	});

	it("keeps the standalone mark decorative so the owning control provides the name", () => {
		const { container } = render(<BrandMark />);
		const images = [...container.querySelectorAll("img")];
		expect(images.map((img) => img.getAttribute("src"))).toEqual([
			"/brand/mark-light.png",
			"/brand/mark-dark.png",
		]);
		for (const img of images) {
			expect(img).toHaveAttribute("alt", "");
			expect(img).toHaveAttribute("aria-hidden", "true");
		}
	});
});
