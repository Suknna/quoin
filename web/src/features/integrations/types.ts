/** Supported integrations intentionally describe user-facing capabilities, not a plugin contract. */
export type IntegrationPlatform =
	| "alertmanager"
	| "prometheus"
	| "thanos"
	| "kubernetes"
	| "browser";

export type IntegrationAvailability = "available" | "unavailable";

export interface IntegrationCatalogItem {
	platform: IntegrationPlatform;
	displayName: string;
	description: string;
	availability: IntegrationAvailability;
}

/** A non-secret projection of a configured platform instance. */
export interface IntegrationInstance {
	id: string;
	platform: IntegrationPlatform;
	displayName: string;
	status: "active" | "disabled" | "unavailable";
	createdAt?: string;
	latestValidEventAt?: string | null;
	rowVersion?: number;
}

export const integrationCatalog: readonly IntegrationCatalogItem[] = [
	{
		platform: "alertmanager",
		displayName: "Alertmanager",
		description: "接收上游告警，并保留可轮换的来源凭据。",
		availability: "available",
	},
		{
			platform: "prometheus",
			displayName: "Prometheus",
			description: "配置面向业务声明的 PromQL 指标查询接入。",
			availability: "available",
		},
		{
			platform: "thanos",
			displayName: "Thanos",
			description: "配置面向业务声明的全局 PromQL 查询接入。",
			availability: "available",
		},
	{
		platform: "kubernetes",
		displayName: "Kubernetes",
		description: "受限集群访问接入将在后续切片提供。",
		availability: "unavailable",
	},
	{
		platform: "browser",
		displayName: "受控浏览器",
		description: "人工登录身份接入将在后续切片提供。",
		availability: "unavailable",
	},
] as const;
