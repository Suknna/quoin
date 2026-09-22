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
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import {
	api,
	type CandidateDetail,
	batchStateLabels,
	CommandConflictError,
	candidateSourceLabels,
	candidateStateLabels,
} from "../api";

/** 候选编辑层:只编辑标题、正文、适用范围;原始建议只读对照。 */
export function CandidateEditor({
	id,
	suspended,
	navigate,
}: {
	id: string;
	suspended: boolean;
	navigate: (route: string) => void;
}) {
	const [candidate, setCandidate] = useState<CandidateDetail>();
	const [title, setTitle] = useState("");
	const [body, setBody] = useState("");
	const [scopeText, setScopeText] = useState("");
	const [scopeError, setScopeError] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const [confirming, setConfirming] = useState(false);
	const [excluding, setExcluding] = useState(false);
	const [sourceOpen, setSourceOpen] = useState(false);

	useEffect(() => {
		let cancelled = false;
		api
			.getCandidate(id)
			.then((value) => {
				if (cancelled) return;
				setCandidate(value);
				setTitle(value.draftTitle ?? value.originalSuggestion.title);
				setBody(value.draftBody ?? value.originalSuggestion.body);
				setScopeText(
					value.draftScope ? JSON.stringify(value.draftScope, null, 2) : "",
				);
			})
			.catch((reason: unknown) => {
				if (!cancelled)
					setError(messageOf(reason, "暂时无法完成操作,请重试。"));
			});
		return () => {
			cancelled = true;
		};
	}, [id]);
	useEffect(() => {
		if (suspended) setBusy(false);
	}, [suspended]);

	if (!candidate)
		return error ? (
			<Alert variant="destructive">
				<AlertDescription>{error}</AlertDescription>
			</Alert>
		) : (
			<DetailSkeleton
				label="正在读取候选"
				rows={["line", "title", "line", "line", "line", "card"]}
			/>
		);
	const current = candidate;
	// 批次围栏与写路径同一事实：终态批次（取消/完成）的候选即使仍处于
	// 待确认状态也已冻结，不能再编辑或确认——只读查看并明确说明原因。
	const frozenByBatch =
		current.state === "AwaitingConfirmation" &&
		current.batchState !== undefined &&
		current.batchState !== "AwaitingConfirmation";
	const editable = current.state === "AwaitingConfirmation" && !frozenByBatch;

	/** 适用范围是可选 JSON 对象;非空且非法时阻止保存,不静默丢弃用户输入。 */
	function parseScope(): Record<string, unknown> | undefined | null {
		const text = scopeText.trim();
		if (!text) return undefined;
		try {
			const value = JSON.parse(text) as unknown;
			if (typeof value !== "object" || value === null || Array.isArray(value))
				throw new Error("not an object");
			return value as Record<string, unknown>;
		} catch {
			setScopeError('适用范围需要是 JSON 对象,例如 {"service": "checkout"}。');
			return null;
		}
	}

	async function save() {
		const scope = parseScope();
		if (scope === null) return;
		setBusy(true);
		setError("");
		setScopeError("");
		try {
			const next = await api.editDraft(id, current.draftRevision, {
				title,
				body,
				...(scope !== undefined ? { scope } : {}),
			});
			setCandidate((value) => (value ? { ...value, ...next } : value));
			notify.success("已保存");
		} catch (reason) {
			setError(
				reason instanceof CommandConflictError
					? `${reason.message}你的输入已保留,基于最新内容修改后再保存。`
					: messageOf(reason, "暂时无法完成操作,请重试。"),
			);
		} finally {
			setBusy(false);
		}
	}
	async function confirm() {
		setBusy(true);
		try {
			const next = await api.confirm(id, current.draftRevision);
			notify.success("已确认为知识");
			if (next.confirmedKnowledgeId)
				navigate(`/knowledge/items/${next.confirmedKnowledgeId}`);
			else navigate("/knowledge");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作,请重试。"));
			setConfirming(false);
		} finally {
			setBusy(false);
		}
	}
	async function exclude() {
		setBusy(true);
		try {
			await api.exclude(id, current.rowVersion);
			notify.success("已排除");
			navigate("/knowledge/candidates");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作,请重试。"));
			setExcluding(false);
		} finally {
			setBusy(false);
		}
	}

	return (
		<section className="space-y-6">
			<header className="space-y-2">
				<div className="flex items-center gap-2">
					<Badge variant={editable ? "default" : "secondary"}>
						{candidateStateLabels[current.state]}
					</Badge>
					<span className="text-sm text-muted-foreground">
						来源:{candidateSourceLabels[current.sourceType]}
						{current.targetKnowledgeId ? " · 修订已有知识" : ""}
					</span>
				</div>
				<h1 className="text-xl font-semibold">
					{editable ? "编辑知识候选" : "查看知识候选"}
				</h1>
			</header>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{frozenByBatch && current.batchState && (
				<Alert>
					<AlertDescription>
						{`所属导入批次${batchStateLabels[current.batchState]},此候选已冻结,仅可查看,不能再编辑或确认。`}
					</AlertDescription>
				</Alert>
			)}
			<Field>
				<FieldLabel htmlFor="candidate-title">标题</FieldLabel>
				<Input
					id="candidate-title"
					value={title}
					onChange={(event) => setTitle(event.target.value)}
					disabled={!editable || busy || suspended}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="candidate-body">正文</FieldLabel>
				<Textarea
					id="candidate-body"
					value={body}
					onChange={(event) => setBody(event.target.value)}
					disabled={!editable || busy || suspended}
					rows={12}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="candidate-scope">适用范围(可选)</FieldLabel>
				<Textarea
					id="candidate-scope"
					value={scopeText}
					onChange={(event) => {
						setScopeText(event.target.value);
						setScopeError("");
					}}
					disabled={!editable || busy || suspended}
					rows={3}
					className="font-mono text-xs"
					placeholder='{"service": "checkout"}'
				/>
				{scopeError ? (
					<FieldDescription className="text-destructive">
						{scopeError}
					</FieldDescription>
				) : (
					<FieldDescription>
						用 JSON 对象描述这条知识适用的服务、场景等;留空表示通用。
					</FieldDescription>
				)}
			</Field>
			<Collapsible open={sourceOpen} onOpenChange={setSourceOpen}>
				<CollapsibleTrigger asChild>
					<Button variant="ghost" size="sm" className="-ml-2">
						<ChevronDown
							className={`transition-transform ${sourceOpen ? "rotate-180" : ""}`}
						/>
						查看 AI 原始建议
					</Button>
				</CollapsibleTrigger>
				<CollapsibleContent>
					<div className="space-y-2 rounded-md bg-muted/50 p-4">
						<p className="text-sm font-medium">
							{current.originalSuggestion.title}
						</p>
						<p className="text-sm leading-6 whitespace-pre-wrap text-muted-foreground">
							{current.originalSuggestion.body}
						</p>
					</div>
				</CollapsibleContent>
			</Collapsible>
			{editable && (
				<div className="flex gap-2 border-t pt-4">
					<Button
						variant="outline"
						disabled={busy || suspended}
						onClick={() => void save()}
					>
						保存草稿
					</Button>
					<Button
						disabled={busy || suspended}
						onClick={() => setConfirming(true)}
					>
						确认知识
					</Button>
					<Button
						variant="ghost"
						className="ml-auto text-destructive hover:text-destructive"
						disabled={busy || suspended}
						onClick={() => setExcluding(true)}
					>
						排除
					</Button>
				</div>
			)}
			<AlertDialog open={confirming} onOpenChange={setConfirming}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>确认发布此知识?</AlertDialogTitle>
						<AlertDialogDescription>
							确认后将创建不可变版本并进入检索;后续修改需通过修订产生新版本。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void confirm();
							}}
						>
							确认
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
			<AlertDialog open={excluding} onOpenChange={setExcluding}>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>排除此知识候选?</AlertDialogTitle>
						<AlertDialogDescription>
							排除后该候选将永久移出确认流程,不可恢复。
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel>取消</AlertDialogCancel>
						<AlertDialogAction
							className="bg-destructive text-white hover:bg-destructive/90"
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void exclude();
							}}
						>
							排除
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}
