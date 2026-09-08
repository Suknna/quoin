import { useAdministrationModule } from "../modules/administration";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the administration hook in the feature chunk and mounted only for admin routes. */
export default function AdministrationRoute({ props, onLogout }: RouteHostProps) {
	const view = useAdministrationModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
