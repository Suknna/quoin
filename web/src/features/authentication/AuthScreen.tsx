import { LoaderCircle, MailIcon, MessageSquareIcon } from "lucide-react";
import { type FormEvent, useCallback, useEffect, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import { WorkbenchApiError } from "@/api/workbench";
import { AuthBrandPanel } from "@/app/AuthBrandPanel";
import { BrandLockup } from "@/app/Brand";
import { ErrorMessage, messageOf } from "@/app/shared";
import { Button } from "@/components/ui/button";
import {
	Field,
	FieldDescription,
	FieldGroup,
	FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { type AuthContactChannel, type AuthFlow, authFlowApi } from "./api";
import { DeliveryPane } from "./DeliveryPane";

/**
 * The unified authentication surface: login, first-run initialization
 * (admin/operator) and the second-factor challenge use separate login and centered verification shells, driven by
 * the server-side flow behind the __Host-quoin-flow cookie. Admin recovery is
 * CLI-only: it sets a temporary password, and the following normal sign-in is
 * classified (and initialized) like a first run. Completing an initialization
 * never starts a session — the user returns to the login form.
 */

const PASSWORD_DESCRIPTION = "使用 15–128 个字符。可以使用空格和中文。";
const RESEND_SECONDS = 60;

type Pane = "login" | "password" | "contact" | "otp" | "finish";

/** Only a first-run administrator registers contacts inside the flow. */
const isAdministratorFlow = (type: AuthFlow["type"]) =>
	type === "admin_initialize";

/**
 * Derives the visible step from the authoritative flow projection. Every
 * mutating step re-reads GET /auth/flow afterwards, so a refresh, a retry or
 * a server-side restart lands on the same step deterministically.
 */
function paneOf(flow: AuthFlow | null): Pane {
	if (!flow) return "login";
	if (flow.type === "login") {
		// Login is always the second-factor challenge: temp credentials that
		// still owe a real password classify as initialization flows, so no
		// pending login row can trap the user in a password step.
		return "otp";
	}
	if (flow.passwordSet === false) return "password";
	// Defensive against a flow projection without contacts: an absent list is
	// the same fact as an empty one for routing purposes.
	const contacts = flow.contacts ?? [];
	if (isAdministratorFlow(flow.type) && contacts.length === 0) {
		return "contact";
	}
	// Per-flow factor verification decides finish vs OTP — never the
	// account-level contact.verified, which survives password recovery and
	// would wrongly skip this flow's challenge.
	return flow.factorVerified ? "finish" : "otp";
}

/** The flow cookie is gone or expired; the only way forward is a fresh login. */
function isFlowGone(reason: unknown) {
	return (
		reason instanceof WorkbenchApiError &&
		(reason.status === 401 ||
			reason.status === 404 ||
			reason.code === "flow_expired")
	);
}

const channelLabel = (channel: AuthContactChannel) =>
	channel === "email" ? "邮箱" : "短信";

function Note({ children }: { children: string }) {
	return (
		<div role="status" className="text-sm text-muted-foreground">
			{children}
		</div>
	);
}

function LoginPane({
	note,
	onFlow,
}: {
	note: string;
	onFlow: (flow: AuthFlow) => void;
}) {
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		setSaving(true);
		try {
			onFlow(await authFlowApi.start({ username, password }));
		} catch (reason) {
			// Invalid credentials and every other failure render the same way;
			// the form stays mounted so the user can correct and retry.
			setError(messageOf(reason, "登录暂时不可用，请重试。"));
			setPassword("");
		} finally {
			setSaving(false);
		}
	}
	return (
		<form className="flex flex-col gap-6" onSubmit={submit}>
			<FieldGroup>
				<div className="flex flex-col items-center gap-1 text-center">
					<h1 className="text-2xl font-bold">登录工作台</h1>
					<p className="text-sm text-balance text-muted-foreground">
						使用你的 Quoin 用户名和密码继续
					</p>
				</div>
				{note && <Note>{note}</Note>}
				{error && <ErrorMessage>{error}</ErrorMessage>}
				<Field>
					<FieldLabel htmlFor="username">用户名</FieldLabel>
					<Input
						id="username"
						autoComplete="username"
						value={username}
						onChange={(e) => setUsername(e.target.value)}
						required
						maxLength={200}
						autoFocus
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor="password">密码</FieldLabel>
					<Input
						id="password"
						type="password"
						autoComplete="current-password"
						value={password}
						onChange={(e) => setPassword(e.target.value)}
						required
						maxLength={128}
					/>
				</Field>
				<Button type="submit" disabled={saving}>
					{saving ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							登录中…
						</>
					) : (
						"登录"
					)}
				</Button>
			</FieldGroup>
		</form>
	);
}

