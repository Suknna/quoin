import { Fragment } from "react";
import { Skeleton } from "@/components/ui/skeleton";

type RowKind = "title" | "line" | "card";

const rowClass: Record<RowKind, string> = {
	title: "h-7 w-1/3",
	line: "h-4",
	card: "h-24 w-full",
};
// Line widths cycle so consecutive lines do not align into a fake block.
const lineWidths = ["w-full", "w-5/6", "w-2/3", "w-4/5"];

/** Shared detail-loading placeholder announced to assistive technology. */
export function DetailSkeleton({
	label,
	rows = ["title", "line", "line", "line"],
}: {
	label: string;
	rows?: RowKind[];
}) {
	let lineIndex = 0;
	let rowOrdinal = 0;
	return (
		<div className="flex flex-col gap-3" role="status" aria-label={label}>
			{rows.map((row) => {
				const ordinal = rowOrdinal++;
				const width =
					row === "line"
						? `${rowClass.line} ${lineWidths[lineIndex++ % lineWidths.length]}`
						: rowClass[row];
				return (
					<Fragment key={`${row}-${ordinal}`}>
						<Skeleton className={width} />
					</Fragment>
				);
			})}
		</div>
	);
}
