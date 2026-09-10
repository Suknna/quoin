import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

const adminUsername = process.env.QUOIN_E2E_ADMIN_USERNAME;
const adminTemporaryPassword = process.env.QUOIN_E2E_ADMIN_PASSWORD;
const adminFinalPassword = process.env.QUOIN_E2E_ADMIN_FINAL_PASSWORD;
const runtimeRoot = process.env.QUOIN_E2E_RUNTIME;
const composeProject = process.env.QUOIN_E2E_PROJECT;
const e2ePort = process.env.QUOIN_E2E_PORT;

if (
	!adminUsername ||
	!adminTemporaryPassword ||
	!adminFinalPassword ||
	!runtimeRoot ||
	!composeProject ||
	!e2ePort
) {
	throw new Error(
		"#97 real E2E requires the isolated scripts/e2e-real/up.sh environment.",
	);
}

const repoRoot = new URL("../../../", import.meta.url).pathname;

function secret(name: string): string {
	// Fixture credentials remain in the generated 0600 runtime directory and are
	// supplied only in the normal connection-create request. No test output logs
	// them or puts them in YAML/business declarations.
	return readFileSync(`${runtimeRoot}/fixture/secrets/${name}`, "utf8").trim();
}

function fixtureHits(
	service: "prometheus-fixture" | "prometheus-tls-fixture" | "thanos-fixture",
): string[] {
	const isTLS = service === "prometheus-tls-fixture";
	// Read fixture counters over the exact protocol/certificate boundary exposed
	// by that fixture. This is assertion-only traffic, never an adapter bypass.
	const reader = isTLS
		? "import ssl,urllib.request; c=ssl.create_default_context(cafile='/run/fixture/tls.crt'); print(urllib.request.urlopen('https://prometheus-tls-fixture:8080/fixture/hits', context=c).read().decode())"
		: "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/fixture/hits').read().decode())";
	const output = execFileSync(
		"docker",
		[
			"compose",
			"--project-directory",
			repoRoot,
			"--env-file",
			"/dev/null",
			"-f",
			`${repoRoot}deploy/e2e-real.compose.yaml`,
			"exec",
			"-T",
			service,
			"python3",
			"-c",
			reader,
		],
		{
			encoding: "utf8",
			env: {
				...process.env,
				QUOIN_E2E_RUNTIME: runtimeRoot,
				QUOIN_E2E_PORT: e2ePort,
				QUOIN_E2E_PROJECT: composeProject,
			},
		},
	);
	return (JSON.parse(output) as { queries: string[] }).queries;
}

async function activateAdmin(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
) {
	// A retained runtime may have already completed the forced password change;
	// both paths establish an actual same-origin session through the public API.
	let response = await context.request.post(`${baseURL}/api/v1/auth/login`, {
		headers: { Origin: baseURL },
		data: { username: adminUsername, password: adminFinalPassword },
	});
	if (response.status() === 200) return;
	expect(response.status()).toBe(401);
	response = await context.request.post(`${baseURL}/api/v1/auth/login`, {
		headers: { Origin: baseURL },
		data: { username: adminUsername, password: adminTemporaryPassword },
	});
	expect(response.status()).toBe(200);
	const changed = await context.request.put(`${baseURL}/api/v1/auth/password`, {
		headers: { Origin: baseURL },
		data: {
			currentPassword: adminTemporaryPassword,
			newPassword: adminFinalPassword,
		},
	});
	expect(changed.status()).toBe(204);
}

