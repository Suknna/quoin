import { newClientCommandId, request } from "@/api/workbench";
import { disableConnection, enableConnection, rotateConnection } from "@/features/admin/connections/api";
import { createAlertSource, revealCredential, type AlertSourceCredentialMetadata } from "@/features/alerts/api";
import type { IntegrationInstance } from "./types";

export type MetricsPlatform = "prometheus" | "thanos";
export type MetricsAuthMode = "none" | "basic" | "bearer";

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
	lastProbe?: { id?: string; outcome: "passed" | "failed" | "cancelled" | "interrupted"; finishedAt?: string; details?: Record<string, unknown> };
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
		id: String(projection.id ?? projection.name),
		platform,
		displayName: String(projection.name ?? projection.id),
		// A rotated metrics connection can remain enabled while intentionally
		// withheld from dispatch until a fresh exact-pair probe requalifies it.
		status: projection.revalidationRequired === true ? "revalidation_required" : projection.enabled === true ? "active" : "disabled",
		revalidationRequired: projection.revalidationRequired === true,
		rowVersion: Number(projection.rowVersion ?? 0),
		endpoint: typeof config?.baseUrl === "string" ? config.baseUrl : undefined,
		authType: config?.authType === "basic" ? "basic" : config?.authType === "bearer" ? "bearer" : "none",
		username: typeof config?.username === "string" ? config.username : undefined,
		tlsCaPem: typeof config?.tlsCaPem === "string" ? config.tlsCaPem : undefined,
		tlsServerName: typeof config?.tlsServerName === "string" ? config.tlsServerName : undefined,
		tlsSkipVerify: config?.tlsSkipVerify === true,
		lastProbe: projection.lastProbe as MetricsInstance["lastProbe"],
	};
}

export async function listMetricsInstances(platform?: MetricsPlatform): Promise<MetricsInstance[]> {
	const page = await request<{ items?: Record<string, unknown>[] }>("/api/v1/connections?limit=100");
	return (page.items ?? []).filter((item) => item.type === "prometheus" || item.type === "thanos").map(metricsInstance).filter((item) => !platform || item.platform === platform);
}

export async function fetchMetricsInstance(name: string): Promise<MetricsInstance> {
	return metricsInstance(await request<Record<string, unknown>>(`/api/v1/connections/${encodeURIComponent(name)}`));
}

/** Creates a disabled connection. Callers must explicitly probe and enable it. */
export async function createMetricsInstance(name: string, connection: MetricsConnectionInput): Promise<MetricsInstance> {
	return metricsInstance(await request<Record<string, unknown>>("/api/v1/connections", { method: "POST", body: JSON.stringify({ clientCommandId: newClientCommandId(), name, connection }) }));
}

/** Polls the authoritative attempt endpoint only until its terminal state is visible. */
export async function probeMetricsInstance(name: string): Promise<MetricsInstance["lastProbe"]> {
	const attempt = await request<{ id: string }>(`/api/v1/connections/${encodeURIComponent(name)}/probe`, { method: "POST", body: JSON.stringify({ clientCommandId: newClientCommandId() }) });
	for (let poll = 0; poll < 30; poll += 1) {
		const detail = await request<{ state: string; endedAt?: string; terminationReason?: string }>(`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attempt.id)}`);
		if (["Succeeded", "Failed", "Cancelled", "Interrupted"].includes(detail.state)) {
			const outcome = detail.state === "Succeeded" ? "passed" : detail.state === "Cancelled" ? "cancelled" : detail.state === "Interrupted" ? "interrupted" : "failed";
			if (outcome !== "passed") return { outcome, finishedAt: detail.endedAt, details: detail.terminationReason ? { reason: detail.terminationReason } : undefined };
			const results = await request<{ items?: { id: string; attemptId: string; outcome: string }[] }>(`/api/v1/connections/${encodeURIComponent(name)}/probe-results?limit=50`);
			const qualified = results.items?.find((result) => result.attemptId === attempt.id && result.outcome === "passed");
			if (!qualified) return { outcome: "failed", finishedAt: detail.endedAt, details: { reason: "qualified_probe_result_missing" } };
			return { id: qualified.id, outcome, finishedAt: detail.endedAt };
		}
		await new Promise((resolve) => setTimeout(resolve, 500));
	}
	return { outcome: "failed", details: { reason: "probe_timeout" } };
}

