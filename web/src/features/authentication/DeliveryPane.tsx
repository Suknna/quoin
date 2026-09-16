import {
	type FormEvent,
	type ReactNode,
	useCallback,
	useEffect,
	useState,
} from "react";
import { WorkbenchApiError } from "@/api/workbench";
import { ErrorMessage, messageOf } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Collapsible,
	CollapsibleContent,
	CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
	FieldTitle,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Item,
	ItemActions,
	ItemContent,
	ItemDescription,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import {
	ChevronDown,
	ChevronLeft,
	ChevronRight,
	Mail,
	Plus,
	Trash2,
	Webhook,
} from "lucide-react";
import {
	Select,
	SelectContent,
	SelectGroup,
	SelectItem,
	SelectTrigger,
	SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
	type DeliverySlot,
	type KVRow,
	type SmtpDraft,
	type SmtpPresetId,
	type WebhookAuthMode,
	type WebhookDraft,
	buildSmtpChannel,
	buildWebhookChannel,
	channelSummary,
	newRow,
	slotFrom,
	SMTP_PRESETS,
	smtpDraftWithPreset,
	smtpPresetById,
	webhookDraftWithAuthMode,
} from "./delivery";
import {
	type AuthDeliveryChannel,
	type AuthDeliveryConfiguration,
	type AuthDeliveryView,
	authFlowApi,
} from "./api";

/**
 * Verification-code delivery settings (SMTP / outbound webhook), editable by
 * the admin initialization flow (or a signed-in admin). The pane opens on a
 * choice between two channels; picking one opens a focused form, and saving
 * returns to the flow. Secret values are write-only: the server stores them
 * by reference and the UI can never read them back — blank secret inputs keep
 * the stored value.
 */

type Screen = "choose" | "smtp" | "webhook";
type SlotKey = "email" | "sms";

