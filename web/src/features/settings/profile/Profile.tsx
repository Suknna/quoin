import { Mail, MessageSquare } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { listOwnContacts, type OwnContact } from "./api";
import { channelLabels, roleLabels } from "@/features/settings/labels";
import { PasswordSection, Sessions } from "../security/Security";

/** 联系方式是用户资料的一部分：任何登录用户可见自己的掩码渠道。自助更换
 * 流程已随 OTP 退役（ADR-0010）：管理员的渠道在用户管理页维护，操作员的
 * 渠道同样由管理员维护。明文目标永不出服务器。 */
function ContactSection({ user }: { user: UserSummary }) {
	const [contacts, setContacts] = useState<OwnContact[]>();
	const [error, setError] = useState("");
	const load = useCallback(() => {
		listOwnContacts().then(
			(items) => {
				setContacts(items);
				setError("");
			},
			(reason) => {
				setContacts(undefined);
				setError(
					reason instanceof Error
						? reason.message
						: "暂时无法读取联系方式，请重试。",
				);
			},
		);
	}, []);
	useEffect(() => {
		load();
	}, [load]);

	return (
		<section className="space-y-3">
			<h3 className="text-sm font-medium">联系方式</h3>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>
						{error}{" "}
						<Button
							variant="link"
							className="h-auto p-0 align-baseline"
							onClick={load}
						>
							重试
						</Button>
					</AlertDescription>
				</Alert>
			)}
			{contacts === undefined && !error ? (
				<div
					className="flex flex-col gap-2"
					role="status"
					aria-label="正在读取联系方式"
				>
					<Skeleton className="h-12 w-full" />
				</div>
			) : (
				<>
					{contacts?.length ? (
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
						<p className="text-sm text-muted-foreground">尚未配置联系方式。</p>
					)}
					<p className="text-xs text-muted-foreground">
						联系方式仅作展示，不用于验证或登录；由管理员在用户管理页维护，如需变更请联系
						{user.role === "admin" ? "其他管理员或在用户管理页操作" : "管理员"}。
					</p>
				</>
			)}
		</section>
	);
}

/** 账户与安全合并了原「个人资料」与「安全」两页：资料与联系方式由管理员
 * 维护，密码与登录设备由本人管理；旧路径 /settings/security 指向同一页面。 */
export function Profile({
	user,
	suspended,
	onUserChanged,
	onLogout,
}: {
	user: UserSummary;
	suspended: boolean;
	onUserChanged: (user: UserSummary) => void;
	onLogout?: () => Promise<void> | void;
}) {
	return (
		<section className="space-y-8">
			<div className="space-y-1">
				<h2 className="text-xl font-semibold">账户与安全</h2>
				<p className="text-sm text-muted-foreground">
					账户资料由管理员维护；密码与登录设备由本人管理。
				</p>
			</div>
			{/* 身份信息是只读事实，直接展示为文本——readOnly 输入框会暗示可编辑。 */}
			<div className="flex flex-col gap-1">
				<div className="flex items-center gap-2">
					<span className="text-lg font-semibold">{user.displayName}</span>
					<Badge>{roleLabels[user.role]}</Badge>
				</div>
				<p className="text-sm text-muted-foreground">@{user.username}</p>
			</div>
			<ContactSection user={user} />
			<Separator />
			<PasswordSection onChanged={onUserChanged} />
			<Separator />
			<Sessions suspended={suspended} onLogout={onLogout} />
		</section>
	);
}
