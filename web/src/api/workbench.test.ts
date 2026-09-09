import { afterEach, describe, expect, it, vi } from "vitest";
import { setUnauthorizedHandler, WorkbenchApiError, workbenchApi } from "@/api/workbench";
import { authUser, queuedProbe, rowVersionConflict } from "./fixtures";

const response = (body: unknown, status = 200) =>
	new Response(JSON.stringify(body), {
		status,
		headers: { "Content-Type": "application/json" },
	});
afterEach(() => {
	vi.unstubAllGlobals();
	setUnauthorizedHandler();
});

describe("workbench API boundary", () => {
	it("uses same-origin credentials and does not expire a session during initial me or login", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(response({ detail: "no session" }, 401))
			.mockResolvedValueOnce(response(authUser));
		vi.stubGlobal("fetch", fetchMock);
		const unauthorized = vi.fn();
		setUnauthorizedHandler(unauthorized);
		await expect(workbenchApi.currentUser()).rejects.toBeInstanceOf(
			WorkbenchApiError,
		);
		await workbenchApi.login({
			username: "admin",
			password: "a password long enough",
		});
		expect(unauthorized).not.toHaveBeenCalled();
		expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/me");
		expect(fetchMock.mock.calls[0][1]).toMatchObject({
			credentials: "include",
		});
	});

	it("reports a later unauthorized request and creates a probe command", async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(
				response({ id: queuedProbe.id, state: queuedProbe.state }, 202),
			)
			.mockResolvedValueOnce(response(queuedProbe))
			.mockResolvedValueOnce(response({ detail: "expired" }, 401));
		vi.stubGlobal("fetch", fetchMock);
		const unauthorized = vi.fn();
		setUnauthorizedHandler(unauthorized);
		await expect(
			workbenchApi.probeConnection("thanos/main"),
		).resolves.toMatchObject({
			id: "42",
			type: "connection_probe",
			state: "Queued",
		});
		await expect(workbenchApi.listConnections()).rejects.toMatchObject({
			status: 401,
		});
		expect(unauthorized).toHaveBeenCalledTimes(1);
		expect(fetchMock.mock.calls[0][0]).toBe(
			"/api/v1/connections/thanos%2Fmain/probe",
		);
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toMatchObject({
			clientCommandId: expect.any(String),
		});
	});

	it("preserves server conflict details for the caller refresh fence", async () => {
		vi.stubGlobal(
			"fetch",
			vi.fn().mockResolvedValue(response(rowVersionConflict, 409)),
		);
		await expect(
			workbenchApi.enableConnection("thanos", 3),
		).rejects.toMatchObject({ status: 409, code: "row_version_conflict" });
	});

	it("sends only the enable command contract without a qualified connection", async () => {
		const fetchMock = vi.fn().mockResolvedValue(
			response({
				name: "thanos/main",
				type: "thanos",
				enabled: true,
				revalidationRequired: false,
				rowVersion: 4,
				config: {},
			}),
		);
		vi.stubGlobal("fetch", fetchMock);

		await workbenchApi.enableConnection("thanos/main", 3);

		expect(fetchMock.mock.calls[0][0]).toBe(
			"/api/v1/connections/thanos%2Fmain/enable",
		);
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
			clientCommandId: expect.any(String),
			expectedRowVersion: 3,
		});
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).not.toHaveProperty(
			"qualified",
		);
	});

	it("sends a qualified probe only for model provider enablement", async () => {
		const fetchMock = vi.fn().mockResolvedValue(response({}));
		vi.stubGlobal("fetch", fetchMock);

		await workbenchApi.enableConnection("models/main", 3, "qualified/7");

		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toMatchObject({
			expectedRowVersion: 3,
			qualifiedProbeResultId: "qualified/7",
		});
	});

	it("sends kubernetes secret fields only in the typed create command", async () => {
		const fetchMock = vi.fn().mockResolvedValue(response({}));
		vi.stubGlobal("fetch", fetchMock);
		await workbenchApi.createConnection("cluster/main", { type: "kubernetes", contextName: "production", defaultNamespace: "apps", kubeconfig: "apiVersion: v1" });
		expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/connections");
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ clientCommandId: expect.any(String), name: "cluster/main", connection: { type: "kubernetes", contextName: "production", defaultNamespace: "apps", kubeconfig: "apiVersion: v1" } });
	});

	it("uses exact disable, rotate, and cancellation fences", async () => {
		const fetchMock = vi.fn().mockResolvedValueOnce(response({})).mockResolvedValueOnce(response({})).mockResolvedValueOnce(response({}));
		vi.stubGlobal("fetch", fetchMock);
		await workbenchApi.disableConnection("thanos/main", 7);
		await workbenchApi.rotateConnection("thanos/main", 8, { type: "thanos", baseUrl: "https://thanos.example", password: "replacement" });
		await workbenchApi.cancelProbeAttempt("thanos/main", "attempt/3", 9);
		expect(fetchMock.mock.calls.map(call => call[0])).toEqual(["/api/v1/connections/thanos%2Fmain/disable", "/api/v1/connections/thanos%2Fmain/rotate", "/api/v1/connections/thanos%2Fmain/probe-attempts/attempt%2F3/cancel"]);
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toMatchObject({ expectedRowVersion: 7 });
		expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toMatchObject({ expectedRowVersion: 8, connection: { password: "replacement" } });
		expect(JSON.parse(fetchMock.mock.calls[2][1].body)).toMatchObject({ expectedRowVersion: 9 });
	});

	it("reads history through the secret-free revision and generation endpoints", async () => {
		const fetchMock = vi.fn().mockResolvedValueOnce(response({ items: [{ id: "r1", revisionSeq: 1, config: { baseUrl: "https://thanos.example" }, createdAt: "2026-09-08T00:00:00Z" }] })).mockResolvedValueOnce(response({ items: [{ id: "g1", generationSeq: 1, createdAt: "2026-09-08T00:00:00Z" }] }));
		vi.stubGlobal("fetch", fetchMock);
		await expect(workbenchApi.listRevisions("thanos/main")).resolves.toEqual([{ id: "r1", revisionSeq: 1, config: { baseUrl: "https://thanos.example" }, createdAt: "2026-09-08T00:00:00Z" }]);
		await expect(workbenchApi.listCredentialGenerations("thanos/main")).resolves.toEqual([{ id: "g1", generationSeq: 1, createdAt: "2026-09-08T00:00:00Z" }]);
		expect(fetchMock.mock.calls.map(call => call[0])).toEqual(["/api/v1/connections/thanos%2Fmain/revisions?limit=50", "/api/v1/connections/thanos%2Fmain/generations?limit=50"]);
	});

	it("keeps a model qualification identifier out of non-model enable commands", async () => {
		const fetchMock = vi.fn().mockResolvedValue(response({}));
		vi.stubGlobal("fetch", fetchMock);
		await workbenchApi.enableConnection("cluster/main", 10);
		expect(JSON.parse(fetchMock.mock.calls[0][1].body)).not.toHaveProperty("qualifiedProbeResultId");
	});

	it("uses the common 401 handler for model discovery", async () => {
		vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response({ detail: "expired" }, 401)));
		const unauthorized = vi.fn();
		setUnauthorizedHandler(unauthorized);

		await expect(workbenchApi.discoverProviderModels("https://provider.invalid", "secret")).rejects.toMatchObject({ status: 401 });
		expect(unauthorized).toHaveBeenCalledOnce();
	});

	it("gets the server-created probe attempt from its exactly encoded location", async () => {
		const created = { id: "attempt/7", state: "Queued" };
		const serverAttempt = {
			...queuedProbe,
			id: "attempt/7",
			rowVersion: 9,
			createdAt: "2026-09-08T12:34:56Z",
		};
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(response(created, 202))
			.mockResolvedValueOnce(response(serverAttempt));
		vi.stubGlobal("fetch", fetchMock);

		await expect(workbenchApi.probeConnection("thanos/main")).resolves.toEqual(
			serverAttempt,
		);
		expect(fetchMock.mock.calls[0][0]).toBe(
			"/api/v1/connections/thanos%2Fmain/probe",
		);
		expect(fetchMock.mock.calls[1][0]).toBe(
			"/api/v1/connections/thanos%2Fmain/probe-attempts/attempt%2F7",
		);
		expect(fetchMock.mock.calls[1][1]).not.toHaveProperty("method");
	});
});
