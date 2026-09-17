import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { parseStructured } from "./structured";
import { RawPayload, StructuredData } from "./StructuredData";

afterEach(() => cleanup());

function stubClipboard(): { writeText: ReturnType<typeof vi.fn> } {
  const writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  return { writeText };
}

describe("parseStructured", () => {
  it("accepts complete JSON objects and arrays only", () => {
    expect(parseStructured('{"a":1}')).toEqual({ ok: true, value: { a: 1 } });
    expect(parseStructured("[1,2]")).toEqual({ ok: true, value: [1, 2] });
  });
  it("rejects prose, primitives, and truncated JSON without guessing", () => {
    expect(parseStructured("hello").ok).toBe(false);
    expect(parseStructured("42").ok).toBe(false);
    expect(parseStructured('{"a":').ok).toBe(false);
    expect(parseStructured("").ok).toBe(false);
    expect(parseStructured("null").ok).toBe(false);
  });
});

describe("StructuredData", () => {
  it("shows object fields with real values and unknown keys preserved", () => {
    render(<StructuredData value={{ status: "ok", custom_field: 3 }} raw='{"status":"ok","custom_field":3}' />);
    expect(screen.getByText("status")).toBeInTheDocument();
    expect(screen.getByText("custom_field")).toBeInTheDocument();
    expect(screen.getByText("ok")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    // 只有真实字段，不伪造健康判断或摘要标题。
    expect(screen.queryByText(/健康/)).not.toBeInTheDocument();
  });

  it("maps only known fields to readable labels while keeping unknown fields", () => {
    render(<StructuredData value={{ executionMode: "read_only", mystery: 1 }} raw="{}" fieldLabels={{ executionMode: "执行模式" }} />);
    expect(screen.getByText("执行模式")).toBeInTheDocument();
    expect(screen.queryByText("executionMode")).not.toBeInTheDocument();
    expect(screen.getByText("mystery")).toBeInTheDocument();
  });

  it("renders record arrays as a bounded table and names the bound honestly", () => {
    const rows = Array.from({ length: 25 }, (_, index) => ({ i: index, v: `x${index}` }));
    render(<StructuredData value={rows} raw={JSON.stringify(rows)} />);
    const table = screen.getByRole("table");
    expect(table).toHaveTextContent("x19");
    expect(table).not.toHaveTextContent("x24");
    expect(screen.getByText("仅显示前 20 项，完整数据见原文。")).toBeInTheDocument();
  });

  it("keeps nested structures expandable instead of dumping an inline JSON blob", () => {
    const value = { resultType: "matrix", result: [{ metric: { up: 1 } }] };
    render(<StructuredData value={value} raw={JSON.stringify(value)} />);
    expect(screen.queryByText("metric")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "数组 · 1 项" }));
    expect(screen.getByText("metric")).toBeInTheDocument();
  });

  it("shows empty structures as empty instead of fabricating content", () => {
    render(<StructuredData value={{ items: [] }} raw="{}" />);
    expect(screen.getByText("空数组")).toBeInTheDocument();
  });

  it("keeps the original JSON collapsed, verbatim, and copyable", () => {
    const { writeText } = stubClipboard();
    const raw = '{"status":"ok"}';
    render(<StructuredData value={{ status: "ok" }} raw={raw} />);
    const rawToggle = screen.getByRole("button", { name: "原始 JSON" });
    expect(rawToggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(rawToggle);
    expect(screen.getByText(raw)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "复制" }));
    expect(writeText).toHaveBeenCalledWith(raw);
  });
});

describe("RawPayload", () => {
  it("shows the exact original text with copy and no interpretation", () => {
    const { writeText } = stubClipboard();
    const broken = '{"truncated';
    render(<RawPayload text={broken} label="报告原文" />);
    fireEvent.click(screen.getByRole("button", { name: "报告原文" }));
    expect(screen.getByText(broken)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "复制" }));
    expect(writeText).toHaveBeenCalledWith(broken);
  });
});
