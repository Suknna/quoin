import { useInvestigationsModule } from "../modules/investigations";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the investigations hook in the feature chunk and mounted only for its routes. */
export default function InvestigationsRoute({ props, onLogout }: RouteHostProps) {
	const view = useInvestigationsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
