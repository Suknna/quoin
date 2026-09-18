import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AiContent, EvidenceLinks } from "./AiContent";

afterEach(() => cleanup());

describe("AiContent", () => {
	it("renders headings, tables, and lists through the shared reader", () => {
		render(
			<AiContent
				content={
					"# 结论\n\n| 检查 | 状态 |\n| --- | --- |\n| 连通性 | 正常 |\n\n- 要点一\n- 要点二"
				}
			/>,
		);
		expect(screen.getByRole("heading", { name: "结论" })).toBeInTheDocument();
		expect(screen.getByRole("table")).toHaveTextContent("连通性");
		expect(screen.getByRole("list")).toBeInTheDocument();
		expect(screen.getByText("要点一")).toBeInTheDocument();
	});

	it("keeps model output safe by never injecting raw HTML", () => {
		render(<AiContent content={"正文<script>alert('unsafe')</script>结尾"} />);
		expect(screen.getByText(/正文/)).toBeInTheDocument();
		expect(document.querySelector("script")).toBeNull();
	});

	it("converts frozen evidence references into reading-layer buttons and leaves unknown references as text", () => {
		const openEvidence = vi.fn();
		render(
			<AiContent
				content={"见 #e-1 与 #e-9。"}
				evidenceIds={["e-1"]}
				openEvidence={openEvidence}
			/>,
		);
		fireEvent.click(screen.getByRole("button", { name: "#e-1" }));
		expect(openEvidence).toHaveBeenCalledWith("e-1");
		// Unknown references stay visible text; they never become fabricated links.
		expect(document.body.textContent).toContain("#e-9");
		expect(
			screen.queryByRole("button", { name: "#e-9" }),
		).not.toBeInTheDocument();
	});

	it("keeps code blocks as text without executing embedded markup", () => {
		render(<AiContent content={"```\n<script>alert('code')</script>\n```"} />);
		expect(screen.getByText(/alert\('code'\)/)).toBeInTheDocument();
		expect(document.querySelector("script")).toBeNull();
	});

	it("keeps long table cells wrapped instead of clipped", () => {
		const long = "很长".repeat(60);
		render(
			<AiContent
				content={`| 字段 | 值 |\n| --- | --- |\n| 详情 | ${long} |`}
			/>,
		);
		const cell = screen.getByText(long);
		expect(cell).toHaveClass("whitespace-normal", "break-words");
	});

	it("reduces model-authored links to plain text so injected URLs are never clickable", () => {
		render(
			<AiContent
				content={
					"详见 [外部文档](https://evil.example/phish) 与 [未知证据](/evidence/ghost-1)。"
				}
				evidenceIds={["e-1"]}
				openEvidence={vi.fn()}
			/>,
		);
		// No anchor survives: external URLs and non-frozen evidence paths alike.
		expect(screen.queryByRole("link")).not.toBeInTheDocument();
		expect(screen.getByText("外部文档")).toBeInTheDocument();
		expect(screen.getByText("未知证据")).toBeInTheDocument();
	});
});

describe("EvidenceLinks", () => {
	it("opens each frozen evidence id through the existing reading layer", () => {
		const openEvidence = vi.fn();
		render(<EvidenceLinks ids={["e-1", "e-2"]} openEvidence={openEvidence} />);
		fireEvent.click(screen.getByRole("button", { name: "证据 e-1 查看证据" }));
		expect(openEvidence).toHaveBeenCalledWith("e-1");
		fireEvent.click(screen.getByRole("button", { name: "证据 e-2 查看证据" }));
		expect(openEvidence).toHaveBeenCalledWith("e-2");
	});
});
