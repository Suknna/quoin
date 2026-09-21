// 富化规则管理页（ADR-0012）：设置-平台组下的管理员面。复用业务视图管理页
// 的表格/表单模式——列表 + 抽屉表单；规则 key 退役不复用（停用 = enabled=0）。

import { LoaderCircle, Plus, Sparkles } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { messageOf, notify } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import {
	type EnrichmentRule,
	createEnrichmentRule,
	listEnrichmentRules,
	parseConditionLines,
	parseOutputLines,
	updateEnrichmentRule,
} from "./api";

interface RuleDraft {
	ruleKey: string
	displayName: string
	description: string
	enabled: boolean
	priority: string
	labelConditions: string
	alertSourceKeys: string
	outputs: string
}

function draftOf(rule: EnrichmentRule): RuleDraft {
	return {
		ruleKey: rule.ruleKey,
		displayName: rule.displayName,
		description: rule.description,
		enabled: rule.enabled,
		priority: String(rule.priority),
		labelConditions: Object.entries(rule.labelConditions)
			.map(([key, value]) => `${key}=${value}`)
			.join("\n"),
		alertSourceKeys:
			rule.alertSourceKeys.length > 0 ? rule.alertSourceKeys.join(",") : "",
		outputs: Object.entries(rule.outputs)
			.map(([key, value]) => `${key}=${value}`)
			.join("\n"),
	}
}

function emptyDraft(): RuleDraft {
	return {
		ruleKey: "",
		displayName: "",
		description: "",
		enabled: true,
		priority: "100",
		labelConditions: "",
		alertSourceKeys: "",
		outputs: "",
	}
}

export function EnrichmentRulesPage({ suspended }: { suspended: boolean }) {
	const [rules, setRules] = useState<EnrichmentRule[]>([])
	const [loaded, setLoaded] = useState(false)
	const [error, setError] = useState("")
	const [editing, setEditing] = useState<EnrichmentRule | null>(null)
	const [creating, setCreating] = useState(false)

	const load = useCallback(async () => {
		try {
			setRules(await listEnrichmentRules())
			setError("")
		} catch (reason) {
			setError(messageOf(reason, "无法读取富化规则。"))
		} finally {
			setLoaded(true)
		}
	}, [])
	useEffect(() => {
		const timer = setTimeout(() => void load(), 0)
		return () => clearTimeout(timer)
	}, [load])

	const editor = creating ? (
		<RuleEditor
			suspended={suspended}
			onDone={() => {
				setCreating(false)
				void load()
			}}
			onCancel={() => setCreating(false)}
		/>
	) : editing ? (
		<RuleEditor
			suspended={suspended}
			rule={editing}
			onDone={() => {
				setEditing(null)
				void load()
			}}
			onCancel={() => setEditing(null)}
		/>
	) : null

	return (
		<div className="space-y-4">
			<div className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h1 className="text-2xl font-semibold tracking-tight">富化规则</h1>
					<p className="mt-1 text-sm text-muted-foreground">
						命中即叠加 outputs 的声明式富化配置：按 priority 升序叠加、后命中不覆盖已写字段；求值结果在告警首观测时冻结。
					</p>
				</div>
				<Button
					disabled={suspended || creating}
					onClick={() => {
						setEditing(null)
						setCreating(true)
					}}
				>
					<Plus data-icon="inline-start" />
					新建
				</Button>
			</div>
			{error ? (
				<Alert variant="destructive">
					<AlertTitle>无法读取富化规则</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			) : (
				<EntityList
					items={rules.map((rule) => ({
						id: rule.ruleKey,
						title: rule.displayName,
						subtitle: rule.ruleKey,
						badge: {
							text: rule.enabled ? `优先级 ${rule.priority}` : "已停用",
							variant: rule.enabled ? "default" : "secondary",
						},
						rule,
					}))}
					columns={["title", "subtitle", "status"]}
					selectedId={editing?.ruleKey}
					onSelect={(item) => {
						setCreating(false)
						setEditing(item.rule)
					}}
					loading={!loaded && !error}
					loadingLabel="正在读取富化规则"
					emptyTitle="尚无富化规则"
					emptyDescription="点击右上角的“新建”创建第一条富化规则；空标签条件的全局规则会命中一切告警。"
				/>
			)}
			{editor && (
				<Sheet open onOpenChange={(open) => {
					if (!open) {
						setCreating(false)
						setEditing(null)
					}
				}}>
					<SheetContent side="right" className="w-full overflow-y-auto sm:max-w-xl">
						<SheetHeader>
							<SheetTitle>{creating ? "新建富化规则" : editing?.displayName}</SheetTitle>
							<SheetDescription>
								{creating
									? "规则 key 创建后不可改写；停用（enabled=0）即退役形态，key 不复用。"
									: `规则标识 ${editing?.ruleKey}；更新不回写任何已冻结的首观测富化文档。`}
							</SheetDescription>
						</SheetHeader>
						{editor}
					</SheetContent>
				</Sheet>
			)}
		</div>
	)
}

