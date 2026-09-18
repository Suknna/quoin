import { useEffect, useState } from "react";
import { notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { TableCell, TableRow } from "@/components/ui/table";
import { DataTable } from "@/components/workbench/DataTable";
import { roleLabels } from "@/features/settings/labels";
import {
	type AdminContactInput,
	type AdminUser,
	configureContacts,
	createUser,
	listUsers,
	messageOf,
	resetPassword,
	revokeSessions,
	updateUser,
} from "@/features/settings/platform/users/api";
import { ConfirmAction } from "../controls";

interface ContactDraft {
	email: string;
	sms: string;
}

const emptyContactDraft: ContactDraft = { email: "", sms: "" };

/** Keeps the wire shape at 1..2 structured targets; a blank input drops its channel. */
function collectContacts(draft: ContactDraft): AdminContactInput[] {
	const contacts: AdminContactInput[] = [];
	if (draft.email.trim())
		contacts.push({ channel: "email", target: draft.email.trim() });
	if (draft.sms.trim())
		contacts.push({ channel: "sms", target: draft.sms.trim() });
	return contacts;
}

/** 唯一管理员模型（docs/authentication-design.md §1）：这里创建的账户一律是操作员，
 * 收码目标由管理员指定且操作员不能自行更换；管理员自身渠道只能经认证流程变更，
 * 因此管理员的行不提供停用或渠道编辑入口。 */
export function Users({ suspended }: { suspended: boolean }) {
	const [items, setItems] = useState<AdminUser[]>([]);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
	/** One-shot creation feedback; survives the dialog closing. */
	const [note, setNote] = useState("");
	const [creating, setCreating] = useState(false);
	const [busy, setBusy] = useState(false);
	const [resetting, setResetting] = useState<AdminUser>();
	const [configuring, setConfiguring] = useState<AdminUser>();
	const empty: {
		username: string;
		displayName: string;
		password: string;
	} & ContactDraft = {
		username: "",
		displayName: "",
		password: "",
		...emptyContactDraft,
	};
	const [draft, setDraft] = useState(empty);
	const [contactDraft, setContactDraft] =
		useState<ContactDraft>(emptyContactDraft);
	const [temporaryPassword, setTemporaryPassword] = useState("");
	const createContacts = collectContacts(draft);
	const configuredContacts = collectContacts(contactDraft);

	const load = async () => {
		try {
			setItems(await listUsers());
		} catch (reason) {
			setError(messageOf(reason, "暂时无法读取用户。"));
		} finally {
			setLoading(false);
		}
	};
	useEffect(() => {
		void load();
	}, []);
	async function create() {
		if (busy) return;
		setBusy(true);
		try {
			await createUser({
				username: draft.username,
				displayName: draft.displayName,
				password: draft.password,
				contacts: createContacts,
			});
			// Close and clear BEFORE the list refresh: a slow or failing reload
			// must never leave the operator staring at an emptied, still-open form.
			setCreating(false);
			setDraft(empty);
			setNote("操作员已创建，请把临时密码交付本人；它不会再次显示。");
			await load();
		} catch (reason) {
			setError(messageOf(reason, "暂时无法创建用户。"));
		} finally {
			setBusy(false);
			setDraft((value) => ({ ...value, password: "" }));
		}
	}
	async function update(
		user: AdminUser,
		changes: Parameters<typeof updateUser>[2],
	) {
		try {
			await updateUser(user.id, user.rowVersion, changes);
			notify.success(changes.enabled ? "已启用" : "已停用");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法更新用户。");
		}
	}
	async function saveContacts() {
		if (!configuring || busy) return;
		setBusy(true);
		try {
			await configureContacts(
				configuring.id,
				configuring.rowVersion,
				configuredContacts,
			);
			notify.success("已保存验证渠道");
			setConfiguring(undefined);
			setContactDraft(emptyContactDraft);
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法配置验证渠道。");
		} finally {
			setBusy(false);
		}
	}
	async function reset() {
		if (!resetting || busy) return;
		setBusy(true);
		try {
			await resetPassword(
				resetting.id,
				resetting.rowVersion,
				temporaryPassword,
			);
			notify.success("已重置密码");
			setResetting(undefined);
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法重置密码。");
		} finally {
			setBusy(false);
			setTemporaryPassword("");
		}
	}
	return (
		<section className="space-y-4">
			<div className="flex justify-between">
				<h2 className="text-xl font-semibold">用户</h2>
				<Button
					disabled={suspended}
					onClick={() => {
						setNote("");
						setCreating(true);
					}}
				>
					新建操作员
				</Button>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{note && (
				<Alert>
					<AlertDescription role="status">{note}</AlertDescription>
				</Alert>
			)}
			<DataTable
				columns={[
					{ label: "显示名" },
					{ label: "用户名" },
					{ label: "角色" },
					{ label: "状态" },
					{ label: "初始化" },
					{ label: "操作" },
				]}
				loading={loading}
				loadingLabel="正在读取用户"
				emptyTitle="尚无用户"
			>
				{items.map((user) => (
					<TableRow key={user.id}>
						<TableCell className="font-medium">{user.displayName}</TableCell>
						<TableCell>{user.username}</TableCell>
						<TableCell>
							<Badge variant={user.role === "admin" ? "default" : "secondary"}>
								{roleLabels[user.role]}
							</Badge>
						</TableCell>
						<TableCell
							className={user.enabled ? undefined : "text-muted-foreground"}
						>
							{user.enabled ? "已启用" : "已停用"}
						</TableCell>
						<TableCell
							className={user.initialized ? undefined : "text-muted-foreground"}
						>
							{user.initialized !== undefined
								? user.initialized
									? "已初始化"
									: "未初始化"
								: "—"}
						</TableCell>
						<TableCell className="whitespace-normal">
							<div className="flex flex-wrap items-center gap-1.5">
								{user.role === "operator" && (
									<ConfirmAction
										title={
											user.enabled
												? `停用 ${user.displayName}？`
												: `启用 ${user.displayName}？`
										}
										description={
											user.enabled
												? "停用后该操作员将立即无法登录工作台。"
												: "启用后该操作员可以立即登录工作台。"
										}
										destructive={user.enabled}
										disabled={suspended}
										onConfirm={() =>
											void update(user, { enabled: !user.enabled })
										}
									>
										{user.enabled ? "停用" : "启用"}
									</ConfirmAction>
								)}
								{user.role === "operator" ? (
									<Button
										size="sm"
										variant="outline"
										disabled={suspended}
										onClick={() => {
											setConfiguring(user);
											setContactDraft(emptyContactDraft);
										}}
									>
										配置渠道
									</Button>
								) : (
									<small className="inline-block max-w-60 whitespace-normal align-middle text-muted-foreground">
										管理员收码渠道请在「设置 →
										个人资料」中通过密码与验证码流程更换，此处不提供编辑。
									</small>
								)}
								{user.role === "operator" && (
									// Only operators: the backend deliberately rejects admin
									// password reset here (account self-service or CLI only),
									// so the button must never offer an inevitable error.
									<Button
										size="sm"
										variant="outline"
										disabled={suspended}
										onClick={() => setResetting(user)}
									>
										重置密码
									</Button>
								)}
								<ConfirmAction
									title="撤销该用户的所有会话？"
									description="该用户需要重新登录后才能继续使用工作台。"
									disabled={suspended}
									onConfirm={() =>
										void revokeSessions(user.id)
											.then(() => {
												notify.success("已撤销会话");
												return load();
											})
											.catch((reason) =>
												notify.error(reason, "暂时无法撤销会话。"),
											)
									}
								>
									撤销会话
								</ConfirmAction>
							</div>
						</TableCell>
					</TableRow>
				))}
			</DataTable>
			<Dialog open={creating} onOpenChange={setCreating}>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>新建操作员</DialogTitle>
						<DialogDescription>
							账户以操作员角色创建；管理员由系统初始化产生，不能在此创建或晋升。临时密码仅随这次提交发送，界面不会保存或回显。
						</DialogDescription>
					</DialogHeader>
					<form
						onSubmit={(event) => {
							event.preventDefault();
							void create();
						}}
						className="flex flex-col gap-4"
					>
						<UserFields draft={draft} setDraft={setDraft} />
						<Button
							type="submit"
							disabled={
								suspended ||
								busy ||
								!draft.username ||
								!draft.displayName ||
								draft.password.length < 15 ||
								createContacts.length === 0
							}
						>
							{busy ? "创建中…" : "创建操作员"}
						</Button>
					</form>
				</DialogContent>
			</Dialog>
			<Dialog
				open={Boolean(configuring)}
				onOpenChange={(open) => {
					if (!open) {
						setConfiguring(undefined);
						setContactDraft(emptyContactDraft);
					}
				}}
			>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>配置验证渠道</DialogTitle>
						<DialogDescription>
							{configuring?.displayName}
							：替换目标会清除该渠道的已验证状态并使绑定旧目标的验证码失效；留空表示移除该渠道，至少保留一个。
						</DialogDescription>
					</DialogHeader>
					<form
						onSubmit={(event) => {
							event.preventDefault();
							void saveContacts();
						}}
						className="flex flex-col gap-4"
					>
						<Field>
							<FieldLabel htmlFor="contact-email-target">
								邮箱收码目标
							</FieldLabel>
							<Input
								id="contact-email-target"
								type="email"
								value={contactDraft.email}
								onChange={(event) =>
									setContactDraft((value) => ({
										...value,
										email: event.target.value,
									}))
								}
							/>
						</Field>
						<Field>
							<FieldLabel htmlFor="contact-sms-target">短信收码目标</FieldLabel>
							<Input
								id="contact-sms-target"
								type="tel"
								value={contactDraft.sms}
								onChange={(event) =>
									setContactDraft((value) => ({
										...value,
										sms: event.target.value,
									}))
								}
							/>
						</Field>
						<Button
							type="submit"
							disabled={suspended || busy || configuredContacts.length === 0}
						>
							{busy ? "保存中…" : "保存渠道"}
						</Button>
					</form>
				</DialogContent>
			</Dialog>
			<Dialog
				open={Boolean(resetting)}
				onOpenChange={(open) => {
					if (!open) {
						setResetting(undefined);
						setTemporaryPassword("");
					}
				}}
			>
				<DialogContent>
					<DialogHeader>
						<DialogTitle>重置密码</DialogTitle>
						<DialogDescription>
							临时密码只在本次提交中使用，成功后会撤销现有会话。
						</DialogDescription>
					</DialogHeader>
					<form
						onSubmit={(event) => {
							event.preventDefault();
							void reset();
						}}
						className="flex flex-col gap-4"
					>
						<Field>
							<FieldLabel>新临时密码</FieldLabel>
							<Input
								type="password"
								autoComplete="new-password"
								value={temporaryPassword}
								onChange={(event) => setTemporaryPassword(event.target.value)}
							/>
						</Field>
						<Button
							type="submit"
							disabled={suspended || busy || temporaryPassword.length < 15}
						>
							{busy ? "重置中…" : "重置并撤销会话"}
						</Button>
					</form>
				</DialogContent>
			</Dialog>
		</section>
	);
}

function UserFields({
	draft,
	setDraft,
}: {
	draft: {
		username: string;
		displayName: string;
		password: string;
	} & ContactDraft;
	setDraft: React.Dispatch<
		React.SetStateAction<
			{ username: string; displayName: string; password: string } & ContactDraft
		>
	>;
}) {
	return (
		<>
			<Field>
				<FieldLabel htmlFor="create-username">用户名</FieldLabel>
				<Input
					id="create-username"
					value={draft.username}
					onChange={(event) =>
						setDraft((value) => ({ ...value, username: event.target.value }))
					}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="create-display-name">显示名</FieldLabel>
				<Input
					id="create-display-name"
					value={draft.displayName}
					onChange={(event) =>
						setDraft((value) => ({ ...value, displayName: event.target.value }))
					}
				/>
			</Field>
			<Field>
				<FieldLabel htmlFor="create-password">临时密码</FieldLabel>
				<Input
					id="create-password"
					type="password"
					autoComplete="new-password"
					value={draft.password}
					onChange={(event) =>
						setDraft((value) => ({ ...value, password: event.target.value }))
					}
				/>
				<FieldDescription>至少 15 个字符。</FieldDescription>
			</Field>
			<Field>
				<FieldLabel htmlFor="create-email-target">邮箱收码目标</FieldLabel>
				<Input
					id="create-email-target"
					type="email"
					placeholder="name@example.com"
					value={draft.email}
					onChange={(event) =>
						setDraft((value) => ({ ...value, email: event.target.value }))
					}
				/>
				<FieldDescription>
					邮箱与短信至少配置一种；操作员初始化时须验证其中之一，之后不能自行更换。
				</FieldDescription>
			</Field>
			<Field>
				<FieldLabel htmlFor="create-sms-target">短信收码目标</FieldLabel>
				<Input
					id="create-sms-target"
					type="tel"
					placeholder="+8613800000000"
					value={draft.sms}
					onChange={(event) =>
						setDraft((value) => ({ ...value, sms: event.target.value }))
					}
				/>
			</Field>
		</>
	);
}