function PasswordPane({
	flow,
	onFlow,
	onFlowGone,
}: {
	flow: AuthFlow;
	onFlow: (flow: AuthFlow) => void;
	onFlowGone: () => void;
}) {
	const [newPassword, setNewPassword] = useState("");
	const [confirmation, setConfirmation] = useState("");
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		if (newPassword !== confirmation) {
			setError("两次输入的新密码不一致。");
			return;
		}
		setSaving(true);
		try {
			await authFlowApi.setPassword(newPassword);
			onFlow(await authFlowApi.resume());
		} catch (reason) {
			if (isFlowGone(reason)) {
				onFlowGone();
				return;
			}
			setError(messageOf(reason, "没有设置成功，请重试。"));
		} finally {
			setSaving(false);
			setNewPassword("");
			setConfirmation("");
		}
	}
	const operator = flow.type === "operator_initialize";
	return (
		<form className="flex flex-col gap-6" onSubmit={submit}>
			<FieldGroup>
				<div className="flex flex-col items-center gap-1 text-center">
					<h1 className="text-2xl font-bold">
						{operator ? "设置你的新密码" : "设置管理员密码"}
					</h1>
					<p className="text-sm text-balance text-muted-foreground">
						{flow.user.displayName
							? `${flow.user.displayName}，请先设置正式密码。`
							: "请先设置正式密码。"}
					</p>
				</div>
				<FieldDescription>{PASSWORD_DESCRIPTION}</FieldDescription>
				{error && <ErrorMessage>{error}</ErrorMessage>}
				<Field>
					<FieldLabel htmlFor="new-password">新密码</FieldLabel>
					<Input
						id="new-password"
						type="password"
						autoComplete="new-password"
						value={newPassword}
						onChange={(e) => setNewPassword(e.target.value)}
						minLength={15}
						maxLength={128}
						required
						autoFocus
					/>
				</Field>
				<Field>
					<FieldLabel htmlFor="confirm-password">再次输入新密码</FieldLabel>
					<Input
						id="confirm-password"
						type="password"
						autoComplete="new-password"
						value={confirmation}
						onChange={(e) => setConfirmation(e.target.value)}
						minLength={15}
						maxLength={128}
						required
					/>
				</Field>
				<Button type="submit" disabled={saving}>
					{saving ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							保存中…
						</>
					) : (
						"保存并继续"
					)}
				</Button>
			</FieldGroup>
		</form>
	);
}

function ContactPane({
	onFlow,
	onFlowGone,
}: {
	onFlow: (flow: AuthFlow) => void;
	onFlowGone: () => void;
}) {
	const [channel, setChannel] = useState<AuthContactChannel | null>(null);
	const [target, setTarget] = useState("");
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (!channel) return;
		setError("");
		setSaving(true);
		try {
			await authFlowApi.addContact(channel, target);
			onFlow(await authFlowApi.resume());
		} catch (reason) {
			if (isFlowGone(reason)) {
				onFlowGone();
				return;
			}
			setError(messageOf(reason, "没有登记成功，请重试。"));
		} finally {
			setSaving(false);
			setTarget("");
		}
	}
	if (!channel)
		return (
			<FieldGroup>
				<h1 className="text-center text-2xl font-semibold">选择验证方式</h1>
				<Button
					variant="outline"
					className="h-14"
					onClick={() => setChannel("email")}
				>
					<MailIcon data-icon="inline-start" />
					邮箱验证码
				</Button>
				<Button
					variant="outline"
					className="h-14"
					onClick={() => setChannel("sms")}
				>
					<MessageSquareIcon data-icon="inline-start" />
					短信验证码
				</Button>
			</FieldGroup>
		);
	return (
		<form
			className="flex flex-col gap-6 motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-right-2"
			onSubmit={submit}
		>
			<FieldGroup>
				<h1 className="text-center text-2xl font-semibold">
					{channel === "email" ? "绑定邮箱" : "绑定手机号"}
				</h1>
				{error && <ErrorMessage>{error}</ErrorMessage>}
				<Field>
					<FieldLabel htmlFor="target">
						{channel === "email" ? "邮箱地址" : "手机号"}
					</FieldLabel>
					<Input
						id="target"
						type={channel === "email" ? "email" : "tel"}
						autoComplete={channel === "email" ? "email" : "tel"}
						value={target}
						onChange={(e) => setTarget(e.target.value)}
						required
						autoFocus
					/>
				</Field>
				<Button type="submit" disabled={saving}>
					{saving ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							保存中…
						</>
					) : (
						"保存联系方式"
					)}
				</Button>
				<Button
					type="button"
					variant="ghost"
					disabled={saving}
					onClick={() => {
						setChannel(null);
						setTarget("");
						setError("");
					}}
				>
					选择其他方式
				</Button>
			</FieldGroup>
		</form>
	);
}

