import { useCursorPages } from '@/hooks/use-cursor-pages';
import { LoaderCircle, Monitor } from "lucide-react";
import { type FormEvent, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import {
	newClientCommandId,
	notifyUnauthorized,
	workbenchApi,
} from "@/api/workbench";
import { messageOf, notify } from "@/app/shared";
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
import { Button } from "@/components/ui/button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
	Item,
	ItemActions,
	ItemContent,
	ItemDescription,
	ItemGroup,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import { Skeleton } from "@/components/ui/skeleton";
import { CursorPagination } from "@/components/workbench/CursorPagination";
import { formatDateTime } from "@/lib/format";

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

const formatTime = (value: string | null) => formatDateTime(value, "从未");

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

export function PasswordSection({
	onChanged,
}: {
	onChanged: (user: UserSummary) => void;
}) {
	const [changing, setChanging] = useState(false);
	return (
		<section className="space-y-3">
			<h3 className="text-sm font-medium">密码</h3>
			<ItemGroup>
				<Item size="sm" className="px-0">
					<ItemContent>
						<ItemTitle>登录密码</ItemTitle>
					</ItemContent>
					<ItemActions>
						<Button
							variant="outline"
							size="sm"
							onClick={() => setChanging(true)}
						>
							修改密码
						</Button>
					</ItemActions>
				</Item>
			</ItemGroup>
			{changing && (
				<PasswordDialog
					onChanged={onChanged}
					onClose={() => setChanging(false)}
				/>
			)}
		</section>
	);
}

/**
 * 修改密码是对话框内的低频操作：不再在页面上常驻三个等待输入的密码框。
 * 成功后关闭对话框并 toast 确认；失败只 toast，输入立即清除。
 */
function PasswordDialog({
	onChanged,
	onClose,
}: {
	onChanged: (user: UserSummary) => void;
	onClose: () => void;
}) {
	const [currentPassword, setCurrentPassword] = useState("");
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
			await workbenchApi.changePassword({ currentPassword, newPassword });
			// The server is authoritative for the cleared password-change requirement and auth revision.
			onChanged(await workbenchApi.currentUser());
			notify.success("密码已更新。其他已登录设备不会自动退出。");
			onClose();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
			setSaving(false);
			setCurrentPassword("");
			setNewPassword("");
			setConfirmation("");
		}
	}
	return (
		<Dialog
			open
			onOpenChange={(open) => {
				if (!open && !saving) onClose();
			}}
		>
			<DialogContent>
				<DialogHeader>
					<DialogTitle>修改密码</DialogTitle>
					<DialogDescription>
						密码只保存在本次输入中，提交后立即清除。
					</DialogDescription>
				</DialogHeader>
				<form className="flex flex-col gap-4" onSubmit={submit}>
					{error && (
						<Alert variant="destructive">
							<AlertDescription>{error}</AlertDescription>
						</Alert>
					)}
					<Field>
						<FieldLabel htmlFor="settings-current-password">
							当前密码
						</FieldLabel>
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
					<DialogFooter>
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
					</DialogFooter>
				</form>
			</DialogContent>
		</Dialog>
	);
}

