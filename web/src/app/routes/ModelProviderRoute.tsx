import { useModelProviderModule } from "@/features/admin/ui/ConnectionsModule";
import type { RouteHostProps } from "./RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/** Settings exposes model providers as its own configuration page and route. */
export default function ModelProviderRoute({ props, onLogout }: RouteHostProps) {
	const view = useModelProviderModule(props);
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
