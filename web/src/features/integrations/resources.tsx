import { useCallback, useEffect, useState } from "react";
import { newClientCommandId, request } from "@/api/workbench";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { Button } from "@/components/ui/button";
import {
	Empty,
	EmptyDescription,
	EmptyHeader,
	EmptyTitle,
} from "@/components/ui/empty";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "@/components/ui/table";

interface Resource {
	id: string;
	objectType: string;
	identityKey: string;
	displayName?: string;
	labels: Record<string, string>;
	identityLabels: Record<string, string>;
	state: "observed" | "not_observed" | "stale";
	lastObservedAt?: string;
	lastSuccessfulRefreshAt?: string;
}
interface ObservationRun {
	id: string;
	state: string;
	resultDetail?: string;
}
const states = {
	observed: "当前观测到",
	not_observed: "当前未观测到",
	stale: "数据陈旧",
};
const message = (error: unknown) =>
	error instanceof Error ? error.message : "暂时无法读取观测结果。";

export function IntegrationResources({
	connectionName,
	navigate,
	suspended,
	enabled = true,
	platform = "prometheus",
	resourceId,
}: {
	connectionName: string;
	navigate: (route: string) => void;
	suspended: boolean;
	enabled?: boolean;
	platform?: "prometheus" | "thanos";
	resourceId?: string;
}) {
	const detailBase = `/settings/platform/integrations/${platform}/${encodeURIComponent(connectionName)}`;
	const base = `/api/v1/integrations/${encodeURIComponent(connectionName)}`;
	const [items, setItems] = useState<Resource[]>([]);
	const [cursor, setCursor] = useState<string>();
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState("");
	const [run, setRun] = useState<ObservationRun>();
	const [busy, setBusy] = useState(false);
	const [selected, setSelected] = useState<Resource>();
	const load = useCallback(
		async (after?: string) => {
			if (suspended) return;
			setLoading(true);
			setError("");
			try {
				const query = new URLSearchParams({ limit: "50" });
				if (after) query.set("cursor", after);
				const page = await request<{ items: Resource[]; nextCursor?: string }>(
					`${base}/resources?${query}`,
				);
				setItems((current) =>
					after ? [...current, ...page.items] : page.items,
				);
				setCursor(page.nextCursor);
			} catch (reason) {
				setError(message(reason));
			} finally {
				setLoading(false);
			}
		},
		[base, suspended],
	);
	useEffect(() => {
		void load();
	}, [load]);
	useEffect(() => {
		if (suspended || !resourceId) return;
		let active = true;
		request<Resource>(`${base}/resources/${encodeURIComponent(resourceId)}`)
			.then((item) => {
				if (active) setSelected(item);
			})
			.catch((reason) => {
				if (active) setError(message(reason));
			});
		return () => {
			active = false;
		};
	}, [base, resourceId, suspended]);
	useEffect(() => {
		if (suspended || !enabled) return;
		let active = true;
		const poll = async () => {
			try {
				const page = await request<{ items: ObservationRun[] }>(
					`${base}/observation-runs?limit=1`,
				);
				if (!active) return;
				const latest = page.items[0];
				if (latest) setRun(latest);
				if (!latest || !["Queued", "Running"].includes(latest.state))
					await load();
			} catch (reason) {
				if (active) setError(message(reason));
			}
		};
		void poll();
		const timer = setInterval(() => void poll(), 5000);
		return () => {
			active = false;
			clearInterval(timer);
		};
	}, [base, enabled, load, suspended]);
	useEffect(() => {
		if (suspended || !run || !["Queued", "Running"].includes(run.state)) return;
		const timer = setTimeout(() => {
			request<ObservationRun>(
				`${base}/observation-runs/${encodeURIComponent(run.id)}`,
			)
				.then((next) => {
					setRun(next);
					if (!["Queued", "Running"].includes(next.state)) void load();
				})
				.catch((reason) => setError(message(reason)));
		}, 1500);
		return () => clearTimeout(timer);
	}, [base, load, run, suspended]);
	async function refresh() {
		setBusy(true);
		setError("");
		try {
			setRun(
				await request<ObservationRun>(`${base}/resources:refresh`, {
					method: "POST",
					body: JSON.stringify({ clientCommandId: newClientCommandId() }),
				}),
			);
		} catch (reason) {
			setError(message(reason));
		} finally {
			setBusy(false);
		}
	}
	return (
		<section className="flex flex-col gap-4" aria-label="接入观测资源">
			<div className="flex flex-wrap items-center justify-between gap-3">
				<div>
					<h2 className="text-lg font-semibold">观测资源</h2>
					<p className="text-sm text-muted-foreground">
						范围来自当前接入。未观测到不表示资源已删除。
					</p>
				</div>
				<div className="flex gap-2">
					<Button
						variant="outline"
						disabled={
							suspended ||
							!enabled ||
							busy ||
							Boolean(run && ["Queued", "Running"].includes(run.state))
						}
						onClick={() => void refresh()}
					>
						刷新观测
					</Button>
					<Button
						disabled={suspended || !enabled}
						onClick={() =>
							navigate(
								`/inspections?${new URLSearchParams({ connectionName })}`,
							)
						}
					>
						立即巡检
					</Button>
				</div>
			</div>
			{run && (
				<p role="status">
					观测任务 {run.id}：{run.state}
					{run.resultDetail ? ` · ${run.resultDetail}` : ""}
				</p>
			)}
			{error && (
				<Alert variant="destructive">
					<AlertTitle>无法更新观测</AlertTitle>
					<AlertDescription>{error}</AlertDescription>
					<Button variant="outline" onClick={() => void load()}>
						重试
					</Button>
				</Alert>
			)}
			{loading && (
				<DetailSkeleton label="正在读取观测结果" rows={["line", "line", "line", "line"]} />
			)}
			{!loading && !error && items.length === 0 ? (
				<Empty>
					<EmptyHeader>
						<EmptyTitle>尚无观测对象</EmptyTitle>
						<EmptyDescription>
							接入启用后自动观测。空结果不代表整个接入范围健康。
						</EmptyDescription>
					</EmptyHeader>
				</Empty>
			) : (
				items.length > 0 && (
					<Table>
						<TableHeader>
							<TableRow>
								<TableHead>对象</TableHead>
								<TableHead>类型</TableHead>
								<TableHead>观测状态</TableHead>
								<TableHead>最后观测时间</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{items.map((item) => (
								<TableRow key={item.id}>
									<TableCell>
										<Button
											variant="link"
											onClick={() =>
												navigate(
													`${detailBase}/resources/${encodeURIComponent(item.id)}`,
												)
											}
										>
											{item.displayName ?? item.labels.instance ?? item.id}
										</Button>
									</TableCell>
									<TableCell>{item.objectType}</TableCell>
									<TableCell>
										<Badge variant="outline">{states[item.state]}</Badge>
									</TableCell>
									<TableCell>
										{item.lastObservedAt
											? new Date(item.lastObservedAt).toLocaleString()
											: "尚无成功观测"}
									</TableCell>
								</TableRow>
							))}
						</TableBody>
					</Table>
				)
			)}
			<LoadMoreButton
				loading={loading || suspended}
				hasMore={Boolean(cursor)}
				onLoadMore={() => void load(cursor)}
			/>
			{resourceId && selected && (
				<section className="flex flex-col gap-3" aria-label="资源详情">
					<div className="flex items-center justify-between">
						<h3 className="font-semibold">
							{selected.displayName ?? selected.id}
						</h3>
						<Button variant="ghost" onClick={() => navigate(detailBase)}>
							关闭详情
						</Button>
					</div>
					<dl>
						<dt>来源接入</dt>
						<dd>{connectionName}</dd>
						<dt>来源身份</dt>
						<dd className="break-all">{selected.identityKey}</dd>
					</dl>
					<pre className="overflow-auto rounded-md bg-muted p-3 text-xs">
						{JSON.stringify(selected.labels, null, 2)}
					</pre>
				</section>
			)}
		</section>
	);
}
