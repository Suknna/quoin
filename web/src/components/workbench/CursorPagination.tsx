import {
	Pagination,
	PaginationButton,
	PaginationContent,
	PaginationItem,
	PaginationNext,
	PaginationPrevious,
} from "@/components/ui/pagination";

/**
 * 游标分页的统一导航（HTTP-PAGE-001 无总页数）：上一页 / 当前页码 / 下一页。
 * 只有一页时不占位。
 */
export function CursorPagination({
	page,
	hasPrev,
	hasNext,
	loading,
	onPrev,
	onNext,
	className,
}: {
	page: number;
	hasPrev: boolean;
	hasNext: boolean;
	loading: boolean;
	onPrev: () => void;
	onNext: () => void;
	className?: string;
}) {
	if (!hasPrev && !hasNext) return null;
	return (
		<Pagination className={className}>
			<PaginationContent>
				<PaginationItem>
					<PaginationPrevious
						disabled={!hasPrev || loading}
						onClick={onPrev}
					/>
				</PaginationItem>
				<PaginationItem>
					<PaginationButton isActive aria-label={`第 ${page} 页`}>
						{page}
					</PaginationButton>
				</PaginationItem>
				<PaginationItem>
					<PaginationNext
						disabled={!hasNext || loading}
						onClick={onNext}
					/>
				</PaginationItem>
			</PaginationContent>
		</Pagination>
	);
}
