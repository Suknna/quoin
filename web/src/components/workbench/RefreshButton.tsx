import { LoaderCircle } from "lucide-react";
import { Button } from "@/components/ui/button";

/** Shared refresh action with the workbench-standard spinner feedback. */
export function RefreshButton({
	loading,
	disabled,
	size,
	onClick,
}: {
	loading: boolean;
	disabled?: boolean;
	size?: "sm" | "default";
	onClick: () => void;
}) {
	return (
		<Button
			variant="outline"
			size={size}
			disabled={disabled || loading}
			onClick={onClick}
		>
			{loading ? (
				<>
					<LoaderCircle
						className="animate-spin"
						data-icon="inline-start"
						aria-hidden="true"
					/>
					刷新中…
				</>
			) : (
				"刷新"
			)}
		</Button>
	);
}
