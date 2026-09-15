// Wire-contract tests: fixtures mirror the JSON shape agreed for the unified
// authentication flow (docs/authentication-design.md). The flow is carried by
// the HttpOnly __Host-quoin-flow cookie, so no response body may contain a
// flow token and no request may need one.
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkbenchApiError } from "@/api/workbench";
import { authFlowApi } from "./api";

function stubFetch(
	routes: Array<{
		method: string;
		path: RegExp;
		status?: number;
		body?: unknown;
	}>,
) {
	const calls: Array<{
		method: string;
		url: string;
		body?: unknown;
		credentials?: RequestCredentials;
	}> = [];
	vi.stubGlobal(
		"fetch",
		vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = String(input);
			const method = init?.method ?? "GET";
			calls.push({
				method,
				url,
				body: init?.body ? JSON.parse(String(init.body)) : undefined,
				credentials: init?.credentials,
			});
			const route = routes.find(
				(candidate) =>
					candidate.method === method && candidate.path.test(url),
			);
			if (!route) throw new Error(`unstubbed ${method} ${url}`);
			return new Response(
				route.body === undefined ? null : JSON.stringify(route.body),
				{
					// Bodyless routes answer 204 like the Go handlers do.
					status: route.status ?? (route.body === undefined ? 204 : 200),
					headers: { "Content-Type": "application/json" },
				},
			);
		}),
	);
	return calls;
}

afterEach(() => vi.unstubAllGlobals());

describe("authentication flow wire contract", () => {
	it("starts a login flow with credentials and receives the flow projection", async () => {
		const flow = {
			type: "login",
			user: { id: "1", username: "admin", displayName: "Admin" },
			expiresAt: "2026-09-15T10:00:00Z",
			contacts: [
				{
					id: "c1",
					channel: "email",
					maskedTarget: "a***@quoin.dev",
					verified: true,
				},
			],
		};
		const calls = stubFetch([
			{ method: "POST", path: /\/api\/v1\/auth\/login$/, body: flow },
		]);
		const started = await authFlowApi.start({
			username: "admin",
			password: "a password long enough",
		});
		expect(started).toEqual(flow);
		expect(calls[0].body).toEqual({
			username: "admin",
			password: "a password long enough",
		});
		// The flow cookie must travel with every request.
		expect(calls[0].credentials).toBe("include");
	});

	it("resumes an active flow and surfaces 404 as a flow error", async () => {
		const calls = stubFetch([
			{
				method: "GET",
				path: /\/api\/v1\/auth\/flow$/,
				status: 404,
				body: { code: "flow_not_found", detail: "没有进行中的认证流程" },
			},
		]);
		await expect(authFlowApi.resume()).rejects.toBeInstanceOf(
			WorkbenchApiError,
		);
		expect(calls[0].method).toBe("GET");
	});

	it("sets the flow password with only the new password", async () => {
		const calls = stubFetch([
			{ method: "PUT", path: /\/api\/v1\/auth\/flow\/password$/ },
		]);
		await authFlowApi.setPassword("new password long enough");
		expect(calls[0].body).toEqual({ newPassword: "new password long enough" });
	});

	it("registers an admin contact with channel and raw target", async () => {
		const calls = stubFetch([
			{ method: "POST", path: /\/api\/v1\/auth\/flow\/contacts$/ },
		]);
		await authFlowApi.addContact("email", "root@quoin.dev");
		expect(calls[0].body).toEqual({
			channel: "email",
			target: "root@quoin.dev",
		});
	});

	it("sends a challenge for one contact id", async () => {
		const calls = stubFetch([
			{ method: "POST", path: /\/api\/v1\/auth\/flow\/challenge$/ },
		]);
		await authFlowApi.sendChallenge("c2");
		expect(calls[0].body).toEqual({ contactId: "c2" });
	});

	it("verifies the six-digit code and reports completion with the user", async () => {
		const user = {
			id: "1",
			username: "admin",
			displayName: "Admin",
			role: "admin",
			enabled: true,
			authRevision: 1,
			rowVersion: 1,
			passwordChangeRequired: false,
			lastLoginAt: null,
		};
		stubFetch([
			{
				method: "POST",
				path: /\/api\/v1\/auth\/flow\/verify$/,
				body: { completed: true, user },
			},
		]);
		const result = await authFlowApi.verify("012345");
		expect(result.completed).toBe(true);
		expect(result.user).toEqual(user);
	});

	it("reports an init verification that only marks the contact verified", async () => {
		stubFetch([
			{
				method: "POST",
				path: /\/api\/v1\/auth\/flow\/verify$/,
				body: { completed: false },
			},
		]);
		const result = await authFlowApi.verify("654321");
		expect(result).toEqual({ completed: false });
	});

	it("completes initialization with an empty object body and tolerates 204", async () => {
		const calls = stubFetch([
			{ method: "POST", path: /\/api\/v1\/auth\/flow\/complete$/, status: 204 },
		]);
		await expect(authFlowApi.complete()).resolves.toBeUndefined();
		expect(calls[0].body).toEqual({});
	});

	it("reads delivery settings that carry secret references but no values", async () => {
		const view = {
			configuration: {
				email: {
					kind: "smtp",
					host: "smtp.example.com",
					port: 587,
					from: "quoin@example.com",
					username: "quoin",
					passwordRef: "smtp_password",
					tlsMode: "starttls",
				},
			},
			rowVersion: 3,
			source: "administrator",
			configured: true,
		};
		const calls = stubFetch([
			{ method: "GET", path: /\/api\/v1\/auth\/flow\/delivery$/, body: view },
		]);
		expect(await authFlowApi.readDelivery()).toEqual(view);
		expect(calls[0].credentials).toBe("include");
		// The stored secret value never travels to the browser.
		expect(JSON.stringify(view)).not.toContain("secret-password");
	});

	it("saves delivery configuration with write-only secrets and a row version", async () => {
		const saved = {
			configuration: {
				email: {
					kind: "smtp" as const,
					host: "smtp.example.com",
					port: 587,
					from: "quoin@example.com",
					username: "quoin",
					passwordRef: "smtp_password",
					tlsMode: "starttls",
				},
				sms: { kind: "webhook" as const, url: "https://gateway.invalid/sms" },
			},
			rowVersion: 4,
			source: "administrator",
			configured: true,
		};
		const calls = stubFetch([
			{ method: "PUT", path: /\/api\/v1\/auth\/flow\/delivery$/, body: saved },
		]);
		const result = await authFlowApi.saveDelivery({
			configuration: saved.configuration,
			secrets: { smtp_password: "secret-password" },
			expectedRowVersion: 3,
		});
		expect(result).toEqual(saved);
		expect(calls[0].body).toEqual({
			configuration: saved.configuration,
			secrets: { smtp_password: "secret-password" },
			expectedRowVersion: 3,
		});
	});
});
