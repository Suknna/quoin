// 每日报告配置编辑器：报告时区、本地触发时间与参与计划的选择。
// 与 PlanEditor 同一分节表单模式；服务端复核全部字段。

import { LoaderCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
	Card,
	CardContent,
	CardDescription,
	CardHeader,
	CardTitle,
} from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import {
	createDailyReportConfig,
	type DailyReportConfigInput,
	dailyConfigKeyPattern,
	dailyTriggerTimePattern,
	defaultTimezone,
	getDailyReportConfig,
	updateDailyReportConfig,
} from "@/features/inspection/daily";
import {
	type InspectionPlan,
	listInspectionPlans,
} from "@/features/inspection/api";

interface FormState {
	configKey: string;
	displayName: string;
	enabled: boolean;
	timezone: string;
	triggerTime: string;
	planKeys: string[];
	reportInstructions: string;
}

function emptyForm(): FormState {
	return {
		configKey: "",
		displayName: "",
		enabled: true,
		timezone: defaultTimezone(),
		triggerTime: "08:00",
		planKeys: [],
		reportInstructions: "",
	};
}

function togglePlan(current: string[], planKey: string): string[] {
	return current.includes(planKey)
		? current.filter((key) => key !== planKey)
		: [...current, planKey];
}

/**
 * Create/edit form for one daily report config, sectioned as the operator
 * answers them: 基本信息 → 触发 → 参与计划。
 */
