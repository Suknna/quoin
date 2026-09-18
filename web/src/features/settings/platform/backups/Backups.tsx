/* eslint-disable react-hooks/exhaustive-deps -- Domain view factories intentionally colocate lifecycle helpers with their route component. */

import { useEffect, useState } from "react";
import {
	newClientCommandId,
	request,
	WorkbenchApiError,
} from "@/api/workbench";
import { messageOf, notify } from "@/app/shared";
import { EntityList } from "@/components/EntityList";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { DetailSkeleton } from "@/components/workbench/DetailSkeleton";
import { LoadMoreButton } from "@/components/workbench/LoadMoreButton";
import { usePolling } from "@/hooks/use-polling";

type Backup = {
	id: string;
	status: string;
	stage: string;
	createdAt: string;
	updatedAt: string;
	sizeBytes: number;
	errorDetail?: string;
};
type BackupPage = {
	items?: Backup[];
	nextCursor?: string;
	retentionHealth?: { lastFailureAt?: string; errorDetail?: string };
};
type BackupSettings = {
	enabled: boolean;
	scheduleCron?: string | null;
	timezone: string;
	backupTarget: string;
	retentionCount: number;
	rowVersion: number;
};
type ArtifactRetention = { generatedRetentionDays: number; rowVersion: number };
const active = (value: Backup) =>
	["queued", "running", "pending"].includes(value.status.toLowerCase());

