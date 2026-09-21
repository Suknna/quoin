import { useCallback, useEffect, useState } from "react";
import { messageOf } from "@/app/shared";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { TableCell, TableRow } from "@/components/ui/table";
import { DataTable } from "@/components/workbench/DataTable";
import { RefreshButton } from "@/components/workbench/RefreshButton";
import { formatDateTime } from "@/lib/format";
import { Maintenance } from "../maintenance/Maintenance";
import { type AboutStatus, fetchAbout } from "./api";

const unknown = (value?: string) => value?.trim() || "未知";
const time = (value?: string) => formatDateTime(value, "未知");

/** Admin-only About gathers bounded, non-secret platform facts and the actions that maintain them. */
export function About({ suspended }: { suspended: boolean }) {
	const [status, setStatus] = useState<AboutStatus | null>(null);
	const [error, setError] = useState("");
	const [loading, setLoading] = useState(false);
	const [refreshRevision, setRefreshRevision] = useState(0);

	const load = useCallback(async () => {
		if (suspended) return;
		try {
			setLoading(true);
			setError("");
			setStatus(await fetchAbout());
			setRefreshRevision((current) => current + 1);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法读取平台关于信息。"));
		} finally {
			setLoading(false);
		}
	}, [suspended]);

	useEffect(() => {
		void load();
	}, [load]);

	return (
		<section className="flex w-full flex-col gap-6">
			<div className="flex flex-wrap items-start justify-between gap-3">
				<div>
					<h2 className="text-xl font-semibold">关于平台</h2>
					<p className="mt-1 text-sm text-muted-foreground">
						组件版本、连接事实和必要的平台维护。未知信息不会被推断为健康。
					</p>
				</div>
				<RefreshButton
					loading={loading}
					disabled={suspended}
					onClick={() => void load()}
				/>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>
						{error}{" "}
						<Button
							variant="link"
							className="h-auto p-0 align-baseline"
							onClick={() => void load()}
							disabled={suspended || loading}
						>
							重试
						</Button>
					</AlertDescription>
				</Alert>
			)}
			{!status && !error && (
				<div
					className="flex flex-col gap-3"
					role="status"
					aria-label="正在读取平台事实"
				>
					<Skeleton className="h-6 w-1/3" />
					<Skeleton className="h-24 w-full" />
					<Skeleton className="h-4 w-2/3" />
				</div>
			)}
			{status && (
				<div className="flex flex-col gap-6">
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">Quoin</h3>
						<p className="wrap-anywhere font-mono text-sm">
							版本：{unknown(status.releaseVersion)}
						</p>
					</section>
					<Separator />
					<section className="flex flex-col gap-3">
						<h3 className="text-sm font-medium">内部组件</h3>
						<DataTable
							columns={[
								{ label: "组件" },
								{ label: "连接" },
								{ label: "版本" },
								{ label: "最近事实" },
							]}
							emptyTitle="暂无内部组件事实。"
						>
							{status.components.map((component) => (
								<TableRow key={component.slot}>
									<TableCell className="font-medium">
										{component.slot}
									</TableCell>
									<TableCell>
										<Badge
											variant={component.connected ? "secondary" : "outline"}
										>
											{component.connected ? "已连接" : "未连接"}
										</Badge>
									</TableCell>
									<TableCell className="max-w-72 whitespace-normal wrap-anywhere font-mono text-xs">
										{unknown(component.releaseVersion)}
									</TableCell>
									<TableCell className="whitespace-normal text-xs tabular-nums text-muted-foreground">
										{component.connected ? time(component.lastSeenAt) : "未知"}
									</TableCell>
								</TableRow>
							))}
						</DataTable>
					</section>
					<Separator />
					<Maintenance refreshRevision={refreshRevision} onChanged={load} />
				</div>
			)}
		</section>
	);
}
