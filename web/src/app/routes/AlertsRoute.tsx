import { useAlertsModule } from "@/features/alerts/ui";
import type { RouteHostProps } from "./RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps the alerts hook in the feature chunk and mounted only for alerts routes. */
export default function AlertsRoute({ props, onLogout }: RouteHostProps) {
	const view = useAlertsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
