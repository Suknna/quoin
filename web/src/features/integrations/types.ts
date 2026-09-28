/** Platform form identities are independent of server-authoritative plugin enablement. */
export type IntegrationPlatform = "alertmanager" | "prometheus" | "thanos";

/** The specialized brand workbenches keep hand-built forms (receiver YAML,
 * metrics endpoints); every other catalog event-source plugin uses the
 * generic source-kind form. */
export function isSpecializedPlatform(
	value: string,
): value is IntegrationPlatform {
	return (
		value === "alertmanager" ||
		value === "prometheus" ||
		value === "thanos"
	);
}

/** ADR-0011 capability vocabulary projected by the plugin catalog endpoint. */
export const EVENT_SOURCE_CAPABILITY = "event_source";

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
	// The specialized brand (alertmanager/prometheus/thanos) or the plugin id
	// of a generic event source (the stable source kind).
	platform: string;
	displayName: string;
	// Rotation withholds dispatch until the current revision and credential pair is verified.
	status: "active" | "revalidation_required" | "disabled" | "unavailable";
	createdAt?: string;
	latestValidEventAt?: string | null;
	rowVersion?: number;
}
