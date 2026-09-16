import type { AuthDeliveryChannel } from "./api";

/**
 * Pure model behind the delivery settings pane: provider presets, editable
 * form drafts and the draft → channel payload builders. Secret values are
 * write-only — drafts only hold freshly typed values, builders collect them
 * into a side map for the request, and stored secrets travel exclusively as
 * reference names.
 */

export const SMTP_PASSWORD_REF = "smtp_password";
export const WEBHOOK_API_KEY_HEADER = "X-Api-Key";

/**
 * Which channel slot a webhook configuration belongs to. Default secret
 * references are scoped per slot so two independently configured webhooks
 * never silently share one stored credential.
 */
export type SecretScope = "email" | "sms";

export const webhookDefaultRefs = (scope: SecretScope) => ({
	bearer: scope === "sms" ? "webhook_bearer" : "webhook_email_bearer",
	apiKey: scope === "sms" ? "webhook_api_key" : "webhook_email_api_key",
});

export type SmtpPresetId = "qq" | "163" | "126" | "custom";

export interface SmtpPreset {
	id: SmtpPresetId;
	label: string;
	/** The provider's correct TLS endpoint for 授权码 authentication. */
	host: string;
	port: number;
	tlsMode: "implicit" | "starttls";
}

/** Chinese mailbox providers ship SMTP behind implicit TLS on 465. */
export const SMTP_PRESETS: readonly SmtpPreset[] = [
	{
		id: "qq",
		label: "QQ 邮箱",
		host: "smtp.qq.com",
		port: 465,
		tlsMode: "implicit",
	},
	{
		id: "163",
		label: "163 邮箱",
		host: "smtp.163.com",
		port: 465,
		tlsMode: "implicit",
	},
	{
		id: "126",
		label: "126 邮箱",
		host: "smtp.126.com",
		port: 465,
		tlsMode: "implicit",
	},
	{ id: "custom", label: "自定义", host: "", port: 0, tlsMode: "starttls" },
];

export const smtpPresetById = (id: SmtpPresetId): SmtpPreset =>
	SMTP_PRESETS.find((preset) => preset.id === id) ?? SMTP_PRESETS[3];

/** Recognizes a loaded channel back into a preset; unknown endpoints stay custom. */
export const smtpPresetOf = (channel: AuthDeliveryChannel): SmtpPresetId =>
	SMTP_PRESETS.find(
		(preset) =>
			preset.id !== "custom" &&
			preset.host === channel.host &&
			preset.port === channel.port,
	)?.id ?? "custom";

/** Fills the provider endpoint; 自定义 keeps whatever the admin typed. */
export function smtpDraftWithPreset(
	draft: SmtpDraft,
	presetId: SmtpPresetId,
): SmtpDraft {
	const preset = smtpPresetById(presetId);
	if (preset.id === "custom") return { ...draft, preset: "custom" };
	return {
		...draft,
		preset: presetId,
		host: preset.host,
		port: String(preset.port),
		tlsMode: preset.tlsMode,
	};
}

export interface KVRow {
	id: number;
	key: string;
	value: string;
}

let rowSeq = 0;

/** New editable row; ids only matter as React keys. */
export const newRow = (key = "", value = ""): KVRow => ({
	id: ++rowSeq,
	key,
	value,
});

export const rowsOf = (record?: Record<string, string>): KVRow[] =>
	Object.entries(record ?? {}).map(([key, value]) => newRow(key, value));

/**
 * Rebuilds a record from rows: blank rows are dropped silently, but a named
 * duplicate or a value without a name is a real mistake worth reporting.
 */
export function recordOf(rows: KVRow[], label: string): Record<string, string> {
	const record: Record<string, string> = {};
	for (const row of rows) {
		const key = row.key.trim();
		if (!key) {
			if (row.value.trim()) throw new Error(`${label}里有未填名称的行。`);
			continue;
		}
		if (key in record)
			throw new Error(`${label}中 "${key}" 重复了，请合并或删除多余的行。`);
		record[key] = row.value;
	}
	return record;
}

export type WebhookAuthMode = "none" | "bearer" | "apiKey" | "custom";

/** Derives the simple auth selection from the secret header rows. */
export const authModeOf = (rows: KVRow[]): WebhookAuthMode => {
	if (rows.length === 0) return "none";
	if (rows.length > 1) return "custom";
	return rows[0].key.trim() === "Authorization" ? "bearer" : "apiKey";
};