function RuleEditor({
	suspended,
	rule,
	onDone,
	onCancel,
}: {
	suspended: boolean
	rule?: EnrichmentRule
	onDone: () => void
	onCancel: () => void
}) {
	const [draft, setDraft] = useState<RuleDraft>(() =>
		rule ? draftOf(rule) : emptyDraft(),
	)
	const [busy, setBusy] = useState(false)
	const [error, setError] = useState("")

	const save = async () => {
		if (!draft.displayName.trim()) return setError("显示名称不能为空。")
		if (!rule && !/^[a-z][a-z0-9-]{0,62}$/.test(draft.ruleKey)) {
			return setError("规则 key 必须匹配 ^[a-z][a-z0-9-]{0,62}$。")
		}
		const { conditions, error: conditionError } = parseConditionLines(
			draft.labelConditions,
		)
		if (conditionError) return setError(conditionError)
		const { outputs, error: outputError } = parseOutputLines(draft.outputs)
		if (outputError) return setError(outputError)
		if (Object.keys(outputs).length === 0) {
			return setError("至少需要一个富化字段（key=value）。")
		}
		const priority = Number(draft.priority) || 100
		const alertSourceKeys = draft.alertSourceKeys
			.split(",")
			.map((key) => key.trim())
			.filter(Boolean)
		setBusy(true)
		setError("")
		try {
			if (rule) {
				await updateEnrichmentRule(
					rule.ruleKey,
					{
						displayName: draft.displayName.trim(),
						description: draft.description,
						enabled: draft.enabled,
						labelConditions: conditions,
						alertSourceKeys,
						outputs,
						priority,
					},
					rule.rowVersion,
				)
				notify.success("富化规则已更新")
			} else {
				await createEnrichmentRule({
					ruleKey: draft.ruleKey,
					displayName: draft.displayName.trim(),
					description: draft.description,
					enabled: draft.enabled,
					labelConditions: conditions,
					alertSourceKeys,
					outputs,
					priority,
				})
				notify.success("富化规则已创建")
			}
			onDone()
		} catch (reason) {
			setError(messageOf(reason, "无法保存富化规则。"))
		} finally {
			setBusy(false)
		}
	}

	return (
		<div className="flex flex-col gap-4 px-4 py-4">
			<FieldGroup>
				<Field>
					<FieldLabel htmlFor="rule-key">规则 key</FieldLabel>
					<Input
						id="rule-key"
						value={draft.ruleKey}
						onChange={(event) =>
							setDraft({ ...draft, ruleKey: event.target.value })
						}
						disabled={suspended || busy || Boolean(rule)}
						placeholder="payments-tier"
						aria-readonly={Boolean(rule)}
					/>
					<FieldDescription>
						创建后不可改写；退役 = 停用，key 不复用。
					</FieldDescription>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-name">显示名称</FieldLabel>
					<Input
						id="rule-name"
						value={draft.displayName}
						onChange={(event) =>
							setDraft({ ...draft, displayName: event.target.value })
						}
						disabled={suspended || busy}
						placeholder="结算服务富化"
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-description">说明</FieldLabel>
					<Input
						id="rule-description"
						value={draft.description}
						onChange={(event) =>
							setDraft({ ...draft, description: event.target.value })
						}
						disabled={suspended || busy}
						placeholder="按 team/tier 标注结算域告警"
					/>
				</Field>
				<Field>
					<FieldLabel>状态</FieldLabel>
					<Select
						value={draft.enabled ? "enabled" : "disabled"}
						onValueChange={(value) =>
							setDraft({ ...draft, enabled: value === "enabled" })
						}
						disabled={suspended || busy}
					>
						<SelectTrigger aria-label="规则状态">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value="enabled">启用</SelectItem>
							<SelectItem value="disabled">停用（退役）</SelectItem>
						</SelectContent>
					</Select>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-priority">叠加优先级</FieldLabel>
					<Input
						id="rule-priority"
						type="number"
						min={1}
						value={draft.priority}
						onChange={(event) =>
							setDraft({ ...draft, priority: event.target.value })
						}
						disabled={suspended || busy}
					/>
					<FieldDescription>
						按升序先生效；同窗口内后命中的规则不覆盖已写字段。
					</FieldDescription>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-conditions">标签条件（每行 key=value）</FieldLabel>
					<Textarea
						id="rule-conditions"
						value={draft.labelConditions}
						onChange={(event) =>
							setDraft({ ...draft, labelConditions: event.target.value })
						}
						disabled={suspended || busy}
						placeholder={"service=checkout"}
						className="min-h-20 font-mono text-xs"
					/>
					<FieldDescription>
						精确 label=value 匹配；留空 = 全局规则，命中一切告警。
					</FieldDescription>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-sources">告警源 key（逗号分隔）</FieldLabel>
					<Input
						id="rule-sources"
						value={draft.alertSourceKeys}
						onChange={(event) =>
							setDraft({ ...draft, alertSourceKeys: event.target.value })
						}
						disabled={suspended || busy}
						placeholder="留空 = 不限告警源"
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor="rule-outputs">富化字段（每行 key=value）</FieldLabel>
					<Textarea
						id="rule-outputs"
						value={draft.outputs}
						onChange={(event) =>
							setDraft({ ...draft, outputs: event.target.value })
						}
						disabled={suspended || busy}
						placeholder={"team=payments\ntier=gold"}
						className="min-h-20 font-mono text-xs"
					/>
					<FieldDescription>
						命中时叠加的富化字段；至少一行，值为非空字符串。
					</FieldDescription>
				</Field>
			</FieldGroup>
			<Separator />
			{error && (
				<Alert variant="destructive" role="alert">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<div className="flex justify-end gap-2">
				<Button variant="outline" onClick={onCancel} disabled={busy}>
					取消
				</Button>
				<Button onClick={() => void save()} disabled={suspended || busy}>
					{busy ? (
						<LoaderCircle className="animate-spin" data-icon="inline-start" />
					) : (
						<Sparkles data-icon="inline-start" />
					)}
					{rule ? "保存更新" : "创建规则"}
				</Button>
			</div>
		</div>
	)
}
