import { formatDateTime } from "@/lib/format";
import { LoaderCircle } from "lucide-react";
import { type FormEvent, useEffect, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import {
	newClientCommandId,
	notifyUnauthorized,
	workbenchApi,
} from "@/api/workbench";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { messageOf } from "@/app/shared";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";

type Session = {
	id: string;
	clientLabel: string;
	createdAt: string;
	lastActiveAt: string;
	idleExpiresAt: string;
	absoluteExpiresAt: string;
	current: boolean;
};
type Page<T> = { items?: T[]; nextCursor?: string };


const formatTime = (value: string | null) =>
	formatDateTime(value, "从未");

/** Security requests use the shared unauthorized recovery rather than rendering a misleading local error. */
async function securityRequest<T>(
	path: string,
	init?: RequestInit,
): Promise<T> {
	const response = await fetch(path, {
		credentials: "include",
		headers: init?.body
			? { "Content-Type": "application/json", ...init.headers }
			: init?.headers,
		...init,
	});
	if (!response.ok) {
		let detail = "暂时无法完成操作，请重试。";
		try {
			const body = (await response.json()) as {
				detail?: string;
				message?: string;
			};
			detail = body.detail ?? body.message ?? detail;
		} catch {
			/* Gateway responses may not be JSON. */
		}
		if (response.status === 401) notifyUnauthorized();
		throw new Error(detail);
	}
	if (response.status === 204) return undefined as T;
	return (await response.json()) as T;
}

function PasswordForm({
	onChanged,
}: {
	onChanged: (user: UserSummary) => void;
}) {
	const [currentPassword, setCurrentPassword] = useState("");
	const [newPassword, setNewPassword] = useState("");
	const [confirmation, setConfirmation] = useState("");
	const [error, setError] = useState("");
	const [success, setSuccess] = useState("");
	const [saving, setSaving] = useState(false);
	async function submit(event: FormEvent) {
		event.preventDefault();
		setError("");
		setSuccess("");
		if (newPassword !== confirmation) {
			setError("两次输入的新密码不一致。");
			return;
		}
		setSaving(true);
		try {
			await workbenchApi.changePassword({ currentPassword, newPassword });
			// The server is authoritative for the cleared password-change requirement and auth revision.
			onChanged(await workbenchApi.currentUser());
			setSuccess("密码已更新。其他已登录设备不会自动退出。");
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSaving(false);
			setCurrentPassword("");
			setNewPassword("");
			setConfirmation("");
		}
	}
	return (
		<form className="space-y-4" onSubmit={submit}>
			<div>
				<h2 className="text-xl font-semibold">修改密码</h2>
				<p className="text-sm text-muted-foreground">
					修改密码不会撤销其他登录设备。
				</p>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{success && (
				<Alert>
					<AlertDescription>{success}</AlertDescription>
				</Alert>
			)}
			<Field>
				<FieldLabel htmlFor="settings-current-password">当前密码</FieldLabel>
				<Input
					id="settings-current-password"
					type="password"
					autoComplete="current-password"
					value={currentPassword}
					onChange={(event) => setCurrentPassword(event.target.value)}
					minLength={15}
					maxLength={128}
					required
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="settings-new-password">新密码</FieldLabel>
				<Input
					id="settings-new-password"
					type="password"
					autoComplete="new-password"
					value={newPassword}
					onChange={(event) => setNewPassword(event.target.value)}
					minLength={15}
					maxLength={128}
					required
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="settings-confirm-password">
					再次输入新密码
				</FieldLabel>
				<Input
					id="settings-confirm-password"
					type="password"
					autoComplete="new-password"
					value={confirmation}
					onChange={(event) => setConfirmation(event.target.value)}
					minLength={15}
					maxLength={128}
					required
				/>
			</Field>
			<FieldDescription>
				使用 15–128 个字符。密码只保存在本次输入中，提交后立即清除。
			</FieldDescription>
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
					"更新密码"
				)}
			</Button>
		</form>
	);
}

