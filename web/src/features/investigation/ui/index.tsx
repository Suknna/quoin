/* eslint-disable react-refresh/only-export-components -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

import {
	Bot,
	FileText,
	LoaderCircle,
	Paperclip,
	RotateCcw,
	Send,
	ShieldAlert,
	Square,
	X,
} from "lucide-react";
import {
	type Dispatch,
	type SetStateAction,
	useCallback,
	useEffect,
	useRef,
	useState,
} from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { messageOf, notify } from "@/app/shared";
import { AiContent } from "@/components/ai/AiContent";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Bubble, BubbleContent } from "@/components/ui/bubble";
import { Button } from "@/components/ui/button";
import {
	Message,
	MessageAvatar,
	MessageContent,
	MessageFooter,
	MessageHeader,
} from "@/components/ui/message";
import {
	MessageScroller,
	MessageScrollerButton,
	MessageScrollerContent,
	MessageScrollerItem,
	MessageScrollerProvider,
	MessageScrollerViewport,
} from "@/components/ui/message-scroller";
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import {
	appendFeedback,
	type FeedbackEvent,
	type FeedbackValue,
	feedbackValueLabels,
	fetchFeedback,
} from "@/features/feedback/api";
import {
	api,
	type InvestigationAttempt,
	type InvestigationDetail,
	type InvestigationMessage,
	type InvestigationSummary,
	sourceLabel,
} from "@/features/investigation/api";
import {
	attachmentCommandId,
	type TextAttachmentSummary,
	uploadAttachment,
} from "@/features/investigation/attachments/api";
import {
	canOfferRetry,
	canOfferUndo,
	effectiveAttemptForMessage,
	latestActiveUserMessage,
	mergeAttachmentIds,
	withdrawnRevision,
} from "@/features/investigation/chatControls";
import { streamInvestigationMessage } from "@/features/investigation/stream";
import {
	listToolCalls,
	type ToolCallItem,
} from "@/features/investigation/tools/api";
import { ToolCallCard } from "@/features/investigation/ui/ToolCallCard";
import { usePolling } from "@/hooks/use-polling";
import { formatDateTime } from "@/lib/format";
import { parseRoute } from "@/lib/parse-route";

const activeStates = new Set(["Queued", "Assigned", "Running", "Cancelling"]);

export function useInvestigationsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const route = parseRoute(props.route);
	const id = route.pathname.match(/^\/investigations\/([^/]+)$/)?.[1];
	const [items, setItems] = useState<InvestigationSummary[]>([]);
	const [error, setError] = useState("");
	const load = useCallback(async () => {
		if (props.suspended) return;
		try {
			setItems((await api.list()).items);
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "无法加载调查。"));
		}
	}, [props.suspended]);
	useEffect(() => {
		const refresh = () => void load();
		window.addEventListener("focus", refresh);
		window.addEventListener("investigation-created", refresh);
		return () => {
			window.removeEventListener("focus", refresh);
			window.removeEventListener("investigation-created", refresh);
		};
	}, [load]);
	useEffect(() => {
		void Promise.resolve().then(load);
	}, [load]);
	const list = (
		<aside className="space-y-3 p-3">
			<Button
				className="w-full justify-start"
				size="sm"
				onClick={() => props.navigate("/investigations/new")}
			>
				<Send />
				新建对话
			</Button>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{items.length === 0 ? (
				<p className="px-2 text-sm text-muted-foreground">尚无对话。</p>
			) : (
				items.map((item) => (
					<Button
						key={item.id}
						variant="ghost"
						className="h-auto w-full justify-start p-3 text-left whitespace-normal"
						onClick={() =>
							props.navigate(`/investigations/${encodeURIComponent(item.id)}`)
						}
					>
						<span className="block w-full min-w-0">
							<strong className="block truncate text-sm">
								{item.displayTitle}
							</strong>
							<small className="mt-1 block text-muted-foreground">
								{formatDateTime(item.lastActivityAt)}
							</small>
						</span>
					</Button>
				))
			)}
		</aside>
	);
	// The AI SRE landing route is a draft-only conversation workspace: it must not
	// create a backend investigation until the operator submits the first turn.
	const content =
		route.pathname === "/investigations" ||
		route.pathname === "/investigations/new" ? (
			<NewInvestigation {...props} />
		) : id ? (
			<InvestigationView key={id} id={id} {...props} />
		) : (
			<section className="p-6">
				<h1 className="text-xl font-semibold">AI SRE</h1>
				<p className="mt-2 text-sm text-muted-foreground">未找到该调查。</p>
			</section>
		);
	return {
		title: "AI SRE",
		crumbs: id
			? [{ label: "对话", to: "/investigations" }, { label: "调查详情" }]
			: undefined,
		list,
		content,
	};
}

const starterPrompts = [
	"总结当前最需要处理的告警，并按影响排序。",
	"排查最近 30 分钟的错误率升高，给出可验证的假设。",
	"为这次事件起草安全的缓解步骤与回滚检查项。",
	"关联证据，解释服务延迟上升可能的根因。",
];

function NewInvestigation({
	route,
	navigate,
	suspended,
}: WorkspaceModuleProps) {
	const query = parseRoute(route).searchParams;
	const occurrence = query.get("occurrence");
	const analysis = query.get("initialAnalysis");
	const [body, setBody] = useState("");
	const [files, setFiles] = useState<TextAttachmentSummary[]>([]);
	const [systems, setSystems] = useState<
		Array<{ key: string; displayName: string }>
	>([]);
	const [businessSystemKey, setBusinessSystemKey] = useState("");
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	useEffect(() => {
		void Promise.resolve(api.businessSystems())
			.then(setSystems)
			.catch((reason: unknown) =>
				setError(messageOf(reason, "无法加载可用业务系统。")),
			);
	}, []);
	const create = async () => {
		if (!body.trim() && files.length === 0)
			return setError("请输入第一条消息或添加附件。");
		setBusy(true);
		setError("");
		try {
			const sources = occurrence
				? [
						{ type: "occurrence", sourceId: occurrence },
						...(analysis
							? [{ type: "initial_analysis", sourceId: analysis }]
							: []),
					]
				: [];
			const item = await api.create(
				body,
				sources,
				files.map((file) => file.id),
				businessSystemKey,
			);
			notify.success("已创建调查");
			window.dispatchEvent(new Event("investigation-created"));
			navigate(`/investigations/${encodeURIComponent(item.id)}`);
		} catch (reason) {
			notify.error(reason, "无法创建调查。");
		} finally {
			setBusy(false);
		}
	};
	return (
		<section className="mx-auto flex min-h-0 w-full max-w-4xl flex-1 flex-col justify-center px-6 py-10">
			<div className="mx-auto w-full max-w-2xl space-y-6">
				<div className="text-center">
					<div className="mx-auto mb-4 flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground">
						<ShieldAlert className="size-6" />
					</div>
					<h1 className="text-2xl font-semibold tracking-tight">
						开始一次 SRE 调查
					</h1>
					<p className="mt-2 text-sm leading-6 text-muted-foreground">
						描述现象、附上日志或从一个提示开始。AI SRE
						会保留每个结论的证据与执行记录。
					</p>
				</div>
				{occurrence && (
					<Alert>
						<AlertDescription>
							将关联告警 {occurrence}
							{analysis ? " 与初步分析" : ""}。
						</AlertDescription>
					</Alert>
				)}
				<div>
					<label
						className="mb-1 block text-sm font-medium"
						htmlFor="business-system"
					>
						业务系统（可选）
					</label>
					<Select
						value={businessSystemKey || "__none"}
						onValueChange={(value) =>
							setBusinessSystemKey(value === "__none" ? "" : value)
						}
						disabled={suspended || busy}
					>
						<SelectTrigger id="business-system" aria-label="业务系统">
							<SelectValue placeholder="不绑定业务系统" />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value="__none">不绑定业务系统</SelectItem>
							{systems.map((system) => (
								<SelectItem key={system.key} value={system.key}>
									{system.displayName}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<p className="mt-1 text-xs text-muted-foreground">
						AI SRE
						默认可使用已启用接入授权的只读指标；绑定业务系统仅冻结其声明作为历史上下文，并非授权前提。
					</p>
				</div>
				<div className="grid gap-2 sm:grid-cols-2">
					{starterPrompts.map((prompt) => (
						<Button
							key={prompt}
							type="button"
							variant="outline"
							className="h-auto justify-start whitespace-normal p-3 text-left text-sm font-normal"
							disabled={suspended || busy}
							onClick={() => setBody(prompt)}
						>
							{prompt}
						</Button>
					))}
				</div>
				<Composer
					body={body}
					setBody={setBody}
					files={files}
					setFiles={setFiles}
					disabled={suspended || busy}
					placeholder="描述需要调查的问题…"
					onSubmit={() => void create()}
					submitLabel={busy ? "创建中…" : "创建并发送"}
					submitting={busy}
				/>
				{error && (
					<Alert variant="destructive" role="alert">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}
			</div>
		</section>
	);
}

function InvestigationView({
	id,
	navigate,
	suspended,
	openEvidence,
}: WorkspaceModuleProps & { id: string }) {
	const [detail, setDetail] = useState<InvestigationDetail | null>(null);
	const [messages, setMessages] = useState<InvestigationMessage[]>([]);
	const [attempts, setAttempts] = useState<InvestigationAttempt[]>([]);
	const [error, setError] = useState("");
	const load = useCallback(async () => {
		if (suspended) return;
		try {
			const [next, messagePage, attemptPage] = await Promise.all([
				api.get(id),
				api.listMessages(id),
				api.listAttempts(id),
			]);
			setDetail(next);
			setMessages(messagePage.items);
			setAttempts(attemptPage.items);
			setError("");
		} catch (reason) {
			setError(messageOf(reason, "无法加载调查。"));
		}
	}, [id, suspended]);
	useEffect(() => {
		void Promise.resolve().then(load);
	}, [load]);
	if (!detail)
		return (
			<section className="p-6">
				{error ? (
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				) : (
					<div
						className="flex flex-col gap-3"
						role="status"
						aria-label="正在加载调查"
					>
						<div className="flex items-start gap-3">
							<Skeleton className="h-8 w-8 shrink-0 rounded-full" />
							<div className="flex min-w-0 flex-1 flex-col gap-2">
								<Skeleton className="h-4 w-1/4" />
								<Skeleton className="h-4 w-full" />
								<Skeleton className="h-4 w-5/6" />
								<Skeleton className="h-4 w-2/3" />
							</div>
						</div>
					</div>
				)}
			</section>
		);
	return (
		<section className="flex min-h-0 min-w-0 flex-1 flex-col">
			<header className="min-w-0 border-b bg-background/95 px-6 py-4 backdrop-blur">
				<h1 className="break-words text-lg font-semibold">
					{detail.displayTitle}
				</h1>
				{detail.businessSystemKey && (
					<div className="mt-2">
						<Badge variant="outline">
							业务系统：{detail.businessSystemName || detail.businessSystemKey}
						</Badge>
					</div>
				)}
				<div className="mt-2 flex flex-wrap gap-2">
					{detail.sources.map((source) => (
						<SourceLink
							key={`${source.type}-${source.sourceId}`}
							type={source.type}
							sourceId={source.sourceId}
							openEvidence={openEvidence}
						/>
					))}
				</div>
			</header>
			{error && (
				<div className="px-6 pt-4">
					<Alert variant="destructive">
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				</div>
			)}
			<Thread
				detail={detail}
				messages={messages}
				attempts={attempts}
				navigate={navigate}
				suspended={suspended}
				openEvidence={openEvidence}
				reload={load}
			/>
		</section>
	);
}

function SourceLink({
	type,
	sourceId,
	openEvidence,
}: {
	type: string;
	sourceId: string;
	openEvidence: (id: string) => void;
}) {
	if (type !== "evidence")
		return (
			<Badge variant="outline">
				<FileText />
				{sourceLabel(type)} {sourceId}
			</Badge>
		);
	return (
		<Button
			size="xs"
			variant="outline"
			onClick={() => openEvidence(sourceId)}
			aria-label={`证据 ${sourceId}，打开证据`}
		>
			<FileText />
			证据 {sourceId}
		</Button>
	);
}

function Thread({
	detail,
	messages,
	attempts,
	navigate,
	suspended,
	openEvidence,
	reload,
}: {
	detail: InvestigationDetail;
	messages: InvestigationMessage[];
	attempts: InvestigationAttempt[];
	navigate: (route: string) => void;
	suspended: boolean;
	openEvidence: (id: string) => void;
	reload: () => Promise<void>;
}) {
	const [body, setBody] = useState("");
	const [files, setFiles] = useState<TextAttachmentSummary[]>([]);
	const [mutating, setMutating] = useState(false);
	const [events, setEvents] = useState<FeedbackEvent[]>([]);
	const [streamText, setStreamText] = useState("");
	const [streaming, setStreaming] = useState(false);
	const [stopping, setStopping] = useState(false);
	const [restoredMessageId, setRestoredMessageId] = useState<string | null>(
		null,
	);
	const controller = useRef<AbortController | null>(null);
	const active = attempts.find((item) => activeStates.has(item.state));
	const isBusy = suspended || mutating || stopping;
	// The server assigns an attempt after accepting a sent message. While the local
	// stream is open, keep the authoritative projection polling so Stop can cancel it.
	usePolling(() => void reload(), 500, (active || streaming) && !suspended);
	useEffect(() => {
		const revision = withdrawnRevision(messages);
		if (!restoredMessageId || revision === 0) return;
		const restored = messages.find(
			(message) =>
				message.id === restoredMessageId && message.status === "withdrawn",
		);
		if (!restored) return;
		setBody(restored.content);
		setFiles((staged) => {
			const restoredText = restored.attachments.map(
				(attachment): TextAttachmentSummary => ({
					...attachment,
					mediaType: "text/plain",
				}),
			);
			const byId = new Map(
				[...restoredText, ...staged].map((attachment) => [
					attachment.id,
					attachment,
				]),
			);
			return mergeAttachmentIds(
				restored.attachments,
				staged.map((attachment) => attachment.id),
			).flatMap((id) => byId.get(id) ?? []);
		});
		setRestoredMessageId(null);
	}, [messages, restoredMessageId]);
	useEffect(
		() => () => {
			controller.current?.abort();
		},
		[],
	);
	const send = async () => {
		if (!body.trim() && files.length === 0) return;
		setMutating(true);
		setStreamText("");
		try {
			const sent = await api.sendMessage(
				detail.id,
				body,
				detail.headMessageId ?? null,
				files.map((file) => file.id),
			);
			// Refresh the durable projection before opening the stream. This prevents the
			// submitted user turn from briefly disappearing while the server assigns it.
			await reload();
			setBody("");
			setFiles([]);
			setStreaming(true);
			controller.current = new AbortController();
			for await (const update of streamInvestigationMessage(
				detail.id,
				sent.id,
				controller.current.signal,
			)) {
				const result = update as unknown as {
					content?: Array<{ type?: string; text?: string }>;
				};
				setStreamText(
					(result.content ?? [])
						.filter((part) => part.type === "text")
						.map((part) => part.text ?? "")
						.join(""),
				);
			}
			await reload();
			notify.success("已发送");
		} catch (reason) {
			if (!(reason instanceof DOMException && reason.name === "AbortError"))
				notify.error(reason, "消息发送失败。");
		} finally {
			controller.current = null;
			setStreaming(false);
			setStreamText("");
			setMutating(false);
		}
	};
	const cancel = async () => {
		// Abort rendering immediately, but do not abandon the cancellation request: a
		// stream may start before the attempt projection has appeared in the poll.
		controller.current?.abort();
		setStopping(true);
		try {
			let target = active;
			for (let retry = 0; !target && retry < 10; retry += 1) {
				await new Promise((resolve) => window.setTimeout(resolve, 250));
				target = (await api.listAttempts(detail.id)).items.find((attempt) =>
					activeStates.has(attempt.state),
				);
			}
			if (!target) throw new Error("正在等待服务确认执行，暂时无法停止。");
			await api.cancelAttempt(detail.id, target.id, target.rowVersion);
			await reload();
			notify.success("已停止");
		} catch (reason) {
			notify.error(reason, "无法停止。");
		} finally {
			setStopping(false);
		}
	};
	const undo = async () => {
		if (!detail.headMessageId) return;
		const restored = latestActiveUserMessage(messages);
		setMutating(true);
		try {
			await api.undo(detail.id, detail.headMessageId);
			setRestoredMessageId(restored?.id ?? null);
			await reload();
			notify.success("已撤回");
		} catch (reason) {
			notify.error(reason, "无法撤回。");
		} finally {
			setMutating(false);
		}
	};
	const retry = async (attempt: InvestigationAttempt) => {
		setMutating(true);
		try {
			await api.retryAttempt(detail.id, attempt.id);
			await reload();
			notify.success("已重试");
		} catch (reason) {
			notify.error(reason, "无法重试。");
		} finally {
			setMutating(false);
		}
	};
	const attemptFacts = Object.fromEntries(
		attempts.map((attempt) => [
			attempt.id,
			{ state: attempt.state, rowVersion: attempt.rowVersion },
		]),
	);
	const canCompose = !isBusy && !active && !streaming;
	return (
		<div className="flex min-h-0 min-w-0 flex-1 flex-col">
			<MessageScrollerProvider autoScroll>
				<MessageScroller className="flex-1">
					<MessageScrollerViewport>
						<MessageScrollerContent className="mx-auto w-full min-w-0 max-w-4xl gap-6 px-6 py-6">
							{messages.map((message) => (
								<MessageScrollerItem
									key={message.id}
									messageId={message.id}
									scrollAnchor={message.role === "user"}
								>
									<ChatMessage
										message={message}
										messages={messages}
										attempts={attempts}
										attemptFacts={attemptFacts}
										investigationId={detail.id}
										activeAttemptId={active?.id}
										navigate={navigate}
										openEvidence={openEvidence}
										showEvents={setEvents}
										onUndo={undo}
										onRetry={retry}
										disabled={isBusy}
									/>
								</MessageScrollerItem>
							))}
							{streaming && (
								<MessageScrollerItem messageId="streaming-response">
									<StreamingMessage content={streamText} />
								</MessageScrollerItem>
							)}
						</MessageScrollerContent>
					</MessageScrollerViewport>
					<MessageScrollerButton />
				</MessageScroller>
			</MessageScrollerProvider>
			<div className="border-t bg-background px-6 py-4">
				<div className="mx-auto w-full max-w-4xl">
					{(active || streaming || stopping) && (
						<div className="mb-3 flex items-center justify-between rounded-lg border border-primary/20 bg-primary/5 px-3 py-2 text-sm">
							<span className="flex items-center gap-2">
								<span className="size-2 animate-pulse rounded-full bg-primary" />
								{stopping
									? "正在停止执行…"
									: active
										? `正在执行：${active.state}`
										: "正在生成回复"}
							</span>
							<Button
								type="button"
								variant="outline"
								size="sm"
								disabled={suspended || stopping}
								onClick={() => void cancel()}
							>
								<Square />
								停止
							</Button>
						</div>
					)}
					<Composer
						body={body}
						setBody={setBody}
						files={files}
						setFiles={setFiles}
						disabled={!canCompose}
						placeholder={
							active || streaming || stopping
								? "等待当前回复完成…"
								: "输入消息，Enter 发送，Shift + Enter 换行"
						}
						onSubmit={() => void send()}
						submitLabel="发送"
					/>
					{events.length > 0 && (
						<p className="mt-2 text-xs text-muted-foreground">
							已记录反馈：
							{events
								.map((event) => feedbackValueLabels[event.value])
								.join("、")}
						</p>
					)}
				</div>
			</div>
		</div>
	);
}

function StreamingMessage({ content }: { content: string }) {
	return (
		<Message>
			<MessageAvatar>
				<Avatar>
					<AvatarFallback>
						<Bot className="size-4" />
					</AvatarFallback>
				</Avatar>
			</MessageAvatar>
			<MessageContent>
				<MessageHeader>
					AI SRE{" "}
					<span className="ml-2 font-normal text-muted-foreground">
						正在回复
					</span>
				</MessageHeader>
				<Bubble variant="tinted">
					<BubbleContent>
						{content ? (
							<AiContent content={content} />
						) : (
							<div
								className="flex flex-col gap-2"
								role="status"
								aria-label="正在整理调查结果"
							>
								<Skeleton className="h-4 w-full" />
								<Skeleton className="h-4 w-5/6" />
								<Skeleton className="h-4 w-2/3" />
							</div>
						)}
						<span className="ml-1 inline-block size-1.5 animate-pulse rounded-full bg-current align-middle" />
					</BubbleContent>
				</Bubble>
			</MessageContent>
		</Message>
	);
}

function ChatMessage({
	message,
	messages,
	attempts,
	attemptFacts,
	investigationId,
	activeAttemptId,
	navigate,
	openEvidence,
	showEvents,
	onUndo,
	onRetry,
	disabled,
}: {
	message: InvestigationMessage;
	messages: InvestigationMessage[];
	attempts: InvestigationAttempt[];
	attemptFacts: Record<
		string,
		{ state: InvestigationAttempt["state"]; rowVersion: number }
	>;
	investigationId: string;
	activeAttemptId?: string;
	navigate: (route: string) => void;
	openEvidence: (id: string) => void;
	showEvents: (items: FeedbackEvent[]) => void;
	onUndo: () => Promise<void>;
	onRetry: (attempt: InvestigationAttempt) => Promise<void>;
	disabled: boolean;
}) {
	const isUser = message.role === "user";
	const attempt = effectiveAttemptForMessage(message, messages, attempts);
	const feedback = async (value: FeedbackValue) => {
		try {
			await appendFeedback(
				{ type: "investigation_message", id: message.id },
				value,
				"",
			);
			showEvents(
				(await fetchFeedback({ type: "investigation_message", id: message.id }))
					.items,
			);
			notify.success("已记录反馈");
		} catch (reason) {
			notify.error(reason, "无法提交反馈。");
		}
	};
	return (
		<Message
			align={isUser ? "end" : "start"}
			className={message.status === "withdrawn" ? "opacity-50" : ""}
		>
			<MessageAvatar>
				<Avatar>
					<AvatarFallback>
						{isUser ? "你" : <Bot className="size-4" />}
					</AvatarFallback>
				</Avatar>
			</MessageAvatar>
			<MessageContent>
				<MessageHeader>
					{isUser ? "你" : "AI SRE"}{" "}
					<span className="ml-2 font-normal text-muted-foreground">
						{formatDateTime(message.createdAt)}
					</span>
					{message.status === "withdrawn" && (
						<Badge className="ml-2" variant="outline">
							已撤回
						</Badge>
					)}
				</MessageHeader>
				<Bubble
					align={isUser ? "end" : "start"}
					variant={isUser ? "default" : "secondary"}
				>
					<BubbleContent>
						<AiContent
							content={message.content}
							evidenceIds={message.evidenceIds ?? undefined}
							openEvidence={openEvidence}
						/>
					</BubbleContent>
				</Bubble>
				{message.attachments.length > 0 && (
					<div className="flex flex-wrap gap-1">
						{message.attachments.map((attachment) => (
							<Badge key={attachment.id} variant="outline">
								<Paperclip />
								{attachment.originalFilename}
							</Badge>
						))}
					</div>
				)}
				{message.evidenceIds?.length ? (
					<div className="flex flex-wrap gap-1">
						{message.evidenceIds.map((id) => (
							<Button
								key={id}
								size="xs"
								variant="outline"
								onClick={() => openEvidence(id)}
							>
								<FileText />
								证据 {id}
							</Button>
						))}
					</div>
				) : null}
				{/* 工具记录归属本回合（经重试语义解析的权威 Attempt），不再堆叠到线程底部。 */}
				{attempt && (
					<AttemptToolCalls
						investigationId={investigationId}
						attemptId={attempt.id}
						active={activeAttemptId === attempt.id}
					/>
				)}
				{attempt && <MessageFooter>执行状态：{attempt.state}</MessageFooter>}
				{isUser && canOfferUndo(messages, message, activeAttemptId) && (
					<Button
						size="xs"
						variant="ghost"
						disabled={disabled}
						onClick={() => void onUndo()}
					>
						<RotateCcw />
						撤回此回合
					</Button>
				)}
				{isUser &&
					message.attemptId &&
					canOfferRetry(
						{
							[message.attemptId]: attemptFacts[message.attemptId] ?? {
								state: attempt?.state ?? "Succeeded",
								rowVersion: attempt?.rowVersion ?? 0,
							},
						},
						message,
						activeAttemptId,
					) &&
					attempt?.state === "Failed" && (
						<Button
							size="xs"
							variant="ghost"
							disabled={disabled}
							onClick={() => void onRetry(attempt)}
						>
							<RotateCcw />
							重试
						</Button>
					)}
				{!isUser && (
					<div className="flex flex-wrap gap-1">
						<span className="mr-1 self-center text-xs text-muted-foreground">
							此回复：
						</span>
						{(
							[
								"adopted",
								"executed",
								"verified_effective",
								"rejected",
							] as FeedbackValue[]
						).map((value) => (
							<Button
								key={value}
								size="xs"
								variant="ghost"
								onClick={() => void feedback(value)}
							>
								{feedbackValueLabels[value]}
							</Button>
						))}
						{/* 知识库已上线：从这里进入检索与候选确认。 */}
						<Button
							size="xs"
							variant="ghost"
							onClick={() => navigate("/knowledge")}
						>
							查看知识库
						</Button>
					</div>
				)}
			</MessageContent>
		</Message>
	);
}

