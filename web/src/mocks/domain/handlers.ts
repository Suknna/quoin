import { delay, HttpResponse, http, type JsonBodyType } from "msw";
import type { PluginInspectionScope } from "../../api/generated/types";
import type { InvestigationMessage } from "../../features/investigation/api";
import {
	adminUser,
	DEMO_CREDENTIALS,
	getMockScenario,
	getMockState,
	nextId,
} from "./store";
import type { AdminUser } from "../../features/settings/platform/users/api";

const json = <T extends JsonBodyType>(body: T, init?: ResponseInit) =>
	HttpResponse.json(body, init);
const problem = (status: number, message: string, code = "mock_error") =>
	json({ message, detail: message, code }, { status });
const page = <T>(items: T[]) => json({ items });
const body = <T>(request: Request) => request.json() as Promise<T>;
function slow() {
	return getMockScenario() === "slow" ? delay(350) : undefined;
}
function gate() {
	const scenario = getMockScenario();
	if (scenario === "unavailable")
		return problem(503, "演示服务当前不可用。", "service_unavailable");
	if (scenario === "session-expired" || !getMockState().currentUser)
		return problem(401, "演示会话未认证或已过期。", "unauthorized");
	if (scenario === "maintenance")
		return problem(503, "演示实例正处于维护模式。", "maintenance_active");
	return null;
}
function conflict(expected: number | undefined, actual: number) {
	return getMockScenario() === "conflict" ||
		(expected !== undefined && expected !== actual)
		? problem(
				409,
				"演示数据已被其他操作者更新，请刷新后重试。",
				"row_version_conflict",
			)
		: null;
}

/**
 * Mirrors the domain write-command contract: commands are unique per
 * (principal, clientCommandId), and the mock stores the command type, the
 * non-secret request digest and the successful result. An identical replay
 * returns the stored result; the same ID with a different request conflicts.
 * Rejected attempts are never cached, so a corrected retry reusing the ID
 * executes normally.
 */
async function replayableCommand(
	commandType: string,
	request: Request,
	run: () => Promise<Response>,
): Promise<Response> {
	const raw = (await request
		.clone()
		.json()
		.catch(() => undefined)) as Record<string, unknown> | undefined;
	const clientCommandId =
		typeof raw?.clientCommandId === "string" ? raw.clientCommandId : "";
	if (!clientCommandId) return run();
	const principal = getMockState().currentUser?.id ?? "anonymous";
	const key = `${principal}:${clientCommandId}`;
	const cached = getMockState().commands.get(key);
	if (cached) {
		if (
			cached.commandType === commandType &&
			cached.digest === JSON.stringify({ ...raw, clientCommandId: undefined })
		)
			return HttpResponse.json(cached.body as JsonBodyType, {
				status: cached.status,
			});
		return problem(
			409,
			"相同命令 ID 已被用于不同请求。",
			"command_id_conflict",
		);
	}
	const response = await run();
	if (response.status >= 400) return response;
	const responseBody = (await response
		.json()
		.catch(() => null)) as JsonBodyType | null;
	if (responseBody === null) return response;
	getMockState().commands.set(key, {
		commandType,
		digest: JSON.stringify({ ...raw, clientCommandId: undefined }),
		status: response.status,
		body: responseBody,
	});
	return HttpResponse.json(responseBody, { status: response.status });
}
function required() {
	return gate();
}
/** Configuration, credentials, and administration mutations require an administrator. */
function adminRequired({
	allowMaintenance = false,
}: {
	allowMaintenance?: boolean;
} = {}) {
	const denied =
		getMockScenario() === "maintenance" && allowMaintenance ? null : gate();
	if (denied) return denied;
	return getMockState().currentUser?.role === "admin"
		? null
		: problem(403, "此演示操作仅向管理员开放。", "forbidden");
}
function detailFor(name: string) {
	return getMockState().connections.find((item) => item.name === name);
}
function viewFor(viewKey: string) {
	return getMockState().businessViews.find((view) => view.viewKey === viewKey);
}
function planFor(planKey: string) {
	return getMockState().inspectionPlans.find(
		(item) => item.planKey === planKey,
	);
}

/** Local structural checks only; the real backend stays schema-authoritative. */
const viewKeyPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
function validViewInput(input: {
	viewKey?: string;
	displayName?: string;
	scope?: { labelConditions?: unknown };
}) {
	if (input.viewKey !== undefined && !viewKeyPattern.test(input.viewKey))
		return problem(
			422,
			"视图标识需为小写字母、数字或连字符。",
			"validation_error",
		);
	if (!input.displayName || !input.displayName.trim())
		return problem(422, "显示名称不能为空。", "validation_error");
	const conditions = input.scope?.labelConditions;
	if (
		conditions !== undefined &&
		(conditions === null ||
			typeof conditions !== "object" ||
			Array.isArray(conditions) ||
			Object.values(conditions).some(
				(value) => typeof value !== "string" || !value,
			))
	)
		return problem(422, "标签条件必须是非空字符串映射。", "validation_error");
	return null;
}

/** Plan references must resolve to existing integrations/views; ambiguous scopes are rejected, never guessed. */
async function validatedPlanInput(request: Request) {
	const input = await body<{
		planKey?: string;
		displayName: string;
		enabled?: boolean;
		connectionName: string;
		pluginId?: string;
		templateId?: string;
		templateVersion?: string | null;
		params?: Record<string, unknown>;
		scope: { kind: string; businessViewKey?: string; objects?: unknown[] };
		cron?: string | null;
		timezone?: string;
		expectedRowVersion?: number;
	}>(request);
	if (!input.displayName || !input.displayName.trim())
		return {
			error: problem(422, "计划显示名称不能为空。", "validation_error"),
		};
	if (
		!getMockState().connections.some(
			(item) => item.name === input.connectionName,
		)
	)
		return { error: problem(422, "来源接入不存在。", "validation_error") };
	if (input.scope.kind === "businessView") {
		if (!viewFor(input.scope.businessViewKey ?? ""))
			return { error: problem(422, "业务视图不存在。", "validation_error") };
	} else if (
		input.scope.kind === "objects" &&
		(!Array.isArray(input.scope.objects) || input.scope.objects.length === 0)
	)
		return {
			error: problem(422, "指定对象范围不能为空。", "validation_error"),
		};
	else if (input.scope.kind !== "integration")
		return {
			error: problem(422, "巡检范围类型不受支持。", "validation_error"),
		};
	return { input };
}

/** Converts the parsed request scope into the wire's discriminated union after validation. */
function typedScope(scope: {
	kind: string;
	businessViewKey?: string;
	objects?: unknown[];
}): PluginInspectionScope {
	if (scope.kind === "businessView")
		return {
			kind: "businessView",
			businessViewKey: String(scope.businessViewKey ?? ""),
		};
	if (scope.kind === "objects")
		return {
			kind: "objects",
			objects: (scope.objects ?? []).map((item) => {
				const value = item as { objectType?: unknown; identityKey?: unknown };
				return {
					objectType: String(value.objectType ?? ""),
					identityKey: String(value.identityKey ?? ""),
				};
			}),
		};
	return { kind: "integration" };
}

