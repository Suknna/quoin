// Audit feature API: typed projections of the consolidated audit contract
// (docs/audit-design.md §6-7). Lists, correlation views and retention settings
// share the platform request client so 401 recovery and problem+json parsing
// stay in one place. Target names (phase, cutoffAt, lastErrorCode) were agreed
// with the coordinator; domainRefType/domainRefId remain the old-projection
// names until main supplies the target object fields.

import { request } from "@/api/workbench";

export type AuditActorType = "user" | "service" | "system";
export type AuditOutcome = "success" | "failure" | "rejected" | "unknown";

/** One audit record. correlationId is absent only for pre-consolidation history. */
export interface AuditEvent {
	id: string;
	correlationId?: string;
	actorType: AuditActorType;
	actorId: string;
	action: string;
	outcome: AuditOutcome;
	phase?: string;
	domainRefType?: string;
	domainRefId?: string;
	clientCommandId?: string;
	requestId?: string;
	taskId?: string;
	attemptId?: string;
	createdAt: string;
}

export interface AuditEventPage {
	items: AuditEvent[];
	nextCursor?: string;
}

export interface AuditEventFilter {
	correlationId?: string;
	actorType?: AuditActorType;
	action?: string;
	outcome?: AuditOutcome;
	/** Inclusive lower bound, RFC3339. */
	since?: string;
	/** Exclusive upper bound, RFC3339. */
	until?: string;
}

export interface AuditCleanupStatus {
	lastRunAt?: string | null;
	lastSuccessCutoffAt?: string | null;
	lastSuccessDeletedCount?: number | null;
	lastFailureAt?: string | null;
	/** Stable mapped code; raw supplier or internal errors never reach the client. */
	lastErrorCode?: string | null;
}

export interface AuditSettings {
	retentionMonths: number;
	minRetentionMonths: number;
	rowVersion: number;
	updatedAt?: string | null;
	updatedBy?: string | null;
	cleanup?: AuditCleanupStatus | null;
}

export interface AuditSettingsPreview {
	retentionMonths: number;
	currentRetentionMonths: number;
	shortening: boolean;
	/** Events recorded before this boundary are expirable; not a deletion promise. */
	cutoffAt?: string | null;
	estimatedExpirableEvents: number;
	estimatedExpirableCorrelations: number;
}

/** Design minimum (docs/audit-design.md §7); the server remains authoritative. */
export const MIN_RETENTION_MONTHS = 6;

const append = (query: URLSearchParams, key: string, value: string | undefined) => {
	if (value) query.set(key, value);
};

/** Empty filters are omitted so the server never interprets blank strings as criteria. */
export function auditEventsQuery(filter: AuditEventFilter, cursor?: string, limit = 50): string {
	const query = new URLSearchParams({ limit: String(limit) });
	append(query, "correlationId", filter.correlationId);
	append(query, "actorType", filter.actorType);
	append(query, "action", filter.action);
	append(query, "outcome", filter.outcome);
	append(query, "since", filter.since);
	append(query, "until", filter.until);
	if (cursor) query.set("cursor", cursor);
	return query.toString();
}

export function listAuditEvents(filter: AuditEventFilter, cursor?: string, signal?: AbortSignal): Promise<AuditEventPage> {
	return request<AuditEventPage>(`/api/v1/audit-events?${auditEventsQuery(filter, cursor)}`, { signal });
}

export function getAuditSettings(signal?: AbortSignal): Promise<AuditSettings> {
	return request<AuditSettings>("/api/v1/admin/audit-settings", { signal });
}

export function previewAuditSettings(retentionMonths: number): Promise<AuditSettingsPreview> {
	return request<AuditSettingsPreview>("/api/v1/admin/audit-settings/preview", {
		method: "POST",
		body: JSON.stringify({ retentionMonths }),
	});
}

export function updateAuditSettings(retentionMonths: number, expectedRowVersion: number, clientCommandId: string): Promise<AuditSettings> {
	return request<AuditSettings>("/api/v1/admin/audit-settings", {
		method: "PATCH",
		body: JSON.stringify({ retentionMonths, expectedRowVersion, clientCommandId }),
	});
}

const phaseLabels: Record<string, string> = {
	access: "访问",
	admission: "准入",
	execution: "执行尝试",
	outcome: "结果",
};

export function phaseLabel(phase: string | undefined): string {
	if (!phase) return "事件";
	return phaseLabels[phase] ?? phase;
}

export const actorLabels: Record<AuditActorType, string> = { user: "用户", service: "服务", system: "系统" };
export const outcomeLabels: Record<AuditOutcome, string> = { success: "成功", failure: "失败", rejected: "已拒绝", unknown: "未知" };

/**
 * Human-readable labels for the registered audit actions (the backend's
 * execution.Operation names, e.g. internal/quoin/auth/ops.go). Unknown or
 * historical actions fall back to the raw dotted name — never hidden.
 */
const actionLabels: Record<string, string> = {
	"auth.login.local": "本地应急登录",
	"auth.login.oidc": "统一身份登录（OIDC）",
	"auth.oidc.jit": "外部身份自动建档",
	"auth.recovery.begin": "管理员离线恢复",
	"auth.session.activity": "会话活动续期",
	"user.create": "创建用户",
	"user.update": "更新用户",
	"user.reset_password": "重置用户密码",
	"user.set_contacts": "配置用户联系方式",
	"user.revoke_sessions": "撤销用户全部会话",
	"user.change_own_password": "修改本人密码",
	"session.logout": "退出登录",
	"session.revoke_own": "撤销本人会话",
	"connection.create": "创建连接",
	"connection.probe.bind": "绑定探测任务",
	"connection.probe.accept": "接受探测结果",
	"connection.probe.cancel_ack": "确认探测取消",
	"connection.probe.interrupt": "中断探测任务",
	"backup.trigger": "触发备份",
	"alert.update": "更新告警",
	"alert_source.credential.reveal": "读取告警源凭据",
	"inspection.run.create": "触发巡检运行",
	"inspection.report.read": "查看巡检报告",
	"audit_settings.update": "调整审计保留期",
	"maintenance.restore.enter": "进入恢复维护",
	"admin.bootstrap": "初始化管理员",
	"auth.bootstrap.seed": "部署引导初始化",
};

export function actionLabel(action: string): string {
	return actionLabels[action] ?? action;
}

/** Object-type labels for the 对象 column; unknown types stay raw. */
const domainRefTypeLabels: Record<string, string> = {
	user: "用户",
	session: "会话",
	connection: "连接",
	backup: "备份",
	deployment: "部署",
	inspection_run: "巡检运行",
	alert_source: "告警源",
	audit_settings: "审计设置",
	maintenance: "维护",
};

export function domainRefTypeLabel(type: string): string {
	return domainRefTypeLabels[type] ?? type;
}

export function formatTimestamp(value: string | null | undefined): string {
	if (!value) return "—";
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN");
}

/** datetime-local values are wall-clock local time; the API expects UTC RFC3339. */
export function localInputToTimestamp(value: string): string | undefined {
	if (!value) return undefined;
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}