export function DeliveryPane({
	onDone,
	onFlowGone,
}: {
	onDone: () => void;
	onFlowGone: () => void;
}) {
	const [view, setView] = useState<AuthDeliveryView>();
	const [loadError, setLoadError] = useState("");
	const [screen, setScreen] = useState<Screen>("choose");
	// Which channel the webhook form edits; SMS is the usual webhook recipient.
	const [target, setTarget] = useState<SlotKey>("sms");
	const [email, setEmail] = useState<DeliverySlot>(() => slotFrom());
	const [sms, setSms] = useState<DeliverySlot>(() => slotFrom());
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	const load = useCallback(() => {
		setLoadError("");
		authFlowApi.readDelivery().then(
			(loaded) => {
				setView(loaded);
				setEmail(slotFrom(loaded.configuration.email));
				setSms(slotFrom(loaded.configuration.sms));
			},
			(reason) => {
				if (
					reason instanceof WorkbenchApiError &&
					(reason.status === 401 || reason.status === 404)
				) {
					onFlowGone();
					return;
				}
				setLoadError(messageOf(reason, "投递设置暂时不可用，请稍后重试。"));
			},
		);
	}, [onFlowGone]);
	useEffect(() => load(), [load]);
	const openSmtp = () => setScreen("smtp");
	const openWebhook = () => {
		// Default to SMS unless the email channel already ships as a webhook.
		setTarget(view?.configuration.email?.kind === "webhook" ? "email" : "sms");
		setScreen("webhook");
	};
	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		if (!view) return;
		// Only the channel owning the focused form is rebuilt; every other
		// channel keeps its stored configuration verbatim, arbitrary settings
		// and secret references included.
		const slot: SlotKey = screen === "smtp" ? "email" : target;
		const secrets: Record<string, string> = {};
		let channel: AuthDeliveryChannel;
		try {
			const built =
				screen === "smtp"
					? buildSmtpChannel(email.smtp)
					: buildWebhookChannel(
							target === "email" ? email.webhook : sms.webhook,
							target,
						);
			channel = built.channel;
			Object.assign(secrets, built.secrets);
		} catch (reason) {
			setError(messageOf(reason, "投递设置没有保存成功，请检查后重试。"));
			return;
		}
		setSaving(true);
		try {
			await authFlowApi.saveDelivery({
				configuration: {
					...view.configuration,
					[slot]: channel,
				} satisfies AuthDeliveryConfiguration,
				secrets,
				expectedRowVersion: view.rowVersion,
			});
			onDone();
		} catch (reason) {
			if (
				reason instanceof WorkbenchApiError &&
				(reason.status === 401 || reason.status === 404)
			) {
				onFlowGone();
				return;
			}
			if (
				reason instanceof WorkbenchApiError &&
				reason.code === "row_version_conflict"
			) {
				// Refresh only the authoritative row version; keep local edits.
				const fresh = await authFlowApi.readDelivery().catch(() => undefined);
				if (fresh) setView(fresh);
				setError("配置已被其他人更新，请重试保存。");
				return;
			}
			if (
				reason instanceof WorkbenchApiError &&
				reason.code === "deployment_owned"
			) {
				setError("投递配置由部署文件管理，无法在此修改。");
				return;
			}
			setError(messageOf(reason, "投递设置没有保存成功，请检查后重试。"));
		} finally {
			setSaving(false);
		}
	}
	const readOnly = view?.source === "deployment";
	// The pane renders inside the shell AuthScreen already owns: no full-height
	// centering or page padding of its own, just the content column. Keying by
	// screen gives every transition a motion-safe entry animation.
	return (
		<div
			key={`${screen}:${target}`}
			className="flex w-full flex-col gap-6 motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-bottom-2"
		>
			{loadError ? (
				<LoadFailure message={loadError} onRetry={load} onDone={onDone} />
			) : !view ? (
				<p role="status" className="text-sm text-muted-foreground">
					正在加载投递设置…
				</p>
			) : readOnly ? (
				<ReadOnlyDelivery view={view} onDone={onDone} />
			) : screen === "choose" ? (
				<ChooseScreen
					view={view}
					onPickSmtp={openSmtp}
					onPickWebhook={openWebhook}
					onDone={onDone}
				/>
			) : screen === "smtp" ? (
				<SmtpForm
					draft={email.smtp}
					onDraft={(smtp) => setEmail((slot) => ({ ...slot, smtp }))}
					saving={saving}
					error={error}
					onSubmit={submit}
					onBack={() => setScreen("choose")}
				/>
			) : (
				<WebhookForm
					target={target}
					onTarget={(next) => {
						setTarget(next);
						(next === "email" ? setEmail : setSms)((slot) => ({
							...slot,
							kind: "webhook",
						}));
					}}
					draft={(target === "email" ? email : sms).webhook}
					onDraft={(webhook) =>
						(target === "email" ? setEmail : setSms)((slot) => ({
							...slot,
							webhook,
						}))
					}
					saving={saving}
					error={error}
					onSubmit={submit}
					onBack={() => setScreen("choose")}
				/>
			)}
		</div>
	);
}

function ChooseScreen({
	view,
	onPickSmtp,
	onPickWebhook,
	onDone,
}: {
	view: AuthDeliveryView;
	onPickSmtp: () => void;
	onPickWebhook: () => void;
	onDone: () => void;
}) {
	const emailChannel = view.configuration.email;
	const webhookParts = [
		emailChannel?.kind === "webhook" ? "邮箱" : undefined,
		view.configuration.sms ? "短信" : undefined,
	]
		.filter((part) => part !== undefined)
		.join("、");
	return (
		<>
			<div className="flex flex-col items-center gap-1 text-center">
				<h1 className="text-2xl font-bold">验证码投递设置</h1>
				<p className="text-sm text-balance text-muted-foreground">
					选择发送方式
				</p>
			</div>
			<div className="grid gap-3">
				<Item asChild variant="outline" className="hover:bg-accent/50">
					<button type="button" onClick={onPickSmtp}>
						<ItemMedia variant="icon">
							<Mail aria-hidden="true" />
						</ItemMedia>
						<ItemContent>
							<ItemTitle>SMTP 邮箱</ItemTitle>
							<ItemDescription>邮箱 + 授权码</ItemDescription>
						</ItemContent>
						<ItemActions>
							{emailChannel && (
								<Badge variant="secondary">
									{emailChannel.kind === "smtp"
										? "邮箱已配置"
										: "邮箱使用 Webhook"}
								</Badge>
							)}
							<ChevronRight
								className="size-4 text-muted-foreground"
								aria-hidden="true"
							/>
						</ItemActions>
					</button>
				</Item>
				<Item asChild variant="outline" className="hover:bg-accent/50">
					<button type="button" onClick={onPickWebhook}>
						<ItemMedia variant="icon">
							<Webhook aria-hidden="true" />
						</ItemMedia>
						<ItemContent>
							<ItemTitle>出站 Webhook</ItemTitle>
							<ItemDescription>连接邮件或短信网关</ItemDescription>
						</ItemContent>
						<ItemActions>
							{webhookParts && (
								<Badge variant="secondary">{`${webhookParts}已配置`}</Badge>
							)}
							<ChevronRight
								className="size-4 text-muted-foreground"
								aria-hidden="true"
							/>
						</ItemActions>
					</button>
				</Item>
			</div>
			<Button variant="ghost" type="button" onClick={onDone}>
				返回继续初始化
			</Button>
		</>
	);
}

