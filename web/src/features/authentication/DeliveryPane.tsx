import { type FormEvent, useEffect, useState } from "react";
import { WorkbenchApiError } from "@/api/workbench";
import { ErrorMessage, messageOf } from "@/app/shared";
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
import { Textarea } from "@/components/ui/textarea";
import {
	type AuthDeliveryChannel,
	type AuthDeliveryConfiguration,
	type AuthDeliveryView,
	authFlowApi,
} from "./api";

/**
 * Verification-code delivery settings (SMTP / outbound webhook), editable by
 * the admin initialization flow (or a signed-in admin). Secret values are
 * write-only: the server stores them by reference and the UI can never read
 * them back — blank secret inputs keep the stored value.
 */

const SMTP_PASSWORD_REF = "smtp_password";

const TLS_MODES = [
	{ value: "starttls", label: "STARTTLS（推荐）" },
	{ value: "implicit", label: "隐式 TLS" },
] as const;

const ENCODINGS = [
	{ value: "json", label: "JSON" },
	{ value: "form", label: "表单" },
] as const;

interface WebhookDraft {
	fieldsText: string;
	headersText: string;
	secretHeadersText: string;
	rootCaText: string;
	allowCidrsText: string;
}

interface ChannelDraft {
	enabled: boolean;
	channel: AuthDeliveryChannel;
	webhook: WebhookDraft;
}

const recordText = (record: Record<string, string> | undefined) =>
	record && Object.keys(record).length > 0
		? JSON.stringify(record, null, 2)
		: "";

/** Parses an advanced JSON mapping; empty text means "field not set". */
function parseRecord(text: string, label: string): Record<string, string> {
	const trimmed = text.trim();
	if (!trimmed) return {};
	let parsed: unknown;
	try {
		parsed = JSON.parse(trimmed);
	} catch {
		throw new Error(`${label} 不是有效的 JSON。`);
	}
	if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
		throw new Error(`${label} 需要一个 JSON 对象。`);
	}
	for (const [key, value] of Object.entries(parsed)) {
		if (typeof value !== "string") {
			throw new Error(`${label} 的 "${key}" 值必须是字符串。`);
		}
	}
	return parsed as Record<string, string>;
}

function draftFrom(
	channel: AuthDeliveryChannel | undefined,
	defaultKind: "smtp" | "webhook",
	defaultEnabled: boolean,
): ChannelDraft {
	return {
		enabled: channel !== undefined || defaultEnabled,
		channel: channel ?? { kind: defaultKind },
		webhook: {
			fieldsText: recordText(channel?.fields),
			headersText: recordText(channel?.headers),
			secretHeadersText: recordText(channel?.secretHeaders),
			rootCaText: channel?.rootCaPem ?? "",
			allowCidrsText: (channel?.allowPrivateCIDRs ?? []).join(", "),
		},
	};
}

