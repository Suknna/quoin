import { useInspectionsModule } from "../modules/inspections";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the inspections hook in the feature chunk and mounted only for its routes. */
export default function InspectionsRoute({ props, onLogout }: RouteHostProps) {
	const view = useInspectionsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
