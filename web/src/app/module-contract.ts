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
