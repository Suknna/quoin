import type {
	LoginRequest,
	PasswordChangeRequest,
	UserSummary,
} from "../src/api/generated/types";

export type ConnectionType = "thanos" | "kubernetes" | "model_provider";

export interface ConnectionSummaryView {
	name: string;
	type: ConnectionType;
	enabled: boolean;
	revalidationRequired: boolean;
	currentRevisionId?: string;
	currentCredentialGenerationId?: string;
	rowVersion: number;
	config: Record<string, unknown>;
}
export interface ConnectionDetailView extends ConnectionSummaryView {
	revisionCount: number;
	generationCount: number;
	activeProbeAttempt?: ProbeAttemptView;
}
export interface ProbeAttemptView {
	id: string;
	type: "connection_probe";
	state:
		| "Queued"
		| "Assigned"
		| "Running"
		| "Cancelling"
		| "Succeeded"
		| "Failed"
		| "Cancelled"
		| "Interrupted";
	rowVersion: number;
	createdAt: string;
	startedAt?: string;
	endedAt?: string;
	terminationReason?: string;
}
export interface ConnectionRevisionView {
	id: string;
	revisionSeq: number;
	config: Record<string, unknown>;
	createdAt: string;
}

export interface CredentialGenerationView {
	id: string;
	generationSeq: number;
	createdBy?: string;
	createdAt: string;
}

export interface ProbeResultView {
	id: string;
	attemptId: string;
	connectionType: ConnectionType;
	connectionRevisionId: string;
	credentialGenerationId: string;
	rootBindingRevision: number;
	actionSetId: string;
	actionSetVersion: number;
	probeContractDigest: string;
	outcome: "passed" | "failed" | "cancelled" | "interrupted";
	resultDigest: string;
	startedAt: string;
	finishedAt: string;
	details: Record<string, unknown>;
}
export interface MaintenanceState {
	active: boolean;
	reason?: "Restore" | "Upgrade" | "RootKeyRebind";
	rowVersion: number;
	items: {
		kind: string;
		objectKey: string;
		safeState: "Safe" | "Blocking";
		detailCode: string;
	}[];
}

export class WorkbenchApiError extends Error {
	constructor(
		readonly status: number,
		message: string,
		readonly code?: string,
	) {
		super(message);
	}
}
type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | undefined;

/** API requests are always same-origin, preventing session cookies from leaving this deployment. */
const apiUrl = (path: string) => path;
export function setUnauthorizedHandler(handler?: UnauthorizedHandler) {
	onUnauthorized = handler;
}

/** Lets feature-local typed clients preserve the shell's single 401 recovery path. */
export function notifyUnauthorized() {
	onUnauthorized?.();
}
export function newClientCommandId(): string {
	return Array.from(crypto.getRandomValues(new Uint8Array(18)), (byte) =>
		byte.toString(16).padStart(2, "0"),
	).join("");
}

export async function request<T>(
	path: string,
	init?: RequestInit,
	notifyUnauthorized = true,
): Promise<T> {
	const response = await fetch(apiUrl(path), {
		credentials: "include",
		headers: init?.body
			? { "Content-Type": "application/json", ...init.headers }
			: init?.headers,
		...init,
	});
	if (!response.ok) {
		let message = "暂时无法完成操作，请重试。";
		let code: string | undefined;
		try {
			const body = (await response.json()) as {
				detail?: string;
				message?: string;
				code?: string;
			};
			message = body.detail ?? body.message ?? message;
			code = body.code;
		} catch {
			/* gateway errors do not have a problem document */
		}
		if (response.status === 401 && notifyUnauthorized) onUnauthorized?.();
		throw new WorkbenchApiError(response.status, message, code);
	}
	if (response.status === 204) return undefined as T;
	return (await response.json()) as T;
}