/** 本回合的工具时间线：按 Attempt 读取并原位展开；执行中的 Attempt 轮询真实阶段。 */
function AttemptToolCalls({
	investigationId,
	attemptId,
	active,
}: {
	investigationId: string;
	attemptId: string;
	active: boolean;
}) {
	const [calls, setCalls] = useState<ToolCallItem[]>([]);
	const [error, setError] = useState("");
	useEffect(() => {
		let cancelled = false;
		listToolCalls(investigationId, attemptId)
			.then((items) => {
				if (!cancelled) setCalls(items);
			})
			.catch((reason: unknown) => {
				if (!cancelled) setError(messageOf(reason, "无法读取工具调用。"));
			});
		return () => {
			cancelled = true;
		};
	}, [investigationId, attemptId]);
	usePolling(
		() => {
			listToolCalls(investigationId, attemptId)
				.then((items) => setCalls(items))
				.catch(() => undefined);
		},
		2000,
		active,
	);
	if (error)
		return (
			<p role="alert" className="text-xs text-destructive">
				{error}
			</p>
		);
	if (calls.length === 0) return null;
	return (
		<div className="flex min-w-0 flex-col gap-2" aria-label="本回合工具记录">
			{calls.map((call) => (
				<ToolCallCard key={call.id} call={call} />
			))}
		</div>
	);
}

