import type { AuthConfig, UserSummary } from "@/api/generated/types";
import { request } from "@/api/workbench";

/**
 * Client for the single-step authentication surface (ADR-0010,
 * docs/authentication-design.md). The login page renders from the public
 * /auth/config projection; the local channel posts credentials once and
 * receives the session cookie directly — no client-side token exists. The
 * OIDC round-trip is a full-page navigation: the browser never touches a
 * token. Every call passes `notifyUnauthorized` = false — these requests
 * run before a workbench session exists, so a 401 is channel state, not
 * session expiry, and must not trigger the shell's global logout path.
 */

export interface LoginCredentials {
	username: string;
	password: string;
}

export interface LoginResult {
	completed: boolean;
	user: UserSummary;
}

export const authApi = {
	/** The public login-channel projection backing the config-driven page. */
	config: () => request<AuthConfig>("/api/v1/auth/config", undefined, false),
	/** Single-step local login; a restricted user still owes the forced change. */
	login: (credentials: LoginCredentials) =>
		request<LoginResult>(
			"/api/v1/auth/login",
			{ method: "POST", body: JSON.stringify(credentials) },
			false,
		),
	/**
	 * The forced first password change (and any self change): the restricted
	 * session unlocks the full admission level in the same transaction.
	 */
	changePassword: (currentPassword: string, newPassword: string) =>
		request<void>(
			"/api/v1/auth/password",
			{ method: "PUT", body: JSON.stringify({ currentPassword, newPassword }) },
			false,
		),
};