/** Reuses the shared connection command wrappers, preserving row-version fencing. */
export async function enableMetricsInstance(instance: MetricsInstance, qualifiedProbeResultId: string): Promise<MetricsInstance> {
	return metricsInstance(await enableConnection(instance.displayName, instance.rowVersion, qualifiedProbeResultId) as unknown as Record<string, unknown>);
}
export async function disableMetricsInstance(instance: MetricsInstance): Promise<MetricsInstance> {
	return metricsInstance(await disableConnection(instance.displayName, instance.rowVersion) as unknown as Record<string, unknown>);
}
export async function rotateMetricsInstance(instance: MetricsInstance, connection: MetricsConnectionInput): Promise<MetricsInstance> {
	return metricsInstance(await rotateConnection(instance.displayName, instance.rowVersion, { ...connection }));
}

interface AlertSourceProjection { key: string; protocol: "alertmanager"; enabled: boolean; rowVersion: number; createdAt?: string; latestValidEventAt?: string | null; }
interface AlertCredentialProjection { id: string; rowVersion: number; state?: string; createdAt?: string; firstUsedAt?: string | null; }
export interface AlertmanagerInstance extends IntegrationInstance { platform: "alertmanager"; status: "active" | "disabled"; rowVersion: number; }
export interface AlertmanagerCredential { id: string; rowVersion: number; state: string; createdAt?: string; firstUsedAt?: string | null; }
export interface PublicReceiverEndpoint { publicReceiverUrl: string; }
function instance(source: AlertSourceProjection): AlertmanagerInstance { return { id: source.key, platform: "alertmanager", displayName: source.key, status: source.enabled ? "active" : "disabled", createdAt: source.createdAt, latestValidEventAt: source.latestValidEventAt, rowVersion: source.rowVersion }; }
export interface AlertmanagerInstancePage { items: AlertmanagerInstance[]; nextCursor?: string; }
export async function listAlertmanagerInstances(cursor?: string): Promise<AlertmanagerInstancePage> { const query = new URLSearchParams({ limit: "50" }); if (cursor) query.set("cursor", cursor); const page = await request<{ items?: AlertSourceProjection[]; nextCursor?: string }>(`/api/v1/alert-sources?${query}`); return { items: (page.items ?? []).map(instance), nextCursor: page.nextCursor }; }
export async function fetchAlertmanagerInstance(key: string): Promise<AlertmanagerInstance> { return instance(await request<AlertSourceProjection>(`/api/v1/alert-sources/${encodeURIComponent(key)}`)); }
export async function createAlertmanagerInstance(key: string): Promise<AlertSourceCredentialMetadata> { return createAlertSource({ key, protocol: "alertmanager", clientCommandId: newClientCommandId() }); }
export async function rotateAlertmanagerCredential(key: string): Promise<AlertSourceCredentialMetadata> { return request<AlertSourceCredentialMetadata>(`/api/v1/alert-sources/${encodeURIComponent(key)}/rotate`, { method: "POST", body: JSON.stringify({ clientCommandId: newClientCommandId() }) }); }
export async function revealAlertmanagerCredential(handle: string): Promise<string> { return (await revealCredential(handle)).bearerToken; }
export async function disableAlertmanagerInstance(instance: AlertmanagerInstance): Promise<void> { await request<void>(`/api/v1/alert-sources/${encodeURIComponent(instance.id)}/disable`, { method: "POST", body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion: instance.rowVersion }) }); }
export async function listAlertmanagerCredentials(key: string): Promise<AlertmanagerCredential[]> { const page = await request<{ items?: AlertCredentialProjection[] }>(`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials?limit=100`); return (page.items ?? []).map((credential) => ({ ...credential, state: credential.state ?? "Unknown" })); }
export async function retireAlertmanagerCredential(key: string, credential: AlertmanagerCredential): Promise<void> { await request<void>(`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials/${encodeURIComponent(credential.id)}/retire`, { method: "POST", body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion: credential.rowVersion }) }); }
export function fetchPublicReceiverEndpoint(): Promise<PublicReceiverEndpoint> { return request<PublicReceiverEndpoint>("/api/v1/alert-sources/receiver-config"); }
export function alertmanagerReceiverYaml(publicReceiverUrl: string, bearerToken: string): string { return ["receivers:", "  - name: quoin", "    webhook_configs:", `      - url: ${JSON.stringify(publicReceiverUrl)}`, "        send_resolved: true", "        http_config:", "          authorization:", "            type: Bearer", `            credentials: ${JSON.stringify(bearerToken)}`].join("\n"); }
