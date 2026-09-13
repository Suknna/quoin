import { FeatureUnderConstruction } from "@/components/FeatureUnderConstruction";
import type { RouteHostProps } from "./RouteHosts";
import { WorkspaceShell } from "../WorkspaceShell";

/**
 * Knowledge data remains preserved server-side, but its operational workbench
 * is intentionally unavailable during this demo scope, including deep links.
 */
export default function KnowledgeRoute({ props, onLogout }: RouteHostProps) {
	const view = {
		title: "知识",
		list: null,
		content: <FeatureUnderConstruction title="知识库开发中" description="知识检索、导入和候选整理尚未开放；已存储的数据不会被删除或修改。" />,
	};
	return <WorkspaceShell user={props.user} route={props.route} view={view} navigate={props.navigate} onLogout={onLogout} suspended={props.suspended} />;
}