/** Collapsed by default; advanced knobs stay out of the primary path. */
function AdvancedSettings({ children }: { children: ReactNode }) {
	return (
		<Collapsible className="group rounded-md border">
			<CollapsibleTrigger asChild>
				<Button
					variant="ghost"
					type="button"
					className="w-full justify-between"
				>
					高级设置
					<ChevronDown
						aria-hidden="true"
						className="transition-transform group-data-[state=open]:rotate-180"
					/>
				</Button>
			</CollapsibleTrigger>
			<CollapsibleContent>
				<FieldGroup className="gap-5 px-4 pb-4">{children}</FieldGroup>
			</CollapsibleContent>
		</Collapsible>
	);
}

function SmtpForm({
	draft,
	onDraft,
	saving,
	error,
	onSubmit,
	onBack,
}: {
	draft: SmtpDraft;
	onDraft: (smtp: SmtpDraft) => void;
	saving: boolean;
	error: string;
	onSubmit: (event: FormEvent) => void;
	onBack: () => void;
}) {
	const preset = smtpPresetById(draft.preset);
	return (
		<>
			<Button
				variant="ghost"
				size="sm"
				type="button"
				className="self-start text-muted-foreground"
				onClick={onBack}
				disabled={saving}
			>
				<ChevronLeft aria-hidden="true" />
				返回选择
			</Button>
			<h1 className="text-2xl font-bold text-center">SMTP 邮箱</h1>
			{error && <ErrorMessage>{error}</ErrorMessage>}
			<form className="flex flex-col gap-6" onSubmit={onSubmit}>
				<FieldGroup className="gap-5">
					<Field>
						<FieldLabel htmlFor="smtp-preset">服务商</FieldLabel>
						<Select
							value={draft.preset}
							onValueChange={(value) =>
								onDraft(smtpDraftWithPreset(draft, value as SmtpPresetId))
							}
							disabled={saving}
						>
							<SelectTrigger id="smtp-preset" className="w-full">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									{SMTP_PRESETS.map((item) => (
										<SelectItem key={item.id} value={item.id}>
											{item.label}
										</SelectItem>
									))}
								</SelectGroup>
							</SelectContent>
						</Select>
					</Field>
					{preset.id === "custom" && (
						<>
							<div className="grid grid-cols-[1fr_7rem] gap-3">
								<Field>
									<FieldLabel htmlFor="smtp-host">SMTP 服务器</FieldLabel>
									<Input
										id="smtp-host"
										value={draft.host}
										onChange={(e) =>
											onDraft({ ...draft, host: e.target.value })
										}
										placeholder="smtp.example.com"
										required
										disabled={saving}
									/>
								</Field>
								<Field>
									<FieldLabel htmlFor="smtp-port">端口</FieldLabel>
									<Input
										id="smtp-port"
										type="number"
										min={1}
										max={65535}
										value={draft.port}
										onChange={(e) =>
											onDraft({ ...draft, port: e.target.value })
										}
										placeholder="587"
										required
										disabled={saving}
									/>
								</Field>
							</div>
							<Field>
								<FieldLabel htmlFor="smtp-tls">TLS 模式</FieldLabel>
								<Select
									value={draft.tlsMode}
									onValueChange={(value) =>
										onDraft({
											...draft,
											tlsMode: value as "starttls" | "implicit",
										})
									}
									disabled={saving}
								>
									<SelectTrigger id="smtp-tls" className="w-full">
										<SelectValue />
									</SelectTrigger>
									<SelectContent>
										<SelectGroup>
											<SelectItem value="starttls">STARTTLS（推荐）</SelectItem>
											<SelectItem value="implicit">隐式 TLS</SelectItem>
										</SelectGroup>
									</SelectContent>
								</Select>
							</Field>
						</>
					)}
					<Field>
						<FieldLabel htmlFor="smtp-from">发件邮箱</FieldLabel>
						<Input
							id="smtp-from"
							type="email"
							value={draft.from}
							onChange={(e) => onDraft({ ...draft, from: e.target.value })}
							placeholder="quoin@example.com"
							required
							disabled={saving}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="smtp-password">授权码</FieldLabel>
						<Input
							id="smtp-password"
							type="password"
							autoComplete="new-password"
							value={draft.password}
							onChange={(e) => onDraft({ ...draft, password: e.target.value })}
							placeholder={
								draft.passwordRef
									? "已保存，留空保持不变"
									: "在服务商设置中生成"
							}
							disabled={saving}
						/>
					</Field>
					<AdvancedSettings>
						<Field>
							<FieldLabel htmlFor="smtp-username">
								SMTP 用户名（可选）
							</FieldLabel>
							<Input
								id="smtp-username"
								autoComplete="off"
								value={draft.username}
								onChange={(e) =>
									onDraft({ ...draft, username: e.target.value })
								}
								placeholder={
									draft.from ? `默认 ${draft.from}` : "默认使用发件邮箱"
								}
								disabled={saving}
							/>
							<FieldDescription>
								默认使用发件邮箱登录；仅在服务商要求时修改。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="smtp-root-ca">
								私有根 CA PEM（可选）
							</FieldLabel>
							<Textarea
								id="smtp-root-ca"
								value={draft.rootCa}
								onChange={(e) => onDraft({ ...draft, rootCa: e.target.value })}
								placeholder={
									"-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----"
								}
								rows={3}
								className="font-mono text-xs"
								disabled={saving}
							/>
							<FieldDescription>
								私网 SMTP 网关使用私有 CA
								签发证书时粘贴其根证书；留空仅信任系统根。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="smtp-cidrs">
								允许的私网 CIDR（可选）
							</FieldLabel>
							<Input
								id="smtp-cidrs"
								value={draft.cidrs}
								onChange={(e) => onDraft({ ...draft, cidrs: e.target.value })}
								placeholder="172.16.0.0/12, 192.168.0.0/16"
								disabled={saving}
							/>
							<FieldDescription>
								私网服务器默认被拒绝；需显式列出允许的网段，逗号分隔。公网地址不受影响。
							</FieldDescription>
						</Field>
					</AdvancedSettings>
					<Button type="submit" disabled={saving}>
						{saving ? "正在保存…" : "保存投递设置"}
					</Button>
				</FieldGroup>
			</form>
		</>
	);
}

