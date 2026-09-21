// 共享的诊断反馈面板：每个不可变诊断输出（初步分析/巡检报告/调查消息）
// 都用它记录实际结果。最新值原位可见，完整历史可展开；
// “不采纳”会让相关候选失效、已确认版本退出检索，因此单独确认并允许说明。

import { ChevronDown } from "lucide-react";
import { useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Collapsible,
	CollapsibleContent,
	CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Textarea } from "@/components/ui/textarea";
import {
	appendFeedback,
	type FeedbackTarget,
	type FeedbackTimeline,
	type FeedbackValue,
	feedbackValueLabels,
	fetchFeedback,
} from "./api";

const values: FeedbackValue[] = [
	"adopted",
	"executed",
	"verified_effective",
	"rejected",
];

function formatTime(iso: string) {
	const date = new Date(iso);
	return Number.isNaN(date.getTime()) ? iso : date.toLocaleString();
}

export function FeedbackPanel({
	target,
	suspended = false,
}: {
	target: FeedbackTarget;
	suspended?: boolean;
}) {
	const [timeline, setTimeline] = useState<FeedbackTimeline>();
	const [error, setError] = useState("");
	const [submitting, setSubmitting] = useState(false);
	const [pending, setPending] = useState<FeedbackValue | null>(null);
	const [note, setNote] = useState("");
	const [historyOpen, setHistoryOpen] = useState(false);

	const { type: targetType, id: targetId } = target;
	useEffect(() => {
		let cancelled = false;
		fetchFeedback({ type: targetType, id: targetId })
			.then((value) => {
				if (!cancelled) setTimeline(value);
			})
			.catch((reason: unknown) => {
				if (!cancelled) setError(messageOf(reason, "无法读取反馈。"));
			});
		return () => {
			cancelled = true;
		};
	}, [targetType, targetId]);

	async function record(value: FeedbackValue, noteText: string) {
		setSubmitting(true);
		setError("");
		try {
			await appendFeedback(target, value, noteText);
			setTimeline(await fetchFeedback(target));
			notify.success("已记录反馈");
		} catch (reason) {
			notify.error(reason, "无法记录反馈。");
		} finally {
			setSubmitting(false);
			setPending(null);
			setNote("");
		}
	}

	// 响应体缺 items 时按空时间线处理,不让反馈面拖垮所在页面。
	const items = timeline?.items ?? [];
	const latest = items[0];
	return (
		<section className="space-y-3">
			<div className="flex flex-wrap items-center gap-2">
				<h3 className="text-sm font-medium">实际结果</h3>
				{latest && (
					<span className="text-xs text-muted-foreground">
						最近:{feedbackValueLabels[latest.value]} ·{" "}
						{formatTime(latest.createdAt)}
					</span>
				)}
			</div>
			<div className="flex flex-wrap gap-2">
				{values.map((value) => (
					<Button
						key={value}
						size="sm"
						variant={
							latest?.value === value
								? "secondary"
								: value === "rejected"
									? "ghost"
									: "outline"
						}
						className={
							value === "rejected"
								? "text-destructive hover:text-destructive"
								: ""
						}
						disabled={suspended || submitting}
						onClick={() => setPending(value)}
					>
						{feedbackValueLabels[value]}
					</Button>
				))}
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{items.length > 0 && (
				<Collapsible open={historyOpen} onOpenChange={setHistoryOpen}>
					<CollapsibleTrigger asChild>
						<Button variant="ghost" size="sm" className="-ml-2">
							<ChevronDown
								className={`transition-transform ${historyOpen ? "rotate-180" : ""}`}
							/>
							反馈记录({items.length})
						</Button>
					</CollapsibleTrigger>
					<CollapsibleContent>
						<ul className="space-y-2 border-l-2 pl-3 text-sm">
							{items.map((event) => (
								<li key={event.id}>
									<Badge variant="outline">
										{feedbackValueLabels[event.value]}
									</Badge>{" "}
									<span className="text-xs text-muted-foreground">
										{formatTime(event.createdAt)}
									</span>
									{event.note && (
										<p className="mt-0.5 text-muted-foreground">{event.note}</p>
									)}
								</li>
							))}
						</ul>
					</CollapsibleContent>
				</Collapsible>
			)}
			<AlertDialog
				open={pending !== null}
				onOpenChange={(open) => {
					if (!open) {
						setPending(null);
						setNote("");
					}
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>
							记录反馈:{pending ? feedbackValueLabels[pending] : ""}
						</AlertDialogTitle>
						<AlertDialogDescription>
							{pending === "rejected"
								? "不采纳会让相关未确认候选失效,已确认的知识版本将永久退出检索。可附一句说明。"
								: "可附一句简短说明(可选)。"}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<Textarea
						aria-label="反馈说明"
						rows={3}
						value={note}
						onChange={(event) => setNote(event.target.value)}
						placeholder="实际情况如何?(可选)"
					/>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							disabled={submitting}
							className={
								pending === "rejected"
									? "bg-destructive text-white hover:bg-destructive/90"
									: ""
							}
							onClick={(event) => {
								event.preventDefault();
								if (pending) void record(pending, note.trim());
							}}
						>
							确认记录
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}
