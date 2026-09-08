import { GalleryVerticalEnd } from "lucide-react";
import { lazy, type FormEvent, type ReactNode, StrictMode, Suspense, useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import type { UserSummary } from "../src/api/generated/types";
import { Button } from "../templates/components/ui/button";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "../templates/components/ui/field";
import { Input } from "../templates/components/ui/input";
import { setUnauthorizedHandler, WorkbenchApiError, workbenchApi } from "./api";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "./components/ui/dialog";
import type { WorkspaceModuleProps } from "./module-contract";
import { RouteHosts, type RouteHostComponents } from "./RouteHosts";
import { EvidenceReader } from "./modules/evidence";
import { migrateLegacyHash, navigateWorkspace, readWorkspaceRoute } from "./router";
import { ServiceUnavailable } from "./ServiceUnavailable";
import { ErrorMessage, messageOf } from "./shared";
import "./styles.css";

/** Each host is a separate dynamic import so a feature module is fetched only for its route. */
const routeHostComponents = {
	alerts: lazy(() => import("./route-hosts/AlertsRoute")),
	connections: lazy(() => import("./route-hosts/ConnectionsRoute")),
	investigations: lazy(() => import("./route-hosts/InvestigationsRoute")),
	inspections: lazy(() => import("./route-hosts/InspectionsRoute")),
	systems: lazy(() => import("./route-hosts/SystemsRoute")),
	knowledge: lazy(() => import("./route-hosts/KnowledgeRoute")),
	administration: lazy(() => import("./route-hosts/AdministrationRoute")),
	account: lazy(() => import("./route-hosts/AccountRoute")),
} satisfies RouteHostComponents;

type AuthScreen = "loading" | "login" | "password-change" | "workbench";

function Brand() {
	return <div className="flex items-center gap-2 font-medium"><div className="flex size-6 items-center justify-center rounded-md bg-primary text-primary-foreground"><GalleryVerticalEnd className="size-4" /></div>Quoin</div>;
}
function AuthLayout({ children }: { children: ReactNode }) {
	return <div className="grid min-h-svh lg:grid-cols-2"><div className="flex flex-col gap-4 p-6 md:p-10"><div className="flex justify-center md:justify-start"><Brand /></div><div className="flex flex-1 items-center justify-center"><div className="w-full max-w-xs">{children}</div></div></div><div className="relative hidden bg-muted lg:block" aria-hidden="true"><img src="/placeholder.svg" alt="" className="absolute inset-0 h-full w-full object-cover dark:brightness-[0.2] dark:grayscale" /></div></div>;
}
function LoginForm({ onAuthenticated }: { onAuthenticated: (user: UserSummary) => void }) {
	const [username, setUsername] = useState(""); const [password, setPassword] = useState(""); const [error, setError] = useState(""); const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) { event.preventDefault(); setError(""); setSaving(true); try { onAuthenticated(await workbenchApi.login({ username, password })); } catch (reason) { const apiError = reason as WorkbenchApiError; setError(apiError.status === 403 && apiError.code === "password_change_required" ? "此账户必须先修改密码。" : messageOf(reason, "登录暂时不可用，请重试。")); } finally { setSaving(false); setPassword(""); } }
	return <form className="flex flex-col gap-6" onSubmit={submit}><FieldGroup><div className="flex flex-col items-center gap-1 text-center"><h1 className="text-2xl font-bold">登录工作台</h1><p className="text-sm text-balance text-muted-foreground">使用你的 Quoin 用户名和密码继续</p></div>{error && <ErrorMessage>{error}</ErrorMessage>}<Field><FieldLabel htmlFor="username">用户名</FieldLabel><Input id="username" autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required maxLength={200} autoFocus /></Field><Field><FieldLabel htmlFor="password">密码</FieldLabel><Input id="password" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={15} maxLength={128} /></Field><Button type="submit" disabled={saving}>{saving ? "正在登录…" : "登录"}</Button></FieldGroup></form>;
}
function PasswordChange({ user, onChanged, onLogout }: { user: UserSummary; onChanged: (user: UserSummary) => void; onLogout: () => void }) {
	const [currentPassword, setCurrentPassword] = useState(""); const [newPassword, setNewPassword] = useState(""); const [confirmation, setConfirmation] = useState(""); const [error, setError] = useState(""); const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) { event.preventDefault(); setError(""); if (newPassword !== confirmation) { setError("两次输入的新密码不一致。"); return; } setSaving(true); try { await workbenchApi.changePassword({ currentPassword, newPassword }); onChanged(await workbenchApi.currentUser()); } catch (reason) { setError(messageOf(reason, "没有修改成功，请重试。")); } finally { setSaving(false); setCurrentPassword(""); setNewPassword(""); setConfirmation(""); } }
	async function logout() { setError(""); try { await workbenchApi.logout(); onLogout(); } catch (reason) { if (reason instanceof WorkbenchApiError && reason.status === 401) onLogout(); else setError(messageOf(reason, "退出登录失败，请重试。")); } }
	return <form className="flex flex-col gap-6" onSubmit={submit}><FieldGroup><div className="flex flex-col items-center gap-1 text-center"><h1 className="text-2xl font-bold">先设置你自己的密码</h1><p className="text-sm text-balance text-muted-foreground">{user.displayName}，临时密码只能用于首次登录。</p></div><FieldDescription>使用 15–128 个字符。可以使用空格和中文。</FieldDescription>{error && <ErrorMessage>{error}</ErrorMessage>}<Field><FieldLabel htmlFor="current-password">当前临时密码</FieldLabel><Input id="current-password" type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} minLength={15} maxLength={128} required autoFocus /></Field><Field><FieldLabel htmlFor="new-password">新密码</FieldLabel><Input id="new-password" type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} minLength={15} maxLength={128} required /></Field><Field><FieldLabel htmlFor="confirm-password">再次输入新密码</FieldLabel><Input id="confirm-password" type="password" autoComplete="new-password" value={confirmation} onChange={(e) => setConfirmation(e.target.value)} minLength={15} maxLength={128} required /></Field><Button type="submit" disabled={saving}>{saving ? "正在保存…" : "保存并进入工作台"}</Button><Button variant="ghost" type="button" onClick={() => void logout()}>退出登录</Button></FieldGroup></form>;
}

