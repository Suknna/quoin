// 日报详情：冻结身份 + 参与计划 + 版本列表 + 已封存事实。
// 事实与 AI 总结严格分开展示：事实缺失/缺口如实呈现，绝不被解读为健康；
// AI 总结缺席时明确标注“暂无”，不虚构结论。

import { useCallback, useEffect, useState } from "react";
import { messageOf } from "@/app/shared";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { EntityList } from "@/components/EntityList";
import { PropertyList } from "@/components/workbench/PropertyList";
import {
	type DailyCheckItem,
	type DailyReportContent,
	type DailyReportDetail,
	type DailySourceReport,
	dailyGapText,
	dailyStateBadgeClass,
	dailyStateText,
	dailyTriggerText,
	dailyWindowText,
	formatDailyTime,
	getDailyReport,
	getDailyReportVersion,
	rerunDailyReport,
	sourceStatusText,
} from "@/features/inspection/daily";

/** One contributing source's frozen outcome: identity, gap reasons, per-check facts. */
function SourceReport({ source }: { source: DailySourceReport }) {
	const gap = source.status === "gap";
	return (
		<div className="rounded-lg border p-4">
			<div className="flex flex-wrap items-center gap-2">
				<span className="text-sm font-medium">
					{source.displayName ?? source.planKey}
				</span>
				<Badge
					variant="outline"
					className={gap ? "border-destructive/50 text-destructive" : "border-success/50 text-success"}
				>
					{sourceStatusText(source.status)}
				</Badge>
				{source.missing && (
					<Badge variant="outline" className="border-destructive/50 text-destructive">
						计划不存在
					</Badge>
				)}
				{!source.enabled && (
					<Badge variant="outline">计划已停用</Badge>
				)}
				{!source.sourceEnabled && (
					<Badge variant="outline">接入已停用</Badge>
				)}
			</div>
			<p className="mt-1 text-xs text-muted-foreground">
				{[source.planKey, source.connectionName, source.pluginId, source.templateId]
					.filter(Boolean)
					.join(" · ")}
			</p>
			{source.gapReasons?.length ? (
				<Alert variant="destructive" className="mt-3">
					<AlertTitle>缺口</AlertTitle>
					<AlertDescription>
						{source.gapReasons.map(dailyGapText).join("；")}
					</AlertDescription>
				</Alert>
			) : null}
			{source.checks?.length ? (
				<ul className="mt-3 space-y-1.5" aria-label="检查项事实">
					{source.checks.map((check) => (
						<CheckItem key={`${check.runId}-${check.checkKey}`} check={check} />
					))}
				</ul>
			) : (
				<p className="mt-3 text-xs text-muted-foreground">没有已采证的检查项事实。</p>
			)}
		</div>
	);
}

function CheckItem({ check }: { check: DailyCheckItem }) {
	const gap = Boolean(check.gapReason);
	return (
		<li className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-sm">
			<span className="font-mono text-xs">{check.checkKey}</span>
			{gap ? (
				<Badge variant="outline" className="border-destructive/50 text-destructive">
					{dailyGapText(check.gapReason ?? "")}
				</Badge>
			) : (
				<Badge variant="outline" className="border-success/50 text-success">
					正常
				</Badge>
			)}
			{check.observedAt && (
				<span className="text-xs text-muted-foreground">
					观测于 {formatDailyTime(check.observedAt)}
				</span>
			)}
		</li>
	);
}

