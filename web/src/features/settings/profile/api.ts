import type { components } from "@/api/generated/types";
import { request } from "@/api/workbench";

/** The only contact shape the server ever exposes: targets stay masked. */
export type OwnContact = components["schemas"]["AuthFlowContact"];

/** Flow projection (docs/authentication-design.md §3-4): candidate is staged on
 * the flow only and becomes the live target at the verified completion. */
export interface ContactChangeFlow {
	type: string;
	contacts?: OwnContact[];
	candidate?: OwnContact | null;
	passwordSet?: boolean;
	expiresAt?: string;
}

const post = (body: unknown): RequestInit => ({
	method: "POST",
	body: JSON.stringify(body),
});

/** Profile reads use the shared request client (same-origin cookies,
 * problem+json parsing, single 401 recovery). */
export const listOwnContacts = () =>
	request<{ items: OwnContact[] }>("/api/v1/auth/contacts").then(
		(page) => page.items,
	);

/** Thin flow wrappers; secrets live in request memory only — no persistence,
 * no logging. HTTP 200 itself proves the challenge for contact_change flows:
 * `completed` is reserved for "logged in" on login flows and stays false. */
export const startContactChange = (currentPassword: string) =>
	request<ContactChangeFlow>(
		"/api/v1/auth/contact-change",
		post({ currentPassword }),
	);
export const readContactChangeFlow = () =>
	request<ContactChangeFlow>("/api/v1/auth/flow", { method: "GET" });
export const stageFlowContact = (
	channel: OwnContact["channel"],
	target: string,
) =>
	request<OwnContact>("/api/v1/auth/flow/contacts", post({ channel, target }));
export const sendFlowChallenge = (contactId: string) =>
	request<OwnContact>("/api/v1/auth/flow/challenge", post({ contactId }));
export const verifyFlowChallenge = (code: string) =>
	request<{ completed: boolean }>("/api/v1/auth/flow/verify", post({ code }));
export const completeContactChange = (currentPassword: string) =>
	request<void>(
		"/api/v1/auth/contact-change/complete",
		post({ currentPassword }),
	);