function RouteFallback() {
	return <main className="flex min-h-svh items-center justify-center p-6"><p role="status">正在加载工作台…</p></main>;
}
function EvidenceOverlay({ id, user, onClose }: { id: string; user: UserSummary; onClose: () => void }) {
	return <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}><DialogContent className="flex h-[100svh] max-h-none w-screen max-w-none flex-col overflow-hidden rounded-none sm:h-[90svh] sm:max-w-4xl sm:rounded-lg"><DialogHeader className="sr-only"><DialogTitle>证据阅读</DialogTitle><DialogDescription>查看调查证据详情。</DialogDescription></DialogHeader><EvidenceReader id={id} user={user} onClose={onClose} /></DialogContent></Dialog>;
}
function Workspace({ user, onLogout, authenticationSuspended, maintenanceActive }: { user: UserSummary; onLogout: () => Promise<void>; authenticationSuspended: boolean; maintenanceActive: boolean }) {
	const [route, setRoute] = useState(() => { migrateLegacyHash(); return readWorkspaceRoute(); });
	const [evidenceSource, setEvidenceSource] = useState<string>();
	const returnFocus = useRef<HTMLElement | null>(null);
	const returnScroll = useRef(0);
	useEffect(() => { const listen = () => setRoute(readWorkspaceRoute()); window.addEventListener("popstate", listen); return () => window.removeEventListener("popstate", listen); }, []);
	const navigate = (to: string) => { navigateWorkspace(to); };
	const evidenceId = route.pathname.match(/^\/evidence\/([^/]+)$/)?.[1];
	const requestedSource = new URLSearchParams(route.search).get("from");
	const sourceRoute = evidenceSource ?? requestedSource ?? "/investigations";
	const activeRoute = evidenceId ? sourceRoute : route.route;
	const props: WorkspaceModuleProps = { user, route: activeRoute, navigate, suspended: authenticationSuspended || maintenanceActive, authenticationSuspended, maintenanceActive, openEvidence: (id) => {
		returnFocus.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
		returnScroll.current = window.scrollY;
		setEvidenceSource(route.route);
		navigate(`/evidence/${encodeURIComponent(id)}?from=${encodeURIComponent(route.route)}`);
	} };
	function closeEvidence() {
		const destination = evidenceSource ?? requestedSource ?? "/investigations";
		setEvidenceSource(undefined);
		navigate(destination);
		requestAnimationFrame(() => { try { window.scrollTo(0, returnScroll.current); } catch { /* jsdom and embedded hosts may not implement scrolling. */ } returnFocus.current?.focus(); });
	}
	return <><div hidden={Boolean(evidenceId)} inert={Boolean(evidenceId) || undefined}><Suspense fallback={<RouteFallback />}><RouteHosts components={routeHostComponents} props={props} onLogout={onLogout} /></Suspense></div>{evidenceId && <EvidenceOverlay id={decodeURIComponent(evidenceId)} user={user} onClose={closeEvidence} />}</>;
}

