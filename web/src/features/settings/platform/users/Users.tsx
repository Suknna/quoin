import { useCallback, useEffect, useState } from "react";
import { KeyRound, LogOut, Mail, MessageSquare, UserCheck, UserX } from "lucide-react";
import { notify } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import {
	Item,
	ItemActions,
	ItemContent,
	ItemDescription,
	ItemGroup,
	ItemMedia,
	ItemTitle,
} from "@/components/ui/item";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { DataTable } from "@/components/workbench/DataTable";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { channelLabels, roleLabels } from "@/features/settings/labels";
import { formatDateTime } from "@/lib/format";
import {
	type AdminContact,
	type AdminContactInput,
	type AdminUser,
	configureContacts,
	createUser,
	listUserContacts,
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

/** Keeps the wire shape at 0..2 structured targets; a blank input drops its channel. */
function collectContacts(draft: ContactDraft): AdminContactInput[] {
	const contacts: AdminContactInput[] = [];
	if (draft.email.trim())
		contacts.push({ channel: "email", target: draft.email.trim() });
	if (draft.sms.trim())
		contacts.push({ channel: "sms", target: draft.sms.trim() });
	return contacts;
}

/** 唯一管理员模型（docs/authentication-design.md §1）：这里创建的账户一律是操作员。
 * 联系方式自 ADR-0010 起（OTP 退役）仅作展示、可不配置：新操作员只凭临时密码登录
 * 并完成强制改密；管理员自身的渠道同样仅展示，可在「个人资料」查看。 */
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
	const [viewing, setViewing] = useState<AdminUser>();
	/** 保存联系方式后递增，让打开的抽屉重取掩码渠道。 */
	const [contactsVersion, setContactsVersion] = useState(0);
	const [replaceContacts, setReplaceContacts] = useState(false);
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
			const users = await listUsers();
			setItems(users);
			// 抽屉打开期间后台状态被刷新时，同步到最新行版本。
			setViewing((current) =>
				current ? users.find((user) => user.id === current.id) : current,
			);
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
		if (!configuring || busy || !replaceContacts) return;
		setBusy(true);
		try {
			await configureContacts(
				configuring.id,
				configuring.rowVersion,
				configuredContacts,
			);
			notify.success("已保存联系方式");
			setConfiguring(undefined);
			setContactDraft(emptyContactDraft);
			setContactsVersion((value) => value + 1);
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法配置联系方式。");
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
			<div className="flex items-start justify-between gap-3">
				<div>
					<h2 className="text-xl font-semibold">用户管理</h2>
					<p className="text-sm text-muted-foreground">
						操作员账户在此创建与维护；管理员由系统初始化产生，不能在此创建或晋升。
					</p>
				</div>
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
					{ label: "来源" },
					{ label: "状态" },
					{ label: "初始化" },
				]}
				loading={loading}
				loadingLabel="正在读取用户"
				emptyTitle="尚无用户"
			>
				{items.map((user) => (
					<TableRow
						key={user.id}
						className="cursor-pointer"
						onClick={() => setViewing(user)}
					>
						<TableCell className="font-medium">{user.displayName}</TableCell>
						<TableCell>{user.username}</TableCell>
						<TableCell>
							<Badge variant={user.role === "admin" ? "default" : "secondary"}>
								{roleLabels[user.role]}
							</Badge>
						</TableCell>
						<TableCell>
							{user.authSource === "oidc" ? "统一身份" : "本地"}
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
					</TableRow>
				))}
			</DataTable>
			{viewing && (
				<UserSheet
					user={viewing}
					suspended={suspended}
					contactsVersion={contactsVersion}
					onClose={() => setViewing(undefined)}
					onToggleEnabled={() => void update(viewing, { enabled: !viewing.enabled })}
					onConfigureContacts={() => {
						setConfiguring(viewing);
						setReplaceContacts(false);
						setContactDraft(emptyContactDraft);
					}}
					onResetPassword={() => setResetting(viewing)}
					onRevokeSessions={() =>
						void revokeSessions(viewing.id)
							.then(() => {
								notify.success("已撤销会话");
								return load();
							})
							.catch((reason) =>
								notify.error(reason, "暂时无法撤销会话。"),
							)
					}
				/>
			)}
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
								draft.password.length < 15
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
						<DialogTitle>配置联系方式</DialogTitle>
						<DialogDescription>
							{configuring?.displayName}
							：此处是全量替换表单，不回显现有联系方式；空白不代表尚未配置。请填写所有需要保留的渠道，留空的渠道会被移除，全部留空将清除全部联系方式。联系方式仅作展示，不用于验证或登录。
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
								邮箱联系方式
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
							<FieldLabel htmlFor="contact-sms-target">短信联系方式</FieldLabel>
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
						<Field orientation="horizontal">
							<Checkbox
								id="confirm-contact-replacement"
								checked={replaceContacts}
								onCheckedChange={(checked) =>
									setReplaceContacts(checked === true)
								}
							/>
							<FieldLabel htmlFor="confirm-contact-replacement">
								我确认以本表单替换全部现有联系方式
							</FieldLabel>
						</Field>
						<Button
							type="submit"
							disabled={suspended || busy || !replaceContacts}
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
				<FieldLabel htmlFor="create-email-target">邮箱联系方式</FieldLabel>
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
					可选，仅作展示，不用于验证或登录；如填写须为合法邮箱。
				</FieldDescription>
			</Field>
			<Field>
				<FieldLabel htmlFor="create-sms-target">短信联系方式</FieldLabel>
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

