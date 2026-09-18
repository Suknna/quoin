import { useSettingsModule } from "@/features/settings/ui";
import { WorkspaceShell } from "../WorkspaceShell";
import type { RouteHostProps } from "./RouteHosts";

/** Keeps the settings hook in the feature chunk and mounted only for settings routes. */
export default function SettingsRoute({ props, onLogout }: RouteHostProps) {
	const view = useSettingsModule(props);
	return (
		<WorkspaceShell
			user={props.user}
			route={props.route}
			view={view}
			navigate={props.navigate}
			onLogout={onLogout}
			suspended={props.suspended}
		/>
	);
}