/** Backups are asynchronous server tasks; this view never invents restore or cancellation commands. */
export function Backups({ suspended }: { suspended: boolean }) {
	const [items, setItems] = useState<Backup[]>([]);
	const [cursor, setCursor] = useState<string>();
	const [settings, setSettings] = useState<BackupSettings>();
	const [retention, setRetention] = useState<ArtifactRetention>();
	const [error, setError] = useState("");
	const [saving, setSaving] = useState(false);
	const [loading, setLoading] = useState(true);
	const [triggering, setTriggering] = useState(false);
	const load = async (more = false) => {
		try {
			const page = await request<BackupPage>(
				`/api/v1/backups?limit=50${more && cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
			);
			setItems((current) =>
				more ? [...current, ...(page.items ?? [])] : (page.items ?? []),
			);
			setCursor(page.nextCursor);
			const [nextSettings, nextRetention] = await Promise.all([
				request<BackupSettings>("/api/v1/backups/settings"),
				request<ArtifactRetention>("/api/v1/artifacts/retention-settings"),
			]);
			setSettings(nextSettings);
			setRetention(nextRetention);
			if (page.retentionHealth?.lastFailureAt)
				setError(
					`旧备份清理失败，将自动重试：${page.retentionHealth.errorDetail ?? "无详情"}`,
				);
		} catch (reason) {
			setError(messageOf(reason, "暂时无法完成操作，请重试。"));
		} finally {
			setLoading(false);
		}
	};
	useEffect(() => {
		void load();
	}, []);
	usePolling(() => void load(), 3000, !suspended && items.some(active));
	async function trigger() {
		if (triggering) return;
		setTriggering(true);
		try {
			await request<Backup>("/api/v1/backups", {
				method: "POST",
				body: JSON.stringify({ clientCommandId: newClientCommandId() }),
			});
			notify.success("已发起备份");
			await load();
		} catch (reason) {
			notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setTriggering(false);
		}
	}
	async function saveSettings(event: React.FormEvent<HTMLFormElement>) {
		event.preventDefault();
		if (!settings) return;
		setSaving(true);
		try {
			setSettings(
				await request<BackupSettings>("/api/v1/backups/settings", {
					method: "PUT",
					body: JSON.stringify({
						clientCommandId: newClientCommandId(),
						expectedRowVersion: settings.rowVersion,
						enabled: settings.enabled,
						scheduleCron: settings.scheduleCron || null,
						timezone: settings.timezone,
						retentionCount: settings.retentionCount,
					}),
				}),
			);
			notify.success("已保存备份设置");
		} catch (reason) {
			if (reason instanceof WorkbenchApiError && reason.status === 409)
				notify.warning("备份设置已被其他管理员修改，请刷新后重试。");
			else notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setSaving(false);
		}
	}
	async function saveRetention(event: React.FormEvent<HTMLFormElement>) {
		event.preventDefault();
		if (!retention) return;
		setSaving(true);
		try {
			setRetention(
				await request<ArtifactRetention>(
					"/api/v1/artifacts/retention-settings",
					{
						method: "PUT",
						body: JSON.stringify({
							clientCommandId: newClientCommandId(),
							expectedRowVersion: retention.rowVersion,
							generatedRetentionDays: retention.generatedRetentionDays,
						}),
					},
				),
			);
			notify.success("已保存产物保留设置");
		} catch (reason) {
			if (reason instanceof WorkbenchApiError && reason.status === 409)
				notify.warning("产物保留设置已被其他管理员修改，请刷新后重试。");
			else notify.error(reason, "暂时无法完成操作，请重试。");
		} finally {
			setSaving(false);
		}
	}
	return (
		<section className="space-y-6">
			<div className="flex justify-between">
				<h2 className="text-xl font-semibold">备份与保留</h2>
				<Button
					disabled={suspended || triggering}
					onClick={() => void trigger()}
				>
					{triggering ? "备份中…" : "立即备份"}
				</Button>
			</div>
			{error && (
				<Alert variant="destructive">
					<AlertDescription>{error}</AlertDescription>
				</Alert>
			)}
			{loading && !settings && !retention && (
				<DetailSkeleton
					label="正在读取备份设置"
					rows={["title", "card", "card"]}
				/>
			)}
			{!loading && !settings && !error && (
				<p className="text-sm text-muted-foreground">无法读取备份设置。</p>
			)}
			{settings && (
				<form className="space-y-3 rounded border p-4" onSubmit={saveSettings}>
					<h3 className="font-semibold">备份设置</h3>
					<Field orientation="horizontal">
						<Switch
							id="backup-enabled"
							checked={settings.enabled}
							disabled={suspended || saving}
							onCheckedChange={(checked) =>
								setSettings(
									(current) =>
										current && { ...current, enabled: checked === true },
								)
							}
						/>
						<FieldLabel htmlFor="backup-enabled">启用计划备份</FieldLabel>
					</Field>
					<Field>
						<FieldLabel htmlFor="backup-cron">Cron</FieldLabel>
						<Input
							id="backup-cron"
							value={settings.scheduleCron ?? ""}
							disabled={suspended || saving}
							placeholder="留空仅手动"
							onChange={(event) =>
								setSettings(
									(current) =>
										current && { ...current, scheduleCron: event.target.value },
								)
							}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="backup-timezone">时区</FieldLabel>
						<Input
							id="backup-timezone"
							value={settings.timezone}
							disabled={suspended || saving}
							onChange={(event) =>
								setSettings(
									(current) =>
										current && { ...current, timezone: event.target.value },
								)
							}
						/>
					</Field>
					<Field>
						<FieldLabel htmlFor="backup-retention">保留份数</FieldLabel>
						<Input
							id="backup-retention"
							type="number"
							min="1"
							value={settings.retentionCount}
							disabled={suspended || saving}
							onChange={(event) =>
								setSettings(
									(current) =>
										current && {
											...current,
											retentionCount: Number(event.target.value),
										},
								)
							}
						/>
						<FieldDescription>目标：{settings.backupTarget}</FieldDescription>
					</Field>
					<Button type="submit" disabled={suspended || saving}>
						保存备份设置
					</Button>
				</form>
			)}
			{retention && (
				<form className="space-y-3 rounded border p-4" onSubmit={saveRetention}>
					<h3 className="font-semibold">生成型产物在线保留</h3>
					<Field>
						<FieldLabel htmlFor="artifact-retention">天数</FieldLabel>
						<Input
							id="artifact-retention"
							type="number"
							min="1"
							value={retention.generatedRetentionDays}
							disabled={suspended || saving}
							onChange={(event) =>
								setRetention(
									(current) =>
										current && {
											...current,
											generatedRetentionDays: Number(event.target.value),
										},
								)
							}
						/>
						<FieldDescription>只影响后续新建的生成型产物。</FieldDescription>
					</Field>
					<Button type="submit" disabled={suspended || saving}>
						保存产物保留
					</Button>
				</form>
			)}
			<section className="space-y-3">
				<h3 className="font-semibold">备份记录</h3>
				<EntityList
					items={items.map((item) => ({
						id: item.id,
						title: item.id,
						subtitle: `阶段 ${item.stage} · 创建 ${item.createdAt}`,
						badge: {
							text: item.status,
							variant:
								item.status.toLowerCase() === "succeeded"
									? ("secondary" as const)
									: ("outline" as const),
						},
						item,
					}))}
					columns={["title", "subtitle", "status", "actions"]}
					renderActions={(row) => (
						<div className="flex items-center gap-2">
							{row.item.errorDetail && (
								<small className="text-destructive">
									{row.item.errorDetail}
								</small>
							)}
							{row.item.status.toLowerCase() === "succeeded" && (
								<Button asChild variant="link" className="h-auto p-0">
									<a href={`/api/v1/backups/${row.item.id}/download`}>
										下载归档
									</a>
								</Button>
							)}
						</div>
					)}
					loading={loading && !items.length}
					loadingLabel="正在读取备份记录"
					emptyTitle="尚无备份记录"
				/>
				<LoadMoreButton
					loading={loading}
					hasMore={Boolean(cursor)}
					onLoadMore={() => void load(true)}
				/>
			</section>
		</section>
	);
}