/** One editable key/value row list shared by mappings, headers and secrets. */
function RowEditor({
	title,
	description,
	rows,
	onRows,
	keyLabel,
	keyPlaceholder,
	valueLabel,
	valuePlaceholder,
	addText,
	disabled,
	children,
}: {
	title: string;
	description: string;
	rows: KVRow[];
	onRows: (rows: KVRow[]) => void;
	keyLabel: string;
	keyPlaceholder: string;
	valueLabel: string;
	valuePlaceholder: string;
	addText: string;
	disabled: boolean;
	children?: ReactNode;
}) {
	const update = (index: number, patch: Partial<KVRow>) =>
		onRows(rows.map((row, at) => (at === index ? { ...row, ...patch } : row)));
	return (
		<div className="flex flex-col gap-2">
			<FieldTitle>{title}</FieldTitle>
			{rows.map((row, index) => (
				<div
					key={row.id}
					className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]"
				>
					<Input
						aria-label={keyLabel}
						placeholder={keyPlaceholder}
						value={row.key}
						onChange={(e) => update(index, { key: e.target.value })}
						disabled={disabled}
					/>
					<Input
						aria-label={valueLabel}
						placeholder={valuePlaceholder}
						value={row.value}
						onChange={(e) => update(index, { value: e.target.value })}
						disabled={disabled}
					/>
					<Button
						variant="ghost"
						size="icon-sm"
						type="button"
						aria-label={`删除第 ${index + 1} 行`}
						onClick={() => onRows(rows.filter((_, at) => at !== index))}
						disabled={disabled}
					>
						<Trash2 aria-hidden="true" />
					</Button>
				</div>
			))}
			<div>
				<Button
					variant="outline"
					size="sm"
					type="button"
					onClick={() => onRows([...rows, newRow()])}
					disabled={disabled}
				>
					<Plus aria-hidden="true" />
					{addText}
				</Button>
			</div>
			<FieldDescription>{description}</FieldDescription>
			{children}
		</div>
	);
}