export function Sessions({
	suspended,
	onLogout,
}: {
	suspended: boolean;
	/** 外壳的退出登录路径；提供时当前设备行直接给出「退出登录」按钮。 */
	onLogout?: () => Promise<void> | void;
}) {
	const list = useCursorPages<Session>(
		(cursor) =>
			securityRequest<Page<Session>>(
				`/api/v1/auth/sessions?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
			).then((page) => ({
				items: page.items ?? [],
				nextCursor: page.nextCursor,
			})),
		{ fallbackError: "暂时无法完成操作，请重试。" },
	);
	const sessions = list.items;
	const sessionsLoading = list.loading;
	const [revokeError, setRevokeError] = useState("");
	const [pending, setPending] = useState<Session>();
	const [busy, setBusy] = useState(false);
	async function revoke() {
		if (!pending) return;
		setBusy(true);
		setRevokeError("");
		try {
			await securityRequest<void>(
				`/api/v1/auth/sessions/${encodeURIComponent(pending.id)}/revoke`,
				{
					method: "POST",
					body: JSON.stringify({ clientCommandId: newClientCommandId() }),
				},
			);
			notify.success("已撤销会话");
			setPending(undefined);
			list.refresh();
		} catch (reason) {
			// 失败时确认框保持打开并把原因钉在框内，可重试或取消。
			setRevokeError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setBusy(false);
		}
	}
	return (
		<section className="space-y-3">
			<div>
				<h3 className="text-sm font-medium">登录设备</h3>
				<p className="text-sm text-muted-foreground">
					仍有效的登录设备及其服务端记录的活动时间。
				</p>
			</div>
			{list.error && (
				<Alert variant="destructive">
					<AlertDescription>{list.error}</AlertDescription>
				</Alert>
			)}
			{sessionsLoading && sessions.length === 0 ? (
				<div
					className="flex flex-col gap-2"
					role="status"
					aria-label="正在读取登录设备"
				>
					<Skeleton className="h-12 w-full" />
					<Skeleton className="h-12 w-full" />
				</div>
			) : sessions.length === 0 ? (
				<p className="text-sm text-muted-foreground">没有登录设备记录。</p>
			) : (
				<ItemGroup>
					{sessions.map((session) => (
						<Item key={session.id} size="sm" className="px-0">
							<ItemMedia variant="icon">
								<Monitor aria-hidden="true" />
							</ItemMedia>
							<ItemContent>
								<ItemTitle>
									{session.clientLabel}
									{session.current && (
										<Badge className="ml-2" variant="secondary">
											当前
										</Badge>
									)}
								</ItemTitle>
								<ItemDescription>
									最后活动 {formatTime(session.lastActiveAt)} · 空闲至{" "}
									{formatTime(session.idleExpiresAt)}
								</ItemDescription>
							</ItemContent>
							<ItemActions>
								{/* HTTP-AUTH-004：revokeOwnSession 只能撤销其他会话；当前
								 * 会话的正确结束路径是 logout（与外壳右上角菜单同一条），
								 * 因此当前行给出真实的退出登录按钮而不是必然失败的撤销。
								 */}
								{session.current ? (
									onLogout ? (
										<Button
											variant="ghost"
											size="sm"
											onClick={() => {
												Promise.resolve(onLogout()).catch((reason) =>
													notify.error(reason, "退出登录失败，请重试。"),
												);
											}}
										>
											退出登录
										</Button>
									) : (
										<span className="text-xs text-muted-foreground">
											本设备请用「退出登录」
										</span>
									)
								) : (
									<Button
										variant="outline"
										size="sm"
										disabled={suspended}
										onClick={() => {
											setRevokeError("");
											setPending(session);
										}}
									>
										撤销
									</Button>
								)}
							</ItemActions>
						</Item>
					))}
				</ItemGroup>
			)}
			<CursorPagination
				page={list.page}
				hasPrev={list.hasPrev}
				hasNext={list.hasNext}
				loading={list.navigating}
				onPrev={list.goPrev}
				onNext={list.goNext}
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
							该设备将需要重新登录；其他设备保持不变。
						</AlertDialogDescription>
					</AlertDialogHeader>
					{revokeError && (
						<Alert variant="destructive">
							<AlertDescription>{revokeError}</AlertDescription>
						</Alert>
					)}
					<AlertDialogFooter>
						<AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
						<AlertDialogAction
							disabled={busy}
							onClick={(event) => {
								event.preventDefault();
								void revoke();
							}}
						>
							{busy ? "撤销中…" : "确认撤销"}
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
		</section>
	);
}

/** 原独立「安全」页已并入「账户与安全」（settings/profile/Profile.tsx）——
 * PasswordForm 与 Sessions 作为分节被组合复用。 */
