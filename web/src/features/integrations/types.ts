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
/** Declared controlled HTTP connection capability (#110): the plugin binds an
 * external platform kind that is independent of both the plugin ID and the
 * inbound sourceKind. */
export const HTTP_CONNECTION_CAPABILITY = "http_connection";

export type ConnectionAuthMode = "none" | "basic" | "bearer";

/** Narrows a raw catalog auth-mode entry; unknown server values fail closed. */
export function isConnectionAuthMode(value: unknown): value is ConnectionAuthMode {
	return value === "none" || value === "basic" || value === "bearer";
}

/** The plugin-declared auth modes, narrowed and order-stable; empty when the
 * declaration is absent or entirely out of vocabulary (no configurable form). */
export function allowedAuthModes(
	modes: unknown,
): ConnectionAuthMode[] {
	if (!Array.isArray(modes)) return [];
	const known: ConnectionAuthMode[] = [];
	for (const mode of modes) {
		if (isConnectionAuthMode(mode) && !known.includes(mode)) known.push(mode);
	}
	// Presentation order is fixed; the server enforces the authoritative set.
	return (["none", "basic", "bearer"] as const).filter((mode) =>
		known.includes(mode),
	);
}

export interface IntegrationCatalogItem {
	id: string;
	/** Registered EventSource.Kind(), not necessarily the plugin ID. */
	sourceKind?: string;
	/** 注册的受控 HTTP 连接类型，独立于插件 ID 与入站 sourceKind。 */
	connectionKind?: string;
	/** Plugin-declared allowed auth modes for the HTTP connection kind. */
	connectionAuthModes?: ("none" | "basic" | "bearer")[];
	/** 插件声明的只读 GET/200 探测路径；无路径的连接类型不得启用新实例。 */
	connectionProbePath?: string;
	displayName: string;
	description: string;
	enabled: boolean;
	version: string;
	capabilities: string[];
}

/** A catalog entry that may host generic HTTP connection instances: enabled,
 * declares the capability, binds a non-specialized connection kind and allows
 * at least one known auth mode. Specialized brand kinds keep their own forms. */
export function isHttpConnectionCatalogItem(
	item: IntegrationCatalogItem,
): boolean {
	return (
		item.enabled &&
		item.capabilities.includes(HTTP_CONNECTION_CAPABILITY) &&
		Boolean(item.connectionKind) &&
		!isSpecializedPlatform(item.connectionKind ?? "") &&
		allowedAuthModes(item.connectionAuthModes).length > 0
	);
}

/** Connection kinds of enabled generic HTTP plugins. Specialized metric kinds
 * are excluded: prometheus/thanos instances keep the dedicated metrics list
 * and brand forms. */
export function genericHttpConnectionKinds(
	items: IntegrationCatalogItem[],
): string[] {
	const kinds: string[] = [];
	for (const item of items) {
		if (
			isHttpConnectionCatalogItem(item) &&
			item.connectionKind &&
			!kinds.includes(item.connectionKind)
		)
			kinds.push(item.connectionKind);
	}
	return kinds;
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
