import { readFileSync } from 'node:fs'
import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const browserTicket = process.env.QUOIN_TICKET ?? 'T36'
const browserFixture = `quoin-${browserTicket.toLowerCase()}-auth-fixture`
const browserFixtureHost = `${browserFixture}:8081`

// T36 coordinated upgrade maintenance through the real UI: the Admin
// prepares the upgrade while a real manual-login browser operation is
// running, the deterministic checklist shows the ActiveBrowserOperation as
// blocking work, the frozen drain cancel ends it, the reconciler verifies
// the pre-upgrade backup, and the prepared banner appears before the
// operator aborts the upgrade back to normal service.
test.describe('T36 协调升级维护 @ticket-36', () => {
  test('准备升级、经 UI 取消运行中的浏览器操作、备份验证后展示已准备并退出', async ({ page }) => {
    test.slow()
    const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', `e2e-stack-${browserTicket.toLowerCase()}`)
    const passwordPath = join(stackDir, 'admin-new-password')
    expect(existsSync(passwordPath)).toBeTruthy()

    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(passwordPath, 'utf-8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // One real running manual-login operation is the drainable browser work.
    const systemKey = `t36-browser-${Date.now()}`
    const started = await page.evaluate(async ({ key, fixtureHost }: { key: string, fixtureHost: string }) => {
      const headers = { 'Content-Type': 'application/json' }
      const suffix = `${Date.now()}`
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((item: { state?: string }) => item.state === 'active')
      if (!active) {
        const form = new FormData()
        form.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        form.append('clientCommandId', `e2e-t36-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
        if (created.status !== 201) throw new Error(`create label contract=${created.status}`)
        active = await created.json()
        const activated = await fetch(`/api/v1/label-contracts/${active.version}/activate`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t36-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }),
        })
        if (!activated.ok) throw new Error(`activate label contract=${activated.status}`)
        active = await activated.json()
      }
      const yaml = `system_key: ${key}\ndisplay_name: T36 浏览器验收\nenabled: false\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n`
      const form = new FormData()
      form.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `e2e-t36-system-${suffix}`)
      form.append('targetLabelContractVersion', String(active.version))
      const created = await fetch('/api/v1/business-systems', { method: 'POST', body: form })
      if (created.status !== 201) throw new Error(`create business system=${created.status}: ${await created.text()}`)
      const identity = await fetch(`/api/v1/business-systems/${key}/browser-identity`, {
        method: 'POST', headers,
        body: JSON.stringify({
          clientCommandId: `e2e-t36-identity-${suffix}`,
          name: 'T36 只读浏览器账号',
          startUrl: `http://${fixtureHost}/login`,
          authenticationProbe: { journeyId: 'authentication.url-prefix.v1', journeyVersion: 1, params: { authenticatedUrlPrefix: `http://${fixtureHost}/authenticated` } },
        }),
      })
      if (identity.status !== 202) throw new Error(`configure browser identity=${identity.status}: ${await identity.text()}`)
      const identityDetail = await (await fetch(`/api/v1/business-systems/${key}/browser-identity`)).json()
      const operation = await fetch(`/api/v1/browser-login/${key}/operations`, {
        method: 'POST', headers,
        body: JSON.stringify({ clientCommandId: `e2e-t36-login-${suffix}`, expectedRowVersion: identityDetail.rowVersion }),
      })
      if (operation.status !== 202) throw new Error(`start manual login=${operation.status}: ${await operation.text()}`)
      return { systemKey: key }
    }, { key: systemKey, fixtureHost: browserFixtureHost })
    expect(started.systemKey).toBe(systemKey)

    // The Admin prepares the coordinated upgrade through the normal-mode UI.
    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '维护与升级' }).click()
    await page.getByRole('button', { name: '准备升级维护' }).click()
    await expect(page.getByRole('heading', { name: '升级协调' })).toBeVisible({ timeout: 30_000 })

    // The deterministic checklist shows the running browser operation as
    // blocking work with a real drain affordance.
    const drainRow = page.locator('.maintenance-checklist li', { hasText: '浏览器操作' }).first()
    await expect(drainRow).toBeVisible({ timeout: 30_000 })
    await expect(drainRow.getByText('需要处理')).toBeVisible()
    await drainRow.getByRole('button', { name: '取消' }).click()

    // The reconciler observes the drain, runs and verifies the pre-upgrade
    // backup, and only then reports the prepared state.
    await expect(page.getByRole('status').filter({ hasText: 'quoin_upgrade_prepared=1' })).toBeVisible({ timeout: 120_000 })
    await expect(drainRow.getByText('已安全')).toBeVisible()

    // The operator aborts the upgrade before the wizard stops anything: the
    // versioned exit restores normal service.
    await page.getByRole('button', { name: '取消升级并退出维护' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })
    const maintenance = await page.evaluate(async () => (await (await fetch('/api/v1/maintenance')).json()) as { active: boolean })
    expect(maintenance.active).toBe(false)
  })
})
