import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

test.use({ trace: 'off', screenshot: 'off', video: 'off' })

type Check = { checkKey: string; status: string; evidenceId?: string }
type AnalysisStatus = { id: string; state: string; terminationReason?: string }
type Detail = { id: string; state: string; rowVersion: number; createdAt: string; checks: Check[]; reportCount: number; latestAnalysis?: AnalysisStatus }
type Report = { version: number; evidenceIds: string[] }
type FixtureHits = { queries: Record<string, number> }
type RunBinding = { id: number; created_at: string; config_version_id: number; label_contract_version_id: number; plan_key: string; rerun_of_id: number | null }

async function api<T>(page: Page, url: string, init?: RequestInit): Promise<T> {
  return page.evaluate(async ({ url, init }) => {
    const response = await fetch(url, init)
    if (!response.ok) throw new Error(`${url}: ${response.status} ${await response.text()}`)
    return await response.json()
  }, { url, init })
}

async function thanosHits(): Promise<FixtureHits> {
  const response = await fetch('http://127.0.0.1:18448/hits')
  if (!response.ok) throw new Error(`fixture /hits: ${response.status}`)
  return await response.json() as FixtureHits
}

// This is read-only SQLite observation of the real Quoin process's durable
// records; fixture code never writes product tables.
function runBindings(ids: string[]): RunBinding[] {
  const mountJSON = execFileSync('docker', ['inspect', '--format', '{{json .Mounts}}', 'quoin-t26-quoin-1'], { encoding: 'utf8' }).trim()
  const mounts = JSON.parse(mountJSON) as Array<{ Source: string; Destination: string }>
  const dataDirectory = mounts.find((mount) => mount.Destination.includes('data'))?.Source
  if (!dataDirectory) throw new Error(`T26 Quoin data mount is unavailable: ${mountJSON}`)
  const database = join(dataDirectory, 'quoin.db')
  const program = 'import json,sqlite3,sys; c=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro", uri=True); c.row_factory=sqlite3.Row; q="SELECT id,created_at,config_version_id,label_contract_version_id,plan_key,rerun_of_id FROM inspection_runs WHERE id IN ("+",".join("?" for _ in json.loads(sys.argv[2]))+") ORDER BY id"; print(json.dumps([dict(r) for r in c.execute(q, json.loads(sys.argv[2]))]))'
  return JSON.parse(execFileSync('python3', ['-c', program, database, JSON.stringify(ids)], { encoding: 'utf8' })) as RunBinding[]
}

function observations(value: unknown) {
  const dir = process.env.QUOIN_EVIDENCE_DIR
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, 't26-inspection-observations.json'), JSON.stringify(value, null, 2))
}

