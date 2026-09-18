import { Fragment, type ReactNode } from "react";
import { cn } from "@/lib/utils";

export type PropertyListEntry = {
	label: ReactNode;
	value: ReactNode;
};

type PropertyListProps = {
	entries: PropertyListEntry[];
	/**
	 * vertical: label stacked above value, one per row (default).
	 * inline: "label value" pairs flow in one wrapping row.
	 * grid-2 / grid-3: vertical pairs across 2/3 columns.
	 */
	layout?: "vertical" | "inline" | "grid-2" | "grid-3";
	/** Render values in a monospace font (IDs, machine labels, digests). */
	mono?: boolean;
	className?: string;
};

const layoutClass: Record<NonNullable<PropertyListProps["layout"]>, string> = {
	vertical: "flex flex-col gap-x-4 gap-y-1.5",
	inline: "flex flex-wrap gap-x-6 gap-y-1.5",
	"grid-2": "grid gap-x-6 gap-y-1.5 sm:grid-cols-2",
	"grid-3": "grid gap-x-6 gap-y-1.5 sm:grid-cols-3",
};

/**
 * Shared label/value fact list for detail surfaces (drawers, cards, alerts).
 * Replaces ad-hoc dl grids so every detail block shares one density and tone.
 */
export function PropertyList({
	entries,
	layout = "vertical",
	mono = false,
	className,
}: PropertyListProps) {
	return (
		<dl className={cn(layoutClass[layout], className)}>
			{entries.map((entry, index) => {
				// Labels are unique in every current call site; non-string labels
				// (badges, nodes) fall back to their position.
				const key =
					typeof entry.label === "string" && entry.label
						? entry.label
						: `entry-${String(index)}`;
				return (
					<Fragment key={key}>
						{layout === "inline" ? (
							<>
								<dt className="text-xs text-muted-foreground">{entry.label}</dt>
								<dd
									className={cn(
										"ml-1.5 break-words text-sm",
										mono && "font-mono text-xs",
									)}
								>
									{entry.value}
								</dd>
							</>
						) : (
							<div className="min-w-0 sm:contents">
								<dt className="text-xs text-muted-foreground">{entry.label}</dt>
								<dd
									className={cn(
										"mt-0.5 break-words text-sm",
										mono && "font-mono text-xs",
									)}
								>
									{entry.value}
								</dd>
							</div>
						)}
					</Fragment>
				);
			})}
		</dl>
	);
}