/** All handlers are local deterministic projections; none contacts a remote service. */
export const domainHandlers = [
	http.get("*/api/v1/integrations/plugins", () => {
		const denied = adminRequired();
		if (denied) return denied;
		return page([
			{
				id: "alertmanager",
				displayName: "Alertmanager",
				description: "接收告警并保留来源。",
				enabled: true,
				version: "1",
				capabilities: [],
			},
			{
				id: "prometheus",
				displayName: "Prometheus",
				description: "自动观测监控目标并提供指标工具。",
				enabled: true,
				version: "1",
				capabilities: [
					"probe",
					"discover",
					"tools",
					"execute_tool",
					"inspection_templates",
					"collect",
				],
			},
			{
				id: "thanos",
				displayName: "Thanos",
				description: "查询和观测授权范围内的指标。",
				enabled: true,
				version: "1",
				capabilities: [
					"probe",
					"discover",
					"tools",
					"execute_tool",
					"inspection_templates",
					"collect",
				],
			},
		]);
	}),
	http.get("*/api/v1/auth/me", async () => {
		await slow();
		const scenario = getMockScenario();
		if (scenario === "unavailable") return problem(503, "演示服务当前不可用。");
		if (scenario === "session-expired" || !getMockState().currentUser)
			return problem(401, "演示会话未认证或已过期。");
		return json(getMockState().currentUser);
	}),
	http.get("*/api/v1/auth/config", () => {
		const scenario = getMockScenario();
		return json({
			local: { enabled: true, visible: true },
			oidc: {
				enabled: scenario === "oidc",
				label: "统一身份登录",
			},
		});
	}),
	http.post("*/api/v1/auth/login", async ({ request }) => {
		await slow();
		const input = await body<{ username: string; password: string }>(request);
		const state = getMockState();
		// A fresh deployment auto-creates the pending admin whose generated
		// initial password is accepted only while the forced change is
		// outstanding; the restricted session drives the same password PUT.
		if (!state.adminInitialized) {
			if (
				input.username !== DEMO_CREDENTIALS.admin.username ||
				input.password !== DEMO_CREDENTIALS.admin.password
			)
				return problem(401, "演示账号或密码不正确。", "invalid_credentials");
			state.currentUser = { ...adminUser, passwordChangeRequired: true };
			return json({ completed: true, user: state.currentUser });
		}
		const matched = state.users.find(
			(candidate) =>
				candidate.username === input.username &&
				state.passwords[candidate.id] === input.password,
		);
		if (!matched)
			return problem(401, "演示账号或密码不正确。", "invalid_credentials");
		// A forced temp credential keeps the restricted marker; everyone else
		// gets the full session directly (ADR-0010 single step).
		const restricted =
			(getMockScenario() === "password-change" && matched.role === "operator") ||
			matched.passwordChangeRequired === true;
		state.currentUser = {
			...matched,
			initialized: matched.initialized !== false,
			authSource: "local" as const,
			passwordChangeRequired: restricted,
		};
		return json({ completed: true, user: state.currentUser });
	}),
	http.put("*/api/v1/auth/password", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const input = await body<{ currentPassword: string; newPassword: string }>(
			request,
		);
		const user = getMockState().currentUser!;
		if (getMockState().passwords[user.id] !== input.currentPassword)
			return problem(401, "当前演示密码不正确。", "invalid_current_password");
		if (input.newPassword.length < 15 || input.newPassword.length > 128)
			return problem(
				422,
				"新密码长度必须在 15 到 128 个字符之间。",
				"validation_error",
			);
		getMockState().passwords[user.id] = input.newPassword;
		const listed = getMockState().users.find((item) => item.id === user.id);
		user.passwordChangeRequired = false;
		user.authRevision += 1;
		if (listed) {
			listed.passwordChangeRequired = false;
			listed.authRevision = user.authRevision;
			listed.rowVersion += 1;
		}
		// Completing the bootstrap admin's forced change also finishes the
		// deployment initialization (the old flow-complete step).
		if (getMockState().adminInitialized === false) getMockState().adminInitialized = true;
		return new HttpResponse(null, { status: 204 });
	}),
	http.post("*/api/v1/auth/logout", () => {
		getMockState().currentUser = null;
		return new HttpResponse(null, { status: 204 });
	}),
	http.get("*/api/v1/auth/sessions", () => {
		const denied = required();
		return denied ?? page(getMockState().sessions);
	}),
	http.get("*/api/v1/auth/contacts", () => {
		const denied = required();
		if (denied) return denied;
		const user = getMockState().currentUser;
		if (!user) return problem(401, "请重新登录。");
		return json({ items: getMockState().contacts[user.id] ?? [] });
	}),
	http.post("*/api/v1/auth/sessions/:id/revoke", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		getMockState().sessions = getMockState().sessions.filter(
			(item) => item.id !== params.id,
		);
		return new HttpResponse(null, { status: 204 });
	}),

	http.get("*/api/v1/alerts", ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const url = new URL(request.url);
		const state = url.searchParams.get("state") ?? "Firing";
		const businessSystemKey = url.searchParams.get("businessSystemKey");
		const items = getMockState().alerts.filter(
			(item) =>
				item.state === state &&
				(!businessSystemKey || item.businessSystemKey === businessSystemKey),
		);
		return json({ snapshotSeq: 10, items });
	}),
	http.get("*/api/v1/alerts/events", () => {
		const denied = required();
		if (denied) return denied;
		return new HttpResponse(
			'event: change\ndata: {"seq":"11","type":"state_changed","occurrenceId":"alert-checkout-latency","rowVersion":3}\n\n',
			{
				headers: {
					"Content-Type": "text/event-stream",
					"Cache-Control": "no-cache",
				},
			},
		);
	}),
	http.get("*/api/v1/alerts/:id/observations", ({ params }) => {
		const denied = required();
		return denied ?? page(getMockState().observations[String(params.id)] ?? []);
	}),
	http.get("*/api/v1/alerts/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = getMockState().alerts.find((alert) => alert.id === params.id);
		return item ? json(item) : problem(404, "未找到告警。", "not_found");
	}),
	http.get("*/api/v1/alert-intake-issues", () => {
		const denied = required();
		return denied ?? page(getMockState().intakeIssues);
	}),
	http.post(
		"*/api/v1/alert-intake-issues/:id/acknowledge",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const issue = getMockState().intakeIssues.find(
				(item) => item.id === params.id,
			);
			if (!issue) return problem(404, "未找到接入问题。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, issue.rowVersion);
			if (stale) return stale;
			getMockState().intakeIssues = getMockState().intakeIssues.filter(
				(item) => item !== issue,
			);
			return new HttpResponse(null, { status: 204 });
		},
	),
	http.get("*/api/v1/alert-sources", () => {
		const denied = adminRequired();
		return denied ?? page(getMockState().alertSources);
	}),
	http.get("*/api/v1/alert-sources/receiver-config", () => {
		const denied = adminRequired();
		return (
			denied ??
			json({
				publicReceiverUrl: "https://quoin.example.test/api/v1/alert-receiver",
			})
		);
	}),
	http.post("*/api/v1/alert-sources", async ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const input = await body<{ key: string; protocol: "alertmanager" }>(
			request,
		);
		if (getMockState().alertSources.some((item) => item.key === input.key))
			return problem(409, "告警源键已存在。", "already_exists");
		const credentialId = nextId("alert-credential");
		getMockState().alertSources.push({
			key: input.key,
			protocol: input.protocol,
			enabled: true,
			rowVersion: 1,
			createdAt: "2026-09-09T09:30:00.000Z",
			credentialCount: 1,
		});
		getMockState().alertCredentials[input.key] = [
			{
				id: credentialId,
				rowVersion: 1,
				state: "Active",
				createdAt: "2026-09-09T09:30:00.000Z",
			},
		];
		return json(
			{
				sourceKey: input.key,
				credentialId,
				revealAvailable: true,
				revealHandle: `${input.key}:${credentialId}`,
			},
			{ status: 201 },
		);
	}),
	http.post(
		"*/api/v1/alert-sources/credentials/reveal",
		async ({ request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const input = await body<{ revealHandle: string }>(request);
			const credentialId =
				input.revealHandle.split(":").at(-1) ?? "alert-credential-1";
			return json({
				credentialId,
				bearerToken: `mock-${credentialId}-not-real`,
			});
		},
	),
	http.get("*/api/v1/alert-sources/:key/credentials", ({ params }) => {
		const denied = adminRequired();
		return (
			denied ?? page(getMockState().alertCredentials[String(params.key)] ?? [])
		);
	}),
	http.post("*/api/v1/alert-sources/:key/rotate", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const source = getMockState().alertSources.find(
			(item) => item.key === params.key,
		);
		if (!source) return problem(404, "未找到告警源。");
		const credentialId = nextId("alert-credential");
		(getMockState().alertCredentials[source.key] ??= []).push({
			id: credentialId,
			rowVersion: 1,
			state: "Active",
			createdAt: "2026-09-09T09:30:00.000Z",
		});
		source.credentialCount += 1;
		source.rowVersion += 1;
		return json({
			sourceKey: source.key,
			credentialId,
			revealAvailable: true,
			revealHandle: `${source.key}:${credentialId}`,
		});
	}),
	http.post(
		"*/api/v1/alert-sources/:key/disable",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const source = getMockState().alertSources.find(
				(item) => item.key === params.key,
			);
			if (!source) return problem(404, "未找到告警源。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, source.rowVersion);
			if (stale) return stale;
			source.enabled = false;
			source.rowVersion += 1;
			return json(source);
		},
	),
	http.post(
		"*/api/v1/alert-sources/:key/credentials/:id/retire",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const credential = (
				getMockState().alertCredentials[String(params.key)] ?? []
			).find((item) => item.id === params.id);
			if (!credential) return problem(404, "未找到告警源凭据。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, credential.rowVersion);
			if (stale) return stale;
			credential.state = "Retired";
			credential.rowVersion += 1;
			return json(credential);
		},
	),
	http.get("*/api/v1/alerts/:id/analyses", ({ params }) => {
		const denied = required();
		return denied ?? page(getMockState().analyses[String(params.id)] ?? []);
	}),
	// The detail view selects the first analysis automatically, making its linked evidence readable on initial navigation.
	http.get("*/api/v1/alerts/:id/analyses/:analysisId", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const value = (getMockState().analyses[String(params.id)] ?? []).find(
			(item) => item.id === params.analysisId,
		);
		return value ? json(value) : problem(404, "未找到分析。");
	}),
	http.post("*/api/v1/alerts/:id/analyses", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const id = nextId("analysis");
		const value = {
			id,
			state: "Succeeded" as const,
			rowVersion: 1,
			createdAt: "2026-09-09T09:30:00.000Z",
			attemptCount: 1,
			output: {
				id: `${id}-output`,
				modelId: "gpt-demo",
				content: "本地演示分析已完成。",
				evidenceIds: ["evidence-latency"],
				createdAt: "2026-09-09T09:30:00.000Z",
			},
		};
		(getMockState().analyses[String(params.id)] ??= []).unshift(value);
		return json(value, { status: 201 });
	}),
	http.get(
		"*/api/v1/alerts/:occurrenceId/analyses/:analysisId/attempts",
		({ params }) => {
			const denied = required();
			return (
				denied ??
				page(getMockState().analysisAttempts[String(params.analysisId)] ?? [])
			);
		},
	),
	http.post("*/api/v1/alerts/:occurrenceId/analyses/:analysisId/cancel", () => {
		const denied = required();
		return (
			denied ?? problem(409, "该演示分析已结束，不能取消。", "attempt_terminal")
		);
	}),

	http.get("*/api/v1/investigations", () => {
		const denied = required();
		return (
			denied ??
			page(
				getMockState().investigations.map((item) => ({
					id: item.id,
					displayTitle: item.displayTitle,
					lastActivityAt: item.lastActivityAt,
					createdAt: item.createdAt,
					createdBy: item.createdBy,
					headMessageId: item.headMessageId,
					activeAttemptId: item.activeAttemptId,
				})),
			)
		);
	}),
	http.post("*/api/v1/investigations", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const input = await body<{
			content: string;
			sources: Array<{
				type:
					| "occurrence"
					| "initial_analysis"
					| "evidence"
					| "inspection_report";
				sourceId: string;
			}>;
		}>(request);
		const id = nextId("investigation");
		const messageId = nextId("message");
		const createdAt = "2026-09-09T09:30:00.000Z";
		const detail = {
			id,
			displayTitle: input.content.slice(0, 48) || "新的本地调查",
			lastActivityAt: createdAt,
			createdAt,
			createdBy: getMockState().currentUser!.id,
			headMessageId: messageId,
			messageCount: 1,
			attemptCount: 0,
			sources: input.sources.map((source, index) => ({
				id: `${id}-source-${index}`,
				...source,
				linkedAt: createdAt,
			})),
		};
		getMockState().investigations.unshift(detail);
		getMockState().messages[id] = [
			{
				id: messageId,
				seq: 1,
				role: "user",
				status: "active",
				content: input.content,
				attachments: [],
				evidenceIds: null,
				createdAt,
			},
		];
		return json(detail, { status: 201 });
	}),
	http.get("*/api/v1/investigations/:id/messages", ({ params }) => {
		const denied = required();
		return denied ?? page(getMockState().messages[String(params.id)] ?? []);
	}),
	http.post(
		"*/api/v1/investigations/:id/messages",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const messages = getMockState().messages[String(params.id)];
			const detail = getMockState().investigations.find(
				(item) => item.id === params.id,
			);
			if (!messages || !detail) return problem(404, "未找到调查。");
			const input = await body<{
				content: string;
				expectedHeadMessageId: string | null;
				attachmentIds: string[];
			}>(request);
			if (input.expectedHeadMessageId !== detail.headMessageId)
				return problem(409, "调查消息已更新，请刷新后重试。", "head_conflict");
			const message = {
				id: nextId("message"),
				seq: messages.length + 1,
				role: "user" as const,
				status: "active" as const,
				content: input.content,
				parentMessageId: detail.headMessageId,
				attachments: input.attachmentIds
					.map((id) => getMockState().attachments.get(id))
					.filter(
						(value): value is InvestigationMessage["attachments"][number] =>
							Boolean(value),
					),
				evidenceIds: null,
				createdAt: "2026-09-09T09:30:00.000Z",
			};
			messages.push(message);
			detail.headMessageId = message.id;
			detail.messageCount = messages.length;
			return json(message, { status: 201 });
		},
	),
	http.post(
		"*/api/v1/investigations/:id/messages/:messageId/stream",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const messages = getMockState().messages[String(params.id)] ?? [];
			const source = messages.find(
				(message) => message.id === params.messageId,
			);
			if (!source) return problem(404, "未找到调查消息。");
			const reply = "本地演示回复：已基于关联告警生成可读的排查建议。";
			const wire = [
				'{"type":"text-start","id":"mock-reply"}',
				`{"type":"text-delta","textDelta":${JSON.stringify(reply)}}`,
				'{"type":"text-end"}',
				'{"type":"finish","finishReason":"stop","usage":{"inputTokens":12,"outputTokens":16}}',
				"[DONE]",
			]
				.map((value) => `data: ${value}\n\n`)
				.join("");
			return new HttpResponse(wire, {
				headers: { "Content-Type": "text/event-stream" },
			});
		},
	),
	http.get("*/api/v1/investigations/:id/attempts", ({ params }) => {
		const denied = required();
		return (
			denied ??
			page(getMockState().investigationAttempts[String(params.id)] ?? [])
		);
	}),
	http.get("*/api/v1/investigations/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = getMockState().investigations.find(
			(investigation) => investigation.id === params.id,
		);
		return item ? json(item) : problem(404, "未找到调查。");
	}),

	http.get("*/api/v1/connections", () => {
		const denied = required();
		return denied ?? page(getMockState().connections);
	}),
	http.post("*/api/v1/connections", async ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const input = await body<{
			name: string;
			connection: Record<string, unknown>;
		}>(request);
		if (detailFor(input.name))
			return problem(409, "连接名称已存在。", "already_exists");
		if (
			input.connection.type !== "prometheus" &&
			input.connection.type !== "thanos" &&
			input.connection.type !== "model_provider"
		)
			return problem(422, "连接类型无效。", "validation_error");
		const connection = {
			id: nextId("connection"),
			name: input.name,
			type: input.connection.type as
				| "prometheus"
				| "thanos"
				| "model_provider",
			enabled: false,
			revalidationRequired: false,
			rowVersion: 1,
			config: {
				...input.connection,
				password: undefined,
				apiKey: undefined,
			},
			revisionCount: 1,
			generationCount: 1,
		};
		getMockState().connections.push(connection);
		return json(connection, { status: 201 });
	}),
	http.post("*/api/v1/model-providers/discover", () => {
		const denied = required();
		return (
			denied ??
			json({
				available: true,
				items: [{ id: "gpt-demo" }, { id: "embedding-demo" }],
			})
		);
	}),
	http.get("*/api/v1/connections/:name/probe-results", ({ params }) => {
		const denied = required();
		return (
			denied ?? page(getMockState().probeResults[String(params.name)] ?? [])
		);
	}),
	http.post("*/api/v1/connections/:name/probe", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const connection = detailFor(String(params.name));
		if (!connection) return problem(404, "未找到连接。");
		const attempt = {
			id: nextId("probe-attempt"),
			type: "connection_probe" as const,
			state: "Succeeded" as const,
			rowVersion: 1,
			createdAt: "2026-09-09T09:30:00.000Z",
			startedAt: "2026-09-09T09:30:00.000Z",
			endedAt: "2026-09-09T09:30:00.000Z",
		};
		(getMockState().probes[connection.name] ??= []).push(attempt);
		return json({ id: attempt.id, state: attempt.state }, { status: 201 });
	}),
	http.get(
		"*/api/v1/connections/:name/probe-attempts/:attemptId",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const attempt = (getMockState().probes[String(params.name)] ?? []).find(
				(item) => item.id === params.attemptId,
			);
			return attempt ? json(attempt) : problem(404, "未找到探测任务。");
		},
	),
	http.post(
		"*/api/v1/connections/:name/enable",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const connection = detailFor(String(params.name));
			if (!connection) return problem(404, "未找到连接。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, connection.rowVersion);
			if (stale) return stale;
			connection.enabled = true;
			connection.rowVersion += 1;
			return json(connection);
		},
	),
	http.post(
		"*/api/v1/connections/:name/disable",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const connection = detailFor(String(params.name));
			if (!connection) return problem(404, "未找到连接。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, connection.rowVersion);
			if (stale) return stale;
			connection.enabled = false;
			connection.rowVersion += 1;
			return json(connection);
		},
	),
	http.get("*/api/v1/connections/:name", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = detailFor(String(params.name));
		return item ? json(item) : problem(404, "未找到连接。");
	}),

	http.get("*/api/v1/business-context", () => {
		const denied = required();
		return (
			denied ??
			page(
				getMockState().businessContext,
			)
		);
	}),
	http.get("*/api/v1/label-contracts", () => {
		const denied = required();
		return denied ?? page(getMockState().labels);
	}),

	// Business views are optional admin-scoped scope-and-description objects
	// (ADR 0004). The mock keeps the real command shapes: clientCommandId on
	// writes, expectedRowVersion fencing on updates, and viewKey immutability.
	http.get("*/api/v1/business-views", () => {
		const denied = adminRequired();
		return denied ?? page(getMockState().businessViews);
	}),
	http.post("*/api/v1/business-views", ({ request }) =>
		replayableCommand("create_business_view", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const input = await body<{
				viewKey: string;
				displayName: string;
				description: string;
				scope: {
					connectionName?: string;
					labelConditions: Record<string, string>;
				};
			}>(request);
			const invalid = validViewInput(input);
			if (invalid) return invalid;
			if (viewFor(input.viewKey))
				return problem(409, "视图标识已存在。", "view_key_exists");
			const now = "2026-09-09T09:30:00.000Z";
			const view = {
				viewKey: input.viewKey,
				displayName: input.displayName,
				description: input.description ?? "",
				scope: {
					...input.scope,
					labelConditions: { ...(input.scope?.labelConditions ?? {}) },
				},
				rowVersion: 1,
				createdAt: now,
				updatedAt: now,
			};
			getMockState().businessViews.unshift(view);
			return json(view, { status: 201 });
		}),
	),
	http.get("*/api/v1/business-views/:viewKey", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const view = viewFor(String(params.viewKey));
		return view ? json(view) : problem(404, "未找到业务视图。");
	}),
	http.put("*/api/v1/business-views/:viewKey", ({ params, request }) =>
		replayableCommand("update_business_view", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const view = viewFor(String(params.viewKey));
			if (!view) return problem(404, "未找到业务视图。");
			const input = await body<{
				displayName: string;
				description: string;
				scope: {
					connectionName?: string;
					labelConditions: Record<string, string>;
				};
				expectedRowVersion: number;
			}>(request);
			const invalid = validViewInput(input);
			if (invalid) return invalid;
			const stale = conflict(input.expectedRowVersion, view.rowVersion);
			if (stale) return stale;
			view.displayName = input.displayName;
			view.description = input.description ?? "";
			view.scope = {
				...input.scope,
				labelConditions: { ...(input.scope?.labelConditions ?? {}) },
			};
			view.updatedAt = "2026-09-09T09:30:00.000Z";
			view.rowVersion += 1;
			return json(view);
		}),
	),

	// Standalone plugin inspection plans: the real handler group sits behind
	// the Admin boundary (the reader surfaces 403 for non-admins), so every
	// read and write here is adminRequired to match.
	http.get("*/api/v1/inspections/plans", () => {
		const denied = adminRequired();
		return denied ?? page(getMockState().inspectionPlans);
	}),
	http.get("*/api/v1/inspections/plans/:planKey", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const plan = planFor(String(params.planKey));
		return plan ? json(plan) : problem(404, "未找到巡检计划。");
	}),
	http.post("*/api/v1/inspections/plans", ({ request }) =>
		replayableCommand("create_inspection_plan", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const parsed = await validatedPlanInput(request);
			if (parsed.error) return parsed.error;
			const input = parsed.input!;
			if (planFor(String(input.planKey)))
				return problem(409, "巡检计划标识已存在。", "plan_key_exists");
			const now = "2026-09-09T09:30:00.000Z";
			const plan = {
				planKey: String(input.planKey),
				displayName: input.displayName,
				enabled: true,
				connectionName: input.connectionName,
				pluginId: input.pluginId ?? "thanos",
				templateId: input.templateId ?? "default",
				templateVersion: input.templateVersion ?? null,
				params: { ...(input.params ?? {}) },
				scope: typedScope(input.scope),
				cron: input.cron ?? null,
				timezone: input.timezone ?? "UTC",
				rowVersion: 1,
				createdAt: now,
				updatedAt: now,
			};
			getMockState().inspectionPlans.unshift(plan);
			return json(plan, { status: 201 });
		}),
	),
	http.put("*/api/v1/inspections/plans/:planKey", ({ params, request }) =>
		replayableCommand("update_inspection_plan", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const plan = planFor(String(params.planKey));
			if (!plan) return problem(404, "未找到巡检计划。");
			const parsed = await validatedPlanInput(request);
			if (parsed.error) return parsed.error;
			const input = parsed.input!;
			const stale = conflict(input.expectedRowVersion, plan.rowVersion);
			if (stale) return stale;
			Object.assign(plan, {
				displayName: input.displayName,
				connectionName: input.connectionName,
				scope: typedScope(input.scope),
				cron: input.cron ?? null,
				timezone: input.timezone ?? plan.timezone,
				updatedAt: "2026-09-09T09:30:00.000Z",
			});
			plan.rowVersion += 1;
			return json(plan);
		}),
	),

	http.get("*/api/v1/inspections/runs", ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const planKey = new URL(request.url).searchParams.get("planKey");
		return page(
			getMockState().inspectionRuns.filter(
				(run) => !planKey || run.planKey === planKey,
			),
		);
	}),
	http.post("*/api/v1/inspections/runs", ({ request }) =>
		replayableCommand("create_inspection_run", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			// Only the plan identity is accepted: scope and connection freeze
			// server-side from the plan, never from the request.
			const input = await body<{ planKey: string }>(request);
			const plan = planFor(input.planKey);
			if (!plan) return problem(404, "未找到巡检计划。");
			const now = "2026-09-09T09:30:00.000Z";
			const run = {
				id: nextId("inspection-run"),
				planKey: plan.planKey,
				connectionName: plan.connectionName,
				state: "Completed" as const,
				rowVersion: 1,
				triggerKind: "manual" as const,
				evidenceAt: now,
				createdAt: now,
				checks: [
					{
						checkKey: "latency",
						status: "ok" as const,
						evidenceId: "evidence-latency",
					},
				],
				reportCount: 0,
				analysisActive: false,
			};
			getMockState().inspectionRuns.unshift(run);
			return json(run, { status: 201 });
		}),
	),
	http.get(
		"*/api/v1/inspections/runs/:runId/reports/:version",
		({ params }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const value = (getMockState().reports[String(params.runId)] ?? []).find(
				(report) => report.version === Number(params.version),
			);
			return value ? json(value) : problem(404, "未找到巡检报告。");
		},
	),
	http.get("*/api/v1/inspections/runs/:runId/reports", ({ params }) => {
		const denied = adminRequired();
		return (
			denied ??
			page(
				(getMockState().reports[String(params.runId)] ?? []).map((item) => ({
					version: item.version,
					modelId: item.modelId,
					createdAt: item.createdAt,
				})),
			)
		);
	}),
	http.get("*/api/v1/inspections/runs/:runId", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const value = getMockState().inspectionRuns.find(
			(run) => run.id === params.runId,
		);
		return value ? json(value) : problem(404, "未找到巡检运行。");
	}),

	http.post("*/api/v1/investigation-attachments", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const form = await request.formData();
		const file = form.get("file");
		if (!(file instanceof File) || file.type !== "text/plain")
			return problem(
				422,
				"离线演示只接受 text/plain 附件。",
				"validation_error",
			);
		const id = nextId("attachment");
		const attachment = {
			id,
			artifactId: `artifact-${id}`,
			originalFilename: file.name,
			mediaType: "text/plain",
			sizeBytes: file.size,
			digest: `sha256:${id}`,
			bodyExpired: false,
			createdAt: "2026-09-09T09:30:00.000Z",
		};
		getMockState().attachments.set(id, attachment);
		return json(attachment, { status: 201 });
	}),
	http.post("*/api/v1/investigations/:id/undo", async ({ params, request }) => {
		const denied = required();
		if (denied) return denied;
		const detail = getMockState().investigations.find(
			(item) => item.id === params.id,
		);
		const messages = getMockState().messages[String(params.id)];
		if (!detail || !messages) return problem(404, "未找到调查。");
		const input = await body<{ expectedHeadMessageId: string }>(request);
		if (detail.headMessageId !== input.expectedHeadMessageId)
			return problem(409, "调查消息已更新，请刷新后重试。", "head_conflict");
		const removed = messages.pop();
		if (removed) {
			detail.headMessageId = messages.at(-1)?.id;
			detail.messageCount = messages.length;
		}
		return json(detail);
	}),
	http.post(
		"*/api/v1/investigations/:id/attempts/:attemptId/cancel",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const attempt = (
				getMockState().investigationAttempts[String(params.id)] ?? []
			).find((item) => item.id === params.attemptId);
			if (!attempt) return problem(404, "未找到调查任务。");
			attempt.state = "Cancelled";
			attempt.rowVersion += 1;
			attempt.endedAt = "2026-09-09T09:30:00.000Z";
			return json(attempt);
		},
	),
	http.post(
		"*/api/v1/investigations/:id/attempts/:attemptId/retry",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const attempt = {
				id: nextId("attempt"),
				type: "investigation",
				state: "Succeeded" as const,
				rowVersion: 1,
				createdAt: "2026-09-09T09:30:00.000Z",
				startedAt: "2026-09-09T09:30:00.000Z",
				endedAt: "2026-09-09T09:30:00.000Z",
			};
			(getMockState().investigationAttempts[String(params.id)] ??= []).push(
				attempt,
			);
			return json(attempt, { status: 201 });
		},
	),
	http.get("*/api/v1/investigations/:id/attempts/:attemptId/tool-calls", () => {
		const denied = required();
		return (
			denied ??
			page([
				{
					id: "tool-call-1",
					attemptId: "attempt-1",
					modelCallId: "model-call-1",
					callSeq: 1,
					toolIndex: 0,
					providerToolCallId: "mock-call",
					toolName: "thanos_query",
					toolVersion: "1",
					arguments: { query: "checkout_latency" },
					executionMode: "read_only",
					failureMode: "fail_closed",
					status: "succeeded",
					rowVersion: 1,
					result: { evidenceId: "evidence-latency" },
					createdAt: "2026-09-09T09:30:00.000Z",
				},
			])
		);
	}),
	http.post("*/api/v1/knowledge/feedback", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const input = await body<{
			targetType: string;
			targetId: string;
			value: string;
			note?: string;
		}>(request);
		const event = {
			id: nextId("feedback"),
			targetType: input.targetType,
			targetId: input.targetId,
			value: input.value,
			note: input.note,
			createdBy: getMockState().currentUser!.id,
			createdAt: "2026-09-09T09:30:00.000Z",
		};
		getMockState().feedback.unshift(event);
		return json(event, { status: 201 });
	}),
	http.get("*/api/v1/knowledge/feedback", ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const url = new URL(request.url);
		const targetType = url.searchParams.get("targetType");
		const targetId = url.searchParams.get("targetId");
		const items = getMockState().feedback.filter(
			(item) => item.targetType === targetType && item.targetId === targetId,
		);
		return json({ latestValue: items[0]?.value, items });
	}),
	http.get("*/api/v1/knowledge/candidates", () => {
		const denied = required();
		return denied ?? page(getMockState().candidates);
	}),
	http.get("*/api/v1/knowledge/candidates/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const value = getMockState().candidates.find(
			(candidate) => candidate.id === params.id,
		);
		return value ? json(value) : problem(404, "未找到知识候选。");
	}),
	http.patch(
		"*/api/v1/knowledge/candidates/:id",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const value = getMockState().candidates.find(
				(candidate) => candidate.id === params.id,
			);
			if (!value) return problem(404, "未找到知识候选。");
			const input = await body<{
				expectedRevision: number;
				title?: string;
				body?: string;
				scope?: Record<string, unknown>;
			}>(request);
			const stale = conflict(input.expectedRevision, value.draftRevision);
			if (stale) return stale;
			if (input.title !== undefined) value.draftTitle = input.title;
			if (input.body !== undefined) value.draftBody = input.body;
			if (input.scope !== undefined) value.draftScope = input.scope;
			value.draftRevision += 1;
			value.rowVersion += 1;
			return json(value);
		},
	),
	http.post(
		"*/api/v1/knowledge/candidates/:id/confirm",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const value = getMockState().candidates.find(
				(candidate) => candidate.id === params.id,
			);
			if (!value) return problem(404, "未找到知识候选。");
			const input = await body<{ expectedRevision: number }>(request);
			const stale = conflict(input.expectedRevision, value.draftRevision);
			if (stale) return stale;
			value.state = "Confirmed";
			value.confirmedKnowledgeId ??= "knowledge-1";
			value.rowVersion += 1;
			return json(value);
		},
	),
	http.get("*/api/v1/knowledge/items/:id/versions/:versionId", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const value = (getMockState().versions[String(params.id)] ?? []).find(
			(version) => version.id === params.versionId,
		);
		return value ? json(value) : problem(404, "未找到知识版本。");
	}),
	http.get("*/api/v1/knowledge/items/:id/versions", ({ params }) => {
		const denied = required();
		return denied ?? page(getMockState().versions[String(params.id)] ?? []);
	}),
	http.get("*/api/v1/knowledge/items/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const value = getMockState().knowledge.find(
			(item) => item.id === params.id,
		);
		return value ? json(value) : problem(404, "未找到知识项。");
	}),
	http.get("*/api/v1/knowledge/import-batches", () => {
		const denied = required();
		return denied ?? page(getMockState().imports);
	}),
	http.post("*/api/v1/knowledge/import-batches", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const input = await body<{ text: string }>(request);
		const id = nextId("import");
		const batch = {
			id,
			state: "AwaitingConfirmation" as const,
			rowVersion: 1,
			generation: 1,
			createdAt: "2026-09-09T09:30:00.000Z",
			candidates: [
				{
					id: `${id}-candidate`,
					sourceType: "source_material" as const,
					sourceId: id,
					state: "AwaitingConfirmation" as const,
					rowVersion: 1,
					generation: 1,
					draftRevision: 1,
					draftTitle: input.text.slice(0, 40) || "导入知识",
					draftBody: input.text,
					originalSuggestion: {
						v: 1,
						source: { type: "source_material" as const, id },
						title: input.text.slice(0, 40) || "导入知识",
						body: input.text,
					},
				},
			],
		};
		getMockState().imports.unshift(batch);
		return json(batch, { status: 201 });
	}),
	http.get("*/api/v1/knowledge", ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const query = new URL(request.url).searchParams.get("q");
		if (query !== null) {
			const hits = getMockState()
				.knowledge.filter((item) => item.title.includes(query))
				.map((knowledge) => ({
					knowledge,
					score: 0.97,
					indexState: "ready" as const,
				}));
			return json({
				mode: "query",
				exactTextMatches: hits,
				semanticMatches: hits,
			});
		}
		return json({ mode: "browse", items: getMockState().knowledge });
	}),

	http.get("*/api/v1/evidence/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = getMockState().evidence[String(params.id)];
		return item ? json(item) : problem(404, "未找到证据。");
	}),
	http.get("*/api/v1/artifacts/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		return json({
			id: String(params.id),
			kind: "tool_result",
			sensitive: false,
			retentionKind: "generated",
			ownerType: "evidence",
			ownerId: "evidence-latency",
			sizeBytes: 48,
			sha256: "sha256:mock",
			bodyExpired: false,
			createdAt: "2026-09-09T09:30:00.000Z",
		});
	}),
	http.get("*/api/v1/artifacts/:id/content", () => {
		const denied = required();
		return (
			denied ??
			new HttpResponse("Mock artifact content. No external data was read.", {
				headers: { "Content-Type": "text/plain" },
			})
		);
	}),

	http.get("*/api/v1/admin/users", () => {
		const denied = required();
		return denied ?? page(getMockState().users);
	}),
	http.get("*/api/v1/audit-events", () => {
		const denied = required();
		return denied ?? page(getMockState().auditEvents);
	}),
	http.get("*/api/v1/admin/about", () => {
		const scenario = getMockScenario();
		const denied = adminRequired({ allowMaintenance: true });
		const long =
			scenario === "platform-boundary"
				? "platform-preview-release-with-an-intentionally-long-non-secret-version-string-for-layout-boundary-verification-2026-09-09"
				: undefined;
		return (
			denied ??
			json({
				releaseVersion: long ?? "mock-preview",
				maintenance: {
					active: scenario === "maintenance",
					reason: scenario === "maintenance" ? "Upgrade" : undefined,
					rowVersion: 1,
				},
				components: [
					{
						slot: "plinth",
						state: "registered",
						currentGeneration: 1,
						rowVersion: 1,
						connected: true,
						lastSeenAt: "2026-09-09T09:30:00.000Z",
						releaseVersion: long ?? "mock-plinth",
					},
				],
			})
		);
	}),
	http.get("*/api/v1/maintenance", () => {
		const scenario = getMockScenario();
		const denied = scenario === "maintenance" ? null : required();
		if (denied) return denied;
		const count =
			scenario === "platform-boundary"
				? 50
				: scenario === "platform-one"
					? 1
					: 0;
		const items = Array.from({ length: count }, (_, index) => {
			const ordinal = index + 1;
			const boundary = ordinal === 50;
			return {
				kind: "preview_check",
				objectKey: boundary
					? "platform-preview-object-key-with-an-intentionally-long-non-secret-identifier-for-50th-row-layout-verification"
					: `platform-preview-${ordinal}`,
				safeState: "Safe" as const,
				detailCode: boundary
					? "preview-detail-with-an-intentionally-long-non-secret-maintenance-description-for-wrapping-and-horizontal-width-verification"
					: "preview_safe",
			};
		});
		return json({
			active: scenario === "maintenance",
			reason: scenario === "maintenance" ? "Upgrade" : undefined,
			rowVersion: 1,
			items,
		});
	}),
	http.post("*/api/v1/maintenance/upgrade/prepare", () => {
		const denied = required();
		return (
			denied ??
			json({ active: true, reason: "Upgrade", rowVersion: 2, items: [] })
		);
	}),
	http.post("*/api/v1/maintenance/exit", () => {
		const denied = required();
		return denied ?? json({ active: false, rowVersion: 2, items: [] });
	}),
	http.post("*/api/v1/admin/users", async ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const input = await body<{
			username: string;
			displayName: string;
			role: "admin" | "operator";
		}>(request);
		const user: AdminUser = {
			id: nextId("user"),
			username: input.username,
			displayName: input.displayName,
			role: input.role,
			enabled: true,
			authRevision: 1,
			rowVersion: 1,
			passwordChangeRequired: true,
			authSource: "local" as const,
			lastLoginAt: null,
		};
		getMockState().users.push(user);
		return json(user, { status: 201 });
	}),
	http.patch("*/api/v1/admin/users/:id", async ({ params, request }) => {
		const denied = required();
		if (denied) return denied;
		const user = getMockState().users.find((item) => item.id === params.id);
		if (!user) return problem(404, "未找到用户。");
		const input = await body<{
			expectedRowVersion: number;
			displayName?: string;
			enabled?: boolean;
			role?: "admin" | "operator";
		}>(request);
		const stale = conflict(input.expectedRowVersion, user.rowVersion);
		if (stale) return stale;
		Object.assign(
			user,
			input.displayName === undefined ? {} : { displayName: input.displayName },
			input.enabled === undefined ? {} : { enabled: input.enabled },
			input.role === undefined ? {} : { role: input.role },
		);
		user.rowVersion += 1;
		return json(user);
	}),
	http.post("*/api/v1/admin/users/:id/reset-password", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const user = getMockState().users.find((item) => item.id === params.id);
		if (!user) return problem(404, "未找到用户。");
		user.passwordChangeRequired = true;
		user.authRevision += 1;
		user.rowVersion += 1;
		return json({ user, revokedSessionCount: 1 });
	}),
	http.post("*/api/v1/admin/users/:id/revoke-sessions", () => {
		const denied = required();
		return denied ?? json({ revokedSessionCount: 1 });
	}),
	http.get("*/api/v1/connections/:name/revisions", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const connection = detailFor(String(params.name));
		return connection
			? page([
					{
						id: connection.currentRevisionId ?? "revision-1",
						revisionSeq: connection.revisionCount,
						config: connection.config,
						createdAt: "2026-09-09T09:30:00.000Z",
					},
				])
			: problem(404, "未找到连接。");
	}),
	http.get("*/api/v1/connections/:name/generations", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const connection = detailFor(String(params.name));
		return connection
			? page([
					{
						id: connection.currentCredentialGenerationId ?? "generation-1",
						generationSeq: connection.generationCount,
						createdBy: "user-admin",
						createdAt: "2026-09-09T09:30:00.000Z",
					},
				])
			: problem(404, "未找到连接。");
	}),
	http.post(
		"*/api/v1/connections/:name/rotate",
		async ({ params, request }) => {
			const denied = required();
			if (denied) return denied;
			const connection = detailFor(String(params.name));
			if (!connection) return problem(404, "未找到连接。");
			const input = await body<{
				expectedRowVersion: number;
				connection: Record<string, unknown>;
			}>(request);
			const stale = conflict(input.expectedRowVersion, connection.rowVersion);
			if (stale) return stale;
			connection.config = {
				...connection.config,
				...input.connection,
				password: undefined,
				apiKey: undefined,
				kubeconfig: undefined,
			};
			connection.rowVersion += 1;
			connection.revisionCount += 1;
			connection.generationCount += 1;
			connection.currentRevisionId = nextId("revision");
			connection.currentCredentialGenerationId = nextId("generation");
			return json(connection);
		},
	),
	http.post(
		"*/api/v1/connections/:name/probe-attempts/:attemptId/cancel",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const attempt = (getMockState().probes[String(params.name)] ?? []).find(
				(item) => item.id === params.attemptId,
			);
			if (!attempt) return problem(404, "未找到探测任务。");
			attempt.state = "Cancelled";
			attempt.rowVersion += 1;
			attempt.endedAt = "2026-09-09T09:30:00.000Z";
			return json(attempt);
		},
	),
	http.post("*/api/v1/knowledge/candidates/:id/exclude", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const candidate = getMockState().candidates.find(
			(item) => item.id === params.id,
		);
		if (!candidate) return problem(404, "未找到知识候选。");
		candidate.state = "Excluded";
		candidate.rowVersion += 1;
		return json(candidate);
	}),
	http.get("*/api/v1/knowledge/import-batches/:id", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const batch = getMockState().imports.find((item) => item.id === params.id);
		return batch ? json(batch) : problem(404, "未找到导入批次。");
	}),
	http.post("*/api/v1/knowledge/import-batches/:id/cancel", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const batch = getMockState().imports.find((item) => item.id === params.id);
		if (!batch) return problem(404, "未找到导入批次。");
		batch.state = "Cancelled";
		batch.rowVersion += 1;
		return json(batch);
	}),
	http.post("*/api/v1/knowledge/import-batches/:id/confirm", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const batch = getMockState().imports.find((item) => item.id === params.id);
		if (!batch) return problem(404, "未找到导入批次。");
		batch.state = "Completed";
		batch.rowVersion += 1;
		return json(batch);
	}),
	http.post("*/api/v1/inspections/runs/:runId/cancel", ({ params, request }) =>
		replayableCommand("cancel_inspection_run", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const run = getMockState().inspectionRuns.find(
				(item) => item.id === params.runId,
			);
			if (!run) return problem(404, "未找到巡检运行。");
			if (!["Queued", "Running"].includes(run.state))
				return problem(409, "演示巡检运行已结束。", "invalid_state");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, run.rowVersion);
			if (stale) return stale;
			run.state = "Cancelled";
			run.rowVersion += 1;
			return json(run);
		}),
	),
	http.post("*/api/v1/inspections/runs/:runId/analyze", ({ params, request }) =>
		replayableCommand("analyze_inspection_run", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const run = getMockState().inspectionRuns.find(
				(item) => item.id === params.runId,
			);
			if (!run) return problem(404, "未找到巡检运行。");
			const attempt = {
				id: nextId("inspection-attempt"),
				type: "inspection_analysis" as const,
				state: "Succeeded" as const,
				rowVersion: 1,
				createdAt: "2026-09-09T09:30:00.000Z",
			};
			run.analysisActive = false;
			run.latestAnalysis = { id: attempt.id, state: attempt.state };
			run.rowVersion += 1;
			return json(attempt);
		}),
	),
	http.post("*/api/v1/inspections/runs/:runId/rerun", ({ params, request }) =>
		replayableCommand("rerun_inspection_run", request, async () => {
			const denied = adminRequired();
			if (denied) return denied;
			const prior = getMockState().inspectionRuns.find(
				(item) => item.id === params.runId,
			);
			if (!prior) return problem(404, "未找到巡检运行。");
			const run = {
				...prior,
				id: nextId("inspection-run"),
				rowVersion: 1,
				createdAt: "2026-09-09T09:30:00.000Z",
			};
			getMockState().inspectionRuns.unshift(run);
			return json(run, { status: 201 });
		}),
	),
	// Retired with the old declaration publish fence; configuration versions
	// stay readable as immutable history only.
	http.post("*/api/v1/knowledge/items/:id/versions", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const knowledge = getMockState().knowledge.find(
			(item) => item.id === params.id,
		);
		if (!knowledge) return problem(404, "未找到知识项。");
		const candidate = getMockState().candidates[0];
		const created = {
			...candidate,
			id: nextId("candidate"),
			sourceType: "knowledge_version" as const,
			sourceId: knowledge.currentVersionId,
			state: "AwaitingConfirmation" as const,
			rowVersion: 1,
			draftRevision: 1,
			targetKnowledgeId: knowledge.id,
		};
		getMockState().candidates.push(created);
		return json(created, { status: 201 });
	}),
	http.post(
		"*/api/v1/knowledge/items/:id/versions/:versionId/stop-reuse",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const version = (getMockState().versions[String(params.id)] ?? []).find(
				(item) => item.id === params.versionId,
			);
			if (!version) return problem(404, "未找到知识版本。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(
				input.expectedRowVersion,
				version.retrievalStateRowVersion,
			);
			if (stale) return stale;
			version.eligible = false;
			version.exitedAt = "2026-09-09T09:30:00.000Z";
			version.exitReason = "operator_stopped";
			version.retrievalStateRowVersion += 1;
			return new HttpResponse(null, { status: 204 });
		},
	),
	http.get("*/api/v1/backups", () => {
		const denied = adminRequired();
		return denied ?? json({ items: getMockState().backups });
	}),
	http.get("*/api/v1/backups/settings", () => {
		const denied = adminRequired();
		return denied ?? json(getMockState().backupSettings);
	}),
	http.put("*/api/v1/backups/settings", async ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const input = await body<{
			expectedRowVersion: number;
			enabled: boolean;
			scheduleCron: string | null;
			timezone: string;
			retentionCount: number;
		}>(request);
		const settings = getMockState().backupSettings;
		const stale = conflict(input.expectedRowVersion, settings.rowVersion);
		if (stale) return stale;
		Object.assign(settings, input, { rowVersion: settings.rowVersion + 1 });
		return json(settings);
	}),
	http.get("*/api/v1/artifacts/retention-settings", () => {
		const denied = adminRequired();
		return denied ?? json(getMockState().artifactRetention);
	}),
	http.put("*/api/v1/artifacts/retention-settings", async ({ request }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const input = await body<{
			expectedRowVersion: number;
			generatedRetentionDays: number;
		}>(request);
		const retention = getMockState().artifactRetention;
		const stale = conflict(input.expectedRowVersion, retention.rowVersion);
		if (stale) return stale;
		retention.generatedRetentionDays = input.generatedRetentionDays;
		retention.rowVersion += 1;
		return json(retention);
	}),
	http.get("*/api/v1/backups/:id/download", () => {
		const denied = adminRequired();
		return (
			denied ??
			new HttpResponse(
				"Mock backup archive metadata only; no backup content exists.",
				{ status: 501, headers: { "Content-Type": "text/plain" } },
			)
		);
	}),
];
