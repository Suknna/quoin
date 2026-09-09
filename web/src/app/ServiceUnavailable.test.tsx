import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ServiceUnavailable } from "./ServiceUnavailable";

afterEach(cleanup);
it("shows a concise error and a single retry action without diagnostics", () => {
	const retry = vi.fn();
	render(<ServiceUnavailable pending={false} onRetry={retry} />);
	expect(screen.getByText("暂时无法连接 Quoin")).toBeInTheDocument();
	expect(screen.getAllByRole("button")).toHaveLength(1);
	expect(screen.queryByRole("status")).not.toBeInTheDocument();
	expect(
		screen.queryByText(/连接诊断|HTTP 状态|GET \/api/),
	).not.toBeInTheDocument();
	fireEvent.click(screen.getByRole("button", { name: "重新连接" }));
	expect(retry).toHaveBeenCalledTimes(1);
});
it("keeps the error layout visible and prevents duplicate retries while pending", () => {
	const retry = vi.fn();
	render(<ServiceUnavailable pending onRetry={retry} />);
	expect(screen.getByText("暂时无法连接 Quoin")).toBeInTheDocument();
	const button = screen.getByRole("button", { name: "正在重新连接…" });
	expect(button).toBeDisabled();
	fireEvent.click(button);
	expect(retry).not.toHaveBeenCalled();
});