function WebhookFields({
	idPrefix,
	channel,
	webhook,
	onChannel,
	onWebhook,
	disabled,
}: {
	idPrefix: string;
	channel: AuthDeliveryChannel;
	webhook: WebhookDraft;
	onChannel: (next: AuthDeliveryChannel) => void;
	onWebhook: (next: WebhookDraft) => void;
	disabled: boolean;
}) {
	const encoding = channel.encoding === "form" ? "form" : "json";
	return (
		<>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-url`}>Webhook 地址</FieldLabel>
				<Input
					id={`${idPrefix}-url`}
					type="url"
					value={channel.url ?? ""}
					onChange={(e) => onChannel({ ...channel, url: e.target.value })}
					placeholder="https://gateway.invalid/sms"
					required
					disabled={disabled}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-encoding`}>载荷编码</FieldLabel>
				<Select
					value={encoding}
					onValueChange={(value) => onChannel({ ...channel, encoding: value })}
					disabled={disabled}
				>
					<SelectTrigger id={`${idPrefix}-encoding`}>
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						{ENCODINGS.map((item) => (
							<SelectItem key={item.value} value={item.value}>
								{item.label}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
			</Field>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-fields`}>
					字段映射（高级，可选）
				</FieldLabel>
				<Textarea
					id={`${idPrefix}-fields`}
					value={webhook.fieldsText}
					onChange={(e) =>
						onWebhook({ ...webhook, fieldsText: e.target.value })
					}
					placeholder={`{\n  "mobile": "{recipient}",\n  "content": "{code}"\n}`}
					rows={4}
					className="font-mono text-xs"
					disabled={disabled}
				/>
				<FieldDescription>
					{`JSON 对象：推送字段路径 → 模板。可用占位符：{recipient}、{code}、{delivery_id}、{expires_in_seconds}。留空使用默认映射。`}
				</FieldDescription>
			</Field>
			<div className="grid grid-cols-2 gap-3">
				<Field>
					<FieldLabel htmlFor={`${idPrefix}-success-field`}>
						成功判定字段（可选）
					</FieldLabel>
					<Input
						id={`${idPrefix}-success-field`}
						value={channel.successField ?? ""}
						onChange={(e) =>
							onChannel({ ...channel, successField: e.target.value })
						}
						disabled={disabled}
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor={`${idPrefix}-success-value`}>
						成功判定值（可选）
					</FieldLabel>
					<Input
						id={`${idPrefix}-success-value`}
						value={channel.successValue ?? ""}
						onChange={(e) =>
							onChannel({ ...channel, successValue: e.target.value })
						}
						disabled={disabled}
					/>
				</Field>
			</div>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-headers`}>
					请求头 JSON（高级，可选）
				</FieldLabel>
				<Textarea
					id={`${idPrefix}-headers`}
					value={webhook.headersText}
					onChange={(e) =>
						onWebhook({ ...webhook, headersText: e.target.value })
					}
					placeholder={`{\n  "X-Source": "quoin"\n}`}
					rows={3}
					className="font-mono text-xs"
					disabled={disabled}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-secret-headers`}>
					秘密请求头 JSON（高级，可选）
				</FieldLabel>
				<Textarea
					id={`${idPrefix}-secret-headers`}
					value={webhook.secretHeadersText}
					onChange={(e) =>
						onWebhook({ ...webhook, secretHeadersText: e.target.value })
					}
					placeholder={`{\n  "X-Api-Key": "sms_api_key"\n}`}
					rows={3}
					className="font-mono text-xs"
					disabled={disabled}
				/>
				<FieldDescription>
					请求头名 →
					秘密引用名；实际秘密值在下方"秘密值"中填写，保存后不可读取。
				</FieldDescription>
			</Field>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-root-ca`}>
					私有根 CA 证书 PEM（高级，可选）
				</FieldLabel>
				<Textarea
					id={`${idPrefix}-root-ca`}
					value={webhook.rootCaText}
					onChange={(e) =>
						onWebhook({ ...webhook, rootCaText: e.target.value })
					}
					placeholder={
						"-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----"
					}
					rows={3}
					className="font-mono text-xs"
					disabled={disabled}
				/>
				<FieldDescription>
					私网网关使用私有 CA 签发证书时粘贴其根证书；留空仅信任系统根。
				</FieldDescription>
			</Field>
			<Field>
				<FieldLabel htmlFor={`${idPrefix}-allow-cidrs`}>
					允许的私网 CIDR（高级，可选）
				</FieldLabel>
				<Input
					id={`${idPrefix}-allow-cidrs`}
					value={webhook.allowCidrsText}
					onChange={(e) =>
						onWebhook({ ...webhook, allowCidrsText: e.target.value })
					}
					placeholder="172.16.0.0/12, 192.168.0.0/16"
					disabled={disabled}
				/>
				<FieldDescription>
					私网 Webhook 网关默认被拒绝；需显式列出允许的
					CIDR，逗号分隔。公网地址不受影响。
				</FieldDescription>
			</Field>
		</>
	);
}

