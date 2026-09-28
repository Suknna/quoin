import { newClientCommandId, request } from "@/api/workbench";
import {
	type AlertSourceCredentialMetadata,
	type CreateAlertSourceRequest,
	createAlertSource,
	revealCredential,
} from "@/features/alerts/api";
import {
	disableConnection,
	enableConnection,
	rotateConnection,
} from "@/features/settings/platform/connections/api";
import type {
	ConnectionAuthMode,
	IntegrationCatalogItem,
	IntegrationInstance,
} from "./types";

export async function listIntegrationPlugins(): Promise<
	IntegrationCatalogItem[]
> {
	const page = await request<{ items: IntegrationCatalogItem[] }>(
		"/api/v1/integrations/plugins",
	);
	return page.items;
}

export interface PluginEventDeadletter {
	deliveryId: number;
	eventId: number;
	subscriberId: string;
	attempts: number;
	lastError: string;
}

export async function listPluginEventDeadletters(): Promise<{ count: number; items: PluginEventDeadletter[] }> {
	return request<{ count: number; items: PluginEventDeadletter[] }>("/api/v1/integrations/plugin-events/deadletters");
}

export async function replayPluginEventDeadletter(deliveryId: number, clientCommandId: string): Promise<void> {
	await request<void>(`/api/v1/integrations/plugin-events/deadletters/${deliveryId}/replay`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ clientCommandId }),
	});
}

export type MetricsPlatform = "prometheus" | "thanos";
export type MetricsAuthMode = ConnectionAuthMode;

/** One terminal observation of a real connection probe. The typed result
 * carries the concrete failure diagnostic; secrets never appear in it. */
export interface ConnectionProbeObservation {
	id?: string;
	outcome: "passed" | "failed" | "cancelled" | "interrupted";
	finishedAt?: string;
	details?: Record<string, unknown>;
}

/** Non-secret metrics projection. The server intentionally never returns passwords or bearer tokens. */
export interface MetricsInstance extends IntegrationInstance {
	id: string;
	platform: MetricsPlatform;
	status: "active" | "revalidation_required" | "disabled";
	revalidationRequired: boolean;
	rowVersion: number;
	endpoint?: string;
	authType: MetricsAuthMode;
	username?: string;
	tlsCaPem?: string;
	tlsServerName?: string;
	tlsSkipVerify: boolean;
	lastProbe?: ConnectionProbeObservation;
}

/** Matches the frozen connectionConfigInput contract for Prometheus and Thanos. */
export interface MetricsConnectionInput {
	type: MetricsPlatform;
	baseUrl: string;
	authType: MetricsAuthMode;
	username?: string;
	password?: string;
	bearerToken?: string;
	tlsCaPem?: string;
	tlsServerName?: string;
	tlsSkipVerify?: boolean;
}

function metricsInstance(value: object): MetricsInstance {
	const projection = value as Record<string, unknown>;
	const platform = projection.type === "prometheus" ? "prometheus" : "thanos";
	const config = projection.config as Record<string, unknown> | undefined;
	return {
		// Server `id` is the numeric DB primary key; every connection read
		// (get/probe/enable/disable/rotate) is keyed by the stable `name`.
		// Route parameters and API paths must carry `displayName`, never `id`.
		id: String(projection.id ?? projection.name),
		platform,
		displayName: String(projection.name ?? projection.id),
		// A rotated metrics connection can remain enabled while intentionally
		// withheld from dispatch until a fresh exact-pair probe requalifies it.
		status: parseStatus(projection),
		revalidationRequired: projection.revalidationRequired === true,
		rowVersion: Number(projection.rowVersion ?? 0),
		endpoint: typeof config?.baseUrl === "string" ? config.baseUrl : undefined,
		authType: parseAuthType(config?.authType),
		username:
			typeof config?.username === "string" ? config.username : undefined,
		tlsCaPem:
			typeof config?.tlsCaPem === "string" ? config.tlsCaPem : undefined,
		tlsServerName:
			typeof config?.tlsServerName === "string"
				? config.tlsServerName
				: undefined,
		tlsSkipVerify: config?.tlsSkipVerify === true,
		lastProbe: projection.lastProbe as MetricsInstance["lastProbe"],
	};
}

function parseStatus(value: Record<string, unknown>): MetricsInstance["status"] {
	// A rotated metrics connection can remain enabled while intentionally
	// withheld from dispatch until a fresh exact-pair probe requalifies it.
	if (value.revalidationRequired === true) return "revalidation_required";
	return value.enabled === true ? "active" : "disabled";
}

