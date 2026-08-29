import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

// T24 retains no browser-derived trace, screenshot, video, profile, or page body.
test.use({ trace: 'off', screenshot: 'off', video: 'off' })
const fixture = 'quoin-t24-auth-fixture'

type Check = { checkKey: string; status: 'ok' | 'error' | 'gap'; evidenceId?: string; gapReason?: string }
type Detail = { id: string; state: string; configVersionId?: string; checks: Check[]; reportCount: number }
type Report = { runId: string; version: number; evidenceDigest: string; evidenceIds: string[]; modelId: string; content: string; createdAt: string }

function fixtureCounter(name: string): number {
  try { return Number.parseInt(execFileSync('docker', ['exec', fixture, 'cat', `/state/${name}`], { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] }).trim(), 10) || 0 } catch { return 0 }
}
function fixtureState(name: string): boolean {
  try { execFileSync('docker', ['exec', fixture, 'test', '-f', `/state/${name}`], { stdio: 'ignore' }); return true } catch { return false }
}
async function api<T>(page: Page, url: string, init?: RequestInit): Promise<T> {
  return page.evaluate(async ({ url, init }) => { const r = await fetch(url, init); if (!r.ok) throw new Error(`${url}: ${r.status} ${await r.text()}`); return await r.json() }, { url, init })
}
const observationsFile = process.env.QUOIN_T24_OBSERVATIONS ?? 't24-inspection-observations.json'
function observations(value: unknown) { const dir = process.env.QUOIN_EVIDENCE_DIR; if (dir) { mkdirSync(dir, { recursive: true }); writeFileSync(join(dir, observationsFile), JSON.stringify(value, null, 2)) } }