/** The frozen fact document: totals, per-source facts, and the explicit AI-summary status. */
function SealedContent({ content }: { content: DailyReportContent }) {
	const { totals } = content;
	const hasGap =
		totals.checksGap > 0 || totals.checksError > 0 || totals.sourcesGap > 0;
	return (
		<div className="space-y-4">
			<div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
				<span>
					检查通过 <span className="font-medium tabular-nums">{totals.checksOk}</span>
				</span>
				<span>
					检查缺口 <span className="font-medium tabular-nums">{totals.checksGap}</span>
				</span>
				<span>
					检查错误 <span className="font-medium tabular-nums">{totals.checksError}</span>
				</span>
				<span>
					缺口来源 <span className="font-medium tabular-nums">{totals.sourcesGap}</span>
				</span>
			</div>
			{hasGap ? (
				<Alert variant="destructive">
					<AlertTitle>本报告存在缺口</AlertTitle>
					<AlertDescription>
						缺失或未完成的数据已在下方如实列出；缺口不代表正常。
					</AlertDescription>
				</Alert>
			) : null}
			<div className="grid gap-3 lg:grid-cols-2">
				{content.sources.map((source) => (
					<SourceReport key={source.planKey} source={source} />
				))}
			</div>
			<Separator />
			<section aria-label="AI 总结">
				<h3 className="text-sm font-medium">AI 总结</h3>
				<p className="mt-1 text-sm text-muted-foreground">
					暂无 AI 总结。事实已封存，可由 Agent
					读取本报告冻结事实后另行生成，不影响以上结论。
				</p>
			</section>
		</div>
	);
}

/**
 * 日报详情页：冻结身份、参与计划、版本列表与最新封存内容。
 * 采集中（尚未封存）时明确呈现“等待采证结束”，绝不预填结论。
 */
