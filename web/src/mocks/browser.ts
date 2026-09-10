import type { PreviewScenario } from "./PreviewPanel";

const scenarioStorageKey = "quoin.mock.scenario";
const workerScope = "/mockServiceWorker.js";

export interface MockRuntime {
	scenario(): PreviewScenario;
	setScenario(scenario: PreviewScenario): Promise<void>;
	reset(): Promise<void>;
	stop(): Promise<void>;
}

/**
 * Starts MSW only in the dev entry point. Handler state is dynamically imported
 * so production chunks cannot acquire fixture data or a service worker import.
 */
export async function startMockRuntime(): Promise<MockRuntime> {
	const [{ setupWorker }, { handlers, getMockScenario, resetMockState, setMockScenario }] = await Promise.all([
		import("msw/browser"),
		import("./handlers"),
	]);
	const worker = setupWorker(...handlers);

	const selected = readScenario();
	setMockScenario(toHandlerScenario(selected));
	await worker.start({
		onUnhandledRequest(request, print) {
			// Static assets and Vite's HMR channel are not application traffic.
			const url = new URL(request.url);
			if (url.origin === location.origin && request.method === "GET" && !url.pathname.startsWith("/api/")) return;
			print.error();
		},
		serviceWorker: { url: `${import.meta.env.BASE_URL.replace(/\/$/, "")}${workerScope}` },
	});

	return {
		scenario: () => fromHandlerScenario(getMockScenario()),
		async setScenario(scenario) {
			writeScenario(scenario);
			setMockScenario(toHandlerScenario(scenario));
		},
		async reset() {
			resetMockState();
		},
		async stop() {
			worker.stop();
		},
	};
}

/**
 * Vite establishes HMR before this bootstrap executes. Subsequent business
 * sockets must remain local in mock mode rather than silently contacting a
 * configured runtime or third-party endpoint.
 */
export function blockExternalBusinessWebSockets(): void {
	const NativeWebSocket = window.WebSocket;
	window.WebSocket = class extends NativeWebSocket {
		constructor(url: string | URL, protocols?: string | string[]) {
			if (new URL(String(url), location.href).origin !== location.origin) {
				throw new DOMException("Mock development blocks external business WebSockets.", "SecurityError");
			}
			super(url, protocols);
		}
	} as typeof WebSocket;
}

/** Remove only the worker that this application owns before real-mode startup. */
export async function unregisterMockWorker(): Promise<void> {
	if (!("serviceWorker" in navigator)) return;
	const registrations = await navigator.serviceWorker.getRegistrations();
	const ownedScope = new URL(import.meta.env.BASE_URL, location.origin).pathname;
	await Promise.all(registrations
		.filter((registration) => new URL(registration.scope).pathname === ownedScope)
		.filter((registration) => registration.active?.scriptURL.endsWith(workerScope) || registration.waiting?.scriptURL.endsWith(workerScope) || registration.installing?.scriptURL.endsWith(workerScope))
		.map((registration) => registration.unregister()));
}

function readScenario(): PreviewScenario {
	const scenario = localStorage.getItem(scenarioStorageKey);
	return scenario === "operator" || scenario === "login" || scenario === "first-password" || scenario === "expired" || scenario === "unavailable" || scenario === "maintenance" || scenario === "platform-one" || scenario === "platform-boundary" || scenario === "empty" || scenario === "slow" || scenario === "conflict" ? scenario : "admin";
}

function writeScenario(scenario: PreviewScenario): void {
	localStorage.setItem(scenarioStorageKey, scenario);
}

const handlerScenarioByPreview: Record<PreviewScenario, import("./handlers").MockScenario> = {
	admin: "administrator", operator: "operator", login: "unauthenticated", "first-password": "password-change",
	expired: "session-expired", unavailable: "unavailable", maintenance: "maintenance", "platform-one": "platform-one", "platform-boundary": "platform-boundary", empty: "empty", slow: "slow", conflict: "conflict",
};
const previewScenarioByHandler: Record<import("./handlers").MockScenario, PreviewScenario> = {
	administrator: "admin", operator: "operator", unauthenticated: "login", "password-change": "first-password",
		"session-expired": "expired", unavailable: "unavailable", maintenance: "maintenance", "platform-one": "platform-one", "platform-boundary": "platform-boundary", empty: "empty", slow: "slow", conflict: "conflict",

};

function toHandlerScenario(scenario: PreviewScenario): import("./handlers").MockScenario {
	return handlerScenarioByPreview[scenario];
}

function fromHandlerScenario(scenario: import("./handlers").MockScenario): PreviewScenario {
	return previewScenarioByHandler[scenario];
}