export function App() {
	const [screen, setScreen] = useState<AuthScreen>("loading"); const [user, setUser] = useState<UserSummary>(); const [expired, setExpired] = useState(false); const [maintenance, setMaintenance] = useState(false); const [bootstrapError, setBootstrapError] = useState(""); const [retrying, setRetrying] = useState(false); const userId = useRef<string | undefined>(undefined); const [workspaceKey, setWorkspaceKey] = useState(0);
	function authenticated(next: UserSummary) { const changedUser = userId.current !== undefined && userId.current !== next.id; userId.current = next.id; if (changedUser) setWorkspaceKey((key) => key + 1); setUser(next); setExpired(false); setBootstrapError(""); setScreen(next.passwordChangeRequired ? "password-change" : "workbench"); }
	function clear() { userId.current = undefined; setUser(undefined); setExpired(false); setScreen("login"); }
	async function logout() {
		try { await workbenchApi.logout(); clear(); }
		catch (reason) { if (reason instanceof WorkbenchApiError && reason.status === 401) clear(); else throw reason; }
	}
	async function bootstrap() { if (retrying) return; setRetrying(true); setScreen("loading"); try { authenticated(await workbenchApi.currentUser()); } catch (reason) { if (reason instanceof WorkbenchApiError && reason.status === 401) clear(); else { setBootstrapError(messageOf(reason, "认证服务当前不可用。")); setScreen("loading"); } } finally { setRetrying(false); } }
	useEffect(() => { setUnauthorizedHandler(() => { if (userId.current) setExpired(true); }); let cancelled = false; workbenchApi.currentUser().then((next) => { if (!cancelled) authenticated(next); }).catch((reason) => { if (cancelled) return; if (reason instanceof WorkbenchApiError && reason.status === 401) clear(); else setBootstrapError(messageOf(reason, "认证服务当前不可用。")); }); return () => { cancelled = true; setUnauthorizedHandler(); }; }, []);
	useEffect(() => {
		if (screen !== "workbench" || !user || expired) return;
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
				if (reason instanceof WorkbenchApiError && reason.status === 401) setExpired(true);
			} finally { running = false; }
		}
		void reconcile();
		const interval = window.setInterval(() => void reconcile(), 30_000);
		const onFocus = () => void reconcile();
		window.addEventListener("focus", onFocus);
		return () => { window.clearInterval(interval); window.removeEventListener("focus", onFocus); };
	}, [screen, user?.id, expired]);
	return <>{screen === "loading" && (bootstrapError ? <ServiceUnavailable pending={retrying} onRetry={() => void bootstrap()} /> : <main className="flex min-h-svh items-center justify-center p-6"><p role="status">正在验证会话…</p></main>)}{screen === "login" && <AuthLayout><LoginForm onAuthenticated={authenticated} /></AuthLayout>}{screen === "password-change" && user && <AuthLayout><PasswordChange user={user} onChanged={authenticated} onLogout={clear} /></AuthLayout>}{screen === "workbench" && user && <div inert={expired || undefined}><Workspace key={workspaceKey} user={user} onLogout={logout} authenticationSuspended={expired} maintenanceActive={maintenance} /></div>}<Dialog open={expired}><DialogContent showCloseButton={false} onPointerDownOutside={(event) => event.preventDefault()} onEscapeKeyDown={(event) => event.preventDefault()}><DialogHeader><DialogTitle>会话已失效</DialogTitle><DialogDescription>请重新登录后继续。工作区仍保留在后台；不同用户登录时会清除其状态和草稿。</DialogDescription></DialogHeader><LoginForm onAuthenticated={authenticated} /></DialogContent></Dialog></>;
}
export function mountWorkbench(root: HTMLElement) { createRoot(root).render(<StrictMode><App /></StrictMode>); }