export function DeliveryPane({
	onDone,
	onFlowGone,
}: {
	onDone: () => void;
	onFlowGone: () => void;
}) {
	const [view, setView] = useState<AuthDeliveryView>();
	// Email starts on SMTP so a first-run admin only fills in their server;
	// SMS stays off unless explicitly chosen (webhook only).
	const [email, setEmail] = useState<ChannelDraft>(() =>
		draftFrom(undefined, "smtp", true),
	);
	const [sms, setSms] = useState<ChannelDraft>(() =>
		draftFrom(undefined, "webhook", false),
	);
	const [smtpPassword, setSmtpPassword] = useState("");
	const [secretValues, setSecretValues] = useState<Record<string, string>>({});
	const [advancedErrors, setAdvancedErrors] = useState<{
		email: string;
		sms: string;
	}>({ email: "", sms: "" });
	const [loadError, setLoadError] = useState("");
	const [error, setError] = useState("");
	const [note, setNote] = useState("");
	const [saving, setSaving] = useState(false);
	useEffect(() => {
		let cancelled = false;
		authFlowApi.readDelivery().then(
			(loaded) => {
				if (cancelled) return;
				setView(loaded);
				setEmail(draftFrom(loaded.configuration.email, "smtp", true));
				setSms(draftFrom(loaded.configuration.sms, "webhook", false));
			},
			(reason) => {
				if (cancelled) return;
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
		return () => {
			cancelled = true;
		};
	}, [onFlowGone]);
	const readOnly = view?.source === "deployment";
	if (readOnly) {
		return (
			<FieldGroup>
				<div className="flex flex-col items-center gap-1 text-center">
					<h1 className="text-2xl font-bold">验证码投递设置</h1>
					<p className="text-sm text-balance text-muted-foreground">
						投递配置由部署文件管理，此处只读。
					</p>
				</div>
				<ErrorMessage>
					当前配置来自部署文件，需要在部署配置中修改。
				</ErrorMessage>
				<pre className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs">
					{JSON.stringify(view?.configuration ?? {}, null, 2)}
				</pre>
				<Button variant="ghost" type="button" onClick={onDone}>
					返回继续初始化
				</Button>
			</FieldGroup>
		);
	}
	/** Reference names that need a (write-only) secret value input. */
	const secretRefs = new Set<string>();
	for (const draft of [email, sms]) {
		if (!draft.enabled || draft.channel.kind !== "webhook") continue;
		try {
			for (const ref of Object.values(
				parseRecord(draft.webhook.secretHeadersText, "秘密请求头"),
			)) {
				secretRefs.add(ref);
			}
		} catch {
			/* surfaced next to the JSON field on save */
		}
	}
	function applyWebhook(draft: ChannelDraft): AuthDeliveryChannel | undefined {
		if (!draft.enabled) return undefined;
		const channel = { ...draft.channel };
		// A private CA applies to both SMTP TLS and webhook HTTPS targets; an
		// empty textarea clears the override (system roots only).
		const rootCa = draft.webhook.rootCaText.trim();
		if (rootCa) channel.rootCaPem = rootCa;
		else delete channel.rootCaPem;
		// Reaching a private-range gateway must be an explicit, per-channel
		// decision — the backend denies private destinations otherwise.
		const cidrs = draft.webhook.allowCidrsText
			.split(/[,，\s]+/)
			.map((entry) => entry.trim())
			.filter(Boolean);
		if (cidrs.length > 0) channel.allowPrivateCIDRs = cidrs;
		else delete channel.allowPrivateCIDRs;
		if (channel.kind === "smtp") {
			// Persist what the TLS select already displays (server default match).
			if (!channel.tlsMode) channel.tlsMode = "starttls";
		} else {
			const fields = parseRecord(draft.webhook.fieldsText, "字段映射");
			const headers = parseRecord(draft.webhook.headersText, "请求头");
			const secretHeaders = parseRecord(
				draft.webhook.secretHeadersText,
				"秘密请求头",
			);
			for (const [record, key] of [
				[fields, "fields"],
				[headers, "headers"],
				[secretHeaders, "secretHeaders"],
			] as const) {
				if (Object.keys(record).length > 0) {
					channel[key] = record;
				} else {
					delete channel[key];
				}
			}
		}
		return channel;
	}
	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		setNote("");
		let emailChannel: AuthDeliveryChannel | undefined;
		let smsChannel: AuthDeliveryChannel | undefined;
		try {
			emailChannel = applyWebhook(email);
		} catch (reason) {
			setAdvancedErrors((current) => ({
				...current,
				email: messageOf(reason, "邮箱渠道的高级 JSON 无效。"),
			}));
			return;
		}
		try {
			smsChannel = applyWebhook(sms);
		} catch (reason) {
			setAdvancedErrors((current) => ({
				...current,
				sms: messageOf(reason, "短信渠道的高级 JSON 无效。"),
			}));
			return;
		}
		setAdvancedErrors({ email: "", sms: "" });
		if (!emailChannel && !smsChannel) {
			setError("至少需要配置一个投递渠道，或先返回跳过。");
			return;
		}
		const secrets: Record<string, string> = {};
		if (emailChannel?.kind === "smtp" && smtpPassword) {
			// A stored custom reference keeps its name; fresh setups get a fixed one.
			const passwordRef = emailChannel.passwordRef || SMTP_PASSWORD_REF;
			secrets[passwordRef] = smtpPassword;
			emailChannel.passwordRef = passwordRef;
		}
		for (const [name, value] of Object.entries(secretValues)) {
			if (value) secrets[name] = value;
		}
		setSaving(true);
		try {
			const saved = await authFlowApi.saveDelivery({
				configuration: {
					...(emailChannel && { email: emailChannel }),
					...(smsChannel && { sms: smsChannel }),
				} satisfies AuthDeliveryConfiguration,
				secrets,
				expectedRowVersion: view?.rowVersion ?? 0,
			});
			setView(saved);
			setEmail(draftFrom(saved.configuration.email, "smtp", true));
			setSms(draftFrom(saved.configuration.sms, "webhook", false));
			setSmtpPassword("");
			setSecretValues({});
			setNote("投递设置已保存。");
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
	return (
		<form className="flex flex-col gap-6" onSubmit={submit}>
			<FieldGroup>
				<div className="flex flex-col items-center gap-1 text-center">
					<h1 className="text-2xl font-bold">验证码投递设置</h1>
					<p className="text-sm text-balance text-muted-foreground">
						配置验证码的发送渠道；至少配置一个后才能接收验证码。
					</p>
				</div>
				{loadError && <ErrorMessage>{loadError}</ErrorMessage>}
				{error && <ErrorMessage>{error}</ErrorMessage>}
				{note && (
					<div role="status" className="text-sm text-muted-foreground">
						{note}
					</div>
				)}
				<Field>
					<FieldLabel htmlFor="email-kind">邮箱渠道</FieldLabel>
					<Select
						value={email.enabled ? email.channel.kind : "off"}
						onValueChange={(value) =>
							setEmail((draft) => ({
								...draft,
								enabled: value !== "off",
								channel:
									value === "off"
										? draft.channel
										: { ...draft.channel, kind: value as "smtp" | "webhook" },
							}))
						}
						disabled={saving}
					>
						<SelectTrigger id="email-kind">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value="off">不配置</SelectItem>
							<SelectItem value="smtp">SMTP 邮件</SelectItem>
							<SelectItem value="webhook">出站 Webhook</SelectItem>
						</SelectContent>
					</Select>
				</Field>
				{email.enabled && email.channel.kind === "smtp" && (
					<>
						<div className="grid grid-cols-[1fr_8rem] gap-3">
							<Field>
								<FieldLabel htmlFor="email-smtp-host">SMTP 服务器</FieldLabel>
								<Input
									id="email-smtp-host"
									value={email.channel.host ?? ""}
									onChange={(e) =>
										setEmail((draft) => ({
											...draft,
											channel: { ...draft.channel, host: e.target.value },
										}))
									}
									placeholder="smtp.example.com"
									required
									disabled={saving}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="email-smtp-port">端口</FieldLabel>
								<Input
									id="email-smtp-port"
									type="number"
									min={1}
									max={65535}
									value={
										email.channel.port === undefined
											? ""
											: String(email.channel.port)
									}
									onChange={(e) =>
										setEmail((draft) => {
											const port = Number(e.target.value);
											return {
												...draft,
												channel: {
													...draft.channel,
													port:
														e.target.value !== "" &&
														Number.isInteger(port) &&
														port > 0
															? port
															: undefined,
												},
											};
										})
									}
									placeholder="587"
									required
									disabled={saving}
								/>
							</Field>
						</div>
						<Field>
							<FieldLabel htmlFor="email-smtp-from">发件地址</FieldLabel>
							<Input
								id="email-smtp-from"
								type="email"
								value={email.channel.from ?? ""}
								onChange={(e) =>
									setEmail((draft) => ({
										...draft,
										channel: { ...draft.channel, from: e.target.value },
									}))
								}
								placeholder="quoin@example.com"
								required
								disabled={saving}
							/>
						</Field>
						<div className="grid grid-cols-2 gap-3">
							<Field>
								<FieldLabel htmlFor="email-smtp-username">
									SMTP 用户名（可选）
								</FieldLabel>
								<Input
									id="email-smtp-username"
									autoComplete="off"
									value={email.channel.username ?? ""}
									onChange={(e) =>
										setEmail((draft) => ({
											...draft,
											channel: { ...draft.channel, username: e.target.value },
										}))
									}
									disabled={saving}
								/>
							</Field>
							<Field>
								<FieldLabel htmlFor="email-smtp-password">
									SMTP 密码（可选）
								</FieldLabel>
								<Input
									id="email-smtp-password"
									type="password"
									autoComplete="new-password"
									value={smtpPassword}
									onChange={(e) => setSmtpPassword(e.target.value)}
									placeholder={
										email.channel.passwordRef
											? "已保存，留空保持不变"
											: undefined
									}
									disabled={saving}
								/>
							</Field>
						</div>
						<Field>
							<FieldLabel htmlFor="email-smtp-tls">TLS 模式</FieldLabel>
							<Select
								value={
									email.channel.tlsMode === "implicit" ? "implicit" : "starttls"
								}
								onValueChange={(value) =>
									setEmail((draft) => ({
										...draft,
										channel: { ...draft.channel, tlsMode: value },
									}))
								}
								disabled={saving}
							>
								<SelectTrigger id="email-smtp-tls">
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									{TLS_MODES.map((item) => (
										<SelectItem key={item.value} value={item.value}>
											{item.label}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</Field>
						<Field>
							<FieldLabel htmlFor="email-smtp-root-ca">
								SMTP 私有根 CA PEM（高级，可选）
							</FieldLabel>
							<Textarea
								id="email-smtp-root-ca"
								value={email.webhook.rootCaText}
								onChange={(e) =>
									setEmail((draft) => ({
										...draft,
										webhook: { ...draft.webhook, rootCaText: e.target.value },
									}))
								}
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
							<FieldLabel htmlFor="email-smtp-allow-cidrs">
								允许的私网 CIDR（高级，可选）
							</FieldLabel>
							<Input
								id="email-smtp-allow-cidrs"
								value={email.webhook.allowCidrsText}
								onChange={(e) =>
									setEmail((draft) => ({
										...draft,
										webhook: {
											...draft.webhook,
											allowCidrsText: e.target.value,
										},
									}))
								}
								placeholder="172.16.0.0/12, 192.168.0.0/16"
								disabled={saving}
							/>
							<FieldDescription>
								私网 SMTP 网关默认被拒绝；需显式列出允许的
								CIDR，逗号分隔。公网地址不受影响。
							</FieldDescription>
						</Field>
					</>
				)}
				{email.enabled && email.channel.kind === "webhook" && (
					<WebhookFields
						idPrefix="email"
						channel={email.channel}
						webhook={email.webhook}
						onChannel={(channel) =>
							setEmail((draft) => ({ ...draft, channel }))
						}
						onWebhook={(webhook) =>
							setEmail((draft) => ({ ...draft, webhook }))
						}
						disabled={saving}
					/>
				)}
				{advancedErrors.email && (
					<ErrorMessage>{advancedErrors.email}</ErrorMessage>
				)}
				<Field>
					<FieldLabel htmlFor="sms-kind">短信渠道</FieldLabel>
					<Select
						value={sms.enabled ? "webhook" : "off"}
						onValueChange={(value) =>
							setSms((draft) => ({
								...draft,
								enabled: value !== "off",
								channel: { ...draft.channel, kind: "webhook" },
							}))
						}
						disabled={saving}
					>
						<SelectTrigger id="sms-kind">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							<SelectItem value="off">不配置</SelectItem>
							<SelectItem value="webhook">出站 Webhook</SelectItem>
						</SelectContent>
					</Select>
					<FieldDescription>短信只支持出站 Webhook 发送。</FieldDescription>
				</Field>
				{sms.enabled && (
					<WebhookFields
						idPrefix="sms"
						channel={sms.channel}
						webhook={sms.webhook}
						onChannel={(channel) => setSms((draft) => ({ ...draft, channel }))}
						onWebhook={(webhook) => setSms((draft) => ({ ...draft, webhook }))}
						disabled={saving}
					/>
				)}
				{advancedErrors.sms && (
					<ErrorMessage>{advancedErrors.sms}</ErrorMessage>
				)}
				{secretRefs.size > 0 && (
					<FieldGroup>
						<FieldLabel>秘密值</FieldLabel>
						<FieldDescription>
							保存后秘密值加密存储，只能覆盖、不能读取；留空保持已保存的值。
						</FieldDescription>
						{[...secretRefs].map((ref) => (
							<Field key={ref}>
								<FieldLabel htmlFor={`secret-${ref}`}>{ref}</FieldLabel>
								<Input
									id={`secret-${ref}`}
									type="password"
									autoComplete="new-password"
									value={secretValues[ref] ?? ""}
									onChange={(e) =>
										setSecretValues((values) => ({
											...values,
											[ref]: e.target.value,
										}))
									}
									disabled={saving}
								/>
							</Field>
						))}
					</FieldGroup>
				)}
				<Button type="submit" disabled={saving}>
					{saving ? "正在保存…" : "保存投递设置"}
				</Button>
				<Button
					variant="ghost"
					type="button"
					onClick={onDone}
					disabled={saving}
				>
					返回继续初始化
				</Button>
			</FieldGroup>
		</form>
	);
}
