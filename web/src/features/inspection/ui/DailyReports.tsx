// 每日报告中枢：报告配置（时区/触发时间/参与计划）+ 跨来源日报列表 + 人工补跑。
// 缺口与未封存状态如实呈现，绝不预填健康结论。

import { useCallback, useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { EntityList } from "@/components/EntityList";
import {
	type DailyReportConfig,
	type DailyReportSummary,
	backfillDailyReport,
	dailyLocalDatePattern,
	dailyStateBadgeClass,
	dailyStateText,
	dailyTriggerText,
	formatDailyTime,
	listDailyReportConfigs,
	listDailyReports,
} from "@/features/inspection/daily";

/** 补跑漏过的整日：窗口仍是所选原日期，服务端绝不偷换为当前日期。 */
function BackfillDialog({
	configs,
	suspended,
	busy,
	error,
	onBusyChange,
	onError,
	onOpenChange,
	onBackfilled,
}: {
	configs: DailyReportConfig[];
	suspended: boolean;
	busy: boolean;
	error: string;
	onBusyChange: (busy: boolean) => void;
	onError: (message: string) => void;
	onOpenChange: (open: boolean) => void;
	onBackfilled: (report: DailyReportSummary) => void;
}) {
	const [configKey, setConfigKey] = useState("");
	const [localDate, setLocalDate] = useState("");
	const selected = configs.find((config) => config.configKey === configKey);
	return (
		<DialogContent>
			<DialogHeader>
				<DialogTitle>补跑每日报告</DialogTitle>
				<DialogDescription>
					为漏过的整日手动创建报告；时间窗固定为所选原日期，不会替换为今天。
				</DialogDescription>
			</DialogHeader>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<FieldGroup>
				<Field>
					<FieldLabel htmlFor="backfill-config">报告配置</FieldLabel>
					<Select value={configKey} onValueChange={setConfigKey} disabled={busy || suspended}>
						<SelectTrigger id="backfill-config" aria-label="报告配置选择">
							<SelectValue placeholder="选择报告配置" />
						</SelectTrigger>
						<SelectContent>
							<SelectGroup>
								{configs.map((config) => (
									<SelectItem key={config.configKey} value={config.configKey}>
										{config.displayName} · {config.timezone}
									</SelectItem>
								))}
							</SelectGroup>
						</SelectContent>
					</Select>
				</Field>
				<Field>
					<FieldLabel htmlFor="backfill-date">报告日期</FieldLabel>
					<Input
						id="backfill-date"
						type="date"
						value={localDate}
						disabled={busy || suspended}
						onChange={(event) => setLocalDate(event.target.value)}
					/>
					<FieldDescription>
						该日期在报告时区的本地日 [00:00, 次日 00:00)。
					</FieldDescription>
				</Field>
			</FieldGroup>
			<DialogFooter>
				<Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
					取消
				</Button>
				<Button
					disabled={!configKey || !dailyLocalDatePattern.test(localDate) || busy || suspended}
					onClick={async () => {
						if (!configKey || !dailyLocalDatePattern.test(localDate)) return;
						onBusyChange(true);
						onError("");
						try {
							const report = await backfillDailyReport(configKey, localDate);
							notify.success("已发起补跑，报告正在采集中。");
							onOpenChange(false);
							onBackfilled(report);
						} catch (reason) {
							onError(messageOf(reason, "无法补跑每日报告。"));
						} finally {
							onBusyChange(false);
						}
					}}
				>
					{busy ? "发起中…" : "发起补跑"}
				</Button>
			</DialogFooter>
			{selected && (
				<p className="text-xs text-muted-foreground">
					配置 {selected.configKey} · 时区 {selected.timezone} · 每天 {selected.triggerTime} 触发
				</p>
			)}
		</DialogContent>
	);
}

/**
 * 每日报告中枢页：上半是管理员配置（时区/本地触发/参与计划），下半是
 * 跨来源冻结报告列表；补跑入口固定在列表标题行。
 */
export function DailyReportsOverview({
	suspended,
	navigate,
}: {
	suspended: boolean;
	navigate: (to: string) => void;
}) {
	const [configs, setConfigs] = useState<DailyReportConfig[]>([]);
	const [reports, setReports] = useState<DailyReportSummary[]>([]);
	const [configName, setConfigName] = useState<Record<string, string>>({});
	const [loaded, setLoaded] = useState(false);
	const [error, setError] = useState("");
	const [backfillOpen, setBackfillOpen] = useState(false);
	const [backfillBusy, setBackfillBusy] = useState(false);
	const [backfillError, setBackfillError] = useState("");

	const load = useCallback(async () => {
		setError("");
		const results = await Promise.allSettled([
			listDailyReportConfigs(),
			listDailyReports({ limit: 50 }),
		]);
		const configResult = results[0];
		const reportResult = results[1];
		const failures: string[] = [];
		if (configResult.status === "fulfilled") {
			setConfigs(configResult.value);
			setConfigName(
				Object.fromEntries(
					configResult.value.map((config) => [config.configKey, config.displayName]),
				),
			);
		} else {
			failures.push(messageOf(configResult.reason, "无法读取报告配置。"));
		}
		if (reportResult.status === "fulfilled") setReports(reportResult.value);
		else failures.push(messageOf(reportResult.reason, "无法读取日报列表。"));
		if (failures.length) setError(failures.join(" "));
		setLoaded(true);
	}, []);
	useEffect(() => {
		const timer = window.setTimeout(() => void load(), 0);
		return () => window.clearTimeout(timer);
	}, [load]);

	const configDisplayName = (configKey: string) =>
		configName[configKey] ?? configKey;
	return (
		<div className="space-y-6">
			{error && (
				<Alert variant="destructive">
					<AlertTitle>读取失败</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
					<Button size="sm" variant="outline" className="mt-2" onClick={() => void load()}>
						重试
					</Button>
				</Alert>
			)}
			<section className="space-y-3">
				<div className="flex flex-wrap items-start justify-between gap-2">
					<div className="space-y-1">
						<h2 className="text-xl font-semibold">报告配置</h2>
						<p className="text-sm text-muted-foreground">
							配置报告时区、每天本地触发时间与参与计划；触发时冻结计划版本与时间窗。
						</p>
					</div>
					<Button
						size="sm"
						disabled={suspended}
						onClick={() => navigate("/inspections/daily/new")}
					>
						新建配置
					</Button>
				</div>
				<EntityList
					items={configs.map((config) => ({
						id: config.configKey,
						title: config.displayName,
						subtitle: [
							config.configKey,
							`时区 ${config.timezone}`,
							`每天 ${config.triggerTime}`,
							`${config.planKeys.length} 个计划`,
						].join(" · "),
						badge: {
							text: config.enabled ? "已启用" : "已停用",
							variant: config.enabled ? ("secondary" as const) : ("outline" as const),
						},
						config,
					}))}
					columns={["title", "subtitle", "status", "actions"]}
					onSelect={(item) =>
						navigate(
							`/inspections/daily/${encodeURIComponent(item.config.configKey)}/edit`,
						)
					}
					renderActions={(item) => (
						<Button
							variant="outline"
							size="sm"
							aria-label={`编辑 ${item.config.displayName}`}
							disabled={suspended}
							onClick={() =>
								navigate(
									`/inspections/daily/${encodeURIComponent(item.config.configKey)}/edit`,
								)
							}
						>
							编辑
						</Button>
					)}
					loading={!loaded}
					loadingLabel="正在读取报告配置"
					emptyTitle="还没有报告配置"
					emptyDescription="使用“新建配置”设置报告时区、触发时间与参与计划。"
				/>
			</section>
			<section className="space-y-3">
				<div className="flex flex-wrap items-start justify-between gap-2">
					<div className="space-y-1">
						<h2 className="text-xl font-semibold">日报列表</h2>
						<p className="text-sm text-muted-foreground">
							按本地日期倒序；封存后的报告不可改写，重分析只会追加新版本。
						</p>
					</div>
					<Button
						variant="outline"
						size="sm"
						disabled={suspended || !configs.length}
						onClick={() => {
							setBackfillError("");
							setBackfillOpen(true);
						}}
					>
						补跑日报
					</Button>
				</div>
				<EntityList
					items={reports.map((report) => ({
						id: report.id,
						title: `${report.localDate} · ${configDisplayName(report.configKey)}`,
						subtitle: [
							dailyTriggerText[report.triggerKind],
							`时区 ${report.timezone}`,
							`版本 ${report.latestVersion || "—"}`,
						].join(" · "),
						badge: {
							text: dailyStateText[report.state],
							variant: "outline" as const,
							className: dailyStateBadgeClass(report.state),
						},
						time: report.sealedAt
							? `封存于 ${formatDailyTime(report.sealedAt)}`
							: "等待封存",
						report,
					}))}
					columns={["title", "subtitle", "status", "time"]}
					onSelect={(item) =>
						navigate(
							`/inspections/daily/${encodeURIComponent(item.report.configKey)}/${encodeURIComponent(item.report.localDate)}`,
						)
					}
					loading={!loaded}
					loadingLabel="正在读取日报列表"
					emptyTitle="还没有日报"
					emptyDescription="配置触发后每天自动生成；也可用“补跑日报”手动创建。"
				/>
			</section>
			<Dialog open={backfillOpen} onOpenChange={setBackfillOpen}>
				<BackfillDialog
					configs={configs}
					suspended={suspended}
					busy={backfillBusy}
					error={backfillError}
					onBusyChange={setBackfillBusy}
					onError={setBackfillError}
					onOpenChange={setBackfillOpen}
					onBackfilled={(report) => {
						setReports((current) => [report, ...current]);
						void navigate(
							`/inspections/daily/${encodeURIComponent(report.configKey)}/${encodeURIComponent(report.localDate)}`,
						);
					}}
				/>
			</Dialog>
		</div>
	);
}
