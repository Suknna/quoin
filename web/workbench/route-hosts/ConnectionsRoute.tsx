import { useConnectionsModule } from "../ConnectionsModule";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the connections hook in the feature chunk and mounted only for its routes. */
export default function ConnectionsRoute({ props, onLogout }: RouteHostProps) {
	const view = useConnectionsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