function Sessions({ suspended }: { suspended: boolean }) {
	const [sessions, setSessions] = useState<Session[]>([]);
	const [cursor, setCursor] = useState<string>();
	const [error, setError] = useState("");
	const [pending, setPending] = useState<Session>();
	const [busy, setBusy] = useState(false);
	const [sessionsLoading, setSessionsLoading] = useState(true);
	async function load(nextCursor?: string, append = false) {
		setError("");
		setSessionsLoading(true);
		try {
			const page = await securityRequest<Page<Session>>(
				`/api/v1/auth/sessions?limit=50${nextCursor ? `&cursor=${encodeURIComponent(nextCursor)}` : ""}`,
			);
			setSessions((previous) =>
				append ? [...previous, ...(page.items ?? [])] : (page.items ?? []),
			);
			setCursor(page.nextCursor);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setSessionsLoading(false);
		}
	}
	useEffect(() => {
		void load();
	}, []);
	async function revoke() {
		if (!pending) return;
		setBusy(true);
		setError("");
		try {
			await securityRequest<void>(
				`/api/v1/auth/sessions/${encodeURIComponent(pending.id)}/revoke`,
				{
					method: "POST",
					body: JSON.stringify({ clientCommandId: newClientCommandId() }),
				},
			);
			if (pending.current) {
				window.location.reload();
				return;
			}
			setPending(undefined);
			await load();
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setBusy(false);
		}
	}
	return (
		<section className="space-y-4">
			<div>
				<h2 className="text-xl font-semibold">我的会话</h2>
				<p className="text-sm text-muted-foreground">
					显示仍有效的登录设备及其服务端记录的活动时间。
				</p>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			<Table>
				<TableHeader>
					<TableRow>
						<TableHead>客户端</TableHead>
						<TableHead>创建时间</TableHead>
						<TableHead>最后活动</TableHead>
						<TableHead>过期时间</TableHead>
						<TableHead>
							<span className="sr-only">操作</span>
						</TableHead>
					</TableRow>
				</TableHeader>
				<TableBody>
					{sessionsLoading ? (
						<TableRow>
							<TableCell colSpan={5}>
								<DetailSkeleton label="正在读取登录设备" rows={["line", "line", "line"]} />
							</TableCell>
						</TableRow>
					) : sessions.length === 0 ? (
						<TableRow>
							<TableCell colSpan={5} className="text-center text-muted-foreground">
								没有登录设备记录。
							</TableCell>
						</TableRow>
					) : (
						sessions.map((session) => (
						<TableRow key={session.id}>
							<TableCell>
								{session.clientLabel}{" "}
								{session.current && (
									<Badge className="ml-2" variant="secondary">
										当前
									</Badge>
								)}
							</TableCell>
							<TableCell>{formatTime(session.createdAt)}</TableCell>
							<TableCell>{formatTime(session.lastActiveAt)}</TableCell>
							<TableCell>{formatTime(session.idleExpiresAt)}</TableCell>
							<TableCell>
								<Button
									variant="outline"
									size="sm"
									disabled={suspended}
									onClick={() => setPending(session)}
								>
									撤销
								</Button>
							</TableCell>
							</TableRow>
						))
					)}
				</TableBody>
			</Table>
			<LoadMoreButton
				loading={sessionsLoading}
				hasMore={Boolean(cursor)}
				onLoadMore={() => void load(cursor, true)}
			/>
			<AlertDialog
				open={Boolean(pending)}
				onOpenChange={(open) => {
					if (!open && !busy) setPending(undefined);
				}}
			>
				<AlertDialogContent>
					<AlertDialogHeader>
						<AlertDialogTitle>撤销此会话？</AlertDialogTitle>
						<AlertDialogDescription>
							{pending?.current
								? "这是当前设备。确认后将清除本设备认证并重新加载登录页面。"
								: "该设备将需要重新登录；其他设备保持不变。"}
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
						<AlertDialogAction
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void revoke();
							}}
						>
							确认撤销
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}

/** Security owns the personal password and session surfaces; logout remains solely in the shell. */
export function Security({
	suspended,
	onUserChanged,
}: {
	suspended: boolean;
	onUserChanged: (user: UserSummary) => void;
}) {
	return (
		<div className="space-y-8">
			<PasswordForm onChanged={onUserChanged} />
			<Sessions suspended={suspended} />
		</div>
	);
}
