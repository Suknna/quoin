import { RefreshCw, ServerOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
	Empty,
	EmptyContent,
	EmptyDescription,
	EmptyHeader,
	EmptyMedia,
	EmptyTitle,
} from "@/components/ui/empty";

export function ServiceUnavailable({
	pending,
	onRetry,
}: {
	pending: boolean;
	onRetry: () => void;
}) {
	return (
		<main className="flex min-h-svh items-center justify-center p-6">
			<Empty className="w-full max-w-lg" aria-busy={pending}>
				<EmptyHeader>
					<EmptyMedia variant="icon">
						<ServerOff aria-hidden="true" />
					</EmptyMedia>
					<EmptyTitle>暂时无法连接 Quoin</EmptyTitle>
					<EmptyDescription>
						目前无法验证你的会话，尚未进入工作区。请稍后重新连接。
					</EmptyDescription>
				</EmptyHeader>
				<EmptyContent>
					<Button onClick={onRetry} disabled={pending}>
						<RefreshCw aria-hidden="true" />
						{pending ? "正在重新连接…" : "重新连接"}
					</Button>
				</EmptyContent>
			</Empty>
		</main>
	);
}