function SecretHeaderEditor({
	draft,
	onDraft,
	saving,
}: {
	draft: WebhookDraft;
	onDraft: (webhook: WebhookDraft) => void;
	saving: boolean;
}) {
	const refs = [
		...new Set(
			draft.secretHeaders.map((row) => row.value.trim()).filter(Boolean),
		),
	];
	return (
		<RowEditor
			title="秘密请求头"
			description="请求头名称 → 秘密引用名；引用值在下方填写，保存后不可读取。"
			rows={draft.secretHeaders}
			onRows={(secretHeaders) => onDraft({ ...draft, secretHeaders })}
			keyLabel="请求头名称"
			keyPlaceholder="X-Sign"
			valueLabel="秘密引用名"
			valuePlaceholder="webhook_sign"
			addText="添加秘密请求头"
			disabled={saving}
		>
			{refs.map((ref) => (
				<Field key={ref}>
					<FieldLabel
						htmlFor={`webhook-secret-${ref}`}
					>{`秘密值 ${ref}`}</FieldLabel>
					<Input
						id={`webhook-secret-${ref}`}
						type="password"
						autoComplete="new-password"
						value={draft.secretValues[ref] ?? ""}
						onChange={(e) =>
							onDraft({
								...draft,
								secretValues: { ...draft.secretValues, [ref]: e.target.value },
							})
						}
						placeholder={
							draft.storedRefs.includes(ref)
								? "已保存，留空保持不变"
								: undefined
						}
						disabled={saving}
					/>
				</Field>
			))}
		</RowEditor>
	);
}

