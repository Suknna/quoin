import { useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import { AuditPage } from "@/features/audit/ui";
import { parseRoute } from "@/lib/parse-route";
import { SettingsNavigation, settingsNavGroups } from "../nav";
import { About } from "../platform/about/About";
import { Backups } from "../platform/backups/Backups";
import { ModelProviderPage } from "../platform/model-providers/ModelProviderModule";
import { Users } from "../platform/users/Users";
import { Profile } from "../profile/Profile";
import { Security } from "../security/Security";

/** Settings consolidates the personal account and the former /admin module.
 * Per docs/audit-design.md §6 the personal account keeps no audit entry — the
 * consolidated audit log lives under 平台 → 审计. */
export function useSettingsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const [updatedUser, setUpdatedUser] = useState<UserSummary>();
	const user = updatedUser?.id === props.user.id ? updatedUser : props.user;
	const pathname = parseRoute(props.route).pathname;
	const list = (
		<SettingsNavigation
			groups={settingsNavGroups}
			route={props.route}
			user={props.user}
			navigate={props.navigate}
		/>
	);
	const platform = (
		content: React.ReactNode,
		title: string,
	): WorkspaceModuleView => ({ title, list, content });
	if (pathname.startsWith("/settings/security")) {
		return {
			title: "安全",
			list,
			content: (
				<Security suspended={props.suspended} onUserChanged={setUpdatedUser} />
			),
		};
	}
	if (pathname.startsWith("/settings/platform/users")) {
		return platform(<Users suspended={props.suspended} />, "用户");
	}
	if (pathname.startsWith("/settings/platform/backups")) {
		return platform(<Backups suspended={props.suspended} />, "备份与保留");
	}
	if (pathname.startsWith("/settings/platform/audit")) {
		return platform(<AuditPage suspended={props.suspended} />, "审计");
	}
	if (pathname.startsWith("/settings/platform/about")) {
		return platform(<About suspended={props.suspended} />, "平台状态");
	}
	if (pathname.startsWith("/settings/platform/model-providers")) {
		const providersBase = "/settings/platform/model-providers";
		// 详情是列表页上的抽屉，只有新建提供方保留面包屑页。
		const creatingProvider = pathname === `${providersBase}/new`;
		return {
			title: "模型提供方",
			list,
			crumbs: creatingProvider
				? [
						{ label: "模型提供方", to: providersBase },
						{ label: "新建模型提供方" },
					]
				: undefined,
			content: <ModelProviderPage {...props} />,
		};
	}
	const unknown = pathname !== "/settings" && pathname !== "/settings/profile";
	return {
		title: "个人资料",
		list,
		content: unknown ? (
			<Empty>
				<EmptyHeader>
					<EmptyTitle>找不到此页面</EmptyTitle>
					<EmptyDescription>该链接无效或页面已被移动。</EmptyDescription>
				</EmptyHeader>
			</Empty>
		) : (
			<Profile user={user} suspended={props.suspended} />
		),
	};
}
