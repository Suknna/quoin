import { Children, type ReactNode } from "react";
import { ErrorMessage } from "@/app/shared";
import { Button } from "@/components/ui/button";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";

export type DataTableColumn = {
	label: ReactNode;
	/** Extra classes on the th, e.g. text-right for numeric columns. */
	className?: string;
};

type DataTableProps = {
	columns: DataTableColumn[];
	/** TableRow children rendered when data is present. */
	children?: ReactNode;
	loading?: boolean;
	/** Accessible label for the loading skeleton; defaults to a generic one. */
	loadingLabel?: string;
	skeletonRows?: number;
	error?: string;
	onRetry?: () => void;
	emptyTitle?: string;
	emptyDescription?: string;
	className?: string;
};

// Skeleton widths cycle so adjacent cells do not align into a fake block.
const skeletonWidths = ["w-full", "w-5/6", "w-2/3", "w-4/5"];

/**
 * Presentational data-table shell over the shadcn Table primitives: callers
 * own row rendering while loading, error and empty states stay consistent
 * with EntityList.
 */
export function DataTable({
	columns,
	children,
	loading,
	loadingLabel = "正在加载",
	skeletonRows = 3,
	error,
	onRetry,
	emptyTitle = "没有数据",
	emptyDescription,
	className,
}: DataTableProps) {
	const span = columns.length;
	let body: ReactNode;
	if (loading) {
		body = Array.from({ length: skeletonRows }, (_, row) => (
			<TableRow key={row} className="hover:bg-transparent">
				{columns.map((column, cell) => (
					<TableCell key={cell} className={column.className}>
						<Skeleton
							className={cn(
								"h-4",
								skeletonWidths[(row + cell) % skeletonWidths.length],
							)}
						/>
					</TableCell>
				))}
			</TableRow>
		));
	} else if (error) {
		body = (
			<TableRow className="hover:bg-transparent">
				<TableCell colSpan={span}>
					<ErrorMessage>
						{error}
						{onRetry && (
							<Button
								className="ml-3"
								size="sm"
								variant="outline"
								onClick={onRetry}
							>
								重试
							</Button>
						)}
					</ErrorMessage>
				</TableCell>
			</TableRow>
		);
	} else {
		// An empty items array still arrives as a children array, so count
		// instead of null-checking to decide on the empty state.
		body = Children.count(children) ? (
			children
		) : (
			<TableRow className="hover:bg-transparent">
				<TableCell colSpan={span}>
					<Empty className="min-h-40">
						<EmptyHeader>
							<EmptyTitle>{emptyTitle}</EmptyTitle>
							{emptyDescription && (
								<EmptyDescription>{emptyDescription}</EmptyDescription>
							)}
						</EmptyHeader>
					</Empty>
				</TableCell>
			</TableRow>
		);
	}
	return (
		<Table className={className}>
			<TableHeader>
				<TableRow>
					{columns.map((column, index) => (
						<TableHead key={index} className={column.className}>
							{column.label}
						</TableHead>
					))}
				</TableRow>
			</TableHeader>
			<TableBody
				{...(loading ? { role: "status", "aria-label": loadingLabel } : {})}
			>
				{body}
			</TableBody>
		</Table>
	);
}
