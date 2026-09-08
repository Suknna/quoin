import { useSystemsModule } from "../modules/systems";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the business-systems hook in the feature chunk and mounted only for its routes. */
export default function SystemsRoute({ props, onLogout }: RouteHostProps) {
	const view = useSystemsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
