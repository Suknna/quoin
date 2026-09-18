import { useState } from "react";
import type { UserSummary } from "@/api/generated/types";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { AuditPage } from "@/features/audit/ui";
import { About } from "../platform/about/About";
import { Backups } from "../platform/backups/Backups";
import { ModelProviderPage } from "../platform/model-providers/ModelProviderModule";
import { Runtimes } from "../platform/runtimes/Runtimes";
import { Users } from "../platform/users/Users";
import { Profile } from "../profile/Profile";
import { Security } from "../security/Security";
import { SettingsNavigation, settingsNavGroups } from "../nav";

/** Settings consolidates the personal account and the former /admin module.
 * Per docs/audit-design.md §6 the personal account keeps no audit entry — the
 * consolidated audit log lives under 平台 → 审计. */
export function useSettingsModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const [updatedUser, setUpdatedUser] = useState<UserSummary>();
	const user = updatedUser?.id === props.user.id ? updatedUser : props.user;
	const pathname = new URL(props.route, "https://workbench.invalid").pathname;
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
	if (pathname.startsWith("/settings/platform/runtime")) {
		return platform(<Runtimes suspended={props.suspended} />, "运行时");
	}
	if (pathname.startsWith("/settings/platform/model-providers")) {
		return platform(<ModelProviderPage {...props} />, "模型提供方");
	}
	const unknown = pathname !== "/settings" && pathname !== "/settings/profile";
	return {
		title: "个人资料",
		list,
		content: unknown ? (
			<Alert variant="destructive">
				<AlertDescription>未找到此设置页面。</AlertDescription>
			</Alert>
		) : (
			<Profile user={user} suspended={props.suspended} />
		),
	};
}
