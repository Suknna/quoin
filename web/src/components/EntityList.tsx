import { ChevronRight } from "lucide-react";
import type { ReactNode } from "react";
import { ErrorMessage } from "@/app/shared";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import {
	Item,
	ItemActions,
	ItemContent,
	ItemDescription,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

export type EntityListColumn =
	| "title"
	| "status"
	| "subtitle"
	| "time"
	| "actions"
	| "media";
export type EntityListItem = {
	id: string;
	title: string;
	badge?: {
		text: string;
		variant?: React.ComponentProps<typeof Badge>["variant"];
		className?: string;
	};
	subtitle?: ReactNode;
	time?: ReactNode;
	/** A compact visual identity for Item rows, such as an alert severity dot. */
	media?: ReactNode;
};

type EntityListProps<T extends EntityListItem> = {
	items: T[];
	columns: EntityListColumn[];
	selectedId?: string | null;
	onSelect?: (item: T) => void;
	renderActions?: (item: T) => ReactNode;
	loading?: boolean;
	/** Accessible label for the loading skeleton; defaults to a generic one. */
	loadingLabel?: string;
	error?: string;
	onRetry?: () => void;
	emptyTitle?: string;
	emptyDescription?: string;
	controls?: ReactNode;
	className?: string;
	listClassName?: string;
	/** A row style applies consistently without bypassing this shared component. */
	rowClassName?: string;
};

/** A presentational Item-list shell; callers own domain state while columns declare visible fields. */
export function EntityList<T extends EntityListItem>({
	items,
	columns,
	selectedId,
	onSelect,
	renderActions,
	loading,
	loadingLabel = "正在加载",
	error,
	onRetry,
	emptyTitle = "没有数据",
	emptyDescription,
	controls,
	className,
	listClassName,
	rowClassName,
}: EntityListProps<T>) {
	const itemBody = (item: T) => (
		<>
			{columns.includes("media") && item.media && (
				<ItemMedia variant="default">{item.media}</ItemMedia>
			)}
			<ItemContent>
				{columns.includes("title") && (
					<ItemTitle className="truncate">{item.title}</ItemTitle>
				)}
				{columns.includes("subtitle") && item.subtitle && (
					<ItemDescription className="mt-0.5 truncate">
						{item.subtitle}
					</ItemDescription>
				)}
			</ItemContent>
			{columns.includes("status") && item.badge && (
				<Badge variant={item.badge.variant} className={item.badge.className}>
					{item.badge.text}
				</Badge>
			)}
			{columns.includes("time") && item.time && (
				<span className="shrink-0 text-right text-xs tabular-nums text-muted-foreground">
					{item.time}
				</span>
			)}
			{onSelect && !renderActions && (
				<ChevronRight
					className="size-4 shrink-0 text-muted-foreground"
					aria-hidden="true"
				/>
			)}
		</>
	);
	const rowClass = (item: T) =>
		cn(
			"w-full gap-3 rounded-none border-0 border-b px-3 py-2.5 text-left last:border-b-0 hover:bg-muted/50",
			selectedId === item.id &&
				"bg-muted shadow-[inset_2px_0_0_0_var(--color-foreground)]",
			rowClassName,
		);
	return (
		<div className={cn("flex flex-col gap-3", className)}>
			{controls}
			{loading ? (
				<div
					className="flex flex-col gap-2"
					role="status"
					aria-label={loadingLabel}
				>
					<Skeleton className="h-14 w-full" />
					<Skeleton className="h-14 w-full" />
					<Skeleton className="h-14 w-full" />
				</div>
			) : error ? (
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
			) : items.length === 0 ? (
				<Empty className="min-h-56">
					<EmptyHeader>
						<EmptyTitle>{emptyTitle}</EmptyTitle>
						{emptyDescription && (
							<EmptyDescription>{emptyDescription}</EmptyDescription>
						)}
					</EmptyHeader>
				</Empty>
			) : (
				<ul
					className={cn("overflow-hidden rounded-lg border", listClassName)}
					// biome-ignore lint/a11y/noRedundantRoles: Tailwind preflight strips list styling; VoiceOver needs the explicit role.
					role="list"
				>
					{items.map((item) => (
						<li
							key={item.id}
							data-selected={selectedId === item.id || undefined}
						>
							{renderActions ? (
								<Item className={rowClass(item)}>
									{onSelect ? (
										<button
											type="button"
											onClick={() => onSelect(item)}
											className="flex min-w-0 flex-1 items-center gap-4 text-left"
										>
											{itemBody(item)}
										</button>
									) : (
										<div className="flex min-w-0 flex-1 items-center gap-4">
											{itemBody(item)}
										</div>
									)}
									<ItemActions>{renderActions(item)}</ItemActions>
								</Item>
							) : onSelect ? (
								<Item asChild className={rowClass(item)}>
									<button type="button" onClick={() => onSelect(item)}>
										{itemBody(item)}
									</button>
								</Item>
							) : (
								<Item className={rowClass(item)}>{itemBody(item)}</Item>
							)}
						</li>
					))}
				</ul>
			)}
		</div>
	);
}