function parseAuthType(value: unknown): MetricsAuthMode {
	return value === "basic" ? "basic" : value === "bearer" ? "bearer" : "none";
}

export async function listMetricsInstances(
	platform?: MetricsPlatform,
): Promise<MetricsInstance[]> {
	const page = await request<{ items?: Record<string, unknown>[] }>(
		"/api/v1/connections?limit=100",
	);
	return (page.items ?? [])
		.filter((item) => item.type === "prometheus" || item.type === "thanos")
		.map(metricsInstance)
		.filter((item) => !platform || item.platform === platform);
}

export async function fetchMetricsInstance(
	name: string,
): Promise<MetricsInstance> {
	return metricsInstance(
		await request<Record<string, unknown>>(
			`/api/v1/connections/${encodeURIComponent(name)}`,
		),
	);
}

/** Creates a disabled connection. Callers must explicitly probe and enable it. */
export async function createMetricsInstance(
	name: string,
	connection: MetricsConnectionInput,
): Promise<MetricsInstance> {
	return metricsInstance(
		await request<Record<string, unknown>>("/api/v1/connections", {
			method: "POST",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				name,
				connection,
			}),
		}),
	);
}

/** Polls the authoritative attempt endpoint only until its terminal state is visible.
 * Shared by the specialized metrics forms and the generic plugin HTTP connections. */
async function pollConnectionProbe(
	name: string,
): Promise<ConnectionProbeObservation> {
	const attempt = await request<{ id: string }>(
		`/api/v1/connections/${encodeURIComponent(name)}/probe`,
		{
			method: "POST",
			body: JSON.stringify({ clientCommandId: newClientCommandId() }),
		},
	);
	for (let poll = 0; poll < 240; poll += 1) {
		const detail = await request<{
			state: string;
			endedAt?: string;
			terminationReason?: string;
		}>(
			`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attempt.id)}`,
		);
		if (
			["Succeeded", "Failed", "Cancelled", "Interrupted"].includes(detail.state)
		) {
			const outcome =
				detail.state === "Succeeded"
					? ("passed" as const)
					: detail.state === "Cancelled"
						? ("cancelled" as const)
						: detail.state === "Interrupted"
							? ("interrupted" as const)
							: ("failed" as const);
			if (outcome !== "passed") {
				// The typed result carries the concrete failure diagnostic (e.g. the
				// gateway error string); the attempt's terminationReason is only the
				// coarse terminal class, so prefer the typed details when present.
				const typed = await probeResultDetails(name, attempt.id);
				return {
					outcome,
					finishedAt: detail.endedAt,
					details:
						typed ??
						(detail.terminationReason
							? { reason: detail.terminationReason }
							: undefined),
				};
			}
			const results = await request<{
				items?: { id: string; attemptId: string; outcome: string }[];
			}>(
				`/api/v1/connections/${encodeURIComponent(name)}/probe-results?limit=50`,
			);
			const qualified = results.items?.find(
				(result) =>
					result.attemptId === attempt.id && result.outcome === "passed",
			);
			if (!qualified)
				return {
					outcome: "failed",
					finishedAt: detail.endedAt,
					details: { reason: "qualified_probe_result_missing" },
				};
			return { id: qualified.id, outcome, finishedAt: detail.endedAt };
		}
		await new Promise((resolve) => setTimeout(resolve, 500));
	}
	throw new Error("验证仍在进行，可稍后在接入详情查看结果；本次等待结束不代表验证失败。");
}

/** Polls the authoritative attempt endpoint only until its terminal state is visible. */
export async function probeMetricsInstance(
	name: string,
): Promise<MetricsInstance["lastProbe"]> {
	return pollConnectionProbe(name);
}

/** Reads the typed probe-result details of one attempt; undefined when absent. */
async function probeResultDetails(
	name: string,
	attemptId: string,
): Promise<Record<string, unknown> | undefined> {
	const results = await request<{
		items?: { attemptId: string; details?: Record<string, unknown> }[];
	}>(`/api/v1/connections/${encodeURIComponent(name)}/probe-results?limit=50`);
	const matched = results.items?.find(
		(result) => result.attemptId === attemptId,
	);
	return matched?.details;
}

