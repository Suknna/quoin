import { ChevronRight, FileText } from "lucide-react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Button } from "@/components/ui/button";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";

/**
 * Shared AI body reader for the three AI entries (alert analysis, inspection
 * report, investigation chat). Model output stays safe: react-markdown escapes
 * raw HTML by default and only frozen evidence IDs are converted into internal
 * reading-layer buttons, never arbitrary links.
 */
export function AiContent({
	content,
	evidenceIds,
	openEvidence,
}: {
	content: string;
	evidenceIds?: readonly string[];
	openEvidence?: (id: string) => void;
}) {
	const refs = new Set(evidenceIds ?? []);
	const markdown =
		openEvidence && refs.size > 0
			? // Convert only frozen IDs into internal links before parsing so evidence
				// navigation keeps working without trusting model-authored hrefs.
				content.replace(/#([A-Za-z0-9_-]+)/g, (reference, id: string) =>
					refs.has(id) ? `[#${id}](/evidence/${id})` : reference,
				)
			: content;
	// Model output may be steered by prompt injection; only frozen evidence
	// paths stay clickable and every other href is reduced to plain text.
	const allowEvidenceHref = (href: string) =>
		href.startsWith("/evidence/") ? href : "";
	return (
		<div className="min-w-0 space-y-3 break-words text-sm leading-6 [&_ol]:list-decimal [&_ol]:pl-6 [&_ul]:list-disc [&_ul]:pl-6">
			<ReactMarkdown
				remarkPlugins={[remarkGfm]}
				urlTransform={allowEvidenceHref}
				components={{
					h1: ({ children }) => (
						<h1 className="text-xl font-semibold">{children}</h1>
					),
					h2: ({ children }) => (
						<h2 className="text-lg font-semibold">{children}</h2>
					),
					h3: ({ children }) => (
						<h3 className="text-base font-semibold">{children}</h3>
					),
					p: ({ children }) => (
						<p className="whitespace-pre-wrap">{children}</p>
					),
					// Metric columns stay readable at a sensible minimum width; wide tables
					// scroll inside their own region instead of widening the page.
					table: ({ children }) => (
						<Table className="min-w-[36rem]">{children}</Table>
					),
					thead: ({ children }) => <TableHeader>{children}</TableHeader>,
					tbody: ({ children }) => <TableBody>{children}</TableBody>,
					tr: ({ children }) => <TableRow>{children}</TableRow>,
					th: ({ children }) => <TableHead>{children}</TableHead>,
					td: ({ children }) => (
						<TableCell className="whitespace-normal break-words">
							{children}
						</TableCell>
					),
					code: ({ className, children, ...props }) =>
						className ? (
							<code
								className="block max-w-full overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs"
								{...props}
							>
								{children}
							</code>
						) : (
							<code
								className="break-all rounded bg-muted px-1 py-0.5 font-mono text-xs"
								{...props}
							>
								{children}
							</code>
						),
					pre: ({ children }) => (
						<pre className="max-w-full overflow-x-auto">{children}</pre>
					),
					a: ({ href = "", children }) => {
						const id = href.startsWith("/evidence/")
							? href.slice("/evidence/".length)
							: "";
						return id && refs.has(id) && openEvidence ? (
							<Button
								variant="link"
								className="h-auto p-0 align-baseline"
								onClick={() => openEvidence(id)}
							>
								{children}
							</Button>
						) : (
							<span>{children}</span>
						);
					},
				}}
			>
				{markdown}
			</ReactMarkdown>
		</div>
	);
}

/**
 * One evidence-link presentation for all AI entries: each frozen ID opens the
 * existing evidence reading layer through the workspace callback.
 */
export function EvidenceLinks({
	ids,
	openEvidence,
}: {
	ids: readonly string[];
	openEvidence: (id: string) => void;
}) {
	return (
		<div className="flex flex-col gap-2">
			{ids.map((id) => (
				<Button
					key={id}
					variant="outline"
					size="sm"
					className="w-full justify-between"
					aria-label={`证据 ${id} 查看证据`}
					onClick={() => openEvidence(id)}
				>
					<span className="flex min-w-0 items-center gap-2">
						<FileText data-icon="inline-start" />
						证据 {id}
					</span>
					<span className="flex items-center gap-1 text-muted-foreground">
						查看证据
						<ChevronRight data-icon="inline-end" />
					</span>
				</Button>
			))}
		</div>
	);
}