function OtpPane({
	flow,
	onFlow,
	onAuthenticated,
	onFlowGone,
}: {
	flow: AuthFlow;
	onFlow: (flow: AuthFlow) => void;
	onAuthenticated: (user: UserSummary) => void;
	onFlowGone: () => void;
}) {
	const [contactId, setContactId] = useState("");
	const [code, setCode] = useState("");
	const [sentContactId, setSentContactId] = useState<string>();
	const [cooldown, setCooldown] = useState(0);
	const [error, setError] = useState("");
	const [sending, setSending] = useState(false);
	const [verifying, setVerifying] = useState(false);
	const contact = flow.contacts.find((candidate) => candidate.id === contactId);
	const cooling = cooldown > 0;
	useEffect(() => {
		if (!cooling) return;
		const timer = window.setInterval(
			() => setCooldown((remaining) => Math.max(0, remaining - 1)),
			1000,
		);
		return () => window.clearInterval(timer);
	}, [cooling]);
	function selectContact(nextId: string) {
		// Returning to the chooser must not discard an already sent challenge or its cooldown.
		setContactId(nextId);
		setCode("");
	}
	async function send() {
		if (!contact || sending) return;
		setError("");
		setSending(true);
		try {
			await authFlowApi.sendChallenge(contact.id);
			setSentContactId(contact.id);
			setCode("");
			setCooldown(RESEND_SECONDS);
		} catch (reason) {
			if (isFlowGone(reason)) {
				onFlowGone();
				return;
			}
			setError(messageOf(reason, "验证码发送失败，请稍后重试。"));
		} finally {
			setSending(false);
		}
	}
	async function submit(event: FormEvent) {
		event.preventDefault();
		if (!contact || verifying || code.length !== 6) return;
		setError("");
		setVerifying(true);
		try {
			const result = await authFlowApi.verify(code);
			if (result.completed && result.user) {
				onAuthenticated(result.user);
				return;
			}
			// Initialization flows only mark the contact verified; re-read the
			// flow to land on the next step (or the finish step).
			onFlow(await authFlowApi.resume());
		} catch (reason) {
			if (isFlowGone(reason)) {
				onFlowGone();
				return;
			}
			// A spent or wrong code is just invalid_code (422): the message
			// explains it, resending stays available through the normal cooldown.
			setError(messageOf(reason, "验证没有通过，请重试。"));
		} finally {
			setVerifying(false);
		}
	}
	if (flow.contacts.length === 0) {
		return (
			<FieldGroup>
				<h1 className="text-2xl font-bold">输入验证码</h1>
				<ErrorMessage>没有可用的验证方式，请重新登录。</ErrorMessage>
				<Button variant="ghost" type="button" onClick={onFlowGone}>
					返回登录
				</Button>
			</FieldGroup>
		);
	}
	if (!contact)
		return (
			<FieldGroup>
				<div className="flex flex-col gap-2 text-center">
					<h1 className="text-2xl font-semibold">二次验证</h1>
					<p className="text-sm text-muted-foreground">选择验证码接收方式</p>
				</div>
				{(["email", "sms"] as const).map((channel) => {
					const contacts = flow.contacts.filter(
						(item) => item.channel === channel,
					);
					const Icon = channel === "email" ? MailIcon : MessageSquareIcon;
					return contacts.length ? (
						contacts.map((candidate) => (
							<Button
								key={candidate.id}
								variant="outline"
								className="h-14 justify-between"
								onClick={() => selectContact(candidate.id)}
							>
								<span className="flex items-center gap-2">
									<Icon data-icon="inline-start" />
									{channelLabel(channel)}验证码
								</span>
								<span className="min-w-0 truncate">
									{candidate.maskedTarget}
								</span>
							</Button>
						))
					) : (
						<Button
							key={channel}
							variant="outline"
							className="h-14 justify-between"
							disabled
						>
							<span className="flex items-center gap-2">
								<Icon data-icon="inline-start" />
								{channelLabel(channel)}验证码
							</span>
							<span>未绑定</span>
						</Button>
					);
				})}
			</FieldGroup>
		);
	const challengeSent = sentContactId === contact.id;
	return (
		<form
			className="flex flex-col gap-6 motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-right-2"
			onSubmit={submit}
		>
			<FieldGroup>
				<div className="flex flex-col items-center gap-2 text-center">
					<h1 className="text-2xl font-semibold">
						{challengeSent ? "输入验证码" : "接收验证码"}
					</h1>
					<p className="text-sm text-muted-foreground">
						{contact.maskedTarget}
					</p>
				</div>
				{error && <ErrorMessage>{error}</ErrorMessage>}
				{challengeSent && (
					<>
						<Field>
							<FieldLabel htmlFor="otp" className="sr-only">
								验证码
							</FieldLabel>
							<Input
								id="otp"
								inputMode="numeric"
								autoComplete="one-time-code"
								pattern="[0-9]{6}"
								maxLength={6}
								placeholder="六位验证码"
								value={code}
								onChange={(e) => setCode(e.target.value.replace(/[^0-9]/g, ""))}
								disabled={verifying}
								autoFocus
								className="h-12 text-center"
							/>
						</Field>
						<Button type="submit" disabled={verifying || code.length !== 6}>
							{verifying ? (
								<>
									<LoaderCircle
										className="animate-spin"
										data-icon="inline-start"
										aria-hidden="true"
									/>
									验证中…
								</>
							) : (
								"验证并继续"
							)}
						</Button>
					</>
				)}
				<Button
					type="button"
					variant={challengeSent ? "outline" : "default"}
					onClick={() => void send()}
					disabled={sending || cooling || verifying}
				>
					{sending ? (
						<>
							<LoaderCircle
								className="animate-spin"
								data-icon="inline-start"
								aria-hidden="true"
							/>
							发送中…
						</>
					) : !challengeSent ? (
						"发送验证码"
					) : cooling ? (
						`重新发送（${cooldown}s）`
					) : (
						"重新发送"
					)}
				</Button>
				<Button
					type="button"
					variant="ghost"
					disabled={sending || verifying}
					onClick={() => {
						selectContact("");
						setError("");
					}}
				>
					选择其他方式
				</Button>
			</FieldGroup>
		</form>
	);
}

