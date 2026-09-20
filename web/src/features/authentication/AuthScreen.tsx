import { ShieldAlert } from "lucide-react";
import { type FormEvent, useCallback, useEffect, useState } from "react";
import type { AuthConfig, UserSummary } from "@/api/generated/types";
import { workbenchApi } from "@/api/workbench";
import { AuthBrandPanel } from "@/app/AuthBrandPanel";
import { BrandLockup } from "@/app/Brand";
import { notify } from "@/app/shared";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { authApi } from "./api";

/**
 * The config-driven login surface (ADR-0010): the public /auth/config
 * projection decides what renders — the local emergency form and/or the SSO
 * button. The OIDC round-trip is a full-page navigation to the backend, so
 * the browser never touches a token; failures return as /login?error=<码>.
 * The first forced password change rides the ordinary self-service endpoint
 * on the restricted session the single-step login issued.
 */

const PASSWORD_DESCRIPTION = "使用 15–128 个字符。可以使用空格和中文。";

/** Stable OIDC failure codes mapped to human text; none carries a secret. */
const OIDC_ERROR_TEXT: Record<string, string> = {
	state_invalid: "登录状态校验失败，请重新发起登录。",
	account_disabled: "该账号已被平台禁用，无法登录。",
	idp_unavailable: "统一身份平台暂时不可用，请稍后重试或使用本地应急通道。",
	oidc_misconfigured: "统一身份登录配置异常，请联系管理员。",
	access_denied: "你或统一身份平台取消了本次登录。",
	login_rejected: "统一身份登录未通过校验，请重试。",
};

function Note({ children }: { children: string }) {
	return (
		<div role="status" className="text-sm text-muted-foreground">
			{children}
		</div>
	);
}

function beginOIDCLogin(returnTo: string) {
	window.location.assign(
		`/api/v1/auth/oidc/login?return_to=${encodeURIComponent(returnTo)}`,
	);
}

function LoginPane({
	config,
	note,
	forceLocal,
	onAuthenticated,
	onRestricted,
}: {
	config: AuthConfig;
	note: string;
	forceLocal: boolean;
	onAuthenticated: (user: UserSummary) => void;
	onRestricted: (user: UserSummary, password: string) => void;
}) {
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) {
		event.preventDefault();
		setSaving(true);
		try {
			const result = await authApi.login({ username, password });
			if (result.user.passwordChangeRequired) onRestricted(result.user, password);
			else onAuthenticated(result.user);
		} catch (reason) {
			// Invalid credentials and every other failure surface the same way;
			// the form stays mounted so the user can correct and retry.
			notify.error(reason, "登录暂时不可用，请重试。");
			setPassword("");
		} finally {
			setSaving(false);
		}
	}
	const showLocal = forceLocal || (config.local.enabled && config.local.visible);
	return (
		<div className="flex flex-col gap-6">
			{showLocal && (
				<form className="flex flex-col gap-6" onSubmit={submit}>
					<FieldGroup>
						<div className="flex flex-col items-center gap-1 text-center">
							<h1 className="text-2xl font-bold">登录工作台</h1>
							<p className="text-sm text-balance text-muted-foreground">
								使用你的 Quoin 用户名和密码继续
							</p>
						</div>
						{note && <Note>{note}</Note>}
						<Field>
							<FieldLabel htmlFor="login-username">用户名</FieldLabel>
							<Input
								id="login-username"
								name="username"
								autoComplete="username"
								autoFocus
								required
								value={username}
								onChange={(event) => setUsername(event.target.value)}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="login-password">密码</FieldLabel>
							<Input
								id="login-password"
								name="password"
								type="password"
								autoComplete="current-password"
								required
								value={password}
								onChange={(event) => setPassword(event.target.value)}
							/>
						</Field>
					</FieldGroup>
					<Button type="submit" disabled={saving}>
						{saving ? "正在登录…" : "登录"}
					</Button>
					{config.oidc.enabled && (
						<p className="text-xs text-balance text-muted-foreground">
							本地账号是 IdP 故障时的应急维护通道；日常登录请使用统一身份入口。
						</p>
					)}
				</form>
			)}
			{config.oidc.enabled && (
				<div className="flex flex-col gap-3">
					{showLocal && (
						<div className="flex items-center gap-3" aria-hidden="true">
							<span className="h-px flex-1 bg-border" />
							<span className="text-xs text-muted-foreground">或</span>
							<span className="h-px flex-1 bg-border" />
						</div>
					)}
					<Button
						type="button"
						variant="outline"
						onClick={() => beginOIDCLogin(window.location.pathname)}
					>
						{config.oidc.label || "统一身份登录"}
					</Button>
				</div>
			)}
		</div>
	);
}

