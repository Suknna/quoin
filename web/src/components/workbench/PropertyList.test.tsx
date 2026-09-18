import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { PropertyList } from "./PropertyList";

afterEach(cleanup);

describe("PropertyList", () => {
	it("renders label/value pairs with semantic dl terms", () => {
		render(
			<PropertyList
				entries={[
					{ label: "端点", value: "https://example.com" },
					{ label: "状态", value: <span data-testid="badge">已启用</span> },
				]}
			/>,
		);
		const terms = screen.getAllByRole("term");
		expect(terms.map((term) => term.textContent)).toEqual(["端点", "状态"]);
		expect(terms[0]).toHaveTextContent("端点");
		expect(screen.getByTestId("badge")).toBeInTheDocument();
	});

	it("supports inline layout for compact summary rows", () => {
		render(
			<PropertyList
				layout="inline"
				entries={[
					{ label: "触发", value: "手动" },
					{ label: "报告", value: "v2" },
				]}
			/>,
		);
		// dt/dd 属于 naming-prohibited 角色，accessible name 为空，按文本断言。
		expect(screen.getByText("触发")).toBeInTheDocument();
		expect(screen.getByText("手动")).toBeInTheDocument();
		expect(screen.getByText("v2")).toBeInTheDocument();
	});

	it("renders mono values for machine identities", () => {
		render(
			<PropertyList mono entries={[{ label: "报告 ID", value: "rpt-123" }]} />,
		);
		expect(screen.getByText("rpt-123")).toHaveClass("font-mono");
	});
});
