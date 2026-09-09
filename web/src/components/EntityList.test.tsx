import "@testing-library/jest-dom/vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { EntityList } from "./EntityList";

describe("EntityList", () => {
  it("renders configured columns, highlights the selected item, and keeps row actions separate", () => {
    const select = vi.fn();
    const action = vi.fn();
    render(<EntityList items={[{ id: "one", title: "First", badge: { text: "Firing", variant: "destructive" }, subtitle: "checkout", time: "now" }]} columns={["title", "status", "subtitle", "time", "actions"]} selectedId="one" onSelect={select} renderActions={() => <button onClick={(event) => { event.stopPropagation(); action(); }}>Confirm</button>} />);
    expect(screen.getByText("First")).toBeInTheDocument();
    expect(screen.getByText("Firing")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "First" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
    fireEvent.keyDown(screen.getByRole("button", { name: "Confirm" }), { key: "Enter" });
    expect(action).toHaveBeenCalledOnce();
    expect(select).not.toHaveBeenCalled();
  });
});
