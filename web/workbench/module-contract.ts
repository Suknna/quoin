import type { ReactNode } from "react";
import type { UserSummary } from "../src/api/generated/types";

export interface WorkspaceModuleProps {
	user: UserSummary;
	route: string;
	navigate: (route: string) => void;
	/** True while the authenticated workspace must not issue mutations, including maintenance. */
	suspended: boolean;
	/** Maintenance repair commands remain available only with a valid authenticated session. */
	authenticationSuspended?: boolean;
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
