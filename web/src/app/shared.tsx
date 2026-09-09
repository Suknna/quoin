import type { ReactNode } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";

export const messageOf = (error: unknown, fallback: string) =>
	error instanceof Error ? error.message : fallback;

export function ErrorMessage({ children }: { children: ReactNode }) {
	return (
		<Alert variant="destructive">
			<AlertDescription>{children}</AlertDescription>
		</Alert>
	);
}
