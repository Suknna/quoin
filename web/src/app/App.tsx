import { lazy, StrictMode, Suspense, useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import type { UserSummary } from "@/api/generated/types";
import {
	setUnauthorizedHandler,
	WorkbenchApiError,
	workbenchApi,
} from "@/api/workbench";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { AuthScreen } from "@/features/authentication/AuthScreen";
import { EvidenceReader } from "@/features/evidence/ui";
import type { WorkspaceModuleProps } from "./module-contract";
import {
	consolidatedRouteTarget,
	migrateLegacyHash,
	navigateWorkspace,
	readWorkspaceRoute,
} from "./router";
import { type RouteHostComponents, RouteHosts } from "./routes/RouteHosts";
import { ServiceUnavailable } from "./ServiceUnavailable";
import { messageOf } from "./shared";
import "@/styles/index.css";

/** Each host is a separate dynamic import so a feature module is fetched only for its route. */
const routeHostComponents = {
	alerts: lazy(() => import("./routes/AlertsRoute")),
	investigations: lazy(() => import("./routes/InvestigationsRoute")),
	inspections: lazy(() => import("./routes/InspectionsRoute")),
	systems: lazy(() => import("./routes/SystemsRoute")),
	knowledge: lazy(() => import("./routes/KnowledgeRoute")),
	administration: lazy(() => import("./routes/AdministrationRoute")),
	modelProvider: lazy(() => import("./routes/ModelProviderRoute")),
	integrations: lazy(() => import("./routes/IntegrationsRoute")),
	account: lazy(() => import("./routes/AccountRoute")),
} satisfies RouteHostComponents;

type AuthScreenStage = "loading" | "auth" | "workbench";

/** Only one activity ping per throttle window, no matter how often the user acts. */
const ACTIVITY_THROTTLE_MS = 5 * 60_000;

function RouteFallback() {
	return (
		<main className="flex min-h-svh" role="status" aria-label="正在加载工作台">
			<div className="hidden w-56 flex-col gap-3 border-r bg-sidebar p-3 md:flex">
				<Skeleton className="h-9 w-full" />
				<Skeleton className="h-9 w-full" />
				<Skeleton className="h-9 w-full" />
				<Skeleton className="h-9 w-4/5" />
			</div>
			<div className="flex min-w-0 flex-1 flex-col">
				<div className="flex items-center gap-2 border-b p-4">
					<Skeleton className="h-5 w-28" />
				</div>
				<div className="flex flex-col gap-4 p-6">
					<Skeleton className="h-7 w-1/3" />
					<Skeleton className="h-24 w-full" />
					<Skeleton className="h-24 w-full" />
					<Skeleton className="h-24 w-3/4" />
				</div>
			</div>
		</main>
	);
}
function EvidenceOverlay({
	id,
	user,
	onClose,
}: {
	id: string;
	user: UserSummary;
	onClose: () => void;
}) {
	return (
		<Dialog
			open
			onOpenChange={(open) => {
				if (!open) onClose();
			}}
		>
			<DialogContent className="flex h-[100svh] max-h-none w-screen max-w-none flex-col overflow-hidden rounded-none sm:h-[90svh] sm:max-w-4xl sm:rounded-lg">
				<DialogHeader className="sr-only">
					<DialogTitle>证据阅读</DialogTitle>
					<DialogDescription>查看调查证据详情。</DialogDescription>
				</DialogHeader>
				<EvidenceReader id={id} user={user} />
			</DialogContent>
		</Dialog>
	);
}
function Workspace({
	user,
	onLogout,
	maintenanceActive,
}: {
	user: UserSummary;
	onLogout: () => Promise<void>;
	maintenanceActive: boolean;
}) {
	const [route, setRoute] = useState(() => {
		migrateLegacyHash();
		return readWorkspaceRoute();
	});
	const [evidenceSource, setEvidenceSource] = useState<string>();
	const returnFocus = useRef<HTMLElement | null>(null);
	const returnScroll = useRef(0);
	useEffect(() => {
		const listen = () => setRoute(readWorkspaceRoute());
		window.addEventListener("popstate", listen);
		return () => window.removeEventListener("popstate", listen);
	}, []);
	const navigate = (to: string) => {
		navigateWorkspace(to);
	};
	useEffect(() => {
		const destination = consolidatedRouteTarget(route.pathname);
		if (destination) navigateWorkspace(destination, true);
	}, [route.pathname]);

	const evidenceId = route.pathname.match(/^\/evidence\/([^/]+)$/)?.[1];
	const requestedSource = new URLSearchParams(route.search).get("from");
	const sourceRoute = evidenceSource ?? requestedSource ?? "/investigations";
	const activeRoute = evidenceId ? sourceRoute : route.route;
	const props: WorkspaceModuleProps = {
		user,
		route: activeRoute,
		navigate,
		suspended: maintenanceActive,
		maintenanceActive,
		openEvidence: (id) => {
			returnFocus.current =
				document.activeElement instanceof HTMLElement
					? document.activeElement
					: null;
			returnScroll.current = window.scrollY;
			setEvidenceSource(route.route);
			navigate(
				`/evidence/${encodeURIComponent(id)}?from=${encodeURIComponent(route.route)}`,
			);
		},
	};
	function closeEvidence() {
		const destination = evidenceSource ?? requestedSource ?? "/investigations";
		setEvidenceSource(undefined);
		navigate(destination);
		requestAnimationFrame(() => {
			try {
				window.scrollTo(0, returnScroll.current);
			} catch {
				/* jsdom and embedded hosts may not implement scrolling. */
			}
			returnFocus.current?.focus();
		});
	}
	return (
		<>
			<div
				hidden={Boolean(evidenceId)}
				inert={Boolean(evidenceId) || undefined}
			>
				<Suspense fallback={<RouteFallback />}>
					<RouteHosts
						components={routeHostComponents}
						props={props}
						onLogout={onLogout}
					/>
				</Suspense>
			</div>
			{evidenceId && (
				<EvidenceOverlay
					id={decodeURIComponent(evidenceId)}
					user={user}
					onClose={closeEvidence}
				/>
			)}
		</>
	);
}

export function App() {
	const [screen, setScreen] = useState<AuthScreenStage>("loading");
	const [user, setUser] = useState<UserSummary>();
	const [maintenance, setMaintenance] = useState(false);
	const [bootstrapError, setBootstrapError] = useState("");
	const [retrying, setRetrying] = useState(false);
	function authenticated(next: UserSummary) {
		setUser(next);
		setBootstrapError("");
		setScreen("workbench");
	}
	// Session expiry and logout share this single path: the workspace unmounts
	// immediately, so every draft and in-memory secret is dropped and any later
	// login (same user or not) starts from a fresh workspace.
	function clear() {
		setUser(undefined);
		setScreen("auth");
	}
	async function logout() {
		try {
			await workbenchApi.logout();
			clear();
		} catch (reason) {
			if (reason instanceof WorkbenchApiError && reason.status === 401) clear();
			else throw reason;
		}
	}
	async function bootstrap() {
		if (retrying) return;
		setRetrying(true);
		setScreen("loading");
		try {
			authenticated(await workbenchApi.currentUser());
		} catch (reason) {
			if (reason instanceof WorkbenchApiError && reason.status === 401) clear();
			else {
				setBootstrapError(messageOf(reason, "认证服务当前不可用。"));
				setScreen("loading");
			}
		} finally {
			setRetrying(false);
		}
	}
	useEffect(() => {
		setUnauthorizedHandler(clear);
		let cancelled = false;
		workbenchApi
			.currentUser()
			.then((next) => {
				if (!cancelled) authenticated(next);
			})
			.catch((reason) => {
				if (cancelled) return;
				if (reason instanceof WorkbenchApiError && reason.status === 401)
					clear();
				else setBootstrapError(messageOf(reason, "认证服务当前不可用。"));
			});
		return () => {
			cancelled = true;
			setUnauthorizedHandler();
		};
	}, []);
	useEffect(() => {
		if (screen !== "workbench" || !user) return;
		let running = false;
		async function reconcile() {
			if (running) return;
			running = true;
			try {
				// A lightweight authoritative check also catches 401s returned by feature APIs
				// that do not use workbenchApi's unauthorized callback.
				authenticated(await workbenchApi.currentUser());
				const state = await workbenchApi.maintenance();
				setMaintenance(state?.active === true);
			} catch (reason) {
				if (reason instanceof WorkbenchApiError && reason.status === 401)
					clear();
			} finally {
				running = false;
			}
		}
		void reconcile();
		const interval = window.setInterval(() => void reconcile(), 30_000);
		const onFocus = () => void reconcile();
		window.addEventListener("focus", onFocus);
		return () => {
			window.clearInterval(interval);
			window.removeEventListener("focus", onFocus);
		};
	}, [screen, user?.id]);
	// Activity pings keep the server-side idle clock fresh, driven only by real
	// user events (pointer, key, tab visible) with a five-minute throttle — no
	// interval runs, so an idle session never renews itself. Ping failures stay
	// silent: the reconcile loop above owns session-expiry recovery.
	const lastActivityRef = useRef(0);
	useEffect(() => {
		if (screen !== "workbench" || !user) return;
		lastActivityRef.current = Date.now();
		const signal = () => {
			const now = Date.now();
			if (now - lastActivityRef.current < ACTIVITY_THROTTLE_MS) return;
			lastActivityRef.current = now;
			void workbenchApi.activity().catch(() => undefined);
		};
		const onVisibility = () => {
			if (document.visibilityState === "visible") signal();
		};
		window.addEventListener("pointerdown", signal);
		window.addEventListener("keydown", signal);
		document.addEventListener("visibilitychange", onVisibility);
		return () => {
			window.removeEventListener("pointerdown", signal);
			window.removeEventListener("keydown", signal);
			document.removeEventListener("visibilitychange", onVisibility);
		};
	}, [screen, user?.id]);
	return (
		<>
			{screen === "loading" &&
				(bootstrapError ? (
					<ServiceUnavailable
						pending={retrying}
						onRetry={() => void bootstrap()}
					/>
				) : (
					<main
						className="flex min-h-svh items-center justify-center p-6"
						role="status"
						aria-label="正在验证会话"
					>
						<div className="flex w-full max-w-sm flex-col items-center gap-4">
							<Skeleton className="h-10 w-10 rounded-full" />
							<div className="flex w-full flex-col gap-2">
								<Skeleton className="h-4 w-full" />
								<Skeleton className="h-4 w-3/5" />
							</div>
						</div>
					</main>
				))}
			{screen === "auth" && <AuthScreen onAuthenticated={authenticated} />}
			{screen === "workbench" && user && (
				<Workspace
					user={user}
					onLogout={logout}
					maintenanceActive={maintenance}
				/>
			)}
		</>
	);
}
export function mountWorkbench(root: HTMLElement) {
	createRoot(root).render(
		<StrictMode>
			<App />
		</StrictMode>,
	);
}
