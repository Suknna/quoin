// 跨来源每日报告 API（ADR-0014）。类型本地镜像 Go 契约
// (internal/quoin/inspection/daily.go + app/inspection/daily.go)；OpenAPI
// 文档再生后应迁移到 generated/types。失败沿用 problem+json 的人话 message。

/** Reuses the plan/run module's problem+json failure projection. */
import { InspectionApiError, newClientCommandId } from "./api";

async function failure(response: Response): Promise<InspectionApiError> {
	let message = "暂时无法完成每日报告操作，请重试。";
	try {
		const body = (await response.json()) as { message?: string };
		message = body.message ?? message;
	} catch {
		// The ordinary-language fallback remains useful for a non-JSON failure.
	}
	return new InspectionApiError(message, response.status);
}

/**
 * One daily report config: the admin-owned report identity — report timezone,
 * local daily trigger time and the participating existing plans.
 */
export interface DailyReportConfig {
	configKey: string;
	displayName: string;
	enabled: boolean;
	timezone: string;
	/** Local wall clock 'HH:MM' (24h). */
	triggerTime: string;
	planKeys: string[];
	rowVersion: number;
	createdAt: string;
	updatedAt: string;
}

/** Create payload; the update path adds expectedRowVersion for optimism. */
export interface DailyReportConfigInput {
	configKey: string;
	displayName: string;
	enabled: boolean;
	timezone: string;
	triggerTime: string;
	planKeys: string[];
}

/** Trigger-time frozen identity of one participating plan and its source. */
export interface DailyContribution {
	planKey: string;
	displayName?: string;
	connectionName?: string;
	pluginId?: string;
	templateId?: string;
	templateVersion?: string;
	enabled: boolean;
	sourceEnabled: boolean;
	/** The configured plan key no longer exists; the report names the hole. */
	missing?: boolean;
}

/** One per-check fact inside a sealed source report; absence is listed, never filled. */
export interface DailyCheckItem {
	runId: number;
	checkKey: string;
	status: string;
	gapReason?: string;
	observedAt?: string;
}

/** Source-level outcome: "gap" whenever any explicit reason or failing check exists. */
export interface DailySourceReport extends DailyContribution {
	status: "ok" | "gap";
	gapReasons?: string[];
	checks?: DailyCheckItem[];
}

/** Coarse sealed roll-up over sources and checks. */
export interface DailyTotals {
	checksOk: number;
	checksGap: number;
	checksError: number;
	sourcesGap: number;
}

/** The frozen sealed document (schemaKind "inspection_daily_report_v1"). */
export interface DailyReportContent {
	schemaKind: string;
	configKey: string;
	localDate: string;
	timezone: string;
	windowStartUtc: string;
	windowEndUtc: string;
	sealedAt: string;
	sources: DailySourceReport[];
	totals: DailyTotals;
}

export type DailyReportState = "Collecting" | "Sealed";
export type DailyReportTriggerKind = "schedule" | "manual";

/** List/detail projection of one daily report. */
export interface DailyReportSummary {
	id: string;
	configKey: string;
	localDate: string;
	timezone: string;
	windowStartUtc: string;
	windowEndUtc: string;
	triggerKind: DailyReportTriggerKind;
	state: DailyReportState;
	sealedAt?: string;
	latestVersion: number;
	createdAt: string;
}

/** One immutable version entry (newest first from the server). */
export interface DailyReportVersionSummary {
	version: number;
	createdAt: string;
}

/** Read model: frozen identity + contributions + versions + sealed content (nil while Collecting). */
export interface DailyReportDetail extends DailyReportSummary {
	configRowVersion: number;
	cutoffAt: string;
	contributions: DailyContribution[];
	versions: DailyReportVersionSummary[];
	latest?: DailyReportContent;
}

function queryOf(options: { configKey?: string; limit?: number }): string {
	const query = new URLSearchParams({ limit: String(options.limit ?? 50) });
	if (options.configKey) query.set("configKey", options.configKey);
	return query.toString();
}

export async function listDailyReportConfigs(): Promise<DailyReportConfig[]> {
	const response = await fetch(
		"/api/v1/inspections/daily-report-configs",
		{ credentials: "include" },
	);
	if (!response.ok) throw await failure(response);
	const page = (await response.json()) as { items?: DailyReportConfig[] };
	return page.items ?? [];
}

export async function getDailyReportConfig(
	configKey: string,
): Promise<DailyReportConfig> {
	const response = await fetch(
		`/api/v1/inspections/daily-report-configs/${encodeURIComponent(configKey)}`,
		{ credentials: "include" },
	);
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportConfig;
}

export async function createDailyReportConfig(
	input: DailyReportConfigInput,
): Promise<DailyReportConfig> {
	const response = await fetch("/api/v1/inspections/daily-report-configs", {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ clientCommandId: newClientCommandId(), ...input }),
	});
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportConfig;
}

