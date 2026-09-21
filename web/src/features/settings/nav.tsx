/* eslint-disable react-refresh/only-export-components -- Navigation groups are data colocated with their renderer. */

import type { UserSummary } from "@/api/generated/types";
import { Button } from "@/components/ui/button";
import { parseRoute } from "@/lib/parse-route";

export type SettingsNavEntry = {
	key: string;
	label: string;
	route: string;
	adminOnly?: boolean;
};

/** /settings 根路径渲染个人资料内容，与个人资料入口共用高亮。 */
const profileRoute = "/settings/profile";

export type SettingsNavGroup = {
	label: string;
	entries: SettingsNavEntry[];
};

export const personalGroup: SettingsNavGroup = {
	label: "个人",
	entries: [
		{ key: "profile", label: "个人资料", route: profileRoute },
		{ key: "security", label: "安全", route: "/settings/security" },
	],
};

export const platformGroup: SettingsNavGroup = {
	label: "平台",
	entries: [
		{
			key: "integrations",
			label: "平台接入",
			route: "/settings/platform/integrations",
			adminOnly: true,
		},
		{
			key: "enrichment-rules",
			label: "富化规则",
			route: "/settings/platform/enrichment-rules",
			adminOnly: true,
		},
		{
			key: "model-providers",
			label: "模型提供方",
			route: "/settings/platform/model-providers",
			adminOnly: true,
		},
		{
			key: "users",
			label: "用户",
			route: "/settings/platform/users",
			adminOnly: true,
		},
		{
			key: "auth-config",
			label: "认证配置",
			route: "/settings/platform/auth-config",
			adminOnly: true,
		},
		{
			key: "backups",
			label: "备份与保留",
			route: "/settings/platform/backups",
			adminOnly: true,
		},
		{
			key: "audit",
			label: "审计",
			route: "/settings/platform/audit",
			adminOnly: true,
		},
		{
			key: "about",
			label: "平台状态",
			route: "/settings/platform/about",
			adminOnly: true,
		},
	],
};

export const settingsNavGroups: readonly SettingsNavGroup[] = [
	personalGroup,
	platformGroup,
];

/** One navigation component for every settings page with a persistent active
 * state, rendered as a single flat column like the operations module nav.
 * Shared by the settings module and the platform access (integrations)
 * module so that surface never becomes a dead end. */
export function SettingsNavigation({
	groups,
	route,
	user,
	navigate,
}: {
	groups: readonly SettingsNavGroup[];
	route: string;
	user: UserSummary;
	navigate: (route: string) => void;
}) {
	const pathname = parseRoute(route).pathname;
	const visible = groups.flatMap((group) =>
		group.entries.filter((entry) => !entry.adminOnly || user.role === "admin"),
	);
	return (
		<nav className="flex flex-col gap-1 p-3" aria-label="设置模块">
			{visible.map((entry) => {
				const active =
					pathname === entry.route ||
					(entry.route === profileRoute && pathname === "/settings") ||
					(entry.route !== "/settings" &&
						pathname.startsWith(`${entry.route}/`));
				return (
					<Button
						key={entry.route}
						variant={active ? "secondary" : "ghost"}
						size="sm"
						className="w-full justify-start"
						aria-current={active ? "page" : undefined}
						onClick={() => navigate(entry.route)}
					>
						{entry.label}
					</Button>
				);
			})}
		</nav>
	);
}