function FinishPane({
	flow,
	onCompleted,
	onFlowGone,
}: {
	flow: AuthFlow;
	onCompleted: () => void;
	onFlowGone: () => void;
}) {
	const [error, setError] = useState("");
	const [completing, setCompleting] = useState(false);
	async function complete() {
		setError("");
		setCompleting(true);
		try {
			await authFlowApi.complete();
			onCompleted();
		} catch (reason) {
			if (isFlowGone(reason)) {
				onFlowGone();
				return;
			}
			setError(messageOf(reason, "没有完成初始化，请重试。"));
		} finally {
			setCompleting(false);
		}
	}
	const targets = flow.contacts
		.map(
			(contact) => `${channelLabel(contact.channel)} ${contact.maskedTarget}`,
		)
		.join("、");
	return (
		<FieldGroup>
			<div className="flex flex-col items-center gap-1 text-center">
				<h1 className="text-2xl font-bold">准备完成初始化</h1>
				<p className="text-sm text-balance text-muted-foreground">
					密码与联系方式（{targets}）已就绪。
				</p>
			</div>
			{error && <ErrorMessage>{error}</ErrorMessage>}
			<Button onClick={() => void complete()} disabled={completing}>
				{completing ? (
					<>
						<LoaderCircle
							className="animate-spin"
							data-icon="inline-start"
							aria-hidden="true"
						/>
						完成中…
					</>
				) : (
					"完成初始化"
				)}
			</Button>
		</FieldGroup>
	);
}