export function DailyConfigEditor({
	suspended,
	navigate,
	/** Present in edit mode: the config is re-read fresh so rowVersion stays current. */
	editKey,
}: {
	suspended: boolean;
	navigate: (to: string) => void;
	editKey?: string;
}) {
	const editing = Boolean(editKey);
	const [form, setForm] = useState<FormState>(emptyForm);
	const [existing, setExisting] = useState<{ rowVersion: number }>();
	const [loading, setLoading] = useState(editing);
	const [plans, setPlans] = useState<InspectionPlan[]>();
	const [plansError, setPlansError] = useState(false);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);

	function update(patch: Partial<FormState>) {
		setForm((current) => ({ ...current, ...patch }));
	}

	useEffect(() => {
		let cancelled = false;
		listInspectionPlans()
			.then((items) => {
				if (cancelled) return;
				setPlans(items);
				setPlansError(false);
			})
			.catch(() => {
				if (!cancelled) setPlansError(true);
			});
		return () => {
			cancelled = true;
		};
	}, []);
	useEffect(() => {
		if (!editKey) return;
		let cancelled = false;
		setLoading(true);
		void getDailyReportConfig(editKey)
			.then((config) => {
				if (cancelled) return;
				setExisting({ rowVersion: config.rowVersion });
				setForm({
					configKey: config.configKey,
					displayName: config.displayName,
					enabled: config.enabled,
					timezone: config.timezone,
					triggerTime: config.triggerTime,
					planKeys: [...config.planKeys],
					reportInstructions: config.reportInstructions ?? "",
				});
			})
			.catch((reason) => {
				if (!cancelled) setError(messageOf(reason, "无法读取每日报告配置。"));
			})
			.finally(() => {
				if (!cancelled) setLoading(false);
			});
		return () => {
			cancelled = true;
		};
	}, [editKey]);

	async function save() {
		if (suspended) return;
		if (!dailyConfigKeyPattern.test(form.configKey.trim())) {
			setError(
				"配置标识必须以小写字母开头，只能包含小写字母、数字和连字符（最长 63 位）。",
			);
			return;
		}
		if (!form.displayName.trim()) {
			setError("显示名称必须填写。");
			return;
		}
		if (!form.timezone.trim()) {
			setError("报告时区必须填写，例如 Asia/Shanghai。");
			return;
		}
		if (!dailyTriggerTimePattern.test(form.triggerTime)) {
			setError("触发时间必须是本地 24 小时制 HH:MM。");
			return;
		}
		if (!form.planKeys.length) {
			setError("至少选择一个参与日报的巡检计划。");
			return;
		}
		if (Array.from(form.reportInstructions).length > 4000) {
			setError("报告期望不能超过 4000 个字符。");
			return;
		}
		const payload: DailyReportConfigInput = {
			configKey: form.configKey.trim(),
			displayName: form.displayName.trim(),
			enabled: form.enabled,
			timezone: form.timezone.trim(),
			triggerTime: form.triggerTime,
			planKeys: form.planKeys,
			reportInstructions: form.reportInstructions.trim() || undefined,
		};
		setBusy(true);
		setError("");
		try {
			if (existing)
				await updateDailyReportConfig({
					...payload,
					expectedRowVersion: existing.rowVersion,
				});
			else await createDailyReportConfig(payload);
			notify.success(existing ? "已保存每日报告配置" : "已创建每日报告配置");
			navigate("/inspections/daily");
		} catch (reason) {
			setError(
				messageOf(
					reason,
					existing ? "无法更新每日报告配置。" : "无法创建每日报告配置。",
				),
			);
		} finally {
			setBusy(false);
		}
	}

	if (loading)
		return (
			<DetailSkeleton
				label="正在读取每日报告配置"
				rows={["title", "line", "line", "line"]}
			/>
		);
	return (
		<div className="space-y-6">
			<header>
				<h2 className="text-xl font-semibold">
					{editing ? "编辑每日报告配置" : "新建每日报告配置"}
				</h2>
				<p className="text-sm text-muted-foreground">
					按报告时区的每个本地日 [00:00, 次日 00:00)
					冻结最近一个已完整结束的自然日，汇总所选计划的事实并封存。
				</p>
			</header>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<FieldGroup className="gap-6">
				<Card>
					<CardHeader>
						<CardTitle>基本信息</CardTitle>
						<CardDescription>配置的标识与启停。</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="daily-config-key">配置标识</FieldLabel>
							<Input
								id="daily-config-key"
								value={form.configKey}
								disabled={editing || busy || suspended}
								onChange={(event) =>
									update({ configKey: event.target.value })
								}
								placeholder="如 mall-daily"
							/>
							<FieldDescription>创建后不可修改。</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="daily-config-name">显示名称</FieldLabel>
							<Input
								id="daily-config-name"
								value={form.displayName}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ displayName: event.target.value })
								}
								placeholder="如 商城每日巡检报告"
							/>
						</Field>
						<Field orientation="horizontal">
							<FieldLabel htmlFor="daily-config-enabled">启用配置</FieldLabel>
							<Switch
								id="daily-config-enabled"
								checked={form.enabled}
								disabled={busy || suspended}
								onCheckedChange={(checked) =>
									update({ enabled: checked === true })
								}
							/>
							<FieldDescription>
								停用后不再按日触发新报告；已生成的报告仍可读。
							</FieldDescription>
						</Field>
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>触发</CardTitle>
						<CardDescription>
							每天在报告时区的本地触发时间汇总前一完整自然日已有的巡检事实；两小时后封存，缺失结果明确记为缺口。
						</CardDescription>
					</CardHeader>
					<CardContent className="grid gap-4 md:grid-cols-2">
						<Field>
							<FieldLabel htmlFor="daily-config-timezone">
								报告时区
							</FieldLabel>
							<Input
								id="daily-config-timezone"
								value={form.timezone}
								disabled={busy || suspended}
								onChange={(event) => update({ timezone: event.target.value })}
								placeholder="Asia/Shanghai"
							/>
							<FieldDescription>
								IANA 时区名称；本地日窗口按此时区换算为 UTC 边界并冻结。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="daily-config-trigger">
								本地触发时间
							</FieldLabel>
							<Input
								id="daily-config-trigger"
								type="time"
								value={form.triggerTime}
								disabled={busy || suspended}
								onChange={(event) =>
									update({ triggerTime: event.target.value })
								}
							/>
							<FieldDescription>24 小时制 HH:MM，报告时区的本地时间。</FieldDescription>
						</Field>
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>报告期望</CardTitle>
						<CardDescription>告诉 Agent 希望看到哪些分析与建议；不会改变采证事实或扩展工具权限。</CardDescription>
					</CardHeader>
					<CardContent>
						<Field>
							<FieldLabel htmlFor="daily-config-instructions">人类期望输出（可选）</FieldLabel>
							<Textarea
								id="daily-config-instructions"
								value={form.reportInstructions}
								onChange={(event) => update({ reportInstructions: event.target.value })}
								maxLength={4000}
								disabled={busy || suspended}
								placeholder="如：按来源列出异常和缺口，引用 Run/Evidence 编号，再给出需要人工核对的下一步。"
							/>
							<FieldDescription>留空时使用默认结构：来源状态、事实变化、缺口、风险与下一步，并引用证据。</FieldDescription>
						</Field>
					</CardContent>
				</Card>
				<Card>
					<CardHeader>
						<CardTitle>参与计划</CardTitle>
						<CardDescription>
							触发时冻结所选计划的版本与接入；之后计划变更不再进入已生成的报告。
						</CardDescription>
					</CardHeader>
					<CardContent>
						{plansError ? (
							<Alert variant="destructive">
								<AlertDescription>
									读取巡检计划失败；请确认已有可参与日报的巡检计划后重试。
								</AlertDescription>
								<Button
									type="button"
									size="sm"
									variant="outline"
									className="mt-2"
									disabled={busy}
									onClick={() => {
												setPlansError(false);
												listInspectionPlans()
													.then((items) => {
												setPlans(items);
												setPlansError(false);
											})
											.catch(() => setPlansError(true));
									}}
								>
									重试读取
								</Button>
							</Alert>
						) : plans === undefined ? (
							<div
								className="grid gap-2"
								role="status"
								aria-label="正在读取巡检计划"
							>
								<Skeleton className="h-10 w-full" />
								<Skeleton className="h-10 w-full" />
							</div>
						) : plans.length === 0 ? (
							<Alert>
								<AlertDescription>
									还没有巡检计划。先在
									<Button
										variant="link"
										size="sm"
										className="h-auto px-1"
										onClick={() => navigate("/inspections/plans/new")}
									>
										巡检计划
									</Button>
									创建计划，再配置每日报告。
								</AlertDescription>
							</Alert>
						) : (
							<FieldGroup aria-label="参与计划列表" className="gap-2">
								{plans.map((plan) => {
									const checked = form.planKeys.includes(plan.planKey);
									return (
										<label
											key={plan.planKey}
											className="flex items-center gap-3 rounded-lg border p-3 text-sm hover:bg-accent/50"
										>
											<Checkbox
												aria-label={`选择计划 ${plan.displayName}`}
												checked={checked}
												disabled={busy || suspended}
												onCheckedChange={() =>
													update({ planKeys: togglePlan(form.planKeys, plan.planKey) })
												}
											/>
											<span className="min-w-0 flex-1">
												<span className="block font-medium">
													{plan.displayName}
												</span>
												<span className="block text-xs text-muted-foreground">
													{plan.planKey} · {plan.connectionName}
												</span>
											</span>
											{!plan.enabled && (
												<span className="text-xs text-warning">已停用</span>
											)}
										</label>
									);
								})}
								<FieldDescription>
									已选 {form.planKeys.length} 个；停用的计划仍可参与，日报会如实记录其缺口。
								</FieldDescription>
							</FieldGroup>
						)}
					</CardContent>
				</Card>
			</FieldGroup>
			<div className="flex justify-end gap-2">
				<Button
					variant="outline"
					onClick={() => navigate("/inspections/daily")}
					disabled={busy}
				>
					取消
				</Button>
				<Button onClick={() => void save()} disabled={busy || suspended}>
					{busy ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							保存中…
						</>
					) : (
						"保存配置"
					)}
				</Button>
			</div>
		</div>
	);
}
