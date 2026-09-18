import { LoaderCircle } from "lucide-react";
import { Button } from "@/components/ui/button";

/** One shared "load more" affordance for every paged list in the workbench. */
export function LoadMoreButton({
	loading,
	hasMore,
	onLoadMore,
	children = "加载更多",
	className,
}: {
	loading: boolean;
	hasMore: boolean;
	onLoadMore: () => void;
	children?: React.ReactNode;
	className?: string;
}) {
	if (!hasMore) return null;
	return (
		<Button
			variant="outline"
			disabled={loading}
			onClick={onLoadMore}
			className={className}
		>
			{loading ? (
				<>
					<LoaderCircle
						className="animate-spin"
						data-icon="inline-start"
						aria-hidden="true"
					/>
					正在加载…
				</>
			) : (
				children
			)}
		</Button>
	);
}
