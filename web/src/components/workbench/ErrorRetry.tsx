import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";

/** Inline error with the standard retry affordance used across workbench lists. */
export function ErrorRetry({
	message,
	onRetry,
	retryLabel = "重试",
}: {
	message: React.ReactNode;
	onRetry: () => void;
	retryLabel?: string;
}) {
	return (
		<Alert variant="destructive">
			<AlertDescription>
				{message}{" "}
				<Button
					variant="link"
					className="h-auto p-0 align-baseline"
					onClick={onRetry}
				>
					{retryLabel}
				</Button>
			</AlertDescription>
		</Alert>
	);
}
