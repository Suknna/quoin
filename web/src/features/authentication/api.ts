import type { UserSummary } from "@/api/generated/types";
import { request } from "@/api/workbench";

/**
 * Client for the server-side authentication flow (docs/authentication-design.md).
 *
 * The flow lives entirely behind an HttpOnly `__Host-quoin-flow` cookie: no
 * client-side token exists, so the browser can only advance, resume or abandon
 * the flow through these endpoints. Every call passes `notifyUnauthorized`
 * = false — flow requests run before a workbench session exists (or on behalf
 * of a scoped flow cookie), so a 401 here is flow state, not session expiry,
 * and must not trigger the shell's global logout path.
 */

/**
 * Flow classification is mechanical on the server: an uninitialized built-in
 * administrator starts `admin_initialize`, any other uninitialized user starts
 * `operator_initialize`, and initialized users start `login`. CLI recovery
 * only sets a temporary password — the following normal sign-in is classified
 * like a first run, so no separate recovery kind exists.
 */
export type AuthFlowKind =
	| "admin_initialize"
	| "operator_initialize"
	| "login";

export type AuthContactChannel = "email" | "sms";

export interface AuthFlowContact {
	id: string;
	channel: AuthContactChannel;
	maskedTarget: string;
	/** Account-level verification state; flow routing uses AuthFlow.factorVerified. */
	verified: boolean;
}

/** Flow-scoped projection of the user; the workbench keeps using UserSummary. */
export interface AuthFlowUser {
	id: string;
	username: string;
	displayName: string;
}

export interface AuthFlow {
	type: AuthFlowKind;
	user: AuthFlowUser;
	/** Instant after which the flow (and any outstanding challenge) is invalid. */
	expiresAt: string;
	contacts: AuthFlowContact[];
	/** Absent means set; `false` while an initialization still owes a real password. */
	passwordSet?: boolean;
	/**
	 * Per-flow marker: a contact verified via THIS flow's challenge (derived
	 * from the flow row, not the account-level contact state). Initialization
	 * completion requires it; pending login flows are always false because a
	 * login completes atomically at verify.
	 */
	factorVerified: boolean;
}

/** Result of POST /auth/login: a started flow. */
export type AuthStart = AuthFlow;

export interface AuthCredentials {
	username: string;
	password: string;
}

export interface AuthVerifyResult {
	/** Login flows complete the session here; init flows only mark verified. */
	completed: boolean;
	/** Present only when completed: the signed-in user, same shape as /auth/me. */
	user?: UserSummary;
}

/**
 * Verification-code delivery channel (internal/quoin/app/auth_delivery.go).
 * `secretHeaders` and `passwordRef` only carry reference NAMES — the actual
 * secret values are write-only through the `secrets` map and never readable.
 */
export interface AuthDeliveryChannel {
	kind: "smtp" | "webhook";
	host?: string;
	port?: number;
	from?: string;
	username?: string;
	passwordRef?: string;
	tlsMode?: string;
	url?: string;
	headers?: Record<string, string>;
	secretHeaders?: Record<string, string>;
	encoding?: string;
	fields?: Record<string, string>;
	successField?: string;
	successValue?: string;
	allowPrivateCIDRs?: string[];
	rootCaPem?: string;
}

export interface AuthDeliveryConfiguration {
	email?: AuthDeliveryChannel;
	sms?: AuthDeliveryChannel;
}

export interface AuthDeliveryView {
	configuration: AuthDeliveryConfiguration;
	rowVersion: number;
	/** "administrator" (editable here) or "deployment" (read-only for the UI). */
	source: string;
	configured: boolean;
}

export interface AuthDeliveryUpdate {
	configuration: AuthDeliveryConfiguration;
	/** Write-only: reference name → actual secret value; blanks keep stored ones. */
	secrets?: Record<string, string>;
	expectedRowVersion: number;
}

export const authFlowApi = {
	start: (credentials: AuthCredentials) =>
		request<AuthStart>("/api/v1/auth/login", {
			method: "POST",
			body: JSON.stringify(credentials),
		}, false),
	/** Resumes an in-flight flow after a refresh; 404 means no active flow. */
	resume: () => request<AuthFlow>("/api/v1/auth/flow", undefined, false),
	setPassword: (newPassword: string) =>
		request<void>("/api/v1/auth/flow/password", {
			method: "PUT",
			body: JSON.stringify({ newPassword }),
		}, false),
	/** Admin initialization only; operators keep their admin-assigned targets. */
	addContact: (channel: AuthContactChannel, target: string) =>
		request<void>("/api/v1/auth/flow/contacts", {
			method: "POST",
			body: JSON.stringify({ channel, target }),
		}, false),
	sendChallenge: (contactId: string) =>
		request<void>("/api/v1/auth/flow/challenge", {
			method: "POST",
			body: JSON.stringify({ contactId }),
		}, false),
	verify: (code: string) =>
		request<AuthVerifyResult>("/api/v1/auth/flow/verify", {
			method: "POST",
			body: JSON.stringify({ code }),
		}, false),
	/** Initialization only; clears the flow and returns 204 — no session starts. */
	complete: () =>
		request<void>("/api/v1/auth/flow/complete", {
			method: "POST",
			body: JSON.stringify({}),
		}, false),
	/**
	 * Delivery settings, readable by an admin_initialize flow or an admin
	 * session; secret values are never part of the response.
	 */
	readDelivery: () =>
		request<AuthDeliveryView>("/api/v1/auth/flow/delivery", undefined, false),
	saveDelivery: (update: AuthDeliveryUpdate) =>
		request<AuthDeliveryView>("/api/v1/auth/flow/delivery", {
			method: "PUT",
			body: JSON.stringify(update),
		}, false),
};
