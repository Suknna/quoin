import type { ReactNode } from "react";
import type { UserSummary } from "@/api/generated/types";

export interface WorkspaceModuleProps {
	user: UserSummary;
	route: string;
	navigate: (route: string) => void;
	/** True while platform maintenance blocks mutations; session expiry unmounts the workspace instead. */
	suspended: boolean;
	maintenanceActive?: boolean;
	openEvidence: (id: string) => void;
}

export interface WorkspaceModuleView {
	title: string;
	list: ReactNode;
	content: ReactNode;
	actions?: ReactNode;
	/**
	 * 内容区占满视口剩余高度并关闭页面滚动,由模块内部自行滚动(对话页:
	 * 消息流内部滚动,输入框始终钉在底部)。默认走窗口文档流滚动。
	 */
	fullHeight?: boolean;
	/**
	 * Drill-down trail for pages below a module root. The last entry is the
	 * current page (rendered without a link); earlier entries navigate.
	 */
	crumbs?: { label: string; to?: string }[];
}

/**
 * Each feature exports one hook that projects its route into the coordinator's
 * sidebar-09 shell. Feature modules never create a competing application shell.
 */
export type WorkspaceModule = (
	props: WorkspaceModuleProps,
) => WorkspaceModuleView;

/** Feature hooks follow this contract; implementations live in their modules. */
export type KnowledgeModule = WorkspaceModule;
export type AdministrationModule = WorkspaceModule;
export type AccountModule = WorkspaceModule;