/** Extracts the human-facing diagnostic line from typed probe details, if any. */
export function probeDiagnostic(
	details: Record<string, unknown> | undefined,
): string | undefined {
	const error = details?.error;
	if (typeof error === "string" && error.trim()) return error.trim();
	const reason = details?.reason;
	if (typeof reason === "string" && reason.trim()) return reason.trim();
	return undefined;
}

/** Reuses the shared connection command wrappers, preserving row-version fencing. */
export async function enableMetricsInstance(
	instance: MetricsInstance,
	qualifiedProbeResultId: string,
): Promise<MetricsInstance> {
	return metricsInstance(
		(await enableConnection(
			instance.displayName,
			instance.rowVersion,
			qualifiedProbeResultId,
		)) as unknown as Record<string, unknown>,
	);
}
export async function disableMetricsInstance(
	instance: MetricsInstance,
): Promise<MetricsInstance> {
	return metricsInstance(
		(await disableConnection(
			instance.displayName,
			instance.rowVersion,
		)) as unknown as Record<string, unknown>,
	);
}
export async function rotateMetricsInstance(
	instance: MetricsInstance,
	connection: MetricsConnectionInput,
): Promise<MetricsInstance> {
	return metricsInstance(
		await rotateConnection(instance.displayName, instance.rowVersion, {
			...connection,
		}),
	);
}

// ---------------------------------------------------------------------------
// Generic plugin HTTP connections (#110, ADR-0014): every registry-declared
// connection kind whose plugin catalog entry carries the http_connection
// capability shares this bounded CRUD surface over /api/v1/connections. The
// server fails closed on unknown or revoked (plugin disabled) kinds; the UI
// mirrors that by deriving the kind from the enabled catalog only.
// ---------------------------------------------------------------------------

/** Non-secret projection of one generic plugin HTTP connection instance.
 * `platform` is the plugin-registered connection kind — never the plugin ID. */
export interface HttpConnectionInstance {
	id: string;
	platform: string;
	displayName: string;
	status: "active" | "revalidation_required" | "disabled";
	revalidationRequired: boolean;
	rowVersion: number;
	endpoint?: string;
	authType: ConnectionAuthMode;
	username?: string;
	tlsCaPem?: string;
	tlsServerName?: string;
	tlsSkipVerify: boolean;
	lastProbe?: ConnectionProbeObservation;
}

function httpConnectionInstance(value: object): HttpConnectionInstance {
	const projection = value as Record<string, unknown>;
	const config = projection.config as Record<string, unknown> | undefined;
	return {
		id: String(projection.id ?? projection.name),
		platform: String(projection.type ?? ""),
		displayName: String(projection.name ?? projection.id),
		status: parseStatus(projection),
		revalidationRequired: projection.revalidationRequired === true,
		rowVersion: Number(projection.rowVersion ?? 0),
		endpoint: typeof config?.baseUrl === "string" ? config.baseUrl : undefined,
		authType: parseAuthType(config?.authType),
		username:
			typeof config?.username === "string" ? config.username : undefined,
		tlsCaPem:
			typeof config?.tlsCaPem === "string" ? config.tlsCaPem : undefined,
		tlsServerName:
			typeof config?.tlsServerName === "string"
				? config.tlsServerName
				: undefined,
		tlsSkipVerify: config?.tlsSkipVerify === true,
		lastProbe: projection.lastProbe as HttpConnectionInstance["lastProbe"],
	};
}

export interface HttpConnectionInstancePage {
	items: HttpConnectionInstance[];
	nextCursor?: string;
}

/** Lists one cursor page of the generic HTTP connection instances of the given
 * plugin-declared kinds; the kinds always come from the enabled server catalog.
 * The server paginates all connections by name with no kind filter
 * (HTTP-PAGE-001), so each page is filtered client-side while `nextCursor` is
 * always passed through verbatim: a page with zero matches may still be
 * followed by pages containing matches, and callers must offer continued
 * navigation instead of rendering an empty state. */
export async function listHttpConnectionInstances(
	connectionKinds: string[],
	cursor?: string,
): Promise<HttpConnectionInstancePage> {
	if (connectionKinds.length === 0) return { items: [] };
	const query = new URLSearchParams({ limit: "100" });
	if (cursor) query.set("cursor", cursor);
	const page = await request<{
		items?: Record<string, unknown>[];
		nextCursor?: string;
	}>(`/api/v1/connections?${query}`);
	return {
		items: (page.items ?? [])
			.filter((item) => connectionKinds.includes(String(item.type)))
			.map(httpConnectionInstance),
		nextCursor: page.nextCursor,
	};
}

