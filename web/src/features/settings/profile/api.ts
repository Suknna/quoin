import type { components } from "@/api/generated/types";
import { request } from "@/api/workbench";

/** The only contact shape the server ever exposes: targets stay masked. */
export type OwnContact = components["schemas"]["AuthFlowContact"];

/** Profile reads use the shared request client (same-origin cookies,
 * problem+json parsing, single 401 recovery). The inline change flow was
 * retired with ADR-0010 (OTP retirement); channels are administered from the
 * Users page and only displayed here. */
export const listOwnContacts = () =>
	request<{ items: OwnContact[] }>("/api/v1/auth/contacts").then(
		(page) => page.items,
	);
