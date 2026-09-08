import { useKnowledgeModule } from "../modules/knowledge";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the knowledge hook in the feature chunk and mounted only for knowledge routes. */
export default function KnowledgeRoute({ props, onLogout }: RouteHostProps) {
	const view = useKnowledgeModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