/** Switching the explicit selection rewrites the rows it manages. */
export function webhookDraftWithAuthMode(
	draft: WebhookDraft,
	mode: WebhookAuthMode,
	scope: SecretScope,
): WebhookDraft {
	const refs = webhookDefaultRefs(scope);
	switch (mode) {
		case "none":
			return {
				...draft,
				authMode: "none",
				secretHeaders: [],
				bearerToken: "",
				apiKeyValue: "",
			};
		case "bearer": {
			// A stored Authorization reference keeps its name so its value stays.
			const ref =
				draft.secretHeaders
					.find((row) => row.key.trim() === "Authorization")
					?.value.trim() || refs.bearer;
			return {
				...draft,
				authMode: "bearer",
				secretHeaders: [newRow("Authorization", ref)],
				apiKeyValue: "",
			};
		}
		case "apiKey": {
			const existing = draft.secretHeaders[0];
			const name =
				existing?.key.trim() && existing.key.trim() !== "Authorization"
					? existing.key.trim()
					: WEBHOOK_API_KEY_HEADER;
			return {
				...draft,
				authMode: "apiKey",
				secretHeaders: [newRow(name, existing?.value.trim() || refs.apiKey)],
				bearerToken: "",
			};
		}
		case "custom":
			return { ...draft, authMode: "custom" };
	}
}

export const cidrsOf = (text: string): string[] =>
	text
		.split(/[,，\s]+/)
		.map((entry) => entry.trim())
		.filter(Boolean);

/** Authorization carries the scheme; the admin types just the token. */
export const bearerValueOf = (token: string): string =>
	/^bearer /i.test(token) ? token : `Bearer ${token}`;

export interface SmtpDraft {
	preset: SmtpPresetId;
	host: string;
	/** Kept as text so the input can be transiently empty while typing. */
	port: string;
	from: string;
	/** Write-only: freshly typed 授权码; blank keeps the stored secret. */
	password: string;
	/** Reference name stored server-side; preserved across saves. */
	passwordRef: string;
	username: string;
	tlsMode: "starttls" | "implicit";
	rootCa: string;
	cidrs: string;
}

export function smtpDraftFrom(channel?: AuthDeliveryChannel): SmtpDraft {
	return {
		// A fresh setup opens on QQ 邮箱: one mailbox plus an 授权码 is the
		// common path; 自定义 stays one select away.
		preset: channel ? smtpPresetOf(channel) : "qq",
		host: channel?.host ?? "",
		port: channel?.port ? String(channel.port) : "",
		from: channel?.from ?? "",
		password: "",
		passwordRef: channel?.passwordRef ?? "",
		username: channel?.username ?? "",
		tlsMode: channel?.tlsMode === "implicit" ? "implicit" : "starttls",
		rootCa: channel?.rootCaPem ?? "",
		cidrs: (channel?.allowPrivateCIDRs ?? []).join(", "),
	};
}

export function buildSmtpChannel(draft: SmtpDraft): {
	channel: AuthDeliveryChannel;
	secrets: Record<string, string>;
} {
	const preset = smtpPresetById(draft.preset);
	// For a known provider the preset table is the single source of truth for
	// the endpoint (a fresh QQ draft never needs the select touched); only
	// 自定义 reads the typed fields.
	const host = (preset.id === "custom" ? draft.host : preset.host).trim();
	if (!host) throw new Error("请填写 SMTP 服务器地址。");
	const from = draft.from.trim();
	if (!from) throw new Error("请填写发件邮箱。");
	const port = preset.id === "custom" ? Number(draft.port) : preset.port;
	if (!Number.isInteger(port) || port < 1 || port > 65535) {
		throw new Error("端口需要是 1–65535 之间的数字。");
	}
	const channel: AuthDeliveryChannel = {
		kind: "smtp",
		host,
		port,
		from,
		// Persist what the form displays so the saved view round-trips.
		tlsMode: preset.id === "custom" ? draft.tlsMode : preset.tlsMode,
	};
	const secrets: Record<string, string> = {};
	// A stored custom reference keeps its name; fresh setups get a fixed one.
	// A blank 授权码 keeps the stored secret, but the reference must still
	// travel — otherwise the saved config has a username without a password.
	const passwordRef =
		draft.passwordRef || (draft.password ? SMTP_PASSWORD_REF : "");
	if (passwordRef) {
		channel.passwordRef = passwordRef;
		if (draft.password) secrets[passwordRef] = draft.password;
	}
	const username = draft.username.trim();
	if (passwordRef) {
		// SMTP AUTH needs both halves; the sender address is the usual username.
		channel.username = username || from;
	} else if (username) {
		throw new Error("自定义用户名需要和授权码一起填写。");
	}
	const rootCa = draft.rootCa.trim();
	if (rootCa) channel.rootCaPem = rootCa;
	const cidrs = cidrsOf(draft.cidrs);
	if (cidrs.length > 0) channel.allowPrivateCIDRs = cidrs;
	return { channel, secrets };
}

export interface WebhookDraft {
	url: string;
	fields: KVRow[];
	headers: KVRow[];
	/** key = header name, value = secret reference name. */
	secretHeaders: KVRow[];
	/**
	 * Explicit form state, never re-derived from the rows: a single custom
	 * secret row would otherwise be misread as the simple API Key mode and
	 * silently ignore the per-row secret values.
	 */
	authMode: WebhookAuthMode;
	encoding: "json" | "form";
	successField: string;
	successValue: string;
	rootCa: string;
	cidrs: string;
	/** Write-only simple-auth inputs; blank keeps the stored secret. */
	bearerToken: string;
	apiKeyValue: string;
	/** Write-only values keyed by reference name for 自定义 rows. */
	secretValues: Record<string, string>;
	/** References that already have a stored value; blank inputs keep them. */
	storedRefs: readonly string[];
}