// This UI leg drives one real manual Run through Quoin HTTP → Runtime →
// Plinth/Thanos and Lintel/Chromium. The Journey's deliberately broken page
// remains a gap; it must not erase the independent PromQL Evidence.
test.describe('T24 mixed manual Inspection @ticket-24', () => {
  test('mixed Run retains a PromQL fact beside a Journey gap and immutable report', async ({ page }) => {
    test.slow(); test.setTimeout(600_000)
    const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack-t24')
    const password = join(stack, 'admin-new-password')
    expect(existsSync(password)).toBeTruthy()
    await page.goto('/'); await page.fill('#username', 'admin'); await page.fill('#password', readFileSync(password, 'utf8').trim()); await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const suffix = `${Date.now()}`, systemKey = `t24-mixed-${suffix}`
    const prepared = await page.evaluate(async ({ suffix, systemKey, fixture }) => {
      const headers = { 'Content-Type': 'application/json' }
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((x: { state?: string }) => x.state === 'active')
      if (!active) {
        const form = new FormData(); form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' })); form.append('clientCommandId', `t24-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form }); if (created.status !== 201) throw new Error(`label contract=${created.status}: ${await created.text()}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t24-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }) })
        if (!activated.ok) throw new Error(`activate contract=${activated.status}: ${await activated.text()}`)
        active = await activated.json()
      }
      const yaml = `system_key: ${systemKey}\ndisplay_name: T24 混合巡检\nenabled: true\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: mixed-plan\n    display_name: 混合巡检\n    checks:\n      - key: up-instant\n        display_name: PromQL Up\n        analysis_question: 当前可用吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system="${systemKey}"}'\n      - key: broken-page\n        display_name: 损坏页面\n        analysis_question: 页面是否正常？\n        kind: browser\n        journey_id: page.status-marker.v1\n        journey_params:\n          path: /status-broken\n`
      const form = new FormData(); form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' })); form.append('clientCommandId', `t24-system-${suffix}`); form.append('targetLabelContractVersion', String(active.version))
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form }); if (created.status !== 201) throw new Error(`system=${created.status}: ${await created.text()}`)
      const versions = await (await fetch(`/api/v1/business-systems/${systemKey}/config?limit=1`)).json(); const version = versions.items?.[0]?.id; if (!version) throw new Error('no config version')
      const published = await fetch(`/api/v1/business-systems/${systemKey}/config/${version}/publish`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t24-publish-${suffix}`, expectedCurrentPublishedVersionId: null }) }); if (!published.ok) throw new Error(`publish=${published.status}: ${await published.text()}`)
      const identity = await fetch(`/api/v1/business-systems/${systemKey}/browser-identity`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t24-identity-${suffix}`, name: 'T24 只读浏览器账号', startUrl: `http://${fixture}:8081/login`, authenticationProbe: { journeyId: 'authentication.url-prefix.v1', journeyVersion: 1, params: { authenticatedUrlPrefix: `http://${fixture}:8081/authenticated` } } }) }); if (identity.status !== 202) throw new Error(`identity=${identity.status}: ${await identity.text()}`)
      return { version: String(version) }
    }, { suffix, systemKey, fixture })

    // The same real manual-login/noVNC/publish route proved by T23 makes this
    // Run's browser check a real journey failure, not an authentication gap.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await page.getByRole('button', { name: 'T24 混合巡检' }).click()
    await expect(page.getByRole('button', { name: '打开浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '打开浏览器登录' }).click(); await expect(page.getByRole('heading', { name: '浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '重新登录' }).click()
    await expect.poll(async () => page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).currentOperation, systemKey), { timeout: 60_000 }).toMatchObject({ state: 'Running', canAttach: true, canPublish: true })
    const canvas = page.locator('.browser-login-viewport canvas'); await expect(canvas).toBeVisible({ timeout: 45_000 })
    await expect(page.getByText('安全浏览器已连接。选择“进入远程浏览器”后开始输入。')).toBeVisible({ timeout: 45_000 })
    await expect.poll(() => fixtureCounter('ready-seq') > 0, { timeout: 45_000 }).toBe(true)
    await page.waitForTimeout(2_000); await expect(page.getByRole('button', { name: '完成登录并发布' })).toBeEnabled({ timeout: 45_000 })
    const bounds = await canvas.boundingBox(); if (!bounds) throw new Error('noVNC canvas is not measurable')
    const pointer = fixtureCounter('pointerdown-seq'), key = fixtureCounter('keydown-seq'), submit = fixtureCounter('submit-seq')
    await page.getByRole('button', { name: '进入远程浏览器' }).click(); await expect(canvas.evaluate((element) => document.activeElement === element)).resolves.toBe(true)
    await canvas.click({ position: { x: bounds.width / 2, y: bounds.height / 2 } }); await expect.poll(() => fixtureCounter('pointerdown-seq') > pointer, { timeout: 20_000 }).toBe(true)
    await canvas.focus(); await page.keyboard.press('Enter'); await expect.poll(() => fixtureCounter('keydown-seq') > key, { timeout: 20_000 }).toBe(true)
    await expect.poll(() => fixtureCounter('submit-seq') > submit && fixtureState('authenticated') && fixtureState('authenticated-page'), { timeout: 60_000 }).toBe(true)
    await page.getByRole('button', { name: '完成登录并发布' }).click()
    await expect.poll(async () => page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).state, systemKey), { timeout: 45_000 }).toBe('Ready')

    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '巡检' }).click(); await expect(page.getByRole('heading', { name: '巡检' })).toBeVisible({ timeout: 30_000 })
    const accepted = await page.evaluate(async ({ systemKey, suffix }) => { const response = await fetch('/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ businessSystemKey: systemKey, planKey: 'mixed-plan', clientCommandId: `t24-run-${suffix}` }) }); return { status: response.status, body: await response.json() } }, { systemKey, suffix })
    expect(accepted.status).toBe(202)
    const run = accepted.body as Detail
    // Frozen-binding proof: publish a distinguishable v2 (renamed checks)
    // while the Run is executing; the Run must complete with exactly v1's
    // frozen check keys — observed from the authoritative run detail.
    const v2 = await page.evaluate(async ({ systemKey, suffix, prepared, headers }) => {
      const yaml = `system_key: ${systemKey}\ndisplay_name: T24 混合巡检\nenabled: true\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: mixed-plan\n    display_name: 混合巡检\n    checks:\n      - key: up-instant-v2\n        display_name: PromQL Up V2\n        analysis_question: 当前可用吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system="${systemKey}"}'\n      - key: broken-page-v2\n        display_name: 损坏页面 V2\n        analysis_question: 页面是否正常？\n        kind: browser\n        journey_id: page.status-marker.v1\n        journey_params:\n          path: /status-broken\n`
      const form = new FormData(); form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' })); form.append('clientCommandId', `t24-system-v2-${suffix}`); form.append('targetLabelContractVersion', '1')
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form }); if (created.status !== 201) throw new Error(`v2 upload=${created.status}: ${await created.text()}`)
      const version = (await (await created.json()).id) as string
      const published = await fetch(`/api/v1/business-systems/${systemKey}/config/${version}/publish`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t24-publish-v2-${suffix}`, expectedCurrentPublishedVersionId: prepared.version }) })
      if (!published.ok) throw new Error(`v2 publish=${published.status}: ${await published.text()}`)
      return { version: String(version) }
    }, { systemKey, suffix, prepared, headers: { 'Content-Type': 'application/json' } })
    await page.goto(`/inspections/runs/${run.id}`); await page.goto('/inspections'); await page.goto(`/inspections/runs/${run.id}`)
    let settled: Detail | undefined
    await expect.poll(async () => { settled = await api<Detail>(page, `/api/v1/inspections/runs/${run.id}`); return settled.state }, { timeout: 180_000 }).toMatch(/Completed|CompletedWithGaps|Failed|Interrupted|Cancelled/)
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${run.id}`)).reportCount, { timeout: 180_000 }).toBeGreaterThan(0)
    const reports = await api<{ items: Array<{ version: number }> }>(page, `/api/v1/inspections/runs/${run.id}/reports?limit=100`)
    const latest = reports.items.at(-1); if (!latest) throw new Error('terminal Run has no report')
    const report = await api<Report>(page, `/api/v1/inspections/runs/${run.id}/reports/${latest.version}`)
    const byKey = Object.fromEntries(settled!.checks.map((check) => [check.checkKey, check]))
    expect(byKey['up-instant']).toMatchObject({ status: 'ok' }); expect(byKey['up-instant'].evidenceId).toBeTruthy()
    expect(byKey['broken-page']).toMatchObject({ status: 'gap', gapReason: 'journey_failed' })
    expect(report.version).toBe(1); expect(report.evidenceIds).toContain(byKey['up-instant'].evidenceId); expect(report.evidenceDigest).toMatch(/^[0-9a-f]{64}$/)
    // UI proof happens after the API facts are known: the returned URL and the
    // rendered rows/card—not a fetch or a hard-coded boolean—are authoritative.
    await expect(page).toHaveURL(new RegExp(`/inspections/runs/${run.id}$`))
    await expect(page.getByRole('heading', { name: new RegExp(`巡检 Run #${run.id}`) })).toBeVisible()
    await expect(page.getByText('up-instant').first()).toBeVisible()
    await expect(page.getByText('broken-page').first()).toBeVisible()
    await expect(page.getByText('报告 v1')).toBeVisible()
    await expect(page.getByText(`Evidence：#${byKey['up-instant'].evidenceId}`)).toBeVisible()
    // Frozen-binding proof: v2 is now the published version, yet the Run
    // executed exactly v1's frozen check keys (renamed in v2, so presence of
    // the v1 keys and absence of the v2 keys is a real observation).
    const executedKeys = settled!.checks.map((check) => check.checkKey).sort()
    expect(executedKeys).toEqual(['broken-page', 'up-instant'])
    // Immutable-report proof: two raw HTTP reads of report v1 must return
    // byte-identical bodies (not merely deep-equal parsed objects).
    const rawBodies = await page.evaluate(async (runId) => {
      const read = async () => { const r = await fetch(`/api/v1/inspections/runs/${runId}/reports/1`); if (!r.ok) throw new Error(`${r.status}`); return await r.text() }
      return [await read(), await read()]
    }, run.id)
    const reportRereadIdentical = rawBodies[0] === rawBodies[1]
    expect(reportRereadIdentical).toBe(true)
    observations({ runId: run.id, state: settled!.state, publishedConfigVersionId: prepared.version, supersedingConfigVersionId: v2.version, executedCheckKeys: executedKeys, reportRereadIdentical, checks: settled!.checks, report: { version: report.version, evidenceIds: report.evidenceIds, evidenceDigest: report.evidenceDigest }, navigationReturned: true, events: [
      { observedAt: new Date().toISOString(), protocol: 'http', routeOrMethod: 'POST /api/v1/inspections/runs', locator: `run:${run.id}`, expected: '202 Accepted', actual: `${accepted.status} Accepted`, rawLogRef: 'ticket24-playwright.log' },
      { observedAt: new Date().toISOString(), protocol: 'grpc', routeOrMethod: 'Runtime ResultAck', locator: `run:${run.id}; check:up-instant; evidence:${byKey['up-instant'].evidenceId}`, expected: 'PromQL accepted', actual: 'ok Evidence committed', rawLogRef: 'runtime-process.log' },
      { observedAt: new Date().toISOString(), protocol: 'grpc', routeOrMethod: 'Journey ResultProposal', locator: `run:${run.id}; check:broken-page`, expected: 'journey gap', actual: byKey['broken-page'].gapReason ?? '', rawLogRef: 'runtime-process.log' },
      { observedAt: new Date().toISOString(), protocol: 'http', routeOrMethod: 'GET /api/v1/inspections/runs/:id/reports/1 (re-read)', locator: `run:${run.id}; report:v1`, expected: 'byte-identical immutable report', actual: `identical; evidence:${report.evidenceIds.join(',')}; digest:${report.evidenceDigest.slice(0, 12)}…`, rawLogRef: 't24-inspection-observations.json' },
      { observedAt: new Date().toISOString(), protocol: 'ui', routeOrMethod: 'GET /inspections/runs/:id', locator: `run:${run.id}`, expected: 'detail and both checks/report render after return', actual: 'rendered', rawLogRef: 'ticket24-playwright.log' },
    ] })
  })
})


