import { BookOpen, FileInput, Inbox } from "lucide-react";
import { useState } from "react";
import type {
	WorkspaceModuleProps,
	WorkspaceModuleView,
} from "@/app/module-contract";
import { Button } from "@/components/ui/button";
import { DetailSheet } from "@/components/workbench/DetailSheet";
import { parseRoute } from "@/lib/parse-route";
import { BrowsePage } from "./BrowsePage";
import { CandidateEditor } from "./CandidateEditor";
import { CandidatesPage } from "./CandidatesPage";
import { ImportBatchDetailPage, ImportBatchNew } from "./ImportBatchPage";
import { ImportsPage } from "./ImportsPage";
import { KnowledgeItemSheet } from "./ItemSheet";

type Section = "browse" | "candidates" | "imports";

const sections: Array<{
	key: Section;
	label: string;
	icon: typeof BookOpen;
	route: string;
}> = [
	{ key: "browse", label: "知识库", icon: BookOpen, route: "/knowledge" },
	{
		key: "candidates",
		label: "待确认",
		icon: Inbox,
		route: "/knowledge/candidates",
	},
	{
		key: "imports",
		label: "导入批次",
		icon: FileInput,
		route: "/knowledge/imports",
	},
];

/**
 * 知识模块:第二栏是纯栏目导航,阅读与操作都在主内容区;
 * 知识详情是 query 驱动的抽屉,候选编辑与批次处理是面包屑页。
 */
export function useKnowledgeModule(
	props: WorkspaceModuleProps,
): WorkspaceModuleView {
	const parsed = parseRoute(props.route);
	const parts = parsed.pathname
		.split("/")
		.filter(Boolean)
		.filter((part, index) => !(index === 0 && part === "knowledge"));
	const candidateId = parts[0] === "candidates" ? parts[1] : undefined;
	const importId = parts[0] === "imports" ? parts[1] : undefined;
	const knowledgeId = parsed.searchParams.get("item") || undefined;
	// 抽屉标题跟随已加载的知识;换条目时由 key 重挂载并重置。
	const [itemTitle, setItemTitle] = useState<string>();
	const section: Section =
		parts[0] === "candidates"
			? "candidates"
			: parts[0] === "imports"
				? "imports"
				: "browse";

	const list = (
		<nav className="flex flex-col gap-1 p-3" aria-label="知识库栏目">
			{sections.map((item) => (
				<Button
					key={item.key}
					className="w-full justify-start"
					variant={section === item.key ? "secondary" : "ghost"}
					aria-current={section === item.key ? "page" : undefined}
					onClick={() => props.navigate(item.route)}
				>
					<item.icon data-icon="inline-start" />
					{item.label}
				</Button>
			))}
		</nav>
	);

	let content: React.ReactNode;
	let crumbs: WorkspaceModuleView["crumbs"];
	if (candidateId) {
		content = (
			<CandidateEditor
				id={candidateId}
				suspended={props.suspended}
				navigate={props.navigate}
			/>
		);
		crumbs = [
			{ label: "待确认", to: "/knowledge/candidates" },
			{ label: "编辑候选" },
		];
	} else if (importId === "new") {
		content = (
			<ImportBatchNew suspended={props.suspended} navigate={props.navigate} />
		);
		crumbs = [
			{ label: "导入批次", to: "/knowledge/imports" },
			{ label: "导入原文" },
		];
	} else if (importId) {
		content = (
			<ImportBatchDetailPage
				id={importId}
				suspended={props.suspended}
				navigate={props.navigate}
			/>
		);
		crumbs = [
			{ label: "导入批次", to: "/knowledge/imports" },
			{ label: "批次详情" },
		];
	} else if (parts[0] === "candidates") {
		content = (
			<CandidatesPage
				suspended={props.suspended}
				onOpen={(id) => props.navigate(`/knowledge/candidates/${id}`)}
			/>
		);
	} else if (parts[0] === "imports") {
		content = (
			<ImportsPage
				suspended={props.suspended}
				onOpen={(id) => props.navigate(`/knowledge/imports/${id}`)}
			/>
		);
	} else {
		content = (
			<>
				<BrowsePage
					suspended={props.suspended}
					onOpen={(id) => props.navigate(`/knowledge?item=${id}`)}
				/>
				{knowledgeId && (
					<DetailSheet
						open
						onClose={() => {
							setItemTitle(undefined);
							props.navigate("/knowledge");
						}}
						title={itemTitle ?? "知识详情"}
						description="当前版本、来源与版本历史。"
					>
						<div className="min-h-0 flex-1 overflow-y-auto">
							<div className="p-4 sm:p-6">
								<KnowledgeItemSheet
									key={knowledgeId}
									id={knowledgeId}
									suspended={props.suspended}
									navigate={props.navigate}
									openEvidence={props.openEvidence}
									onLoaded={setItemTitle}
								/>
							</div>
						</div>
					</DetailSheet>
				)}
			</>
		);
	}

	return {
		title: "知识",
		list,
		content,
		crumbs,
		actions:
			section === "imports" ? (
				<Button
					disabled={props.suspended}
					onClick={() => props.navigate("/knowledge/imports/new")}
				>
					导入原文
				</Button>
			) : undefined,
	};
}
