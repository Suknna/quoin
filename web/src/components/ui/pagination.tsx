import { ChevronLeft, ChevronRight, MoreHorizontal } from "lucide-react";
import type * as React from "react";
import { cn } from "cn";
import { buttonVariants } from "@/components/ui/button";

function Pagination({ className, ...props }: React.ComponentProps<"nav">) {
	return (
		<nav
			aria-label="分页"
			data-slot="pagination"
			className={cn("flex justify-center", className)}
			{...props}
		/>
	);
}

function PaginationContent({
	className,
	...props
}: React.ComponentProps<"ul">) {
	return (
		<ul
			data-slot="pagination-content"
			className={cn("flex flex-row items-center gap-1", className)}
			{...props}
		/>
	);
}

function PaginationItem({ ...props }: React.ComponentProps<"li">) {
	return <li data-slot="pagination-item" {...props} />;
}

type PaginationButtonProps = {
	isActive?: boolean;
	disabled?: boolean;
} & Pick<React.ComponentProps<"button">, "className" | "onClick" | "children" | "aria-label">;

/** Cursor-paged lists have no totals, so items are buttons, not links. */
function PaginationButton({
	className,
	isActive,
	disabled,
	...props
}: PaginationButtonProps) {
	return (
		<button
			type="button"
			aria-current={isActive ? "page" : undefined}
			data-slot="pagination-link"
			disabled={disabled}
			className={cn(
				buttonVariants({
					variant: isActive ? "outline" : "ghost",
					size: "icon",
				}),
				className,
			)}
			{...props}
		/>
	);
}

function PaginationPrevious({
	className,
	...props
}: PaginationButtonProps) {
	return (
		<PaginationButton
			aria-label="上一页"
			className={cn("gap-1 px-2.5", className)}
			{...props}
		>
			<ChevronLeft aria-hidden="true" />
			<span className="hidden sm:block">上一页</span>
		</PaginationButton>
	);
}

function PaginationNext({ className, ...props }: PaginationButtonProps) {
	return (
		<PaginationButton
			aria-label="下一页"
			className={cn("gap-1 px-2.5", className)}
			{...props}
		>
			<span className="hidden sm:block">下一页</span>
			<ChevronRight aria-hidden="true" />
		</PaginationButton>
	);
}

function PaginationEllipsis({
	className,
	...props
}: React.ComponentProps<"span">) {
	return (
		<span
			aria-hidden
			data-slot="pagination-ellipsis"
			className={cn("flex size-9 items-center justify-center", className)}
			{...props}
		>
			<MoreHorizontal className="size-4" />
			<span className="sr-only">更多页</span>
		</span>
	);
}

export {
	Pagination,
	PaginationContent,
	PaginationItem,
	PaginationButton,
	PaginationPrevious,
	PaginationNext,
	PaginationEllipsis,
};
