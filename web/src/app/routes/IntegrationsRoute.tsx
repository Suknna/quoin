import { useIntegrationsModule } from "@/features/integrations/ui";
import type { RouteHostProps } from "./RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Keeps platform-management state in its own lazy route chunk. */
export default function IntegrationsRoute({ props, onLogout }: RouteHostProps) {
	const view = useIntegrationsModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
