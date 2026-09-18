import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { expect, type Page, test } from "@playwright/test";

type Check = {
	checkKey: string;
	status: "ok" | "error" | "gap";
	evidenceId?: string;
	gapReason?: string;
};
type Detail = {
	id: string;
	state: string;
	configVersionId?: string;
	checks: Check[];
	reportCount: number;
};

async function api<T>(page: Page, url: string, init?: RequestInit): Promise<T> {
	return page.evaluate(
		async ({ url, init }) => {
			const r = await fetch(url, init);
			if (!r.ok) throw new Error(`${url}: ${r.status} ${await r.text()}`);
			return await r.json();
		},
		{ url, init },
	);
}

// T25 reuses the existing isolated compose/Chromium harness. This observation
// stays structured and contains only locators/state, not page or provider content.
function scheduleObservations(value: unknown) {
	const dir = process.env.QUOIN_EVIDENCE_DIR;
	if (dir) {
		mkdirSync(dir, { recursive: true });
		writeFileSync(
			join(dir, "t25-schedule-observations.json"),
			JSON.stringify(value, null, 2),
		);
	}
}

test.describe("T25 deterministic scheduled Inspection @ticket-25", () => {
	test("current published cron creates one visible UTC-keyed Run through the real timer", async ({
		page,
	}) => {
		test.slow();
		test.setTimeout(420_000);
		const stack = join(
			import.meta.dirname,
			"..",
			"..",
			".artifacts",
			"e2e-stack-t25",
		);
		const password = join(stack, "admin-new-password");
		expect(existsSync(password)).toBeTruthy();
		await page.goto("/");
		await page.fill("#username", "admin");
		await page.fill("#password", readFileSync(password, "utf8").trim());
		await page.getByRole("button", { name: "登录" }).click();
		await expect(
			page.getByRole("navigation", { name: "全局模块" }),
		).toBeVisible({ timeout: 30_000 });

		const suffix = `${Date.now()}`;
		const systemKey = `t25-schedule-${suffix}`;
		const prepared = await page.evaluate(
			async ({ suffix, systemKey }) => {
				const headers = { "Content-Type": "application/json" };
				let active = (
					await (await fetch("/api/v1/label-contracts?limit=100")).json()
				).items?.find((x: { state?: string }) => x.state === "active");
				if (!active) {
					const form = new FormData();
					form.append(
						"file",
						new File(
							["label_contract:\n  business_system_label: business_system\n"],
							"contract.yaml",
							{ type: "application/yaml" },
						),
					);
					form.append("clientCommandId", `t25-contract-${suffix}`);
					const created = await fetch("/api/v1/label-contracts", {
						method: "POST",
						body: form,
					});
					if (created.status !== 201)
						throw new Error(
							`label contract=${created.status}: ${await created.text()}`,
						);
					active = await created.json();
					const activated = await fetch(
						`/api/v1/label-contracts/${active.version}/activate`,
						{
							method: "POST",
							headers,
							body: JSON.stringify({
								clientCommandId: `t25-activate-${suffix}`,
								expectedStateRowVersion: active.rowVersion,
								expectedCurrentContractVersionId: null,
								expectedTargetRowVersion: active.rowVersion,
								compatibleVersions: [],
							}),
						},
					);
					if (!activated.ok)
						throw new Error(
							`activate contract=${activated.status}: ${await activated.text()}`,
						);
					active = await activated.json();
				}
				const yaml = `system_key: ${systemKey}\ndisplay_name: T25 定时巡检\nenabled: true\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: each-minute\n    display_name: 每分钟巡检\n    cron: "* * * * *"\n    checks:\n      - key: up-instant\n        display_name: PromQL Up\n        analysis_question: 当前可用吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system="${systemKey}"}'\n`;
				const form = new FormData();
				form.append(
					"file",
					new File([yaml], "system.yaml", { type: "application/yaml" }),
				);
				form.append("clientCommandId", `t25-system-${suffix}`);
				form.append("targetLabelContractVersion", String(active.version));
				const created = await fetch("/api/v1/business-systems", {
					method: "POST",
					body: form,
				});
				if (created.status !== 201)
					throw new Error(`system=${created.status}: ${await created.text()}`);
				const version = String((await created.json()).id);
				const published = await fetch(
					`/api/v1/business-systems/${systemKey}/config/${version}/publish`,
					{
						method: "POST",
						headers,
						body: JSON.stringify({
							clientCommandId: `t25-publish-${suffix}`,
							expectedCurrentPublishedVersionId: null,
						}),
					},
				);
				if (!published.ok)
					throw new Error(
						`publish=${published.status}: ${await published.text()}`,
					);
				return { version };
			},
			{ suffix, systemKey },
		);

		// Restart after publication but before observing a boundary. Remember
		// pre-restart runs so only a post-restart historical slot can fail this
		// proof; a boundary that legitimately won the race before restart is not
		// evidence of backfill.
		type ScheduledSummary = {
			id: string;
			planKey: string;
			state: string;
			triggerKind: string;
			scheduledFor?: string;
		};
		const beforeRestartIDs = new Set(
			(
				(await page.evaluate(async (key) => {
					const r = await fetch(
						`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`,
					);
					if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
					return (await r.json()).items ?? [];
				}, systemKey)) as ScheduledSummary[]
			).map((run) => run.id),
		);

		// A fresh Quoin process obtains scheduling solely from SQLite and does not
		// invent a missed historical slot while it was down.
		const restartStartedAt = new Date();
		const compose = join(
			stack,
			"state",
			"quoin",
			"compose",
			"generated",
			"compose.yaml",
		);
		execFileSync(
			"docker",
			[
				"compose",
				"--project-name",
				"quoin-t25",
				"--file",
				compose,
				"restart",
				"quoin",
			],
			{ stdio: "ignore" },
		);
		const restartCompleted = new Date();
		const restartCompletedAt = restartCompleted.toISOString();
		const firstBoundaryAfterRestart = new Date(restartCompleted);
		firstBoundaryAfterRestart.setUTCSeconds(0, 0);
		firstBoundaryAfterRestart.setUTCMinutes(
			firstBoundaryAfterRestart.getUTCMinutes() + 1,
		);

		let scheduled: ScheduledSummary | undefined;
		let postRestartRuns: ScheduledSummary[] = [];
		await expect
			.poll(
				async () => {
					const response = (await page.evaluate(async (key) => {
						const r = await fetch(
							`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`,
						);
						// Restart deliberately creates a brief proxy 502 window. It is not a
						// result; polling must wait for Quoin to become readable again.
						return r.ok ? ((await r.json()).items ?? []) : [];
					}, systemKey)) as ScheduledSummary[];
					postRestartRuns = response.filter(
						(run) =>
							!beforeRestartIDs.has(run.id) &&
							run.planKey === "each-minute" &&
							run.triggerKind === "schedule",
					);
					scheduled = postRestartRuns.find(
						(run) =>
							Date.parse(run.scheduledFor ?? "") ===
							firstBoundaryAfterRestart.getTime(),
					);
					return scheduled?.id ?? "";
				},
				{ timeout: 150_000, intervals: [1_000, 2_000, 5_000] },
			)
			.not.toBe("");
		expect(scheduled?.scheduledFor).toMatch(
			/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/,
		);
		expect(Date.parse(scheduled!.scheduledFor!)).toBe(
			firstBoundaryAfterRestart.getTime(),
		);
		const lateBackfills = postRestartRuns.filter(
			(run) =>
				Date.parse(run.scheduledFor ?? "") <
				firstBoundaryAfterRestart.getTime(),
		);
		expect(lateBackfills).toEqual([]);

		let detail: Detail | undefined;
		await expect
			.poll(
				async () => {
					detail = await api<Detail>(
						page,
						`/api/v1/inspections/runs/${scheduled!.id}`,
					);
					return detail.state;
				},
				{ timeout: 120_000, intervals: [1_000, 2_000, 5_000] },
			)
			.toMatch(/Completed|CompletedWithGaps/);
		expect(detail?.checks).toHaveLength(1);
		await page.goto(`/inspections/runs/${scheduled!.id}`);
		await expect(
			page.getByRole("heading", {
				name: new RegExp(`巡检 Run #${scheduled!.id}`),
			}),
		).toBeVisible();
		await expect(page.getByText("up-instant").first()).toBeVisible();

		// Wait across a second boundary and prove the first durable UTC key did
		// not duplicate.
		await page.waitForTimeout(65_000);
		const all = (await page.evaluate(async (key) => {
			const r = await fetch(
				`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`,
			);
			if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
			return (await r.json()).items ?? [];
		}, systemKey)) as ScheduledSummary[];
		const sameKey = all.filter(
			(run) =>
				run.planKey === "each-minute" &&
				run.triggerKind === "schedule" &&
				run.scheduledFor === scheduled!.scheduledFor,
		);
		expect(sameKey).toHaveLength(1);
		const finalPostRestartRuns = all.filter(
			(run) =>
				!beforeRestartIDs.has(run.id) &&
				run.planKey === "each-minute" &&
				run.triggerKind === "schedule",
		);
		const finalLateBackfills = finalPostRestartRuns.filter(
			(run) =>
				Date.parse(run.scheduledFor ?? "") <
				firstBoundaryAfterRestart.getTime(),
		);
		expect(finalLateBackfills).toEqual([]);
		scheduleObservations({
			systemKey,
			configVersionId: prepared.version,
			restartStartedAt: restartStartedAt.toISOString(),
			restartCompletedAt,
			firstBoundaryAfterRestart: firstBoundaryAfterRestart.toISOString(),
			runId: scheduled!.id,
			triggerKind: scheduled!.triggerKind,
			scheduledFor: scheduled!.scheduledFor,
			state: detail?.state,
			check: detail?.checks[0],
			sameUTCKeyRows: sameKey.length,
			postRestartRunCount: finalPostRestartRuns.length,
			noLateBackfills: finalLateBackfills.length === 0,
			events: [
				{
					observedAt: new Date().toISOString(),
					protocol: "process",
					routeOrMethod: "docker compose restart quoin",
					locator: "compose project:quoin-t25; service:quoin",
					expected:
						"post-restart scheduler reads committed plan without backfill",
					actual: `completed=${restartCompletedAt}; first_boundary=${firstBoundaryAfterRestart.toISOString()}`,
					rawLogRef: "ticket25-playwright.log",
				},
				{
					observedAt: new Date().toISOString(),
					protocol: "timer",
					routeOrMethod: "scheduler minute boundary",
					locator: `system:${systemKey}; plan:each-minute; run:${scheduled!.id}`,
					expected: "one post-restart schedule Run with canonical UTC key",
					actual: `${scheduled!.triggerKind}/${scheduled!.scheduledFor}`,
					rawLogRef: "ticket25-playwright.log",
				},
				{
					observedAt: new Date().toISOString(),
					protocol: "http",
					routeOrMethod: "GET /api/v1/inspections/runs",
					locator: `system:${systemKey}; scheduled_for:${scheduled!.scheduledFor}`,
					expected: "no duplicate deterministic key after another tick",
					actual: `rows=${sameKey.length}`,
					rawLogRef: "ticket25-playwright.log",
				},
				{
					observedAt: new Date().toISOString(),
					protocol: "ui",
					routeOrMethod: "GET /inspections/runs/:id",
					locator: `run:${scheduled!.id}`,
					expected: "scheduled Run detail renders",
					actual: "rendered",
					rawLogRef: "ticket25-playwright.log",
				},
			],
		});
	});
});