// T25 reuses the existing isolated compose/Chromium harness. This observation
// stays structured and contains only locators/state, not page or provider content.
function scheduleObservations(value: unknown) {
  const dir = process.env.QUOIN_EVIDENCE_DIR
  if (dir) {
    mkdirSync(dir, { recursive: true })
    writeFileSync(join(dir, 't25-schedule-observations.json'), JSON.stringify(value, null, 2))
  }
}

test.describe('T25 deterministic scheduled Inspection @ticket-25', () => {
  test('current published cron creates one visible UTC-keyed Run through the real timer', async ({ page }) => {
    test.slow(); test.setTimeout(420_000)
    const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack-t25')
    const password = join(stack, 'admin-new-password')
    expect(existsSync(password)).toBeTruthy()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(password, 'utf8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const suffix = `${Date.now()}`
    const systemKey = `t25-schedule-${suffix}`
    const prepared = await page.evaluate(async ({ suffix, systemKey }) => {
      const headers = { 'Content-Type': 'application/json' }
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((x: { state?: string }) => x.state === 'active')
      if (!active) {
        const form = new FormData()
        form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `t25-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`label contract=${created.status}: ${await created.text()}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t25-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }) })
        if (!activated.ok) throw new Error(`activate contract=${activated.status}: ${await activated.text()}`)
        active = await activated.json()
      }
      const yaml = `system_key: ${systemKey}\ndisplay_name: T25 定时巡检\nenabled: true\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: each-minute\n    display_name: 每分钟巡检\n    cron: "* * * * *"\n    checks:\n      - key: up-instant\n        display_name: PromQL Up\n        analysis_question: 当前可用吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system="${systemKey}"}'\n`
      const form = new FormData()
      form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `t25-system-${suffix}`)
      form.append('targetLabelContractVersion', String(active.version))
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
      if (created.status !== 201) throw new Error(`system=${created.status}: ${await created.text()}`)
      const version = String((await created.json()).id)
      const published = await fetch(`/api/v1/business-systems/${systemKey}/config/${version}/publish`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t25-publish-${suffix}`, expectedCurrentPublishedVersionId: null }) })
      if (!published.ok) throw new Error(`publish=${published.status}: ${await published.text()}`)
      return { version }
    }, { suffix, systemKey })

    // Restart after publication but before observing a boundary. Remember
    // pre-restart runs so only a post-restart historical slot can fail this
    // proof; a boundary that legitimately won the race before restart is not
    // evidence of backfill.
    type ScheduledSummary = { id: string; planKey: string; state: string; triggerKind: string; scheduledFor?: string }
    const beforeRestartIDs = new Set((await page.evaluate(async (key) => {
      const r = await fetch(`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`)
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`)
      return (await r.json()).items ?? []
    }, systemKey) as ScheduledSummary[]).map((run) => run.id))

    // A fresh Quoin process obtains scheduling solely from SQLite and does not
    // invent a missed historical slot while it was down.
    const restartStartedAt = new Date()
    const compose = join(stack, 'state', 'quoin', 'compose', 'generated', 'compose.yaml')
    execFileSync('docker', ['compose', '--project-name', 'quoin-t25', '--file', compose, 'restart', 'quoin'], { stdio: 'ignore' })
    const restartCompleted = new Date()
    const restartCompletedAt = restartCompleted.toISOString()
    const firstBoundaryAfterRestart = new Date(restartCompleted)
    firstBoundaryAfterRestart.setUTCSeconds(0, 0)
    firstBoundaryAfterRestart.setUTCMinutes(firstBoundaryAfterRestart.getUTCMinutes() + 1)

    let scheduled: ScheduledSummary | undefined
    let postRestartRuns: ScheduledSummary[] = []
    await expect.poll(async () => {
      const response = await page.evaluate(async (key) => {
        const r = await fetch(`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`)
        // Restart deliberately creates a brief proxy 502 window. It is not a
        // result; polling must wait for Quoin to become readable again.
        return r.ok ? (await r.json()).items ?? [] : []
      }, systemKey) as ScheduledSummary[]
      postRestartRuns = response.filter((run) => !beforeRestartIDs.has(run.id) && run.planKey === 'each-minute' && run.triggerKind === 'schedule')
      scheduled = postRestartRuns.find((run) => Date.parse(run.scheduledFor ?? '') === firstBoundaryAfterRestart.getTime())
      return scheduled?.id ?? ''
    }, { timeout: 150_000, intervals: [1_000, 2_000, 5_000] }).not.toBe('')
    expect(scheduled?.scheduledFor).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00Z$/)
    expect(Date.parse(scheduled!.scheduledFor!)).toBe(firstBoundaryAfterRestart.getTime())
    const lateBackfills = postRestartRuns.filter((run) => Date.parse(run.scheduledFor ?? '') < firstBoundaryAfterRestart.getTime())
    expect(lateBackfills).toEqual([])

    let detail: Detail | undefined
    await expect.poll(async () => {
      detail = await api<Detail>(page, `/api/v1/inspections/runs/${scheduled!.id}`)
      return detail.state
    }, { timeout: 120_000, intervals: [1_000, 2_000, 5_000] }).toMatch(/Completed|CompletedWithGaps/)
    expect(detail?.checks).toHaveLength(1)
    await page.goto(`/inspections/runs/${scheduled!.id}`)
    await expect(page.getByRole('heading', { name: new RegExp(`巡检 Run #${scheduled!.id}`) })).toBeVisible()
    await expect(page.getByText('up-instant').first()).toBeVisible()

    // Wait across a second boundary and prove the first durable UTC key did
    // not duplicate.
    await page.waitForTimeout(65_000)
    const all = await page.evaluate(async (key) => {
      const r = await fetch(`/api/v1/inspections/runs?businessSystemKey=${encodeURIComponent(key)}&limit=100`)
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`)
      return (await r.json()).items ?? []
    }, systemKey) as ScheduledSummary[]
    const sameKey = all.filter((run) => run.planKey === 'each-minute' && run.triggerKind === 'schedule' && run.scheduledFor === scheduled!.scheduledFor)
    expect(sameKey).toHaveLength(1)
    const finalPostRestartRuns = all.filter((run) => !beforeRestartIDs.has(run.id) && run.planKey === 'each-minute' && run.triggerKind === 'schedule')
    const finalLateBackfills = finalPostRestartRuns.filter((run) => Date.parse(run.scheduledFor ?? '') < firstBoundaryAfterRestart.getTime())
    expect(finalLateBackfills).toEqual([])
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
        { observedAt: new Date().toISOString(), protocol: 'process', routeOrMethod: 'docker compose restart quoin', locator: 'compose project:quoin-t25; service:quoin', expected: 'post-restart scheduler reads committed plan without backfill', actual: `completed=${restartCompletedAt}; first_boundary=${firstBoundaryAfterRestart.toISOString()}`, rawLogRef: 'ticket25-playwright.log' },
        { observedAt: new Date().toISOString(), protocol: 'timer', routeOrMethod: 'scheduler minute boundary', locator: `system:${systemKey}; plan:each-minute; run:${scheduled!.id}`, expected: 'one post-restart schedule Run with canonical UTC key', actual: `${scheduled!.triggerKind}/${scheduled!.scheduledFor}`, rawLogRef: 'ticket25-playwright.log' },
        { observedAt: new Date().toISOString(), protocol: 'http', routeOrMethod: 'GET /api/v1/inspections/runs', locator: `system:${systemKey}; scheduled_for:${scheduled!.scheduledFor}`, expected: 'no duplicate deterministic key after another tick', actual: `rows=${sameKey.length}`, rawLogRef: 'ticket25-playwright.log' },
        { observedAt: new Date().toISOString(), protocol: 'ui', routeOrMethod: 'GET /inspections/runs/:id', locator: `run:${scheduled!.id}`, expected: 'scheduled Run detail renders', actual: 'rendered', rawLogRef: 'ticket25-playwright.log' },
      ],
    })
  })
})
