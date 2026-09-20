import { useEffect, useState } from "react";
import { ErrorMessage } from "@/app/shared";
import { PropertyList, type PropertyListEntry } from "@/components/workbench/PropertyList";
/** The read-only authentication configuration view (ADR-0010): deployment
 * declares the login channels; editing means config file + restart. */
export function AuthConfigPage() {
	const [entries, setEntries] = useState<PropertyListEntry[]>();
	const [error, setError] = useState("");
	useEffect(() => {
		let cancelled = false;
		fetch("/api/v1/admin/auth-config", {
			credentials: "include",
			headers: { Accept: "application/json" },
		})
			.then(async (response) => {
				if (!response.ok) throw new Error(`HTTP ${response.status}`);
				const body = (await response.json()) as {
					entries: Array<{ key: string; value: string }>;
				};
				if (!cancelled)
					setEntries(
						body.entries.map((entry) => ({
							label: entry.key,
							value: entry.value,
						})),
					);
			})
			.catch(() => {
				if (!cancelled) setError("暂时无法读取认证配置。");
			});
		return () => {
			cancelled = true;
		};
	}, []);
	if (error)
		return (
			<section className="space-y-4 p-6" aria-label="认证配置">
				<ErrorMessage>{error}</ErrorMessage>
			</section>
		);
	return (
		<section className="space-y-4 p-6" aria-label="认证配置">
			<header className="space-y-1">
				<h1 className="text-lg font-semibold">认证配置</h1>
				<p className="text-sm text-muted-foreground">
					登录通道由部署配置声明（配置文件 + 重启生效），此处仅作展示；client
					secret 永不回显。
				</p>
			</header>
			{entries === undefined ? (
				<p className="text-sm text-muted-foreground" role="status">
					正在读取认证配置…
				</p>
			) : (
				<PropertyList entries={entries} />
			)}
		</section>
	);
}

