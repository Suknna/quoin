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
	size = "default",
}: {
	open: boolean;
	onClose: () => void;
	title: ReactNode;
	description?: ReactNode;
	children: ReactNode;
	/**
	 * 两档语义尺寸，避免各页面自由传宽度导致抽屉大小不一：
	 * default（sm:max-w-4xl）用于富内容详情（时间线、表格、长表单，源自告警详情）；
	 * narrow（sm:max-w-xl）用于简单记录视图（概要属性 + 少量操作，如审计事件、用户）。
	 */
	size?: "default" | "narrow";
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
				className={`flex h-dvh w-full flex-col gap-0 overflow-hidden p-0 ${
					size === "narrow" ? "sm:max-w-xl" : "sm:max-w-4xl"
				}`}
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
