import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TableCell, TableRow } from "@/components/ui/table";
import { DataTable } from "./DataTable";

afterEach(cleanup);

const columns = [
	{ label: "名称" },
	{ label: "状态" },
	{ label: <span className="sr-only">操作</span> },
];

describe("DataTable", () => {
	it("renders column headers and caller-provided rows", () => {
		render(
			<DataTable columns={columns}>
				<TableRow>
					<TableCell>alpha</TableCell>
					<TableCell>已启用</TableCell>
					<TableCell>
						<button type="button">编辑</button>
					</TableCell>
				</TableRow>
			</DataTable>,
		);
		expect(
			screen.getByRole("columnheader", { name: "名称" }),
		).toBeInTheDocument();
		expect(screen.getByRole("cell", { name: "alpha" })).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "编辑" })).toBeInTheDocument();
	});

	it("shows a skeleton instead of rows while loading", () => {
		render(<DataTable columns={columns} loading loadingLabel="正在读取" />);
		expect(
			screen.getByRole("status", { name: "正在读取" }),
		).toBeInTheDocument();
		// 骨架占满数据行位置，没有真实单元格内容。
		expect(screen.queryByText("alpha")).not.toBeInTheDocument();
		expect(
			screen.getAllByText("", { selector: "[data-slot=skeleton]" }).length,
		).toBeGreaterThan(0);
	});

	it("renders an error row with retry and hides rows", () => {
		const onRetry = vi.fn();
		render(
			<DataTable columns={columns} error="读取失败" onRetry={onRetry}>
				<TableRow>
					<TableCell>alpha</TableCell>
					<TableCell>已启用</TableCell>
					<TableCell />
				</TableRow>
			</DataTable>,
		);
		expect(screen.getByText("读取失败")).toBeInTheDocument();
		expect(screen.queryByText("alpha")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "重试" }));
		expect(onRetry).toHaveBeenCalledOnce();
	});

	it("renders the empty state when no rows are provided", () => {
		render(
			<DataTable
				columns={columns}
				emptyTitle="没有数据"
				emptyDescription="调整筛选条件"
			/>,
		);
		expect(screen.getByText("没有数据")).toBeInTheDocument();
		expect(screen.getByText("调整筛选条件")).toBeInTheDocument();
	});
});