async function ensurePlinthRegistered(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
	suffix: number,
) {
	const runtime = await context.request.get(`${baseURL}/api/v1/runtime`);
	expect(runtime.status()).toBe(200);
	const before = (await runtime.json()) as {
		plinth: { state: string; connected: boolean; rowVersion: number };
	};
	if (before.plinth.state === "registered") {
		await expect
			.poll(
				async () => {
					const response = await context.request.get(
						`${baseURL}/api/v1/runtime`,
					);
					return (
						response.status() === 200 &&
						((await response.json()) as { plinth: { connected: boolean } })
							.plinth.connected
					);
				},
				{ timeout: 20_000 },
			)
			.toBe(true);
		return;
	}
	const prepared = await context.request.post(
		`${baseURL}/api/v1/runtime-slots/plinth/registration/prepare`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `plinth-prepare-${suffix}`,
				expectedRowVersion: before.plinth.rowVersion,
			},
		},
	);
	expect(prepared.status()).toBe(200);
	const preparation = (await prepared.json()) as {
		registrationTokenAvailable: boolean;
		registrationTokenHandle: string;
	};
	expect(preparation.registrationTokenAvailable).toBe(true);
	const revealed = await context.request.post(
		`${baseURL}/api/v1/runtime-slots/registration-token/reveal`,
		{
			headers: { Origin: baseURL },
			data: { registrationTokenHandle: preparation.registrationTokenHandle },
		},
	);
	expect(revealed.status()).toBe(200);
	const token = (await revealed.json()) as { registrationToken: string };
	// The production CLI consumes the one-time token only through attached stdin.
	// It never becomes a test log, YAML value, environment variable, or argv.
	execFileSync(
		"bash",
		[`${repoRoot}scripts/e2e-real/register-plinth.sh`, "--stdin"],
		{
			input: `${token.registrationToken}\n`,
			encoding: "utf8",
			env: {
				...process.env,
				QUOIN_E2E_RUNTIME: runtimeRoot,
				QUOIN_E2E_PORT: e2ePort,
				QUOIN_E2E_PROJECT: composeProject,
			},
		},
	);
	await expect
		.poll(
			async () => {
				const response = await context.request.get(`${baseURL}/api/v1/runtime`);
				return (
					response.status() === 200 &&
					(
						(await response.json()) as {
							plinth: { state: string; connected: boolean };
						}
					).plinth
				);
			},
			{ timeout: 20_000 },
		)
		.toMatchObject({ state: "registered", connected: true });
}

async function createAndActivateLabelContract(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
	suffix: number,
) {
	// Retained disposable harnesses may already have an active contract from a
	// prior failed pass. Reuse it only when its real label authority matches;
	// never replace an unrelated active contract just to make #97 pass.
	const listed = await context.request.get(`${baseURL}/api/v1/label-contracts`);
	expect(listed.status()).toBe(200);
	const existing = (await listed.json()) as {
		items: Array<{ version: number; state: string }>;
	};
	const active = existing.items.find((item) => item.state === "active");
	if (active) {
		const detail = await context.request.get(
			`${baseURL}/api/v1/label-contracts/${active.version}`,
		);
		expect(detail.status()).toBe(200);
		const contract = (await detail.json()) as {
			contractJson?: { label_contract?: { business_system_label?: string } };
		};
		// contractJson preserves the strict YAML root, rather than flattening its
		// label_contract object. Read the public projection exactly as returned.
		if (
			contract.contractJson?.label_contract?.business_system_label !==
			"system_id"
		) {
			throw new Error(
				`retained #97 runtime active Label Contract ${active.version} does not use system_id; it is left unchanged`,
			);
		}
		return active.version;
	}

	const created = await context.request.post(
		`${baseURL}/api/v1/label-contracts`,
		{
			headers: { Origin: baseURL },
			multipart: {
				clientCommandId: `contract-draft-${suffix}`,
				file: {
					name: "e2e-label-contract.yaml",
					mimeType: "application/yaml",
					buffer: Buffer.from(
						"label_contract:\n  business_system_label: system_id\n",
					),
				},
			},
		},
	);
	expect(created.status()).toBe(201);
	const draft = (await created.json()) as {
		version: number;
		rowVersion: number;
	};
	const activated = await context.request.post(
		`${baseURL}/api/v1/label-contracts/${draft.version}/activate`,
		{
			headers: { Origin: baseURL },
			// No active pointer exists on this fresh contract state, so the guarded
			// first activation uses the documented null pointer/initial state fence.
			data: {
				clientCommandId: `contract-activate-${suffix}`,
				expectedStateRowVersion: 1,
				expectedCurrentContractVersionId: null,
				expectedTargetRowVersion: draft.rowVersion,
				compatibleVersions: [],
			},
		},
	);
	expect(activated.status()).toBe(200);
	return draft.version;
}

