import { delay, HttpResponse, http, type JsonBodyType } from "msw";
import type { UserSummary } from "../../api/generated/types";
import type { InvestigationMessage } from "../../features/investigation/api";
import { getMockScenario, getMockState, nextId } from "./store";

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
function required() {
	return gate();
}
/** Configuration, credentials, and administration mutations require an administrator. */
function adminRequired() {
	const denied = gate();
	if (denied) return denied;
	return getMockState().currentUser?.role === "admin"
		? null
		: problem(403, "此演示操作仅向管理员开放。", "forbidden");
}
function detailFor(name: string) {
	return getMockState().connections.find((item) => item.name === name);
}
function systemFor(key: string) {
	return getMockState().systems.find((item) => item.key === key);
}

/** All handlers are local deterministic projections; none contacts a remote service. */
export const domainHandlers = [
	http.get("*/api/v1/auth/me", async () => {
		await slow();
		const scenario = getMockScenario();
		if (scenario === "unavailable") return problem(503, "演示服务当前不可用。");
		if (scenario === "session-expired" || !getMockState().currentUser)
			return problem(401, "演示会话未认证或已过期。");
		return json(getMockState().currentUser);
	}),
	http.post("*/api/v1/auth/login", async ({ request }) => {
		await slow();
		const input = await body<{ username: string; password: string }>(request);
		const matched = getMockState().users.find(
			(candidate) =>
				candidate.username === input.username &&
				getMockState().passwords[candidate.id] === input.password,
		);
		const user: UserSummary | null = matched ? { ...matched } : null;
		if (!user)
			return problem(401, "演示账号或密码不正确。", "invalid_credentials");
		getMockState().currentUser =
			getMockScenario() === "password-change"
				? { ...user, passwordChangeRequired: true }
				: user;
		return json(getMockState().currentUser);
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
			input.connection.type !== "thanos" &&
			input.connection.type !== "kubernetes" &&
			input.connection.type !== "model_provider"
		)
			return problem(422, "连接类型无效。", "validation_error");
		const connection = {
			id: nextId("connection"),
			name: input.name,
			type: input.connection.type as "thanos" | "kubernetes" | "model_provider",
			enabled: false,
			revalidationRequired: false,
			rowVersion: 1,
			config: {
				...input.connection,
				password: undefined,
				apiKey: undefined,
				kubeconfig: undefined,
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

	http.get("*/api/v1/business-systems", () => {
		const denied = required();
		return denied ?? page(getMockState().systems);
	}),
	http.get("*/api/v1/business-systems/:key/resources", ({ params }) => {
		const denied = required();
		return (
			denied ??
			page(
				systemFor(String(params.key))
					? [
							{
								id: "resource-checkout-1",
								discoveryKey: "checkout-pods",
								identityLabels: { namespace: "checkout", pod: "checkout-7db6" },
								observedAt: "2026-09-09T09:30:00.000Z",
								current: true,
								stale: false,
								lastSuccessfulRefreshAt: "2026-09-09T09:30:00.000Z",
							},
						]
					: [],
			)
		);
	}),
	http.get(
		"*/api/v1/business-systems/:key/kubernetes-connections",
		({ params }) => {
			const denied = required();
			return denied ?? json(getMockState().mappings[String(params.key)] ?? []);
		},
	),
	http.post(
		"*/api/v1/business-systems/:key/kubernetes-connections",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const key = String(params.key);
			if (!systemFor(key)) return problem(404, "未找到业务系统。");
			const input = await body<{ connectionId: string }>(request);
			const connection = getMockState().connections.find(
				(item) =>
					(item as { id?: string }).id === input.connectionId &&
					item.type === "kubernetes",
			);
			if (!connection)
				return problem(
					422,
					"请选择有效的 Kubernetes 连接。",
					"validation_error",
				);
			const mapping = {
				id: nextId("mapping-k8s"),
				connectionId: input.connectionId,
				connectionName: connection.name,
				state: "Active" as const,
				rowVersion: 1,
				createdBy: getMockState().currentUser!.id,
				createdAt: "2026-09-09T09:30:00.000Z",
				retiredBy: null,
			};
			(getMockState().mappings[key] ??= []).push(mapping);
			return json(mapping, { status: 201 });
		},
	),
	http.post(
		"*/api/v1/business-systems/:key/kubernetes-connections/:id/retire",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const mapping = (getMockState().mappings[String(params.key)] ?? []).find(
				(item) => item.id === params.id,
			);
			if (!mapping) return problem(404, "未找到 Kubernetes 映射。");
			const input = await body<{ expectedRowVersion: number }>(request);
			const stale = conflict(input.expectedRowVersion, mapping.rowVersion);
			if (stale) return stale;
			mapping.state = "Retired";
			mapping.retiredBy = getMockState().currentUser!.id;
			mapping.retiredAt = "2026-09-09T09:30:00.000Z";
			mapping.rowVersion += 1;
			return json(mapping);
		},
	),
	http.get(
		"*/api/v1/business-systems/:key/resource-refresh-runs/:runId",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const run = (getMockState().refreshRuns[String(params.key)] ?? []).find(
				(item) => item.id === params.runId,
			);
			return run ? json(run) : problem(404, "未找到资源刷新运行。");
		},
	),
	http.post(
		"*/api/v1/business-systems/:key/resources:refresh",
		({ params }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const run = {
				id: nextId("resource-refresh"),
				businessSystemId: String(params.key),
				configVersionId:
					systemFor(String(params.key))?.currentConfigVersionId ?? "config-1",
				labelContractVersionId: "label-1",
				triggerKind: "manual" as const,
				state: "Completed" as const,
				rowVersion: 1,
				evidenceAt: "2026-09-09T09:30:00.000Z",
				createdAt: "2026-09-09T09:30:00.000Z",
			};
			(getMockState().refreshRuns[String(params.key)] ??= []).push(run);
			return json(run, { status: 201 });
		},
	),
	http.get("*/api/v1/business-systems/:key/config/:versionId", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = (getMockState().configVersions[String(params.key)] ?? []).find(
			(version) => version.id === params.versionId,
		);
		return item ? json(item) : problem(404, "未找到配置版本。");
	}),
	http.get("*/api/v1/business-systems/:key/config", ({ params }) => {
		const denied = required();
		return (
			denied ?? page(getMockState().configVersions[String(params.key)] ?? [])
		);
	}),
	http.get("*/api/v1/business-systems/:key", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const item = systemFor(String(params.key));
		return item ? json(item) : problem(404, "未找到业务系统。");
	}),
	http.get("*/api/v1/label-contracts", () => {
		const denied = required();
		return denied ?? page(getMockState().labels);
	}),
	http.get("*/api/v1/journey-catalog", () => {
		const denied = required();
		return (
			denied ??
			json({
				version: "1",
				digest: "sha256:journeys",
				catalogJson: { journeys: [] },
			})
		);
	}),

	http.get("*/api/v1/inspections/runs", ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const system = new URL(request.url).searchParams.get("businessSystemKey");
		return page(
			getMockState().inspectionRuns.filter(
				(run) => !system || run.businessSystemKey === system,
			),
		);
	}),
	http.post("*/api/v1/inspections/runs", async ({ request }) => {
		const denied = required();
		if (denied) return denied;
		const input = await body<{ businessSystemKey: string; planKey: string }>(
			request,
		);
		const run = {
			id: nextId("inspection-run"),
			businessSystemKey: input.businessSystemKey,
			planKey: input.planKey,
			state: "Completed" as const,
			rowVersion: 1,
			triggerKind: "manual" as const,
			evidenceAt: "2026-09-09T09:30:00.000Z",
			createdAt: "2026-09-09T09:30:00.000Z",
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
	http.get(
		"*/api/v1/inspections/runs/:runId/reports/:version",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const value = (getMockState().reports[String(params.runId)] ?? []).find(
				(report) => report.version === Number(params.version),
			);
			return value ? json(value) : problem(404, "未找到巡检报告。");
		},
	),
	http.get("*/api/v1/inspections/runs/:runId/reports", ({ params }) => {
		const denied = required();
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
		const denied = required();
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
	http.get("*/api/v1/runtime", () => {
		const denied = required();
		return (
			denied ??
			json({
				plinth: {
					slot: "plinth",
					state: "registered",
					currentGeneration: 1,
					rowVersion: 1,
					connected: true,
					bootId: "mock-plinth",
					connectionEpoch: 1,
					lastSeenAt: "2026-09-09T09:30:00.000Z",
				},
				lintel: {
					slot: "lintel",
					state: "registered",
					currentGeneration: 1,
					rowVersion: 1,
					connected: true,
					bootId: "mock-lintel",
					connectionEpoch: 1,
					lastSeenAt: "2026-09-09T09:30:00.000Z",
				},
			})
		);
	}),
	http.get("*/api/v1/maintenance", () => {
		const denied = getMockScenario() === "maintenance" ? null : required();
		if (denied) return denied;
		return json({
			active: getMockScenario() === "maintenance",
			reason: getMockScenario() === "maintenance" ? "Upgrade" : undefined,
			rowVersion: 1,
			items: [],
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
		const user = {
			id: nextId("user"),
			username: input.username,
			displayName: input.displayName,
			role: input.role,
			enabled: true,
			authRevision: 1,
			rowVersion: 1,
			passwordChangeRequired: true,
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
	http.post(
		"*/api/v1/runtime-slots/:slot/registration/prepare",
		({ params }) => {
			const denied = required();
			return (
				denied ??
				json({
					slot: params.slot,
					state: "unregistered",
					currentGeneration: 1,
					rowVersion: 1,
					registrationTokenAvailable: true,
					registrationTokenHandle: "mock-runtime-token",
				})
			);
		},
	),
	http.post("*/api/v1/runtime-slots/registration-token/reveal", () => {
		const denied = required();
		return (
			denied ??
			json({
				slot: "plinth",
				generation: 1,
				registrationToken: "mock-registration-token-not-real",
			})
		);
	}),
	http.post(
		"*/api/v1/runtime-slots/:slot/retiring-credential/retire",
		({ params }) => {
			const denied = required();
			return (
				denied ??
				json({
					slot: params.slot,
					state: "registered",
					currentGeneration: 2,
					rowVersion: 2,
					connected: true,
				})
			);
		},
	),
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
	http.post("*/api/v1/inspections/runs/:runId/cancel", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		const run = getMockState().inspectionRuns.find(
			(item) => item.id === params.runId,
		);
		if (!run) return problem(404, "未找到巡检运行。");
		run.state = "Cancelled";
		run.rowVersion += 1;
		return json(run);
	}),
	http.post("*/api/v1/inspections/runs/:runId/analyze", ({ params }) => {
		const denied = required();
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
	http.post("*/api/v1/inspections/runs/:runId/rerun", ({ params }) => {
		const denied = required();
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
	http.post(
		"*/api/v1/business-systems/:key/config/:versionId/publish",
		async ({ params, request }) => {
			const denied = adminRequired();
			if (denied) return denied;
			const system = systemFor(String(params.key));
			const version = (
				getMockState().configVersions[String(params.key)] ?? []
			).find((item) => item.id === params.versionId);
			if (!system || !version) return problem(404, "未找到业务系统配置。");
			const input = await body<{
				expectedCurrentPublishedVersionId: string | null;
			}>(request);
			if (
				input.expectedCurrentPublishedVersionId !==
				(system.currentConfigVersionId ?? null)
			)
				return problem(409, "当前发布配置已变化。", "row_version_conflict");
			version.state = "published";
			version.publishedAt = "2026-09-09T09:30:00.000Z";
			system.currentConfigVersionId = version.id;
			system.rowVersion += 1;
			return json(system);
		},
	),
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
	// Browser identity reads expose only revision/profile metadata; they never start a remote browser.
	http.get("*/api/v1/business-systems/:key/browser-identity", ({ params }) => {
		const denied = required();
		if (denied) return denied;
		if (!systemFor(String(params.key))) return problem(404, "未找到业务系统。");
		return json({
			id: `browser-identity-${params.key}`,
			state: "Ready",
			rowVersion: 1,
			currentRevision: {
				id: `browser-revision-${params.key}`,
				revision: 1,
				name: "结算后台身份",
				startUrl: "https://checkout.demo.invalid/login",
				authenticationProbe: {
					journeyId: "checkout-authentication",
					journeyVersion: 1,
					params: { tenant: "demo" },
				},
				catalogDigest: "sha256:journeys",
				catalogVersion: "1",
				createdAt: "2026-09-09T09:30:00.000Z",
			},
			currentProfile: {
				id: `browser-profile-${params.key}`,
				generation: 1,
				chromiumRevision: "mock-chromium",
				publishedAt: "2026-09-09T09:30:00.000Z",
			},
			lastProbe: {
				phase: "authentication",
				result: "Authenticated",
				journeyId: "checkout-authentication",
				journeyVersion: 1,
				observedAt: "2026-09-09T09:30:00.000Z",
			},
			currentOperation: null,
		});
	}),
	http.get(
		"*/api/v1/business-systems/:key/config/:versionId/verifications",
		({ params }) => {
			const denied = required();
			return (
				denied ??
				page(
					(
						getMockState().verifications[`${params.key}/${params.versionId}`] ??
						[]
					).map((item) => ({
						id: item.id,
						purpose: item.purpose,
						configVersionId: item.configVersionId,
						labelContractVersionId: item.labelContractVersionId,
						state: item.state,
						rowVersion: item.rowVersion,
						evidenceAt: item.evidenceAt,
						createdAt: item.createdAt,
					})),
				)
			);
		},
	),
	http.get(
		"*/api/v1/business-systems/:key/config/:versionId/verifications/:runId",
		({ params }) => {
			const denied = required();
			if (denied) return denied;
			const item = (
				getMockState().verifications[`${params.key}/${params.versionId}`] ?? []
			).find((run) => run.id === params.runId);
			return item ? json(item) : problem(404, "未找到配置验证运行。");
		},
	),
	http.get("*/api/v1/label-contracts/:version", ({ params }) => {
		const denied = adminRequired();
		if (denied) return denied;
		const item = getMockState().labels.find(
			(label) => label.version === Number(params.version),
		);
		return item
			? json({
					...item,
					yamlBody: "version: 1\nidentityLabels: [namespace, pod]\n",
				})
			: problem(404, "未找到标签契约。");
	}),
	http.post("*/api/v1/backups", () => {
		const denied = adminRequired();
		return (
			denied ??
			problem(501, "离线演示不执行真实备份。", "mock_unsupported_operation")
		);
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