function SetPasswordPane({
	temporary,
	onDone,
}: {
	temporary: string;
	onDone: (user: UserSummary) => void;
}) {
	const [currentPassword, setCurrentPassword] = useState(temporary);
	const [newPassword, setNewPassword] = useState("");
	const [confirmPassword, setConfirmPassword] = useState("");
	const [saving, setSaving] = useState(false);
	const [formError, setFormError] = useState("");
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (newPassword !== confirmPassword) {
			setFormError("两次输入的新密码不一致。");
			return;
		}
		setSaving(true);
		try {
			await authApi.changePassword(currentPassword, newPassword);
			onDone(await workbenchApi.currentUser());
		} catch (reason) {
			setFormError("");
			notify.error(reason, "暂时无法完成密码设置，请重试。");
		} finally {
			setSaving(false);
		}
	}
	return (
		<form className="flex flex-col gap-6" onSubmit={submit}>
			<FieldGroup>
				{formError && (
					<p role="alert" className="text-sm text-destructive">
						{formError}
					</p>
				)}
				<div className="flex flex-col items-center gap-1 text-center">
					<h1 className="text-2xl font-bold">设置你的新密码</h1>
					<p className="text-sm text-balance text-muted-foreground">
						首次登录或密码被重置后必须设置正式密码
					</p>
				</div>
				<Field>
					<FieldLabel htmlFor="change-current">当前密码</FieldLabel>
					<Input
						id="change-current"
						type="password"
						autoComplete="current-password"
						required
						value={currentPassword}
						onChange={(event) => setCurrentPassword(event.target.value)}
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor="change-new">新密码</FieldLabel>
					<Input
						id="change-new"
						type="password"
						autoComplete="new-password"
						required
						value={newPassword}
						onChange={(event) => setNewPassword(event.target.value)}
					/>
					<FieldDescription>{PASSWORD_DESCRIPTION}</FieldDescription>
				</Field>
				<Field>
					<FieldLabel htmlFor="change-confirm">确认新密码</FieldLabel>
					<Input
						id="change-confirm"
						type="password"
						autoComplete="new-password"
						required
						value={confirmPassword}
						onChange={(event) => setConfirmPassword(event.target.value)}
					/>
				</Field>
			</FieldGroup>
			<Button type="submit" disabled={saving}>
				{saving ? "正在保存…" : "保存并进入工作台"}
			</Button>
		</form>
	);
}

export function AuthScreen({
	onAuthenticated,
}: {
	onAuthenticated: (user: UserSummary) => void;
}) {
	const [config, setConfig] = useState<AuthConfig>();
	const [errorText] = useState(() => {
		const code = new URLSearchParams(window.location.search).get("error");
		return code ? (OIDC_ERROR_TEXT[code] ?? "登录未完成，请重试。") : "";
	});
	const [restrictedUser, setRestrictedUser] = useState<UserSummary>();
	const [temporaryPassword, setTemporaryPassword] = useState("");
	const forceLocal =
		typeof window !== "undefined" &&
		new URLSearchParams(window.location.search).get("local") === "1";
	const holdLogin =
		typeof window !== "undefined" &&
		new URLSearchParams(window.location.search).get("login") === "1";

	useEffect(() => {
		let cancelled = false;
		authApi
			.config()
			.then((next) => {
				if (!cancelled) setConfig(next);
			})
			.catch(() => {
				if (!cancelled)
					setConfig({
						local: { enabled: true, visible: true },
						oidc: { enabled: false },
					});
			});
		return () => {
			cancelled = true;
		};
	}, []);

	// A single visible channel that is OIDC goes straight to the IdP; ?login=1
	// holds the page (and ?local=1 forces the emergency form).
	const autoRedirect =
		config !== undefined &&
		config.oidc.enabled &&
		!(forceLocal && config.local.enabled) &&
		!holdLogin &&
		!(config.local.enabled && config.local.visible) &&
		!restrictedUser &&
		!errorText;
	useEffect(() => {
		if (autoRedirect && config)
			beginOIDCLogin(window.sessionStorage.getItem("quoin.login.retain") ?? "/");
	}, [autoRedirect, config]);

	const onRestricted = useCallback((user: UserSummary, password: string) => {
		setRestrictedUser(user);
		setTemporaryPassword(password);
	}, []);

	if (restrictedUser) {
		return (
			<main className="flex min-h-svh items-center justify-center p-6">
				<div className="flex w-full max-w-sm flex-col gap-6">
					<BrandLockup />
					<SetPasswordPane
						temporary={temporaryPassword}
						onDone={onAuthenticated}
					/>
				</div>
			</main>
		);
	}

	return (
		<main className="flex min-h-svh">
			<section className="flex flex-1 items-center justify-center p-6">
				<div className="flex w-full max-w-sm flex-col gap-6">
					<BrandLockup />
					{errorText && (
						<div
							role="alert"
							className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive"
						>
							<ShieldAlert className="mt-0.5 size-4 shrink-0" />
							<span>{errorText}</span>
						</div>
					)}
					{config === undefined ? (
						<div className="flex flex-col gap-4" role="status" aria-label="正在加载登录配置">
							<Skeleton className="h-10 w-full" />
							<Skeleton className="h-10 w-full" />
							<Skeleton className="h-10 w-2/3" />
						</div>
					) : (
						<LoginPane
							config={config}
							forceLocal={forceLocal}
							note=""
							onAuthenticated={onAuthenticated}
							onRestricted={onRestricted}
						/>
					)}
					{config !== undefined && !config.oidc.enabled && !errorText && (
						<Note>忘记密码请联系管理员重置。</Note>
					)}
				</div>
			</section>
			<AuthBrandPanel />
		</main>
	);
}