export function AuthScreen({
	onAuthenticated,
}: {
	onAuthenticated: (user: UserSummary) => void;
}) {
	const [phase, setPhase] = useState<"resuming" | "ready">("resuming");
	const [flow, setFlow] = useState<AuthFlow | null>(null);
	const [note, setNote] = useState("");
	const [deliveryOpen, setDeliveryOpen] = useState(false);
	// A page refresh must land back in the running flow instead of the login form.
	useEffect(() => {
		let cancelled = false;
		authFlowApi.resume().then(
			(resumed) => {
				if (!cancelled) {
					setFlow(resumed);
					setPhase("ready");
				}
			},
			() => {
				// No active flow (or the server is unreachable) — show the login form.
				if (!cancelled) setPhase("ready");
			},
		);
		return () => {
			cancelled = true;
		};
	}, []);
	const resetToLogin = useCallback((message?: string) => {
		setFlow(null);
		setDeliveryOpen(false);
		if (message) setNote(message);
	}, []);
	const showFlow = useCallback((next: AuthFlow) => {
		setNote("");
		setFlow(next);
	}, []);
	const flowGone = useCallback(
		() => resetToLogin("认证流程已失效，请重新登录。"),
		[resetToLogin],
	);
	const afterPassword = useCallback(
		(next: AuthFlow) => {
			// First-run (and recovering) admins configure verification-code
			// delivery before registering contacts; without a channel no
			// challenge can be sent.
			setDeliveryOpen(isAdministratorFlow(next.type));
			showFlow(next);
		},
		[showFlow],
	);
	const openDelivery = useCallback(() => setDeliveryOpen(true), []);
	const pane = paneOf(flow);
	const adminFlow = flow !== null && isAdministratorFlow(flow.type);
	const deliveryOverride =
		flow &&
		adminFlow &&
		deliveryOpen &&
		(pane === "contact" || pane === "otp" || pane === "finish");
	// Every admin-init step keeps a delivery-settings entry: the post-password
	// overlay is transient, so after a refresh during contact/OTP this is the
	// only route back to a missing channel configuration. Closing the overlay
	// returns to the same flow-derived pane, so resuming stays safe.
	const deliveryEntry =
		adminFlow &&
		!deliveryOverride &&
		(pane === "contact" || pane === "otp" || pane === "finish");
	return (
		<div className={cn("min-h-svh", !flow && "grid lg:grid-cols-2")}>
			<div
				className={cn(
					"flex flex-col gap-4 p-6 md:p-10",
					flow && "mx-auto min-h-svh w-full max-w-lg pt-16 md:pt-20",
				)}
			>
				<div className={cn("flex justify-center", !flow && "md:justify-start")}>
					<BrandLockup className="h-7" />
				</div>
				<main
					className={cn(
						"flex justify-center",
						flow ? "pt-8" : "flex-1 items-center",
					)}
				>
					<div
						key={deliveryOverride ? "delivery" : pane}
						className={cn(
							"w-full motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-bottom-2 motion-safe:duration-300",
							flow ? "max-w-sm" : "max-w-xs",
						)}
					>
						{phase === "resuming" ? (
							<div
								className="flex flex-col items-center gap-3"
								role="status"
								aria-label="正在恢复认证状态"
							>
								<Skeleton className="h-7 w-2/3" />
								<Skeleton className="h-9 w-full" />
								<Skeleton className="h-9 w-full" />
								<Skeleton className="h-9 w-1/2" />
							</div>
						) : (
							<>
								{pane === "login" && (
									<LoginPane note={note} onFlow={showFlow} />
								)}
								{pane === "password" && flow && (
									<PasswordPane
										flow={flow}
										onFlow={afterPassword}
										onFlowGone={flowGone}
									/>
								)}
								{deliveryOverride && (
									<DeliveryPane
										onDone={() => setDeliveryOpen(false)}
										onFlowGone={flowGone}
									/>
								)}
								{pane === "contact" && !deliveryOverride && (
									<ContactPane onFlow={showFlow} onFlowGone={flowGone} />
								)}
								{pane === "otp" && flow && !deliveryOverride && (
									<OtpPane
										flow={flow}
										onFlow={showFlow}
										onAuthenticated={onAuthenticated}
										onFlowGone={flowGone}
									/>
								)}
								{pane === "finish" && flow && !deliveryOverride && (
									<FinishPane
										flow={flow}
										onCompleted={() =>
											resetToLogin("初始化完成，请使用你的用户名和新密码登录。")
										}
										onFlowGone={flowGone}
									/>
								)}
								{deliveryEntry && (
									<Button
										className="mt-6 w-full"
										variant="ghost"
										type="button"
										onClick={openDelivery}
									>
										验证码投递设置
									</Button>
								)}
							</>
						)}
					</div>
				</main>
			</div>
			{!flow && <AuthBrandPanel />}
		</div>
	);
}
