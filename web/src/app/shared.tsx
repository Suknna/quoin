import type { ReactNode } from "react";
import { toast } from "sonner";
import { Alert, AlertDescription } from "@/components/ui/alert";

export const messageOf = (error: unknown, fallback: string) =>
	error instanceof Error ? error.message : fallback;

/** 全局操作反馈的唯一入口：写操作成功/失败、告警提示都走这里，
 *  避免各调用点重复组合 toast 与 messageOf，也便于日后替换实现。 */
export const notify = {
	success: (message: string) => toast.success(message),
	error: (reason: unknown, fallback: string) =>
		toast.error(messageOf(reason, fallback)),
	warning: (message: string) => toast.warning(message),
	info: (message: string) => toast.info(message),
};

export function ErrorMessage({ children }: { children: ReactNode }) {
	return (
		<Alert variant="destructive">
			<AlertDescription>{children}</AlertDescription>
		</Alert>
	);
}
