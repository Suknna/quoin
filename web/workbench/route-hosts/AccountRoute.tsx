import { useAccountModule } from "../modules/account";
import type { RouteHostProps } from "../RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the account hook in the feature chunk and mounted only for account routes. */
export default function AccountRoute({ props, onLogout }: RouteHostProps) {
	const view = useAccountModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