async function createMetricsConnection(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
	name: string,
	connection: Record<string, string | boolean>,
) {
	const response = await context.request.post(`${baseURL}/api/v1/connections`, {
		headers: { Origin: baseURL },
		data: { clientCommandId: `create-${name}`, name, connection },
	});
	expect(response.status(), name).toBe(201);
	return (await response.json()) as {
		id: string;
		rowVersion: number;
		config: Record<string, unknown>;
	};
}

async function qualifyConnection(
	context: import("@playwright/test").BrowserContext,
	baseURL: string,
	name: string,
) {
	const started = await context.request.post(
		`${baseURL}/api/v1/connections/${name}/probe`,
		{
			headers: { Origin: baseURL },
			data: { clientCommandId: `probe-${name}` },
		},
	);
	expect(started.status(), name).toBe(202);
	let qualifiedProbeID = "";
	await expect
		.poll(
			async () => {
				const response = await context.request.get(
					`${baseURL}/api/v1/connections/${name}/probe-results`,
				);
				if (response.status() !== 200) return "pending";
				const body = (await response.json()) as {
					items?: Array<{ id?: string; outcome?: string }>;
				};
				qualifiedProbeID = body.items?.[0]?.id ?? "";
				return body.items?.[0]?.outcome ?? "pending";
			},
			{ timeout: 20_000 },
		)
		.toBe("passed");
	const current = await context.request.get(
		`${baseURL}/api/v1/connections/${name}`,
	);
	expect(current.status()).toBe(200);
	const detail = (await current.json()) as { rowVersion: number };
	const enabled = await context.request.post(
		`${baseURL}/api/v1/connections/${name}/enable`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `enable-${name}`,
				expectedRowVersion: detail.rowVersion,
				qualifiedProbeResultId: qualifiedProbeID,
			},
		},
	);
	expect(enabled.status(), name).toBe(200);
}

/**
 * #97's production-only intake chain: independent Basic/Bearer upstreams,
 * public Admin API persistence, live Plinth adapter probes, an explicitly
 * selected business connection, draft verification samples, then publication.
 * Fixture counters prove save/publish themselves never cause an independent
 * resource scan; only explicit probes and the selected verification query hit.
 */
