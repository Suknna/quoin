import { useKnowledgeModule } from "@/features/knowledge/ui";
import { WorkspaceShell } from "../WorkspaceShell";
import type { RouteHostProps } from "./RouteHosts";

/** Keeps knowledge workbench state in its own lazy route chunk. */
export default function KnowledgeRoute({ props, onLogout }: RouteHostProps) {
	const view = useKnowledgeModule(props);
	return (
		<WorkspaceShell
			user={props.user}
			route={props.route}
			view={view}
			navigate={props.navigate}
			onLogout={onLogout}
			suspended={props.suspended}
		/>
	);
}