function Composer({
	body,
	setBody,
	files,
	setFiles,
	disabled,
	placeholder,
	onSubmit,
	submitLabel,
	submitting = false,
}: {
	body: string;
	setBody: (value: string) => void;
	files: TextAttachmentSummary[];
	setFiles: Dispatch<SetStateAction<TextAttachmentSummary[]>>;
	disabled: boolean;
	placeholder: string;
	onSubmit: () => void;
	submitLabel: string;
	submitting?: boolean;
}) {
	const [pendingUploads, setPendingUploads] = useState(0);
	const [uploadError, setUploadError] = useState("");
	const upload = async (file?: File) => {
		if (!file) return;
		setPendingUploads((count) => count + 1);
		setUploadError("");
		try {
			const attachment = await uploadAttachment(file, attachmentCommandId());
			setFiles((current) => [...current, attachment]);
			// 上传失败保留内联展示：ui/index.test.tsx 断言 role="alert" 的失败原因，不迁移到全局 toast。
		} catch (reason) {
			setUploadError(messageOf(reason, "附件上传失败，请重试。"));
		} finally {
			setPendingUploads((count) => count - 1);
		}
	};
	const blocked = disabled || pendingUploads > 0;
	return (
		<div className="rounded-xl border bg-background p-2 shadow-sm">
			<Textarea
				aria-label="消息内容"
				value={body}
				onChange={(event) => setBody(event.target.value)}
				onKeyDown={(event) => {
					if (event.key === "Enter" && !event.shiftKey) {
						event.preventDefault();
						if (!blocked && (body.trim() || files.length)) onSubmit();
					}
				}}
				disabled={blocked}
				placeholder={placeholder}
				className="min-h-24 resize-none border-0 bg-transparent shadow-none focus-visible:ring-0"
			/>
			{files.length > 0 && (
				<div className="flex flex-wrap gap-1 px-1 pb-2">
					{files.map((file) => (
						<Badge key={file.id} variant="secondary">
							<Paperclip />
							{file.originalFilename}
							<Button
								type="button"
								variant="ghost"
								size="icon-xs"
								className="ml-1 hover:text-destructive"
								aria-label={`移除 ${file.originalFilename}`}
								onClick={() =>
									setFiles((current) =>
										current.filter((item) => item.id !== file.id),
									)
								}
								disabled={blocked}
							>
								<X className="size-3" />
							</Button>
						</Badge>
					))}
				</div>
			)}
			{pendingUploads > 0 && (
				<p className="px-1 pb-2 text-xs text-muted-foreground" role="status">
					正在上传附件…
				</p>
			)}
			{uploadError && (
				<p className="px-1 pb-2 text-xs text-destructive" role="alert">
					{uploadError}
				</p>
			)}
			<div className="flex items-center justify-between gap-2 px-1">
				<label className="inline-flex cursor-pointer items-center gap-1 text-xs text-muted-foreground hover:text-foreground">
					<Paperclip className="size-4" />
					添加附件
					<input
						className="sr-only"
						type="file"
						disabled={blocked}
						onChange={(event) => {
							const file = event.target.files?.[0];
							event.currentTarget.value = "";
							void upload(file);
						}}
					/>
				</label>
				<Button
					type="button"
					size="sm"
					disabled={blocked || (!body.trim() && files.length === 0)}
					onClick={onSubmit}
				>
					{submitting ? (
						<LoaderCircle
							className="animate-spin"
							data-icon="inline-start"
							aria-hidden="true"
						/>
					) : (
						<Send />
					)}
					{submitLabel}
				</Button>
			</div>
		</div>
	);
}