test("real Prometheus and Thanos adapters verify selected business declaration without scans", async ({
	browser,
	baseURL,
}) => {
	const admin = await browser.newContext({ ignoreHTTPSErrors: true });
	await activateAdmin(admin, baseURL!);
	const suffix = Date.now();
	const systemKey = `e2e-metrics-${suffix}`;
	const prometheusName = `prometheus-basic-${suffix}`;
	const prometheusTLSName = `prometheus-noauth-tls-${suffix}`;
	const thanosName = `thanos-bearer-${suffix}`;
	await ensurePlinthRegistered(admin, baseURL!, suffix);
	const contractVersion = await createAndActivateLabelContract(
		admin,
		baseURL!,
		suffix,
	);
	const prometheusBefore = fixtureHits("prometheus-fixture");
	const prometheusTLSBefore = fixtureHits("prometheus-tls-fixture");
	const thanosBefore = fixtureHits("thanos-fixture");

	const prometheus = await createMetricsConnection(
		admin,
		baseURL!,
		prometheusName,
		{
			type: "prometheus",
			baseUrl: "http://prometheus-fixture:8080",
			authType: "basic",
			username: "e2e-prometheus",
			password: secret("metrics-basic-password"),
		},
	);
	await createMetricsConnection(admin, baseURL!, prometheusTLSName, {
		type: "prometheus",
		baseUrl: "https://prometheus-tls-fixture:8080",
		tlsCaPem: readFileSync(`${runtimeRoot}/fixture/prometheus-tls.crt`, "utf8"),
		tlsServerName: "prometheus-tls-fixture",
		authType: "none",
	});
	const thanos = await createMetricsConnection(admin, baseURL!, thanosName, {
		type: "thanos",
		baseUrl: "http://thanos-fixture:8080",
		authType: "bearer",
		bearerToken: secret("metrics-bearer-token"),
	});
	// Non-secret projections must not reflect either submitted credential value.
	expect(JSON.stringify(prometheus.config)).not.toContain(
		secret("metrics-basic-password"),
	);
	expect(JSON.stringify(thanos.config)).not.toContain(
		secret("metrics-bearer-token"),
	);

	await qualifyConnection(admin, baseURL!, prometheusName);
	await qualifyConnection(admin, baseURL!, prometheusTLSName);
	await qualifyConnection(admin, baseURL!, thanosName);
	expect(
		fixtureHits("prometheus-fixture").slice(prometheusBefore.length),
	).toEqual(["vector(1)"]);
	expect(
		fixtureHits("prometheus-tls-fixture").slice(prometheusTLSBefore.length),
	).toEqual(["vector(1)"]);
	expect(fixtureHits("thanos-fixture").slice(thanosBefore.length)).toEqual([
		"vector(1)",
	]);

	const yaml = [
		`system_key: ${systemKey}`,
		"display_name: E2E Metrics Business",
		"enabled: true",
		"timezone: UTC",
		`metrics_connection_id: '${thanos.id}'`,
		// Verification currently executes declared PromQL checks only. Keep a
		// legal discovery declaration for future controlled identity validation,
		// but do not claim that verification scanned it or returned identities.
		"resource_discoveries:",
		"  - key: fixture-identity",
		"    display_name: Fixture identity",
		`    selector: fixture_resource{system_id="${systemKey}"}`,
		"    identity_labels: [instance]",
		"inspection_plans:",
		"  - key: explicit-verification",
		"    display_name: Explicit verification",
		"    checks:",
		"      - key: selected-query",
		"        display_name: Selected query",
		"        analysis_question: Is the selected business metric endpoint available?",
		"        kind: promql",
		"        query:",
		"          mode: instant",
		`          expression: up{system_id="${systemKey}"}`,
		"",
	].join("\n");
	const upload = await admin.request.post(
		`${baseURL}/api/v1/business-systems`,
		{
			headers: { Origin: baseURL },
			multipart: {
				clientCommandId: `business-draft-${suffix}`,
				targetLabelContractVersion: String(contractVersion),
				file: {
					name: "e2e-metrics.yaml",
					mimeType: "application/yaml",
					buffer: Buffer.from(yaml),
				},
			},
		},
	);
	expect(upload.status()).toBe(201);
	const draft = (await upload.json()) as { id: string };
	const draftVersionID = draft.id;
	expect(draftVersionID).toBeTruthy();

	// The draft exists but must neither become the published identity nor trigger
	// a query merely by saving it. The removed refresh route must not be revived.
	const beforeVerification = fixtureHits("thanos-fixture").length;
	const systemBefore = await admin.request.get(
		`${baseURL}/api/v1/business-systems/${systemKey}`,
	);
	expect(systemBefore.status()).toBe(200);
	const beforeBody = (await systemBefore.json()) as {
		currentConfigVersionId?: string;
	};
	expect(beforeBody.currentConfigVersionId).not.toBe(draftVersionID);
	const obsoleteRefresh = await admin.request.post(
		`${baseURL}/api/v1/business-systems/${systemKey}/resources:refresh`,
		{
			headers: { Origin: baseURL },
			data: { clientCommandId: `no-refresh-${suffix}` },
		},
	);
	expect(obsoleteRefresh.status()).toBe(404);
	expect(fixtureHits("thanos-fixture")).toHaveLength(beforeVerification);

	const verification = await admin.request.post(
		`${baseURL}/api/v1/business-systems/${systemKey}/config/${draftVersionID}/verifications`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `verify-draft-${suffix}`,
				purpose: "prepublish",
			},
		},
	);
	expect(verification.status()).toBe(202);
	const startedRun = (await verification.json()) as { id: string };
	await expect
		.poll(
			async () => {
				const response = await admin.request.get(
					`${baseURL}/api/v1/business-systems/${systemKey}/config/${draftVersionID}/verifications/${startedRun.id}`,
				);
				if (response.status() !== 200) return "pending";
				const body = (await response.json()) as {
					state?: string;
					metricsConnectionId?: string;
					checkResults?: unknown[];
					identitySamples?: Array<{
						discoveryKey: string;
						labels: Record<string, string>;
					}>;
					queryResults?: Array<{
						planKey: string;
						checkKey: string;
						resultType: string;
						sampleCount: number;
						samples?: Array<{
							metric?: Record<string, string>;
							value?: [number, string];
						}>;
					}>;
				};
				// Verification performs only this declaration's explicit discovery and
				// check. Both sample kinds must contain concrete adapter-returned labels,
				// proving they are real results rather than declared identity shapes.
				const evidenceIsScoped =
					body.metricsConnectionId === thanos.id &&
					(body.checkResults?.length ?? 0) > 0 &&
					body.identitySamples?.some(
						(sample) =>
							sample.discoveryKey === "fixture-identity" &&
							sample.labels.instance === "fixture-identity" &&
							sample.labels.fixture_endpoint === "thanos-bearer" &&
							sample.labels.system_id === systemKey,
					) &&
					body.queryResults?.some(
						(result) =>
							result.planKey === "explicit-verification" &&
							result.checkKey === "selected-query" &&
							result.resultType === "vector" &&
							result.sampleCount === 1 &&
							result.samples?.some(
								(sample) =>
									sample.metric?.fixture_endpoint === "thanos-bearer" &&
									sample.metric.system_id === systemKey &&
									sample.value?.[1] === "1",
							),
					);
				return body.state === "Passed" && evidenceIsScoped
					? "verified"
					: (body.state ?? "pending");
			},
			{ timeout: 20_000 },
		)
		.toBe("verified");
	// Discovery and check attempts are independently dispatched, so their HTTP
	// completion order is deliberately unspecified. Require the exact two-query
	// multiset: no implicit extra scan or fallback request is permitted.
	expect(fixtureHits("thanos-fixture").slice(beforeVerification).sort()).toEqual([
		`fixture_resource{system_id="${systemKey}"}`,
		`up{system_id="${systemKey}"}`,
	].sort());

	const published = await admin.request.post(
		`${baseURL}/api/v1/business-systems/${systemKey}/config/${draftVersionID}/publish`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `publish-draft-${suffix}`,
				expectedCurrentPublishedVersionId: null,
			},
		},
	);
	expect(published.status()).toBe(200);
	const publishedBody = (await published.json()) as {
		currentConfigVersionId?: string;
	};
	expect(publishedBody.currentConfigVersionId).toBe(draftVersionID);
	// Publishing only promotes identity; it cannot start a second query/scan.
	// Discovery and check attempts are independently dispatched, so their HTTP
	// completion order is deliberately unspecified. Require the exact two-query
	// multiset: no implicit extra scan or fallback request is permitted.
	expect(fixtureHits("thanos-fixture").slice(beforeVerification).sort()).toEqual([
		`fixture_resource{system_id="${systemKey}"}`,
		`up{system_id="${systemKey}"}`,
	].sort());

	// A second declaration revision explicitly reselects Prometheus. Its trial
	// must reach only that named endpoint, proving the runtime never falls back
	// to the prior Thanos connection or an implicit globally enabled source.
	const prometheusDraft = await admin.request.post(
		`${baseURL}/api/v1/business-systems`,
		{
			headers: { Origin: baseURL },
			multipart: {
				clientCommandId: `business-reselect-${suffix}`,
				targetLabelContractVersion: String(contractVersion),
				file: {
					name: "e2e-metrics-prometheus.yaml",
					mimeType: "application/yaml",
					buffer: Buffer.from(
						yaml.replace(
							`metrics_connection_id: '${thanos.id}'`,
							`metrics_connection_id: '${prometheus.id}'`,
						),
					),
				},
			},
		},
	);
	expect(prometheusDraft.status()).toBe(201);
	const prometheusVersion = (await prometheusDraft.json()) as { id: string };
	const prometheusHitsBefore = fixtureHits("prometheus-fixture").length;
	const thanosHitsBeforeReselect = fixtureHits("thanos-fixture").length;
	const prometheusVerification = await admin.request.post(
		`${baseURL}/api/v1/business-systems/${systemKey}/config/${prometheusVersion.id}/verifications`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `verify-prometheus-${suffix}`,
				purpose: "prepublish",
			},
		},
	);
	expect(prometheusVerification.status()).toBe(202);
	const prometheusRun = (await prometheusVerification.json()) as { id: string };
	await expect
		.poll(
			async () => {
				const response = await admin.request.get(
					`${baseURL}/api/v1/business-systems/${systemKey}/config/${prometheusVersion.id}/verifications/${prometheusRun.id}`,
				);
				if (response.status() !== 200) return "pending";
				const body = (await response.json()) as {
					state?: string;
					metricsConnectionId?: string;
					identitySamples?: Array<{
						discoveryKey: string;
						labels: Record<string, string>;
					}>;
					queryResults?: Array<{
						planKey: string;
						checkKey: string;
						resultType: string;
						sampleCount: number;
						samples?: Array<{
							metric?: Record<string, string>;
							value?: [number, string];
						}>;
					}>;
				};
				const realPrometheusEvidence =
					body.identitySamples?.some(
						(sample) =>
							sample.discoveryKey === "fixture-identity" &&
							sample.labels.instance === "fixture-identity" &&
							sample.labels.fixture_endpoint === "prometheus-basic" &&
							sample.labels.system_id === systemKey,
					) &&
					body.queryResults?.some(
						(result) =>
							result.planKey === "explicit-verification" &&
							result.checkKey === "selected-query" &&
							result.resultType === "vector" &&
							result.sampleCount === 1 &&
							result.samples?.some(
								(sample) =>
									sample.metric?.fixture_endpoint === "prometheus-basic" &&
									sample.metric.system_id === systemKey &&
									sample.value?.[1] === "1",
							),
					);
				return body.state === "Passed" &&
					body.metricsConnectionId === prometheus.id &&
					realPrometheusEvidence
					? "verified"
					: (body.state ?? "pending");
			},
			{ timeout: 20_000 },
		)
		.toBe("verified");
	expect(
		fixtureHits("prometheus-fixture").slice(prometheusHitsBefore).sort(),
	).toEqual(
		[
			`fixture_resource{system_id="${systemKey}"}`,
			`up{system_id="${systemKey}"}`,
		].sort(),
	);
	expect(fixtureHits("thanos-fixture")).toHaveLength(thanosHitsBeforeReselect);

	const operatorUsername = `operator97${suffix}`;
	const createOperator = await admin.request.post(
		`${baseURL}/api/v1/admin/users`,
		{
			headers: { Origin: baseURL },
			data: {
				clientCommandId: `operator-${suffix}`,
				username: operatorUsername,
				displayName: "Operator 97",
				role: "operator",
				password: `Operator 97 temporary ${suffix}!`,
			},
		},
	);
	expect(createOperator.status()).toBe(201);
	const operator = await browser.newContext({ ignoreHTTPSErrors: true });
	const operatorLogin = await operator.request.post(
		`${baseURL}/api/v1/auth/login`,
		{
			headers: { Origin: baseURL },
			data: {
				username: operatorUsername,
				password: `Operator 97 temporary ${suffix}!`,
			},
		},
	);
	expect(operatorLogin.status()).toBe(200);
	for (const path of [
		"/api/v1/connections",
		"/api/v1/business-systems",
		`/api/v1/business-systems/${systemKey}/config/${draftVersionID}/verifications`,
	]) {
		const denied = await operator.request.get(`${baseURL}${path}`);
		expect(denied.status(), path).toBe(403);
	}
	await operator.close();
	await admin.close();
});
