import { newClientCommandId, request } from "@/api/workbench";
import { createAlertSource, revealCredential, type AlertSourceCredentialMetadata } from "@/features/alerts/api";
import type { IntegrationInstance } from "./types";

interface AlertSourceProjection {
	key: string;
	protocol: "alertmanager";
	enabled: boolean;
	rowVersion: number;
	createdAt?: string;
	latestValidEventAt?: string | null;
}

interface AlertCredentialProjection {
	id: string;
	rowVersion: number;
	state?: string;
	createdAt?: string;
	firstUsedAt?: string | null;
}

export interface AlertmanagerInstance extends IntegrationInstance {
	platform: "alertmanager";
	status: "active" | "disabled";
	rowVersion: number;
}

export interface AlertmanagerCredential {
	id: string;
	rowVersion: number;
	state: string;
	createdAt?: string;
	firstUsedAt?: string | null;
}

export interface PublicReceiverEndpoint {
	publicReceiverUrl: string;
}

function instance(source: AlertSourceProjection): AlertmanagerInstance {
	return {
		id: source.key,
		platform: "alertmanager",
		displayName: source.key,
		status: source.enabled ? "active" : "disabled",
		createdAt: source.createdAt,
		latestValidEventAt: source.latestValidEventAt,
		rowVersion: source.rowVersion,
	};
}

/** Reads only non-secret source state for the integration workbench. */
export interface AlertmanagerInstancePage {
	items: AlertmanagerInstance[];
	nextCursor?: string;
}

/** Cursor pagination makes the visible search boundary explicit instead of silently truncating sources. */
export async function listAlertmanagerInstances(cursor?: string): Promise<AlertmanagerInstancePage> {
	const query = new URLSearchParams({ limit: "50" });
	if (cursor) query.set("cursor", cursor);
	const page = await request<{ items?: AlertSourceProjection[]; nextCursor?: string }>(`/api/v1/alert-sources?${query}`);
	return { items: (page.items ?? []).map(instance), nextCursor: page.nextCursor };
}

export async function fetchAlertmanagerInstance(key: string): Promise<AlertmanagerInstance> {
	return instance(await request<AlertSourceProjection>(`/api/v1/alert-sources/${encodeURIComponent(key)}`));
}

export async function createAlertmanagerInstance(key: string): Promise<AlertSourceCredentialMetadata> {
	return createAlertSource({ key, protocol: "alertmanager", clientCommandId: newClientCommandId() });
}

export async function rotateAlertmanagerCredential(key: string): Promise<AlertSourceCredentialMetadata> {
	return request<AlertSourceCredentialMetadata>(`/api/v1/alert-sources/${encodeURIComponent(key)}/rotate`, {
		method: "POST",
		body: JSON.stringify({ clientCommandId: newClientCommandId() }),
	});
}

export async function revealAlertmanagerCredential(handle: string): Promise<string> {
	return (await revealCredential(handle)).bearerToken;
}

export async function disableAlertmanagerInstance(instance: AlertmanagerInstance): Promise<void> {
	await request<void>(`/api/v1/alert-sources/${encodeURIComponent(instance.id)}/disable`, {
		method: "POST",
		body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion: instance.rowVersion }),
	});
}

export async function listAlertmanagerCredentials(key: string): Promise<AlertmanagerCredential[]> {
	const page = await request<{ items?: AlertCredentialProjection[] }>(`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials?limit=100`);
	return (page.items ?? []).map((credential) => ({
		...credential,
		state: credential.state ?? "Unknown",
	}));
}

export async function retireAlertmanagerCredential(key: string, credential: AlertmanagerCredential): Promise<void> {
	await request<void>(`/api/v1/alert-sources/${encodeURIComponent(key)}/credentials/${encodeURIComponent(credential.id)}/retire`, {
		method: "POST",
		body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion: credential.rowVersion }),
	});
}

/** The server owns the public origin; the client never derives it from an internal deployment URL. */
export function fetchPublicReceiverEndpoint(): Promise<PublicReceiverEndpoint> {
	return request<PublicReceiverEndpoint>("/api/v1/alert-sources/receiver-config");
}

export function alertmanagerReceiverYaml(publicReceiverUrl: string, bearerToken: string): string {
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
