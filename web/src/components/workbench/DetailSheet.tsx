import type { ReactNode } from "react";
import {
	Sheet,
	SheetContent,
	SheetDescription,
	SheetHeader,
	SheetTitle,
} from "@/components/ui/sheet";

/**
 * Shared right-hand detail drawer: list pages stay mounted underneath, the
 * detail opens over them driven by a URL query flag, and every close path
 * (Esc, overlay, close button) funnels into onClose so the caller can strip
 * that flag. Mirrors the alert detail interaction.
 */
export function DetailSheet({
	open,
	onClose,
	title,
	description,
	children,
	width = "sm:max-w-4xl",
}: {
	open: boolean;
	onClose: () => void;
	title: ReactNode;
	description?: ReactNode;
	children: ReactNode;
	/** Drawer width class; defaults to the alert-detail width. */
	width?: string;
}) {
	return (
		<Sheet
			open={open}
			onOpenChange={(next) => {
				if (!next) onClose();
			}}
		>
			<SheetContent
				side="right"
				className={`flex h-dvh w-full flex-col gap-0 overflow-hidden p-0 ${width}`}
			>
				<SheetHeader className="shrink-0 border-b p-4 pr-12">
					<SheetTitle className="text-lg">{title}</SheetTitle>
					{description && <SheetDescription>{description}</SheetDescription>}
				</SheetHeader>
				{children}
			</SheetContent>
		</Sheet>
	);
}