function WebhookForm({
	target,
	onTarget,
	draft,
	onDraft,
	saving,
	error,
	onSubmit,
	onBack,
}: {
	target: SlotKey;
	onTarget: (next: SlotKey) => void;
	draft: WebhookDraft;
	onDraft: (webhook: WebhookDraft) => void;
	saving: boolean;
	error: string;
	onSubmit: (event: FormEvent) => void;
	onBack: () => void;
}) {
	const mode = draft.authMode;
	const authRef = draft.secretHeaders[0]?.value.trim() ?? "";
	const stored = draft.storedRefs.includes(authRef);
	return (
		<>
			<Button
				variant="ghost"
				size="sm"
				type="button"
				className="self-start text-muted-foreground"
				onClick={onBack}
				disabled={saving}
			>
				<ChevronLeft aria-hidden="true" />
				返回选择
			</Button>
			<div className="flex flex-col items-center gap-1 text-center">
				<h1 className="text-2xl font-bold">出站 Webhook</h1>
			</div>
			{error && <ErrorMessage>{error}</ErrorMessage>}
			<form className="flex flex-col gap-6" onSubmit={onSubmit}>
				<FieldGroup className="gap-5">
					<Field>
						<FieldLabel htmlFor="webhook-target">接收渠道</FieldLabel>
						<Select
							value={target}
							onValueChange={(value) => onTarget(value as SlotKey)}
							disabled={saving}
						>
							<SelectTrigger id="webhook-target" className="w-full">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									<SelectItem value="sms">短信验证码</SelectItem>
									<SelectItem value="email">邮箱验证码</SelectItem>
								</SelectGroup>
							</SelectContent>
						</Select>
					</Field>
					<Field>
						<FieldLabel htmlFor="webhook-url">Webhook 地址</FieldLabel>
						<Input
							id="webhook-url"
							value={draft.url}
							onChange={(e) => onDraft({ ...draft, url: e.target.value })}
							placeholder="https://gateway.example.com/sms"
							required
							disabled={saving}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="webhook-auth">鉴权方式</FieldLabel>
						<Select
							value={mode}
							onValueChange={(value) =>
								onDraft(
									webhookDraftWithAuthMode(
										draft,
										value as WebhookAuthMode,
										target,
									),
								)
							}
							disabled={saving}
						>
							<SelectTrigger id="webhook-auth" className="w-full">
								<SelectValue />
							</SelectTrigger>
							<SelectContent>
								<SelectGroup>
									<SelectItem value="none">无鉴权</SelectItem>
									<SelectItem value="bearer">Bearer 令牌</SelectItem>
									<SelectItem value="apiKey">API Key 请求头</SelectItem>
									<SelectItem value="custom">自定义秘密请求头</SelectItem>
								</SelectGroup>
							</SelectContent>
						</Select>
					</Field>
					{mode === "bearer" && (
						<Field>
							<FieldLabel htmlFor="webhook-bearer">访问令牌</FieldLabel>
							<Input
								id="webhook-bearer"
								type="password"
								autoComplete="new-password"
								value={draft.bearerToken}
								onChange={(e) =>
									onDraft({ ...draft, bearerToken: e.target.value })
								}
								placeholder={stored ? "已保存，留空保持不变" : undefined}
								disabled={saving}
							/>
						</Field>
					)}
					{mode === "apiKey" && (
						<div className="grid gap-3 sm:grid-cols-2">
							<Field>
								<FieldLabel htmlFor="webhook-api-key-name">
									请求头名称
								</FieldLabel>
								<Input
									id="webhook-api-key-name"
									value={draft.secretHeaders[0]?.key ?? ""}
									onChange={(e) =>
										onDraft({
											...draft,
											secretHeaders: draft.secretHeaders.map((row, at) =>
												at === 0 ? { ...row, key: e.target.value } : row,
											),
										})
									}
									disabled={saving}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="webhook-api-key">API Key</FieldLabel>
								<Input
									id="webhook-api-key"
									type="password"
									autoComplete="new-password"
									value={draft.apiKeyValue}
									onChange={(e) =>
										onDraft({ ...draft, apiKeyValue: e.target.value })
									}
									placeholder={stored ? "已保存，留空保持不变" : undefined}
									disabled={saving}
								/>
							</Field>
						</div>
					)}
					{mode === "custom" && (
						<SecretHeaderEditor
							draft={draft}
							onDraft={onDraft}
							saving={saving}
						/>
					)}
					<AdvancedSettings>
						<RowEditor
							title="字段映射（可选）"
							description="留空使用默认载荷；模板可用占位符 {recipient}、{code}、{delivery_id}、{expires_in_seconds}。"
							rows={draft.fields}
							onRows={(fields) => onDraft({ ...draft, fields })}
							keyLabel="字段路径"
							keyPlaceholder="mobile"
							valueLabel="模板"
							valuePlaceholder="{recipient}"
							addText="添加字段"
							disabled={saving}
						/>
						<RowEditor
							title="自定义请求头（可选）"
							description="随每次推送一起发送的额外请求头。"
							rows={draft.headers}
							onRows={(headers) => onDraft({ ...draft, headers })}
							keyLabel="请求头名称"
							keyPlaceholder="X-Source"
							valueLabel="请求头值"
							valuePlaceholder="quoin"
							addText="添加请求头"
							disabled={saving}
						/>

						<Field>
							<FieldLabel htmlFor="webhook-encoding">载荷编码</FieldLabel>
							<Select
								value={draft.encoding}
								onValueChange={(value) =>
									onDraft({ ...draft, encoding: value as "json" | "form" })
								}
								disabled={saving}
							>
								<SelectTrigger id="webhook-encoding" className="w-full">
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									<SelectGroup>
										<SelectItem value="json">JSON</SelectItem>
										<SelectItem value="form">表单</SelectItem>
									</SelectGroup>
								</SelectContent>
							</Select>
							<FieldDescription>
								按接收端要求选择 JSON 载荷或表单编码。
							</FieldDescription>
						</Field>
						<div className="grid gap-3 sm:grid-cols-2">
							<Field>
								<FieldLabel htmlFor="webhook-success-field">
									成功判定字段（可选）
								</FieldLabel>
								<Input
									id="webhook-success-field"
									value={draft.successField}
									onChange={(e) =>
										onDraft({ ...draft, successField: e.target.value })
									}
									disabled={saving}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="webhook-success-value">
									成功判定值（可选）
								</FieldLabel>
								<Input
									id="webhook-success-value"
									value={draft.successValue}
									onChange={(e) =>
										onDraft({ ...draft, successValue: e.target.value })
									}
									disabled={saving}
								/>
							</Field>
						</div>
						<FieldDescription>
							部分网关在 HTTP 200
							中返回业务失败；设置后按响应中的字段判断投递是否成功。
						</FieldDescription>
						<Field>
							<FieldLabel htmlFor="webhook-root-ca">
								私有根 CA PEM（可选）
							</FieldLabel>
							<Textarea
								id="webhook-root-ca"
								value={draft.rootCa}
								onChange={(e) => onDraft({ ...draft, rootCa: e.target.value })}
								placeholder={
									"-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----"
								}
								rows={3}
								className="font-mono text-xs"
								disabled={saving}
							/>
							<FieldDescription>
								私网网关使用私有 CA 签发证书时粘贴其根证书；留空仅信任系统根。
							</FieldDescription>
						</Field>
						<Field>
							<FieldLabel htmlFor="webhook-cidrs">
								允许的私网 CIDR（可选）
							</FieldLabel>
							<Input
								id="webhook-cidrs"
								value={draft.cidrs}
								onChange={(e) => onDraft({ ...draft, cidrs: e.target.value })}
								placeholder="172.16.0.0/12, 192.168.0.0/16"
								disabled={saving}
							/>
							<FieldDescription>
								私网网关默认被拒绝；需显式列出允许的网段，逗号分隔。公网地址不受影响。
							</FieldDescription>
						</Field>
					</AdvancedSettings>
					<Button type="submit" disabled={saving}>
						{saving ? "正在保存…" : "保存投递设置"}
					</Button>
				</FieldGroup>
			</form>
		</>
	);
}

