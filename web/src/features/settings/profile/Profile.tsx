import { formatDateTime } from "@/lib/format";
import { useCallback, useEffect, useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { listOwnContacts, type OwnContact } from "./api";
import { channelLabels, roleLabels } from "@/features/settings/labels";

const formatTime = (value: string | null) =>
	formatDateTime(value, "从未");

function ContactRow({ contact }: { contact: OwnContact }) {
	return (
		<div className="flex items-center gap-2 text-sm">
			<span className="text-muted-foreground">
				{channelLabels[contact.channel]}
			</span>
			<span className="font-medium">{contact.maskedTarget}</span>
			{contact.verified ? (
				<Badge>已验证</Badge>
			) : (
				<Badge variant="outline">待验证</Badge>
			)}
		</div>
	);
}

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
					<Skeleton className="h-5 w-56" />
					<Skeleton className="h-5 w-44" />
				</div>
			) : (
				<div className="space-y-2">
					{(contacts ?? []).map((contact) => (
						<ContactRow key={contact.id} contact={contact} />
					))}
					{contacts?.length === 0 && (
						<p className="text-sm text-muted-foreground">尚未配置联系方式。</p>
					)}
					<p className="text-sm text-muted-foreground">
						联系方式仅作展示，不用于验证或登录；由管理员在用户管理页维护，如需变更请联系
						{user.role === "admin" ? "其他管理员或在用户管理页操作" : "管理员"}。
					</p>
				</div>
			)}
		</section>
	);
}

/** 基本资料由管理员维护（与后端一致），页面同时呈现上次登录与联系方式。 */
export function Profile({ user }: { user: UserSummary }) {
	return (
		<section className="space-y-6">
			<div>
				<h2 className="text-xl font-semibold">个人资料</h2>
				<p className="text-sm text-muted-foreground">账户资料由管理员维护。</p>
			</div>
			<div className="space-y-5">
				<Field>
					<FieldLabel htmlFor="settings-display-name">显示名称</FieldLabel>
					<Input id="settings-display-name" value={user.displayName} readOnly />
				</Field>
				<Field>
					<FieldLabel htmlFor="settings-username">用户名</FieldLabel>
					<Input id="settings-username" value={user.username} readOnly />
				</Field>
				<Field>
					<FieldLabel>角色</FieldLabel>
					<div>
						<Badge>{roleLabels[user.role]}</Badge>
					</div>
				</Field>
				<Field>
					<FieldLabel>上次登录</FieldLabel>
					<p className="text-sm text-muted-foreground">
						{formatTime(user.lastLoginAt)}
					</p>
				</Field>
			</div>
			<ContactSection user={user} />
		</section>
	);
}
