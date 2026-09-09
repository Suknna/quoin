import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Page } from '@playwright/test'

// T23 runs without trace/screenshot/video: the journey path drives the same
// authenticated profile a human published, and Browser policy forbids
// retaining page content, traces, or input recordings.
test.use({ trace: 'off', screenshot: 'off', video: 'off' })

const ticket = 'T23'
const browserFixture = `quoin-${ticket.toLowerCase()}-auth-fixture`
const browserFixtureHost = `${browserFixture}:8081`

function fixtureCounter(name: string): number {
  try {
    return Number.parseInt(execFileSync('docker', ['exec', browserFixture, 'cat', `/state/${name}`], { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] }).trim(), 10) || 0
  } catch { return 0 }
}

function fixtureState(name: string): boolean {
  try {
    execFileSync('docker', ['exec', browserFixture, 'test', '-f', `/state/${name}`], { stdio: 'ignore' })
    return true
  } catch { return false }
}

type CheckResult = { planKey: string, checkKey: string, status: 'ok' | 'error' | 'gap', evidenceId?: string, gapReason?: string, gapDetail?: string }
type RunDetail = { id: string, state: string, checkResults: CheckResult[], resultDetail?: string }

async function runDetail(page: Page, systemKey: string, versionId: string, runId: string): Promise<RunDetail> {
  return await page.evaluate(async ({ key, version, run }) => {
    return await (await fetch(`/api/v1/business-systems/${key}/config/${version}/verifications/${run}`)).json()
  }, { key: systemKey, version: versionId, run: runId })
}

