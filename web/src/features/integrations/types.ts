/** Platform form identities are independent of server-authoritative plugin enablement. */
export type IntegrationPlatform = "alertmanager" | "prometheus" | "thanos";

export interface IntegrationCatalogItem {
	id: string;
	displayName: string;
	description: string;
	enabled: boolean;
	version: string;
	capabilities: string[];
}

/** A non-secret projection of a configured platform instance. */
export interface IntegrationInstance {
	id: string;
	platform: IntegrationPlatform;
	displayName: string;
	// Rotation withholds dispatch until the current revision and credential pair is verified.
	status: "active" | "revalidation_required" | "disabled" | "unavailable";
	createdAt?: string;
	latestValidEventAt?: string | null;
	rowVersion?: number;
}