// T26 uses the established real Chromium/Runtime stack. It deliberately makes
// the distinct user actions through the public HTTP and rendered UI paths; no
// fixture writes product tables.
test.describe('T26 cancel, reanalyze, and recollect Inspection @ticket-26', () => {
  test('keeps reports immutable, creates a fresh evidence chain, and remains readable across Runtime restart', async ({ page }) => {
    test.slow(); test.setTimeout(600_000)
    const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack-t26')
    const password = join(stack, 'admin-new-password')
    expect(existsSync(password)).toBeTruthy()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(password, 'utf8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const suffix = `${Date.now()}`
    const systemKey = `t26-retry-${suffix}`
    await page.evaluate(async ({ systemKey, suffix }) => {
      const headers = { 'Content-Type': 'application/json' }
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((item: { state?: string }) => item.state === 'active')
      if (!active) {
        const form = new FormData()
        form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `t26-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`contract=${created.status}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t26-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }) })
        if (!activated.ok) throw new Error(`activate=${activated.status}`)
      }
      const yaml = `system_key: ${systemKey}\ndisplay_name: T26 重试巡检\nenabled: true\ntimezone: UTC\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: retry-plan\n    display_name: 重试巡检\n    checks:\n      - key: up\n        display_name: Up\n        analysis_question: 可用吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'up{business_system="${systemKey}"}'\n  - key: cancel-plan\n    display_name: 取消中的巡检\n    checks:\n      - key: slow\n        display_name: Slow\n        analysis_question: 已取消吗？\n        kind: promql\n        query:\n          mode: instant\n          expression: 'slow{business_system="${systemKey}"}'\n`
      const form = new FormData()
      form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `t26-system-${suffix}`)
      form.append('targetLabelContractVersion', String(active.version))
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
      if (created.status !== 201) throw new Error(`system=${created.status}: ${await created.text()}`)
      const version = String((await created.json()).id)
      const published = await fetch(`/api/v1/business-systems/${systemKey}/config/${version}/publish`, { method: 'POST', headers, body: JSON.stringify({ clientCommandId: `t26-publish-${suffix}`, expectedCurrentPublishedVersionId: null }) })
      if (!published.ok) throw new Error(`publish=${published.status}: ${await published.text()}`)
    }, { systemKey, suffix })

    const create = async (command: string, planKey = 'retry-plan') => await api<Detail>(page, '/api/v1/inspections/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ businessSystemKey: systemKey, planKey, clientCommandId: command }) })
    const source = await create(`t26-source-${suffix}`)
    let closed: Detail | undefined
    await expect.poll(async () => { closed = await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`); return closed.state }, { timeout: 180_000 }).toMatch(/Completed|CompletedWithGaps/)
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`)).reportCount, { timeout: 180_000 }).toBe(1)
    const firstReport = await api<Report>(page, `/api/v1/inspections/runs/${source.id}/reports/1`)

    const sourceQuery = `up{business_system="${systemKey}"}`
    const hitsBeforeReanalysis = await thanosHits()
    await page.goto(`/inspections/runs/${source.id}`)
    await page.getByRole('button', { name: '重新分析' }).click()
    await expect(page.getByText('已受理重新分析，正在复用本 Run 已收集的 Evidence 生成新报告版本。')).toBeVisible()
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`)).reportCount, { timeout: 180_000 }).toBe(2)
    const analyzedDetail = await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`)
    const analysis = analyzedDetail.latestAnalysis
    expect(analysis?.id).toBeTruthy()
    expect(analysis?.state).toBe('Succeeded')
    const secondReport = await api<Report>(page, `/api/v1/inspections/runs/${source.id}/reports/2`)
    expect(secondReport.evidenceIds).toEqual(firstReport.evidenceIds)
    const hitsAfterReanalysis = await thanosHits()
    expect(hitsAfterReanalysis.queries[sourceQuery] ?? 0).toBe(hitsBeforeReanalysis.queries[sourceQuery] ?? 0)

    // This re-analysis reaches an accepted Plinth worker before the UI cancel:
    // the model fixture's live HTTP request must observe request-context
    // cancellation, and the authoritative Run detail must reach Cancelled.
    await page.goto(`/inspections/runs/${source.id}`)
    const providerLog = join(process.env.QUOIN_EVIDENCE_DIR!, 'fixture-provider.log')
    const delayedBefore = existsSync(providerLog) ? (readFileSync(providerLog, 'utf8').match(/stream delayed: model=fixture-chat-1/g) ?? []).length : 0
    await page.getByRole('button', { name: '重新分析' }).click()
    await expect(page.getByRole('button', { name: '取消进行中的分析' })).toBeVisible({ timeout: 30_000 })
    await expect.poll(() => existsSync(providerLog) ? (readFileSync(providerLog, 'utf8').match(/stream delayed: model=fixture-chat-1/g) ?? []).length : 0, { timeout: 30_000 }).toBeGreaterThan(delayedBefore)
    await page.getByRole('button', { name: '取消进行中的分析' }).click()
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`)).latestAnalysis?.state, { timeout: 30_000 }).toBe('Cancelled')
    await expect.poll(() => existsSync(providerLog) && readFileSync(providerLog, 'utf8').includes('stream cancelled: model=fixture-chat-1'), { timeout: 30_000 }).toBe(true)
    const cancelledAnalysis = await api<Detail>(page, `/api/v1/inspections/runs/${source.id}`)
    expect(cancelledAnalysis.reportCount).toBe(2)

    await page.goto(`/inspections/runs/${source.id}`)
    await expect(page.getByRole('button', { name: '重新采证' })).toBeVisible()
    await page.getByRole('button', { name: '重新采证' }).click()
    await expect.poll(() => page.url(), { timeout: 30_000 }).not.toContain(`/inspections/runs/${source.id}`)
    await expect(page).toHaveURL(/\/inspections\/runs\/\d+$/)
    const recollectedID = page.url().split('/').at(-1)!
    expect(recollectedID).not.toBe(source.id)
    const recollected = await api<Detail>(page, `/api/v1/inspections/runs/${recollectedID}`)
    expect(Date.parse(recollected.createdAt)).toBeGreaterThanOrEqual(Date.parse(source.createdAt))
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${recollectedID}`)).state, { timeout: 180_000 }).toMatch(/Completed|CompletedWithGaps/)
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${recollectedID}`)).reportCount, { timeout: 180_000 }).toBe(1)
    const recollectedReport = await api<Report>(page, `/api/v1/inspections/runs/${recollectedID}/reports/1`)
    expect(recollectedReport.evidenceIds).not.toEqual(firstReport.evidenceIds)
    const bindings = runBindings([source.id, recollectedID])
    expect(bindings).toHaveLength(2)
    expect(bindings[1]).toMatchObject({ rerun_of_id: Number(source.id), config_version_id: bindings[0].config_version_id, label_contract_version_id: bindings[0].label_contract_version_id, plan_key: bindings[0].plan_key })
    expect(Date.parse(bindings[1].created_at)).toBeGreaterThanOrEqual(Date.parse(bindings[0].created_at))

    const cancelled = await create(`t26-cancel-${suffix}`, 'cancel-plan')
    await page.goto(`/inspections/runs/${cancelled.id}`)
    await page.getByRole('button', { name: '取消巡检' }).click()
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${cancelled.id}`)).state, { timeout: 30_000 }).toBe('Cancelled')
    const thanosLog = join(process.env.QUOIN_EVIDENCE_DIR!, 'fixture-thanos.log')
    await expect.poll(() => existsSync(thanosLog) && readFileSync(thanosLog, 'utf8').includes('slow query cancelled'), { timeout: 30_000 }).toBe(true)
    const cancelResult = await api<Detail>(page, `/api/v1/inspections/runs/${cancelled.id}`)

    // Restart while a deliberately slow PromQL worker is live. This proves the
    // persisted Run converges after the Runtime connection is interrupted, not
    // merely that already-terminal history remains readable.
    const slowQuery = `slow{business_system="${systemKey}"}`
    const hitsBeforeInterrupt = await thanosHits()
    const interrupted = await create(`t26-interrupt-${suffix}`, 'cancel-plan')
    await page.goto(`/inspections/runs/${interrupted.id}`)
    await expect(page.getByRole('button', { name: '取消巡检' })).toBeVisible()
    await expect.poll(async () => (await thanosHits()).queries[slowQuery] ?? 0, { timeout: 30_000 }).toBeGreaterThan(hitsBeforeInterrupt.queries[slowQuery] ?? 0)
    const compose = join(stack, 'state', 'quoin', 'compose', 'generated', 'compose.yaml')
    execFileSync('docker', ['compose', '--project-name', 'quoin-t26', '--file', compose, 'restart', 'plinth'], { stdio: 'ignore' })
    await expect.poll(async () => (await api<Detail>(page, `/api/v1/inspections/runs/${interrupted.id}`)).state, { timeout: 90_000 }).toBe('Interrupted')
    await page.reload()
    await expect(page.getByText('已中断', { exact: true })).toBeVisible()
    // A separate Quoin restart checks that terminal history stays readable
    // without incorrectly treating same-boot Runtime recovery as interruption.
    execFileSync('docker', ['compose', '--project-name', 'quoin-t26', '--file', compose, 'restart', 'quoin'], { stdio: 'ignore' })
    await expect.poll(async () => page.evaluate(async (id) => {
      const response = await fetch(`/api/v1/inspections/runs/${id}`)
      if (!response.ok) return ''
      return (await response.json() as { id: string }).id
    }, source.id), { timeout: 90_000 }).toBe(source.id)
    await page.goto(`/inspections/runs/${source.id}`)
    await expect(page.getByRole('heading', { name: new RegExp(`巡检 Run #${source.id}`) })).toBeVisible()

    observations({ sourceRunId: source.id, reanalysisAttemptId: analysis.id, sourceEvidenceIds: firstReport.evidenceIds, reanalysisEvidenceIds: secondReport.evidenceIds, thanosHitsBeforeReanalysis: hitsBeforeReanalysis.queries[sourceQuery] ?? 0, thanosHitsAfterReanalysis: hitsAfterReanalysis.queries[sourceQuery] ?? 0, cancelledAnalysisAttemptId: cancelledAnalysis.latestAnalysis?.id, cancelledAnalysisState: cancelledAnalysis.latestAnalysis?.state, recollectedRunId: recollectedID, recollectedEvidenceIds: recollectedReport.evidenceIds, sqliteRunBindings: bindings, sourceCreatedAt: source.createdAt, recollectedCreatedAt: recollected.createdAt, cancelledRunId: cancelled.id, cancelledState: cancelResult.state, interruptedRunId: interrupted.id, restartReadableRunId: source.id, events: [
{ protocol: 'ui/http', routeOrMethod: 'Run detail 重新分析', expected: 'new report version with existing Evidence only and no additional PromQL collection', actual: `v1=${firstReport.evidenceIds.join(',')}; v2=${secondReport.evidenceIds.join(',')}; thanos-hits=${hitsBeforeReanalysis.queries[sourceQuery] ?? 0}->${hitsAfterReanalysis.queries[sourceQuery] ?? 0}` },
        { protocol: 'ui/http/grpc', routeOrMethod: 'Run detail 取消进行中的分析', expected: 'accepted report worker receives Runtime cancel and reaches Cancelled', actual: `attempt=${cancelledAnalysis.latestAnalysis?.id}; state=${cancelledAnalysis.latestAnalysis?.state}; model-provider=stream-context-cancelled` },
       { protocol: 'ui/http/sqlite', routeOrMethod: 'Run detail 重新采证', expected: 'new Run, fresh Evidence, later time, and frozen source bindings', actual: `source=${source.id}:${firstReport.evidenceIds.join(',')}; recollected=${recollectedID}:${recollectedReport.evidenceIds.join(',')}; bindings=${JSON.stringify(bindings)}` },
      { protocol: 'ui/http/grpc', routeOrMethod: 'Run detail 取消巡检', expected: 'durable cancellation fence reaches the live Runtime PromQL worker', actual: `run=${cancelled.id}; state=${cancelResult.state}; thanos=slow-query-context-cancelled` },
       { protocol: 'process/grpc', routeOrMethod: 'docker compose restart plinth during active Run', expected: 'new Runtime boot interrupts the active persisted Run', actual: `interrupted=${interrupted.id}:Interrupted` },
       { protocol: 'process/ui', routeOrMethod: 'docker compose restart quoin + Run detail', expected: 'terminal history remains readable after Quoin restart', actual: `preserved=${source.id}` },
    ] })
  })
})