export function DailyReportDetailView({
	configKey,
	localDate,
	suspended,
	navigate,
}: {
	configKey: string;
	localDate: string;
	suspended: boolean;
	navigate: (to: string) => void;
}) {
	const [detail, setDetail] = useState<DailyReportDetail>();
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [rerunOpen, setRerunOpen] = useState(false);
	const [versionText, setVersionText] = useState("");
	const [versionOpen, setVersionOpen] = useState(false);
	const [versionLoading, setVersionLoading] = useState(false);

	const load = useCallback(async () => {
		setLoading(true);
		setError("");
		try {
			setDetail(await getDailyReport(configKey, localDate));
		} catch (reason) {
			setError(messageOf(reason, "无法读取每日报告。"));
		} finally {
			setLoading(false);
		}
	}, [configKey, localDate]);
	useEffect(() => {
		const timer = window.setTimeout(() => void load(), 0);
		return () => window.clearTimeout(timer);
	}, [load]);

	async function openVersion(version: number) {
		setVersionOpen(true);
		setVersionLoading(true);
		setVersionText("");
		try {
			setVersionText(await getDailyReportVersion(configKey, localDate, version));
		} catch (reason) {
			setVersionText(`读取失败：${messageOf(reason, "无法读取该版本。")}`);
		} finally {
			setVersionLoading(false);
		}
	}

	async function rerun() {
		if (suspended) return;
		setBusy(true);
		try {
			await rerunDailyReport(configKey, localDate);
			setRerunOpen(false);
			await load();
		} catch (reason) {
			setError(messageOf(reason, "无法发起重分析。"));
			setRerunOpen(false);
		} finally {
			setBusy(false);
		}
	}

	if (loading)
		return (
			<div
				className="space-y-4"
				role="status"
				aria-label="正在读取每日报告"
			>
				<Skeleton className="h-8 w-1/3" />
				<Skeleton className="h-24 w-full" />
				<Skeleton className="h-40 w-full" />
			</div>
		);
	if (!detail)
		return (
			<Alert variant="destructive">
				<AlertTitle>读取失败</AlertTitle>
				<AlertDescription>{error || "未找到该每日报告。"}</AlertDescription>
				<div className="mt-3 flex gap-2">
					<Button size="sm" variant="outline" onClick={() => void load()}>
						重试
					</Button>
					<Button
						size="sm"
						variant="outline"
						onClick={() => navigate("/inspections/daily")}
					>
						返回每日报告
					</Button>
				</div>
			</Alert>
		);
	const collecting = detail.state === "Collecting";
	return (
		<div className="space-y-6">
			<section className="space-y-3">
				<div className="flex flex-wrap items-center gap-2">
					<h2 className="text-xl font-semibold">
						{detail.configKey} · {detail.localDate}
					</h2>
					<Badge
						variant="outline"
						className={dailyStateBadgeClass(detail.state)}
					>
						{dailyStateText[detail.state]}
					</Badge>
					<Badge variant="outline">
						{dailyTriggerText[detail.triggerKind]}
					</Badge>
					<div className="ml-auto">
						<Button
							size="sm"
							variant="outline"
							disabled={suspended || busy}
							onClick={() => setRerunOpen(true)}
						>
							重新分析
						</Button>
					</div>
				</div>
				<PropertyList
					layout="grid-2"
					entries={[
						{ label: "报告时区", value: detail.timezone },
						{ label: "冻结窗口 (UTC)", value: dailyWindowText(detail.windowStartUtc, detail.windowEndUtc) },
						{ label: "采证截止", value: formatDailyTime(detail.cutoffAt) },
						{ label: "封存时间", value: formatDailyTime(detail.sealedAt) },
						{ label: "最新版本", value: detail.latestVersion || "尚未封存" },
						{ label: "创建时间", value: formatDailyTime(detail.createdAt) },
					]}
				/>
			</section>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{collecting ? (
				<Alert>
					<AlertTitle>正在采集中</AlertTitle>
					<AlertDescription>
						触发时间与参与计划已冻结；采证截止前完成的结果会进入封存，
						未完成的将记录为缺口。当前没有可展示的结论。
					</AlertDescription>
				</Alert>
			) : null}
			<section className="space-y-3">
				<h3 className="text-sm font-medium">参与计划（触发时冻结）</h3>
				<EntityList
					items={(detail.contributions ?? []).map((contribution) => ({
						id: contribution.planKey,
						title: contribution.displayName ?? contribution.planKey,
						subtitle: [
							contribution.planKey,
							contribution.connectionName,
							contribution.pluginId,
							contribution.templateId,
							contribution.templateVersion
								? `@${contribution.templateVersion}`
								: undefined,
						]
							.filter(Boolean)
							.join(" · "),
						badge: contribution.missing
							? {
									text: "计划不存在",
									variant: "outline" as const,
									className: "border-destructive/50 text-destructive",
								}
							: !contribution.enabled
								? { text: "计划已停用", variant: "outline" as const }
								: !contribution.sourceEnabled
									? { text: "接入已停用", variant: "outline" as const }
									: undefined,
					}))}
					columns={["title", "subtitle", "status"]}
					emptyTitle="没有参与计划"
				/>
			</section>
			{detail.latest ? (
				<section className="space-y-3">
					<h3 className="text-sm font-medium">封存事实</h3>
					<SealedContent content={detail.latest} />
				</section>
			) : null}
			<section className="space-y-3">
				<h3 className="text-sm font-medium">版本</h3>
				<EntityList
					items={(detail.versions ?? []).map((version) => ({
						id: String(version.version),
						title: `版本 ${version.version}`,
						time: formatDailyTime(version.createdAt),
					}))}
					columns={["title", "time"]}
					onSelect={(item) => void openVersion(Number(item.id))}
					emptyTitle="还没有已封存版本"
				/>
			</section>
			<Dialog open={rerunOpen} onOpenChange={setRerunOpen}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>重新分析这份日报？</DialogTitle>
						<DialogDescription>
							重分析从同一冻结窗口追加一个新版本；旧版本与事实保持可读、可比较。
						</DialogDescription>
					</DialogHeader>
					<DialogFooter>
						<Button variant="outline" disabled={busy} onClick={() => setRerunOpen(false)}>
							取消
						</Button>
						<Button disabled={busy || suspended} onClick={() => void rerun()}>
							{busy ? "发起中…" : "发起重分析"}
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
			<Dialog open={versionOpen} onOpenChange={setVersionOpen}>
				<DialogContent className="max-w-2xl">
					<DialogHeader>
						<DialogTitle>版本内容（原始 JSON）</DialogTitle>
						<DialogDescription>
							已封存的不可变文档；结构与报告页面的解读一致。
						</DialogDescription>
					</DialogHeader>
					{versionLoading ? (
						<div role="status" aria-label="正在读取版本内容" className="space-y-2">
							<Skeleton className="h-4 w-full" />
							<Skeleton className="h-4 w-4/5" />
							<Skeleton className="h-4 w-3/5" />
						</div>
					) : (
						<pre className="max-h-96 overflow-auto rounded-lg border bg-muted/40 p-3 font-mono text-xs">
							{versionText}
						</pre>
					)}
				</DialogContent>
			</Dialog>
		</div>
	);
}