export async function fetchHttpConnectionInstance(
	name: string,
): Promise<HttpConnectionInstance> {
	return httpConnectionInstance(
		await request<Record<string, unknown>>(
			`/api/v1/connections/${encodeURIComponent(name)}`,
		),
	);
}

/** Matches the frozen connectionConfigInput contract for registry-declared
 * HTTP kinds: credentials ride request-only and the server re-verifies the
 * auth-mode membership against the plugin declaration. */
export interface HttpConnectionInput {
	type: string;
	baseUrl: string;
	authType: ConnectionAuthMode;
	username?: string;
	password?: string;
	bearerToken?: string;
	tlsCaPem?: string;
	tlsServerName?: string;
	tlsSkipVerify?: boolean;
}

/** Creates a disabled connection of one generic kind. Callers must run the
 * plugin-declared probe and explicitly enable it; a kind without a declared
 * probe path can never enable new instances. */
export async function createHttpConnectionInstance(
	name: string,
	connection: HttpConnectionInput,
): Promise<HttpConnectionInstance> {
	return httpConnectionInstance(
		await request<Record<string, unknown>>("/api/v1/connections", {
			method: "POST",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				name,
				connection,
			}),
		}),
	);
}

export async function probeHttpConnectionInstance(
	name: string,
): Promise<ConnectionProbeObservation> {
	return pollConnectionProbe(name);
}

/** Reuses the shared connection command wrappers, preserving row-version fencing. */
export async function enableHttpConnectionInstance(
	instance: HttpConnectionInstance,
	qualifiedProbeResultId: string,
): Promise<HttpConnectionInstance> {
	return httpConnectionInstance(
		(await enableConnection(
			instance.displayName,
			instance.rowVersion,
			qualifiedProbeResultId,
		)) as unknown as Record<string, unknown>,
	);
}

export async function disableHttpConnectionInstance(
	instance: HttpConnectionInstance,
): Promise<HttpConnectionInstance> {
	return httpConnectionInstance(
		(await disableConnection(
			instance.displayName,
			instance.rowVersion,
		)) as unknown as Record<string, unknown>,
	);
}

export async function rotateHttpConnectionInstance(
	instance: HttpConnectionInstance,
	connection: HttpConnectionInput,
): Promise<HttpConnectionInstance> {
	return httpConnectionInstance(
		await rotateConnection(instance.displayName, instance.rowVersion, {
			...connection,
		}),
	);
}

interface AlertSourceProjection {
	key: string;
	// Source kind; equals the owning plugin's catalog id. The server accepts
	// every registered + enabled plugin with EventSource and AlertNormalizer.
	protocol: string;
	enabled: boolean;
	rowVersion: number;
	createdAt?: string;
	latestValidEventAt?: string | null;
	/** 来源实例的非秘密设置权威文档（ADR-0014 story 2）；始终至少为 `{}`。 */
	settings?: Record<string, unknown>;
}
interface AlertCredentialProjection {
	id: string;
	rowVersion: number;
	state?: string;
	createdAt?: string;
	firstUsedAt?: string | null;
}
/** One configured alert event source of any registered source kind. The
 * protocol (source kind) equals the owning plugin's catalog id; specialized
 * admin UIs exist only for alertmanager and the metrics platforms. */
export interface EventSourceInstance extends IntegrationInstance {
	platform: string;
	status: "active" | "disabled";
	rowVersion: number;
	/** Non-secret per-instance settings; the server's authoritative document. */
	settings?: Record<string, unknown>;
}
export interface EventSourceCredential {
	id: string;
	rowVersion: number;
	state: string;
	createdAt?: string;
	firstUsedAt?: string | null;
}
export interface PublicReceiverEndpoint {
	publicReceiverUrl: string;
}
function instance(source: AlertSourceProjection): EventSourceInstance {
	return {
		id: source.key,
		platform: source.protocol,
		displayName: source.key,
		status: source.enabled ? "active" : "disabled",
		createdAt: source.createdAt,
		latestValidEventAt: source.latestValidEventAt,
		rowVersion: source.rowVersion,
		settings: source.settings,
	};
}
export interface EventSourceInstancePage {
	items: EventSourceInstance[];
	nextCursor?: string;
}
export async function listEventSourceInstances(
	cursor?: string,
): Promise<EventSourceInstancePage> {
	const query = new URLSearchParams({ limit: "50" });
	if (cursor) query.set("cursor", cursor);
	const page = await request<{
		items?: AlertSourceProjection[];
		nextCursor?: string;
	}>(`/api/v1/alert-sources?${query}`);
	return {
		items: (page.items ?? []).map(instance),
		nextCursor: page.nextCursor,
	};
}
export async function fetchEventSourceInstance(
	key: string,
): Promise<EventSourceInstance> {
	return instance(
		await request<AlertSourceProjection>(
			`/api/v1/alert-sources/${encodeURIComponent(key)}`,
		),
	);
}
/** The shared create command plus the story-2 optional initial settings
 * document; kept local so the alerts feature type stays untouched. */
