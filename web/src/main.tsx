import { mountWorkbench } from "./app/App";

const root = document.getElementById("root");
if (!root) throw new Error("Workbench root is missing");
const appRoot = root;

/** The mock runtime must be ready before React reads its initial auth session. */
async function bootstrap(): Promise<void> {
	if (import.meta.env.DEV && import.meta.env.VITE_QUOIN_MODE !== "real") {
		const [{ startMockRuntime, blockExternalBusinessWebSockets }, { PreviewPanel }, { createRoot }, { createElement }] = await Promise.all([
			import("./mocks/browser"),
			import("./mocks/PreviewPanel"),
			import("react-dom/client"),
			import("react"),
		]);
		const runtime = await startMockRuntime();
		blockExternalBusinessWebSockets();
		const panelRoot = document.createElement("div");
		document.body.append(panelRoot);
		const renderPanel = () => createRoot(panelRoot).render(createElement(PreviewPanel, {
			activeScenario: runtime.scenario(),
			onScenarioChange: async (scenario) => { await runtime.setScenario(scenario); location.reload(); },
			onReset: async () => { await runtime.reset(); location.reload(); },
		}));
		mountWorkbench(appRoot);
		renderPanel();
		return;
	}

	if (import.meta.env.DEV) await (await import("./mocks/browser")).unregisterMockWorker();
	mountWorkbench(appRoot);
}

void bootstrap().catch((reason: unknown) => {
	// Mock startup must fail closed: never render the app or fall through to a real API.
	const detail = reason instanceof Error ? reason.message : "未知启动错误";
	appRoot.replaceChildren();
	const failure = document.createElement("main");
	failure.className = "flex min-h-svh items-center justify-center p-6";
	failure.innerHTML = `<section role="alert" class="max-w-lg rounded border border-destructive p-6"><h1 class="text-xl font-semibold">模拟数据启动失败</h1><p class="mt-2 text-sm">Mock Service Worker 未能启动。开发预览已停止，未连接真实服务。</p><pre class="mt-3 overflow-auto text-xs">${detail.replace(/[&<>]/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[character]!)}</pre></section>`;
	appRoot.append(failure);
});