export function webhookDraftFrom(channel?: AuthDeliveryChannel): WebhookDraft {
	const secretHeaders = rowsOf(channel?.secretHeaders);
	return {
		url: channel?.url ?? "",
		fields: rowsOf(channel?.fields),
		headers: rowsOf(channel?.headers),
		secretHeaders,
		// The loaded channel picks its initial mode once; afterwards the form
		// state is authoritative.
		authMode: authModeOf(secretHeaders),
		encoding: channel?.encoding === "form" ? "form" : "json",
		successField: channel?.successField ?? "",
		successValue: channel?.successValue ?? "",
		rootCa: channel?.rootCaPem ?? "",
		cidrs: (channel?.allowPrivateCIDRs ?? []).join(", "),
		bearerToken: "",
		apiKeyValue: "",
		secretValues: {},
		storedRefs: [...new Set(Object.values(channel?.secretHeaders ?? {}))],
	};
}

export function buildWebhookChannel(
	draft: WebhookDraft,
	scope: SecretScope,
): { channel: AuthDeliveryChannel; secrets: Record<string, string> } {
	const url = draft.url.trim();
	if (!url) throw new Error("请填写 Webhook 地址。");
	// The backend only accepts a fixed HTTPS destination; say so up front.
	if (!/^https:\/\//i.test(url))
		throw new Error("Webhook 地址需要以 https:// 开头。");
	const channel: AuthDeliveryChannel = {
		kind: "webhook",
		url,
		encoding: draft.encoding,
	};
	const fields = recordOf(draft.fields, "字段映射");
	if (Object.keys(fields).length > 0) channel.fields = fields;
	const headers = recordOf(draft.headers, "请求头");
	if (Object.keys(headers).length > 0) channel.headers = headers;
	const secrets: Record<string, string> = {};
	/** A brand-new reference without a typed value can never pass server validation. */
	const requireValue = (ref: string, typed: string, label: string) => {
		if (!typed && !draft.storedRefs.includes(ref))
			throw new Error(`请填写${label}。`);
	};
	const defaults = webhookDefaultRefs(scope);
	// The mode is an explicit form state; freshly generated reference names
	// are scoped per channel so two webhooks never share one credential.
	if (draft.authMode === "bearer") {
		const ref = draft.secretHeaders[0]?.value.trim() || defaults.bearer;
		channel.secretHeaders = { Authorization: ref };
		requireValue(ref, draft.bearerToken, "访问令牌");
		if (draft.bearerToken) secrets[ref] = bearerValueOf(draft.bearerToken);
	} else if (draft.authMode === "apiKey") {
		const name = draft.secretHeaders[0]?.key.trim() || WEBHOOK_API_KEY_HEADER;
		const ref = draft.secretHeaders[0]?.value.trim() || defaults.apiKey;
		channel.secretHeaders = { [name]: ref };
		requireValue(ref, draft.apiKeyValue, "API Key");
		if (draft.apiKeyValue) secrets[ref] = draft.apiKeyValue;
	} else if (draft.authMode === "custom") {
		const refs = recordOf(draft.secretHeaders, "秘密请求头");
		if (Object.keys(refs).length > 0) {
			channel.secretHeaders = refs;
			for (const ref of Object.values(refs)) {
				requireValue(ref, draft.secretValues[ref] ?? "", `秘密值 "${ref}"`);
			}
			for (const [ref, value] of Object.entries(draft.secretValues)) {
				if (value) secrets[ref] = value;
			}
		}
	}
	if (draft.successField.trim())
		channel.successField = draft.successField.trim();
	if (draft.successValue.trim())
		channel.successValue = draft.successValue.trim();
	const rootCa = draft.rootCa.trim();
	if (rootCa) channel.rootCaPem = rootCa;
	const cidrs = cidrsOf(draft.cidrs);
	if (cidrs.length > 0) channel.allowPrivateCIDRs = cidrs;
	return { channel, secrets };
}

/** Per-channel editable drafts; the focused form decides which one saves. */
export interface DeliverySlot {
	smtp: SmtpDraft;
	webhook: WebhookDraft;
}

export function slotFrom(channel?: AuthDeliveryChannel): DeliverySlot {
	return {
		smtp: smtpDraftFrom(channel?.kind === "smtp" ? channel : undefined),
		webhook: webhookDraftFrom(
			channel?.kind === "webhook" ? channel : undefined,
		),
	};
}

/** Human summary for read-only views; carries no secrets, only endpoints. */
export const channelSummary = (
	channel: AuthDeliveryChannel | undefined,
): string | undefined => {
	if (!channel) return undefined;
	return channel.kind === "smtp"
		? `SMTP（${channel.host}:${channel.port}）`
		: `Webhook（${channel.url}）`;
};