type CreateAlertSourceWithSettings = CreateAlertSourceRequest & {
	settings?: Record<string, unknown>;
};

/** Creates an enabled source of any registered source kind; the server
 * validates the protocol against the compiled, enabled plugin registry and
 * the optional initial non-secret settings against the owning plugin's
 * closed EventSourceConfigSchema (a rejecting server produces no row). */
export async function createEventSourceInstance(
	key: string,
	protocol: string,
	settings?: Record<string, unknown>,
): Promise<AlertSourceCredentialMetadata> {
	const payload: CreateAlertSourceWithSettings = {
		key,
		protocol,
		clientCommandId: newClientCommandId(),
		...(settings ? { settings } : {}),
	};
	return createAlertSource(payload);
}
/** Replaces one source instance's non-secret settings (ADR-0014 story 2).
 * Versioned command: stale expectedRowVersion fails closed with 409 and the
 * caller re-reads; the response is the refreshed source detail. Settings are
 * non-secret by server-side schema, so the response never carries credentials. */
export async function setEventSourceSettings(
	key: string,
	settings: Record<string, unknown>,
	expectedRowVersion: number,
): Promise<EventSourceInstance> {
	return instance(
		await request<AlertSourceProjection>(
			`/api/v1/alert-sources/${encodeURIComponent(key)}/settings`,
			{
				method: "POST",
				body: JSON.stringify({
					clientCommandId: newClientCommandId(),
					expectedRowVersion,
					settings,
				}),
			},
		),
	);
}
export async function rotateEventSourceCredential(
	key: string,
): Promise<AlertSourceCredentialMetadata> {
	return request<AlertSourceCredentialMetadata>(
		`/api/v1/alert-sources/${encodeURIComponent(key)}/rotate`,
		{
			method: "POST",
			body: JSON.stringify({ clientCommandId: newClientCommandId() }),
		},
	);
}
export async function revealEventSourceCredential(
	handle: string,
): Promise<string> {
	return (await revealCredential(handle)).bearerToken;
}
export async function disableEventSourceInstance(
	instance: EventSourceInstance,
): Promise<void> {
	await request<void>(
		`/api/v1/alert-sources/${encodeURIComponent(instance.id)}/disable`,
		{
			method: "POST",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				expectedRowVersion: instance.rowVersion,
			}),
		},
	);
}
export async function listEventSourceCredentials(
	key: string,
): Promise<EventSourceCredential[]> {
	const page = await request<{ items?: AlertCredentialProjection[] }>(
		`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials?limit=100`,
	);
	return (page.items ?? []).map((credential) => ({
		...credential,
		state: credential.state ?? "Unknown",
	}));
}
export async function retireEventSourceCredential(
	key: string,
	credential: EventSourceCredential,
): Promise<void> {
	await request<void>(
		`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials/${encodeURIComponent(credential.id)}/retire`,
		{
			method: "POST",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				expectedRowVersion: credential.rowVersion,
			}),
		},
	);
}
export function fetchPublicReceiverEndpoint(kind?: string): Promise<PublicReceiverEndpoint> {
	return request<PublicReceiverEndpoint>(
		`/api/v1/alert-sources/receiver-config${kind ? `?kind=${encodeURIComponent(kind)}` : ""}`,
	);
}
export function alertmanagerReceiverYaml(
	publicReceiverUrl: string,
	bearerToken: string,
): string {
	return [
		"receivers:",
		"  - name: quoin",
		"    webhook_configs:",
		`      - url: ${JSON.stringify(publicReceiverUrl)}`,
		"        send_resolved: true",
		"        http_config:",
		"          authorization:",
		"            type: Bearer",
		`            credentials: ${JSON.stringify(bearerToken)}`,
	].join("\n");
}