function ReadOnlyDelivery({
	view,
	onDone,
}: {
	view: AuthDeliveryView;
	onDone: () => void;
}) {
	const entries: Array<[string, string | undefined]> = [
		["邮箱", channelSummary(view.configuration.email)],
		["短信", channelSummary(view.configuration.sms)],
	];
	return (
		<FieldGroup>
			<div className="flex flex-col items-center gap-1 text-center">
				<h1 className="text-2xl font-bold">验证码投递设置</h1>
				<p className="text-sm text-balance text-muted-foreground">
					投递配置由部署文件管理，此处只读。
				</p>
			</div>
			<Alert>
				<AlertDescription>
					当前配置来自部署文件，需要在部署配置中修改并重新部署后生效。
				</AlertDescription>
			</Alert>
			<div className="flex flex-col gap-1 text-sm">
				{entries.map(([label, summary]) => (
					<p key={label} className="text-muted-foreground">
						{summary ? `${label}：${summary}` : `${label}：未配置`}
					</p>
				))}
			</div>
			<Button variant="ghost" type="button" onClick={onDone}>
				返回继续初始化
			</Button>
		</FieldGroup>
	);
}

function LoadFailure({
	message,
	onRetry,
	onDone,
}: {
	message: string;
	onRetry: () => void;
	onDone: () => void;
}) {
	return (
		<FieldGroup>
			<h1 className="text-2xl font-bold">验证码投递设置</h1>
			<ErrorMessage>{message}</ErrorMessage>
			<div className="flex gap-2">
				<Button type="button" onClick={onRetry}>
					重试
				</Button>
				<Button variant="ghost" type="button" onClick={onDone}>
					返回继续初始化
				</Button>
			</div>
		</FieldGroup>
	);
}