export const workbenchApi = {
	currentUser: () => request<UserSummary>("/api/v1/auth/me", undefined, false),
	login: (body: LoginRequest) =>
		request<UserSummary>(
			"/api/v1/auth/login",
			{ method: "POST", body: JSON.stringify(body) },
			false,
		),
	changePassword: (body: PasswordChangeRequest) =>
		request<void>("/api/v1/auth/password", {
			method: "PUT",
			body: JSON.stringify(body),
		}),
	logout: () => request<void>("/api/v1/auth/logout", { method: "POST" }, false),
	maintenance: async (): Promise<MaintenanceState | null> => {
		try {
			return await request<MaintenanceState>("/api/v1/maintenance");
		} catch (error) {
			if (error instanceof WorkbenchApiError && error.status === 404)
				return null;
			throw error;
		}
	},
	listConnections: async (): Promise<ConnectionSummaryView[]> =>
		(
			await request<{ items?: ConnectionSummaryView[] }>(
				"/api/v1/connections?limit=100",
			)
		).items ?? [],
	fetchConnection: (name: string, signal?: AbortSignal) =>
		request<ConnectionDetailView>(
			`/api/v1/connections/${encodeURIComponent(name)}`,
			{ signal },
		),
	createConnection: (name: string, connection: ConnectionInput) =>
		request<ConnectionSummaryView>("/api/v1/connections", {
			method: "POST",
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				name,
				connection,
			}),
		}),
	probeConnection: async (name: string): Promise<ProbeAttemptView> => {
		const response = await request<{
			id: string;
			state: ProbeAttemptView["state"];
		}>(`/api/v1/connections/${encodeURIComponent(name)}/probe`, {
			method: "POST",
			body: JSON.stringify({ clientCommandId: newClientCommandId() }),
		});
		return request<ProbeAttemptView>(
			`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(response.id)}`,
		);
	},
		fetchProbeAttempt: (name: string, attemptId: string) =>
			request<ProbeAttemptView>(
				`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attemptId)}`,
			),
		cancelProbeAttempt: (name: string, attemptId: string, expectedRowVersion: number) =>
			request<ProbeAttemptView>(
				`/api/v1/connections/${encodeURIComponent(name)}/probe-attempts/${encodeURIComponent(attemptId)}/cancel`,
				{
					method: "POST",
					body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
				},
			),
		listProbeResults: async (name: string): Promise<ProbeResultView[]> =>
		(
			await request<{ items?: ProbeResultView[] }>(
				`/api/v1/connections/${encodeURIComponent(name)}/probe-results?limit=50`,
			)
		).items ?? [],
		listRevisions: async (name: string): Promise<ConnectionRevisionView[]> =>
			(
				await request<{ items?: ConnectionRevisionView[] }>(
					`/api/v1/connections/${encodeURIComponent(name)}/revisions?limit=50`,
				)
			).items ?? [],
		listCredentialGenerations: async (name: string): Promise<CredentialGenerationView[]> =>
			(
				await request<{ items?: CredentialGenerationView[] }>(
					`/api/v1/connections/${encodeURIComponent(name)}/generations?limit=50`,
				)
			).items ?? [],
		discoverProviderModels: (baseUrl: string, apiKey: string) =>
		request<ProviderDiscoveryResult>("/api/v1/model-providers/discover", {
			method: "POST",
			body: JSON.stringify({ baseUrl, apiKey }),
		}),
		enableConnection: (
			name: string,
			expectedRowVersion: number,
			qualifiedProbeResultId?: string,
		) =>
			request<ConnectionSummaryView>(
				`/api/v1/connections/${encodeURIComponent(name)}/enable`,
				{
					method: "POST",
					body: JSON.stringify({
						clientCommandId: newClientCommandId(),
						expectedRowVersion,
						...(qualifiedProbeResultId && { qualifiedProbeResultId }),
					}),
				},
			),
		disableConnection: (name: string, expectedRowVersion: number) =>
			request<ConnectionSummaryView>(
				`/api/v1/connections/${encodeURIComponent(name)}/disable`,
				{
					method: "POST",
					body: JSON.stringify({ clientCommandId: newClientCommandId(), expectedRowVersion }),
				},
			),
		rotateConnection: (name: string, expectedRowVersion: number, connection: ConnectionInput) =>
			request<ConnectionDetailView>(
				`/api/v1/connections/${encodeURIComponent(name)}/rotate`,
				{
					method: "POST",
					body: JSON.stringify({
						clientCommandId: newClientCommandId(),
						expectedRowVersion,
						connection,
					}),
				},
			),
};
export interface ThanosConnectionInput {
	type: "thanos";
	baseUrl: string;
	username?: string;
	password?: string;
	tlsCaPem?: string;
	tlsServerName?: string;
	tlsSkipVerify?: boolean;
}

export interface KubernetesConnectionInput {
	type: "kubernetes";
	contextName: string;
	defaultNamespace: string;
	kubeconfig: string;
}

export interface ModelProviderConnectionInput {
	type: "model_provider";
	baseUrl: string;
	chatModelId: string;
	embeddingModelId: string;
	contextBudgetTokens: number;
	maxOutputTokens: number;
	apiKey: string;
}

export type ConnectionInput = ThanosConnectionInput | KubernetesConnectionInput | ModelProviderConnectionInput;

export interface ProviderDiscoveryResult {
	available: boolean;
	items: { id: string; metadata?: Record<string, unknown> }[];
	detail?: string;
}