/** 用户详情抽屉：顶部一行事实徽章，联系方式为可读渠道列表（掩码），
 * 账户操作收为带图标的紧凑设置行。 */
function UserSheet({
	user,
	suspended,
	contactsVersion,
	onClose,
	onToggleEnabled,
	onConfigureContacts,
	onResetPassword,
	onRevokeSessions,
}: {
	user: AdminUser;
	suspended: boolean;
	contactsVersion: number;
	onClose: () => void;
	onToggleEnabled: () => void;
	onConfigureContacts: () => void;
	onResetPassword: () => void;
	onRevokeSessions: () => void;
}) {
	const [contacts, setContacts] = useState<AdminContact[]>();
	const [contactsError, setContactsError] = useState("");
	const loadContacts = useCallback(() => {
		listUserContacts(user.id).then(
			(items) => {
				setContacts(items);
				setContactsError("");
			},
			(reason) => {
				setContacts(undefined);
				setContactsError(messageOf(reason, "暂时无法读取联系方式。"));
			},
		);
	}, [user.id]);
	useEffect(() => {
		loadContacts();
	}, [loadContacts, contactsVersion]);

	const canConfigureContacts = user.role === "operator" && user.authSource !== "oidc";
	return (
		<DetailSheet
			open
			onClose={onClose}
			title={user.displayName}
			description={`@${user.username}`}
			size="narrow"
		>
			<div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
				{/* 一行事实徽章替代属性网格：扫一眼即知账户状态。 */}
				<div className="flex flex-wrap items-center gap-2 text-sm">
					<Badge variant={user.role === "admin" ? "default" : "secondary"}>
						{roleLabels[user.role]}
					</Badge>
					<Badge variant="outline">
						{user.authSource === "oidc" ? "统一身份" : "本地"}
					</Badge>
					<Badge variant={user.enabled ? "secondary" : "outline"}>
						{user.enabled ? "已启用" : "已停用"}
					</Badge>
					{user.initialized !== undefined && (
						<Badge variant={user.initialized ? "secondary" : "outline"}>
							{user.initialized ? "已初始化" : "未初始化"}
						</Badge>
					)}
					<span className="text-xs text-muted-foreground tabular-nums">
						上次登录 {formatDateTime(user.lastLoginAt, "从未")}
					</span>
				</div>
				<Separator />
				<section className="flex flex-col gap-3">
					<div className="flex items-center justify-between gap-2">
						<h3 className="text-sm font-medium">联系方式</h3>
						{canConfigureContacts && (
							<Button
								size="sm"
								variant="outline"
								disabled={suspended}
								onClick={onConfigureContacts}
							>
								配置渠道
							</Button>
						)}
					</div>
					{contactsError && (
						<Alert variant="destructive">
							<AlertDescription>
								{contactsError}{" "}
								<Button
									variant="link"
									className="h-auto p-0 align-baseline"
									onClick={loadContacts}
								>
									重试
								</Button>
							</AlertDescription>
						</Alert>
					)}
					{contacts === undefined && !contactsError ? (
						<div
							className="flex flex-col gap-2"
							role="status"
							aria-label="正在读取联系方式"
						>
							<Skeleton className="h-12 w-full" />
						</div>
					) : contacts && contacts.length > 0 ? (
						<ItemGroup>
							{contacts.map((contact) => (
								<Item key={contact.id} size="sm" className="px-0">
									<ItemMedia variant="icon">
										{contact.channel === "email" ? (
											<Mail aria-hidden="true" />
										) : (
											<MessageSquare aria-hidden="true" />
										)}
									</ItemMedia>
									<ItemContent>
										<ItemTitle>{contact.maskedTarget}</ItemTitle>
										<ItemDescription>
											{channelLabels[contact.channel]}
										</ItemDescription>
									</ItemContent>
									<ItemActions>
										{contact.verified ? (
											<Badge>已验证</Badge>
										) : (
											<Badge variant="outline">待验证</Badge>
										)}
									</ItemActions>
								</Item>
							))}
						</ItemGroup>
					) : (
						!contactsError && (
							<p className="text-sm text-muted-foreground">
								尚未配置联系方式。
							</p>
						)
					)}
					{user.role === "admin" && (
						<p className="text-xs text-muted-foreground">
							管理员联系方式仅作展示，可在「设置 → 账户与安全」查看。
						</p>
					)}
				</section>
				<Separator />
				<section className="flex flex-col gap-3">
					<h3 className="text-sm font-medium">账户操作</h3>
					<ItemGroup>
						{user.role === "operator" && (
							<Item size="sm" className="px-0">
								<ItemMedia variant="icon">
									{user.enabled ? (
										<UserX aria-hidden="true" />
									) : (
										<UserCheck aria-hidden="true" />
									)}
								</ItemMedia>
								<ItemContent>
									<ItemTitle>
										{user.enabled ? "停用账户" : "启用账户"}
									</ItemTitle>
								</ItemContent>
								<ItemActions>
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
										onConfirm={onToggleEnabled}
									>
										{user.enabled ? "停用" : "启用"}
									</ConfirmAction>
								</ItemActions>
							</Item>
						)}
						{user.authSource === "oidc" ? (
							// External accounts have no local password by construction.
							<p className="text-sm text-muted-foreground">
								密码由统一身份平台管理
							</p>
						) : (
							user.role === "operator" && (
								// Only local operators: the backend deliberately rejects
								// admin password reset here for the admin itself.
								<Item size="sm" className="px-0">
									<ItemMedia variant="icon">
										<KeyRound aria-hidden="true" />
									</ItemMedia>
									<ItemContent>
										<ItemTitle>重置密码</ItemTitle>
									</ItemContent>
									<ItemActions>
										<Button
											size="sm"
											variant="outline"
											disabled={suspended}
											onClick={onResetPassword}
										>
											重置密码
										</Button>
									</ItemActions>
								</Item>
							)
						)}
						<Item size="sm" className="px-0">
							<ItemMedia variant="icon">
								<LogOut aria-hidden="true" />
							</ItemMedia>
							<ItemContent>
								<ItemTitle>撤销会话</ItemTitle>
							</ItemContent>
							<ItemActions>
								<ConfirmAction
									title="撤销该用户的所有会话？"
									description="该用户需要重新登录后才能继续使用工作台。"
									disabled={suspended}
									onConfirm={onRevokeSessions}
								>
									撤销会话
								</ConfirmAction>
							</ItemActions>
						</Item>
					</ItemGroup>
				</section>
			</div>
		</DetailSheet>
	);
}