test.describe('T23 Config Verification browser Journeys @ticket-23', () => {
  test('Published-config browser checks execute real Journeys end to end', async ({ page }) => {
    const serverFailures: string[] = []
    page.on('response', (response) => {
      if (response.url().includes('/api/') && response.status() >= 500) {
        void response.text().then((body) => serverFailures.push(`${response.status()} ${response.url()} ${body}`))
      }
    })
    test.slow()
    // The full chain (manual login over RFB, two serialized journeys, the
    // auth-gap run, stop fences, then the UI slice) exceeds the default
    // tripled budget on cold stacks.
    test.setTimeout(600_000)
    const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', `e2e-stack-${ticket.toLowerCase()}`)
    const passwordPath = join(stackDir, 'admin-new-password')
    expect(existsSync(passwordPath)).toBeTruthy()

    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(passwordPath, 'utf-8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const happySystem = `t23-journey-${Date.now()}`
    const authSystem = `t23-auth-${Date.now()}`
    const prepared = await page.evaluate(async ({ happy, auth, fixtureHost }) => {
      const headers = { 'Content-Type': 'application/json' }
      const suffix = `${Date.now()}`
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((item: { state?: string }) => item.state === 'active')
      if (!active) {
        const form = new FormData()
        form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `e2e-t23-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`create label contract=${created.status}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t23-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }),
        })
        if (!activated.ok) throw new Error(`activate label contract=${activated.status}`)
      }
      const systemYAML = (key: string, name: string) => `system_key: ${key}\ndisplay_name: ${name}\nenabled: true\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans:\n  - key: browser-plan\n    display_name: 浏览器验证\n    checks:\n      - key: status-page\n        display_name: 状态页\n        analysis_question: 状态页是否正常?\n        kind: browser\n        journey_id: page.status-marker.v1\n        journey_params:\n          path: /status\n      - key: broken-page\n        display_name: 异常状态页\n        analysis_question: 异常页是否被正确识别?\n        kind: browser\n        journey_id: page.status-marker.v1\n        journey_params:\n          path: /status-broken\n`
      const upload = async (key: string, name: string) => {
        const form = new FormData()
        form.append('file', new File([systemYAML(key, name)], 'system.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `e2e-t23-system-${key}-${suffix}`)
        form.append('targetLabelContractVersion', String(active.version))
        const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`create ${key}=${created.status}: ${await created.text()}`)
        const versions = await (await fetch(`/api/v1/business-systems/${key}/config?limit=1`)).json() as { items?: Array<{ id: string }> }
        const versionId = versions.items?.[0]?.id
        if (!versionId) throw new Error(`${key} upload returned no draft version`)
        return versionId
      }
      const identity = async (key: string, name: string) => {
        const response = await fetch(`/api/v1/business-systems/${key}/browser-identity`, {
          method: 'POST', headers,
          body: JSON.stringify({
            clientCommandId: `e2e-t23-identity-${key}-${suffix}`,
            name,
            startUrl: `http://${fixtureHost}/login`,
            authenticationProbe: { journeyId: 'authentication.url-prefix.v1', journeyVersion: 1, params: { authenticatedUrlPrefix: `http://${fixtureHost}/authenticated` } },
          }),
        })
        if (response.status !== 202) throw new Error(`configure identity ${key}=${response.status}: ${await response.text()}`)
        return await response.json()
      }
      return {
        happyVersion: await upload(happy, 'T23 浏览器验证'),
        authVersion: await upload(auth, 'T23 未登录验证'),
        happyIdentity: await identity(happy, 'T23 只读浏览器账号'),
        authIdentity: await identity(auth, 'T23 未登录账号'),
      }
    }, { happy: happySystem, auth: authSystem, fixtureHost: browserFixtureHost })
    expect(prepared.happyIdentity.identity.state).toBe('AuthenticationRequired')

    // Publish the happy system's profile through the real Operator path:
    // manual login workbench, noVNC RFB input, publish probe.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await page.getByRole('button', { name: 'T23 浏览器验证' }).click()
    await expect(page.getByRole('button', { name: '打开浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '打开浏览器登录' }).click()
    await expect(page.getByRole('heading', { name: '浏览器登录' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '重新登录' }).click()
    await expect.poll(async () => page.evaluate(async (key) => {
      const identity = await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()
      return identity.currentOperation
    }, happySystem), { timeout: 60_000 }).toMatchObject({ state: 'Running', canAttach: true, canPublish: true })
    const canvas = page.locator('.browser-login-viewport canvas')
    await expect(canvas).toBeVisible({ timeout: 45_000 })
    await expect(page.getByText('安全浏览器已连接。选择“进入远程浏览器”后开始输入。')).toBeVisible({ timeout: 45_000 })
    await expect.poll(() => fixtureCounter('ready-seq') > 0, { timeout: 45_000 }).toBe(true)
    await page.waitForTimeout(2_000)
    await expect(page.getByRole('button', { name: '完成登录并发布' })).toBeEnabled({ timeout: 45_000 })
    const canvasBounds = await canvas.boundingBox()
    if (canvasBounds === null) throw new Error('noVNC canvas is not measurable')
    // Preserve each input-plane milestone: observing only the final authenticated
    // page cannot distinguish an RFB focus/pointer/key loss from fixture failure.
    const pointerBaseline = fixtureCounter('pointerdown-seq')
    const keyBaseline = fixtureCounter('keydown-seq')
    const submitBaseline = fixtureCounter('submit-seq')
    await page.getByRole('button', { name: '进入远程浏览器' }).click()
    await expect(canvas.evaluate((element) => document.activeElement === element)).resolves.toBe(true)
    await canvas.click({ position: { x: canvasBounds.width / 2, y: canvasBounds.height / 2 } })
    await expect.poll(() => fixtureCounter('pointerdown-seq') > pointerBaseline, { timeout: 20_000 }).toBe(true)
    await canvas.focus()
    await page.keyboard.press('Enter')
    await expect.poll(() => fixtureCounter('keydown-seq') > keyBaseline, { timeout: 20_000 }).toBe(true)
    await expect.poll(() => fixtureCounter('submit-seq') > submitBaseline && fixtureState('authenticated') && fixtureState('authenticated-page'), { timeout: 20_000 }).toBe(true)
    await page.getByRole('button', { name: '完成登录并发布' }).click()
    await expect
      .poll(async () => {
        if (await page.locator('.error-summary').count() > 0) {
          throw new Error(`发布失败：${await page.locator('.error-summary').textContent()}`)
        }
        return page.url().endsWith('/browser-login') ? 'stayed' : 'navigated'
      }, { timeout: 45_000 })
      .toBe('navigated')
    await expect.poll(async () => page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).state, happySystem), { timeout: 45_000 }).toBe('Ready')

    // --- Case 1+2: one run carries the happy and the failing Journey. -----
    const statusBaseline = fixtureCounter('status-seq')
    const brokenBaseline = fixtureCounter('status-broken-seq')
    const happyRun = await page.evaluate(async ({ key, version }) => {
      const response = await fetch(`/api/v1/business-systems/${key}/config/${version}/verifications`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `e2e-t23-run-${Date.now()}`, purpose: 'prepublish' }),
      })
      if (response.status !== 202) throw new Error(`run verification=${response.status}: ${await response.text()}`)
      return await response.json()
    }, { key: happySystem, version: prepared.happyVersion }) as RunDetail
    let settled: RunDetail | undefined
    await expect.poll(async () => {
      settled = await runDetail(page, happySystem, prepared.happyVersion, happyRun.id)
      return settled.state
    }, { timeout: 120_000 }).toMatch(/Passed|Failed|Cancelled|Interrupted/)
    // Persist the observation before any assertion can abort the run: the
    // ticket's evidence must carry the machine facts even of a failing leg.
    const authRunStarted = await page.evaluate(async ({ key, version }) => {
      const response = await fetch(`/api/v1/business-systems/${key}/config/${version}/verifications`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `e2e-t23-auth-run-${Date.now()}`, purpose: 'prepublish' }),
      })
      if (response.status !== 202) throw new Error(`run auth verification=${response.status}: ${await response.text()}`)
      return await response.json()
    }, { key: authSystem, version: prepared.authVersion }) as RunDetail
    const authPoll = expect.poll(async () => (await runDetail(page, authSystem, prepared.authVersion, authRunStarted.id)).state, { timeout: 60_000 }).toMatch(/Passed|Failed|Cancelled|Interrupted/)
    await authPoll
    const authSettled = await runDetail(page, authSystem, prepared.authVersion, authRunStarted.id)
    for (const check of authSettled.checkResults) {
      expect(check, `auth check outcome: ${JSON.stringify(authSettled.checkResults)}`).toMatchObject({ status: 'gap', gapReason: 'authentication_required' })
    }
    writeT23Observations({
      ticket,
      realRuntime: {
        edge: 'https://127.0.0.1:18480 (TLS) → http://127.0.0.1:18080',
        fixture: browserFixtureHost,
        journeyCatalog: 'embedded v1 (page.status-marker.v1, authentication.url-prefix.v1)',
      },
      happyRun: {
        id: happyRun.id,
        state: settled!.state,
        resultDetail: settled!.resultDetail ?? null,
        checks: settled!.checkResults.map((check) => ({ checkKey: check.checkKey, status: check.status, gapReason: check.gapReason ?? null, gapDetail: check.gapDetail ?? null, evidence: check.evidenceId ?? null })),
      },
      authRun: {
        id: authRunStarted.id,
        state: authSettled.state,
        checks: authSettled.checkResults.map((check) => ({ checkKey: check.checkKey, status: check.status, gapReason: check.gapReason ?? null })),
      },
      noWholeRunRetry: { statusSeqDelta: fixtureCounter('status-seq') - statusBaseline, brokenSeqDelta: fixtureCounter('status-broken-seq') - brokenBaseline },
      cleanupClosure: { identityReleased: false },
    })
    const happyChecks = Object.fromEntries((settled!.checkResults ?? []).map((check) => [check.checkKey, check]))
    expect(happyChecks['status-page'], `happy check outcome: ${JSON.stringify(settled!.checkResults)}`).toMatchObject({ status: 'ok' })
    expect(happyChecks['status-page'].evidenceId).toBeTruthy()
    expect(happyChecks['broken-page']).toMatchObject({ status: 'gap', gapReason: 'journey_failed' })
    expect(settled!.state).toBe('Failed')
    // No whole-run or mid-step retry: each fixed Playwright Journey performs
    // exactly one target-page navigation. The separate start-page probes never
    // touch these counters.
    const statusDelta = fixtureCounter('status-seq') - statusBaseline
    const brokenDelta = fixtureCounter('status-broken-seq') - brokenBaseline
    expect(statusDelta, 'happy-page fetches exactly once').toBe(1)
    expect(brokenDelta, 'broken-page fetches exactly once').toBe(1)
    const runCount = await page.evaluate(async ({ key, version }) => {
      const runs = await (await fetch(`/api/v1/business-systems/${key}/config/${version}/verifications?limit=100`)).json() as { items?: unknown[] }
      return runs.items?.length ?? 0
    }, { key: happySystem, version: prepared.happyVersion })
    expect(runCount, 'no second verification run was created').toBe(1)
    await expect.poll(async () => page.evaluate(async (key) => (await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()).currentOperation, happySystem), { timeout: 60_000 }).toBeNull()

    // --- UI slice: the version page renders per-check outcomes. -----------
    await page.goto(`/business-systems/${happySystem}/configs/${prepared.happyVersion}`)
    await page.getByRole('button', { name: '查看结果' }).first().click()
    await expect(page.getByRole('table').getByText('status-page')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已留存 Evidence #').first()).toBeVisible()
    await expect(page.getByText('Journey 步骤失败')).toBeVisible()
    await expect(page.getByText('浏览器身份未登录')).toHaveCount(0)
    expect(serverFailures).toEqual([])

    // Refresh the observation with the post-fence facts.
    writeT23Observations({
      ticket,
      realRuntime: {
        edge: 'https://127.0.0.1:18480 (TLS) → http://127.0.0.1:18080',
        fixture: browserFixtureHost,
        journeyCatalog: 'embedded v1 (page.status-marker.v1, authentication.url-prefix.v1)',
      },
      happyRun: {
        id: happyRun.id,
        state: settled!.state,
        resultDetail: settled!.resultDetail ?? null,
        checks: settled!.checkResults.map((check) => ({ checkKey: check.checkKey, status: check.status, gapReason: check.gapReason ?? null, gapDetail: check.gapDetail ?? null, evidence: check.evidenceId ?? null })),
      },
      authRun: {
        id: authRunStarted.id,
        state: authSettled.state,
        checks: authSettled.checkResults.map((check) => ({ checkKey: check.checkKey, status: check.status, gapReason: check.gapReason ?? null })),
      },
      noWholeRunRetry: { statusSeqDelta: fixtureCounter('status-seq') - statusBaseline, brokenSeqDelta: fixtureCounter('status-broken-seq') - brokenBaseline },
      cleanupClosure: { identityReleased: true },
    })
  })
})

function writeT23Observations(value: unknown) {
  const evidenceDir = process.env.QUOIN_EVIDENCE_DIR
  if (!evidenceDir) return
  mkdirSync(evidenceDir, { recursive: true })
  writeFileSync(join(evidenceDir, 't23-journey-observations.json'), JSON.stringify(value, null, 2))
}