export async function updateDailyReportConfig(
	update: DailyReportConfigInput & { expectedRowVersion: number },
): Promise<DailyReportConfig> {
	const response = await fetch(
		`/api/v1/inspections/daily-report-configs/${encodeURIComponent(update.configKey)}`,
		{
			method: "PUT",
			credentials: "include",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({
				clientCommandId: newClientCommandId(),
				...update,
			}),
		},
	);
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportConfig;
}

/** Reads one server page of reports; callers filter by config server-side. */
export async function listDailyReports(
	options: { configKey?: string; limit?: number } = {},
): Promise<DailyReportSummary[]> {
	const response = await fetch(
		`/api/v1/inspections/daily-reports?${queryOf(options)}`,
		{ credentials: "include" },
	);
	if (!response.ok) throw await failure(response);
	const page = (await response.json()) as { items?: DailyReportSummary[] };
	return page.items ?? [];
}

export async function getDailyReport(
	configKey: string,
	localDate: string,
): Promise<DailyReportDetail> {
	const response = await fetch(
		`/api/v1/inspections/daily-reports/${encodeURIComponent(configKey)}/${encodeURIComponent(localDate)}`,
		{ credentials: "include" },
	);
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportDetail;
}

/** One immutable version's raw content document (JSON text). */
export async function getDailyReportVersion(
	configKey: string,
	localDate: string,
	version: number,
): Promise<string> {
	const response = await fetch(
		`/api/v1/inspections/daily-reports/${encodeURIComponent(configKey)}/${encodeURIComponent(localDate)}/versions/${version}`,
		{ credentials: "include" },
	);
	if (!response.ok) throw await failure(response);
	const body = (await response.json()) as { content: string };
	return body.content;
}

/** 人工补跑（漏过的整日）：窗口仍是请求的原日期，绝不偷换为当前日期。 */
export async function backfillDailyReport(
	configKey: string,
	localDate: string,
): Promise<DailyReportSummary> {
	const response = await fetch("/api/v1/inspections/daily-reports/backfill", {
		method: "POST",
		credentials: "include",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({
			clientCommandId: newClientCommandId(),
			configKey,
			localDate,
		}),
	});
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportSummary;
}

/** 人工重分析：从同一冻结窗口追加新版本，旧版本保持可读。 */
export async function rerunDailyReport(
	configKey: string,
	localDate: string,
): Promise<DailyReportSummary> {
	const response = await fetch(
		`/api/v1/inspections/daily-reports/${encodeURIComponent(configKey)}/${encodeURIComponent(localDate)}/rerun`,
		{
			method: "POST",
			credentials: "include",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ clientCommandId: newClientCommandId() }),
		},
	);
	if (!response.ok) throw await failure(response);
	return (await response.json()) as DailyReportSummary;
}

// ---- Human projections (server facts verbatim; never inferred into health) ----

export const dailyStateText: Record<DailyReportState, string> = {
	Collecting: "采集中",
	Sealed: "已封存",
};

export const dailyTriggerText: Record<DailyReportTriggerKind, string> = {
	schedule: "定时",
	manual: "手动",
};

/** Gap vocabulary is content from the sealed report; unknown reasons stay visible verbatim. */
export function dailyGapText(reason: string): string {
	const known: Record<string, string> = {
		plan_disabled: "计划已停用",
		source_disabled: "接入已停用",
		plan_missing: "计划不存在",
		no_collection: "未采证",
		cutoff_exceeded: "超过采证截止时间",
		run_failed: "采集运行失败",
		run_cancelled: "采集已取消",
		run_interrupted: "采集已中断",
	};
	return known[reason] ?? reason;
}

export function dailyStateBadgeClass(
	state: DailyReportState,
): string | undefined {
	// Collecting warns (result pending); a sealed report stays neutral — its gap
	// facts live inside the content and must not be pre-judged by the badge.
	return state === "Collecting" ? "border-warning/50 text-warning" : undefined;
}

export function sourceStatusText(status: DailySourceReport["status"]): string {
	return status === "ok" ? "正常" : "有缺口";
}

/** Formats the frozen UTC window as one line, e.g. "09-27 16:00 → 09-28 16:00"。 */
export function dailyWindowText(
	windowStartUtc: string,
	windowEndUtc: string,
): string {
	return `${formatDateTime(windowStartUtc)} → ${formatDateTime(windowEndUtc)} (UTC)`;
}

function formatDateTime(value: string): string {
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN");
}

export function formatDailyTime(value?: string | null): string {
	return value ? formatDateTime(value) : "—";
}

// ---- Client-side validation mirrors (server re-validates everything) ----

export const dailyConfigKeyPattern = /^[a-z][a-z0-9-]{0,62}$/;
export const dailyTriggerTimePattern = /^([01][0-9]|2[0-3]):[0-5][0-9]$/;
export const dailyLocalDatePattern = /^\d{4}-\d{2}-\d{2}$/;

/** The operator's own timezone is the least surprising default (same rule as plans). */
export function defaultTimezone(): string {
	try {
		return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
	} catch {
		return "UTC";
	}
}
