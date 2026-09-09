import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ReportBody } from "./index";

describe("inspection report body", () => {
  it("renders report prose in paragraphs and opens frozen evidence references", () => {
    const openEvidence = vi.fn();
    render(<ReportBody content={"结论见 #e-1。\n\n下一步继续观察。"} evidenceIds={["e-1"]} openEvidence={openEvidence} />);
    expect(screen.getByText("下一步继续观察。")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "#e-1" }));
    expect(openEvidence).toHaveBeenCalledWith("e-1");
  });
});
