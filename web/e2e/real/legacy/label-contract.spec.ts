import { execSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// T17 — the Label Contract activation and alert attribution browser path over
// the real compose stack: the readiness view with blockers, the verification
// run evidence, the explicit candidate selection + atomic activation confirm,
// and the live alerts list with business-system attribution and filter.

// T17 uses a freshly named fixture project rather than the shared e2e stack.
// Its non-secret manifest is the only cross-process hand-off; the password
// remains in that manifest's private run directory.
const fixtureManifest = join(import.meta.dirname, '..', '..', '.artifacts', 'tickets', 'T17', 'ticket17-browser-fixture.json')

function readFixture(): { stack: string } {
  if (!existsSync(fixtureManifest)) throw new Error(`T17 browser fixture manifest missing: ${fixtureManifest}`)
  const fixture = JSON.parse(readFileSync(fixtureManifest, 'utf-8')) as { stack?: unknown }
  if (typeof fixture.stack !== 'string' || !fixture.stack.startsWith(join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-t17-'))) {
    throw new Error('T17 browser fixture manifest has an unsafe stack path')
  }
  return { stack: fixture.stack }
}

function readAdminPassword(): string {
  const path = join(readFixture().stack, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

const amBase = 'http://127.0.0.1:19093'

function postAlert(alertname: string, labels: Record<string, string>) {
  const now = new Date().toISOString()
  const body = [{ labels: { alertname, ...labels }, startsAt: now, generatorURL: '' }]
  execSync(
    `curl -sf -X POST -H 'Content-Type: application/json' -d '${JSON.stringify(body).replace(/'/g, `'\\''`)}' ${amBase}/api/v2/alerts`,
  )
}

const contractYAML = 'label_contract:\n  business_system_label: business_system\n'

const enabledZeroCheckYAML = `system_key: payments
display_name: 支付系统
enabled: true
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries: []
inspection_plans: []
`

test.describe('T17 Label Contract 激活与告警归属 @ticket-17', () => {
  test('就绪视图、原子激活与告警归属筛选的真实浏览器路径', async ({ page, browser }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // Pre-contract alert: it must stay 未归属 forever (write-once attribution).
    postAlert('T17Pre', { business_system: 'payments', severity: 'warning' })
    await expect(page.locator('.object-row', { hasText: 'T17Pre' })).toBeVisible({ timeout: 30_000 })

    // Fixture through the real production endpoints from inside the page. The
    // first upload only creates a Disabled aggregate, so the real path is:
    // activate v1 with zero enabled systems → publish enabled v1 → upload v2.
    // This makes payments an enabled-system blocker for v2 exactly as production
    // does; it does not forge rows or skip the publish transition.
    const setup = await page.evaluate(
      async ({ contractYAML, v1YAML, v2YAML }) => {
        const suffix = Date.now()
        async function createContract(command: string) {
          const form = new FormData()
          form.append('file', new File([contractYAML], 'contract.yaml', { type: 'application/yaml' }))
          form.append('clientCommandId', command)
          const response = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
          return { status: response.status, body: await response.json() as { version: number } }
        }
        async function uploadConfig(command: string, yaml: string, target: number) {
          const form = new FormData()
          form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
          form.append('clientCommandId', command)
          form.append('targetLabelContractVersion', String(target))
          const response = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
          return { status: response.status, body: await response.json() as { id: string } }
        }

        const create1 = await createContract(`e2e-t17-contract-1-${suffix}`)
        const activate1 = await fetch(`/api/v1/label-contracts/${create1.body.version}/activate`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            clientCommandId: `e2e-t17-activate-1-${suffix}`,
            expectedStateRowVersion: 1,
            expectedCurrentContractVersionId: null,
            expectedTargetRowVersion: 1,
            compatibleVersions: [],
          }),
        })
        const uploadV1 = await uploadConfig(`e2e-t17-upload-v1-${suffix}`, v1YAML, create1.body.version)
        const publishV1 = await fetch(`/api/v1/business-systems/payments/config/${uploadV1.body.id}/publish`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ clientCommandId: `e2e-t17-publish-v1-${suffix}`, expectedCurrentPublishedVersionId: null }),
        })
        const create2 = await createContract(`e2e-t17-contract-2-${suffix}`)
        const uploadV2 = await uploadConfig(`e2e-t17-upload-v2-${suffix}`, v2YAML, create2.body.version)
        return {
          create1: create1.status,
          activate1: activate1.status,
          uploadV1: uploadV1.status,
          publishV1: publishV1.status,
          create2: create2.status,
          uploadV2: uploadV2.status,
          version: uploadV2.body,
        }
      },
      {
        contractYAML,
        v1YAML: enabledZeroCheckYAML,
        v2YAML: enabledZeroCheckYAML.replace('resource_refresh_interval_seconds: 300', 'resource_refresh_interval_seconds: 301'),
      },
    )
    expect(setup.create1).toBe(201)
    expect(setup.activate1).toBe(200)
    expect(setup.uploadV1).toBe(201)
    expect(setup.publishV1).toBe(200)
    expect(setup.create2).toBe(201)
    expect(setup.uploadV2).toBe(201)
    // --- Readiness view with blockers -------------------------------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await page.getByRole('button', { name: '标签契约' }).click()
    const panel = page.getByRole('dialog', { name: 'Label Contract 与激活就绪' })
    await expect(panel).toBeVisible()
    // v2 is the newest draft and is auto-selected; the system shows the blocker.
    await expect(panel.getByText('逐系统就绪')).toBeVisible({ timeout: 15_000 })
    await expect(panel.getByText(/草稿还没有运行过 Config Verification Run/)).toBeVisible()
    await expect(panel.getByRole('button', { name: '原子激活此契约' })).toBeDisabled()

    // --- Verification run evidence from the version detail -----------------
    await panel.getByRole('button', { name: '前往业务系统' }).click()
    await expect(page.getByRole('heading', { name: '支付系统' })).toBeVisible({ timeout: 15_000 })
    await page.getByRole('button', { name: /v2/ }).click()
    await expect(page.getByRole('heading', { name: /配置版本 v2/ })).toBeVisible({ timeout: 15_000 })
    await page.getByRole('button', { name: '运行测试' }).click()
    await expect(page.getByText('已通过').first()).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText(/Run #1/)).toBeVisible()

    // --- Explicit candidate + atomic activation confirm --------------------
    await page.getByRole('button', { name: '标签契约' }).click()
    await expect(panel.getByText(/配置版本 #\d+ · 验证 Run #\d+（已通过）/)).toBeVisible({ timeout: 15_000 })
    await panel.getByRole('button', { name: '原子激活此契约' }).click()
    const confirm = panel.getByRole('dialog', { name: '确认原子激活' })
    await expect(confirm).toBeVisible()
    await confirm.getByRole('button', { name: '确认原子激活' }).click()
    await expect(panel).toBeHidden({ timeout: 15_000 })

    // The activated system now shows Enabled with its v2 config published.
    await page.getByRole('button', { name: '返回系统详情' }).click()
    await expect(page.getByRole('heading', { name: '支付系统' })).toBeVisible({ timeout: 15_000 })
    await expect(page.locator('.detail-header .status-pill', { hasText: 'Enabled' })).toBeVisible()

    // --- Attribution + live filter behavior -------------------------------
    postAlert('T17Paid', { business_system: 'payments', severity: 'critical' })
    postAlert('T17Ghost', { business_system: 'ghost-system', severity: 'warning' })
    postAlert('T17Bare', { severity: 'info' })

    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '告警' }).click()
    const paidRow = page.locator('.object-row', { hasText: 'T17Paid' }).first()
    await expect(paidRow).toBeVisible({ timeout: 30_000 })
    await expect(paidRow.getByText('payments')).toBeVisible()
    const ghostRow = page.locator('.object-row', { hasText: 'T17Ghost' }).first()
    await expect(ghostRow.getByText('未归属')).toBeVisible()
    const bareRow = page.locator('.object-row', { hasText: 'T17Bare' }).first()
    await expect(bareRow.getByText('未归属')).toBeVisible()
    // The pre-contract alert stays 未归属 (write-once attribution).
    const preRow = page.locator('.object-row', { hasText: 'T17Pre' }).first()
    await expect(preRow.getByText('未归属')).toBeVisible()

    // Combobox filter: only the payments-attributed row remains, in a URL
    // query that survives reload (UI-ROUTE-001).
    await page.getByLabel('按业务系统筛选').click()
    await page.getByRole('option', { name: /支付系统/ }).click()
    await expect(page.locator('.object-row', { hasText: 'T17Ghost' })).toHaveCount(0, { timeout: 15_000 })
    await expect(page.locator('.object-row', { hasText: 'T17Bare' })).toHaveCount(0)
    await expect(page.locator('.object-row', { hasText: 'T17Pre' })).toHaveCount(0)
    await expect(paidRow).toBeVisible()
    await expect(page).toHaveURL(/businessSystemKey=payments/)
    await page.reload()
    await expect(paidRow).toBeVisible({ timeout: 30_000 })
    await expect(page.locator('.object-row', { hasText: 'T17Ghost' })).toHaveCount(0)

    // Live behavior inside the filtered view: a new payments alert arrives
    // over SSE and joins the list without a reload.
    postAlert('T17Paid2', { business_system: 'payments', severity: 'critical' })
    await expect(page.locator('.object-row', { hasText: 'T17Paid2' })).toBeVisible({ timeout: 30_000 })

    // Detail carries the attribution line.
     await paidRow.click()
     await expect(page.getByRole('heading', { name: 'T17Paid' })).toBeVisible()
     await expect(page).toHaveURL(/\/alerts\/\d+\?businessSystemKey=payments/)
     await expect(page.getByText('业务系统 payments')).toBeVisible()

    const evidenceDir = process.env.QUOIN_EVIDENCE_DIR
    if (evidenceDir) {
      mkdirSync(evidenceDir, { recursive: true })
      await page.screenshot({ path: join(evidenceDir, 'ticket17-browser-final.png'), fullPage: true })
      const fixturePath = join(evidenceDir, 'ticket17-browser-fixture.json')
      const fixture = existsSync(fixturePath) ? JSON.parse(readFileSync(fixturePath, 'utf-8')) : null
      const browserEvidence = {
        url: page.url(),
        setup,
        browser: { version: browser.version(), userAgent: await page.evaluate(() => navigator.userAgent) },
        fixture,
        verified: ['readiness blocker', 'verification run', 'atomic activation', 'attribution filter', 'URL reload', 'SSE update', 'detail attribution'],
      }
      writeFileSync(join(evidenceDir, 'ticket17-browser-final.json'), JSON.stringify(browserEvidence, null, 2))
      // The ticket's Go acceptance writes the root evidence first; attach the
      // real Chromium result rather than creating a parallel, competing proof.
      const runtimePath = join(evidenceDir, 'runtime-evidence.json')
      if (existsSync(runtimePath)) {
        const runtime = JSON.parse(readFileSync(runtimePath, 'utf-8'))
        runtime.browserAcceptance = browserEvidence
        writeFileSync(runtimePath, JSON.stringify(runtime, null, 2))
      }
    }
  })
})
