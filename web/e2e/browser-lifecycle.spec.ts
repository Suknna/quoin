import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// This exercise deliberately uses the real T20 Compose fixture (Quoin, gRPC
// Runtime control stream, Lintel, Chromium, Xvfb and x0vncserver). It does not
// mock a Start acknowledgement or capacity decision.
test.use({ trace: 'off', screenshot: 'off', video: 'off' })

test.describe('T21 Browser Operation lifecycle @ticket-21', () => {
  test('Quoin, not Lintel, holds a capacity-one FIFO queue across real browser processes', async ({ page }) => {
    test.slow()
    const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack-t20')
    const passwordPath = join(stackDir, 'admin-new-password')
    expect(existsSync(passwordPath)).toBeTruthy()

    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readFileSync(passwordPath, 'utf-8').trim())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    const result = await page.evaluate(async () => {
      const suffix = `${Date.now()}`
      const headers = { 'Content-Type': 'application/json' }
      let active = (await (await fetch('/api/v1/label-contracts?limit=100')).json()).items?.find((item: { state?: string }) => item.state === 'active')
      if (!active) {
        const contract = new FormData()
        contract.append('file', new File(['label_contract:\n  business_system_label: business_system\n'], 'contract.yaml', { type: 'application/yaml' }))
        contract.append('clientCommandId', `e2e-t21-contract-${suffix}`)
        const created = await fetch('/api/v1/label-contracts', { method: 'POST', body: contract })
        if (created.status !== 201) throw new Error(`create label contract=${created.status}`)
        active = await created.json()
        const activate = await fetch(`/api/v1/label-contracts/${active.version}/activate`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t21-activate-${suffix}`, expectedStateRowVersion: active.rowVersion, expectedCurrentContractVersionId: null, expectedTargetRowVersion: active.rowVersion, compatibleVersions: [] }),
        })
        if (!activate.ok) throw new Error(`activate label contract=${activate.status}`)
        active = await activate.json()
      }
      const createIdentity = async (ordinal: number) => {
        const key = `t21-browser-${suffix}-${ordinal}`
        const yaml = `system_key: ${key}\ndisplay_name: T21 Browser ${ordinal}\nenabled: false\ntimezone: Asia/Shanghai\nresource_refresh_interval_seconds: 300\nresource_discoveries: []\ninspection_plans: []\n`
        const system = new FormData()
        system.append('file', new File([yaml], 'system.yaml', { type: 'application/yaml' }))
        system.append('clientCommandId', `e2e-t21-system-${suffix}-${ordinal}`)
        system.append('targetLabelContractVersion', String(active.version))
        const created = await fetch('/api/v1/business-systems', { method: 'POST', body: system })
        if (created.status !== 201) throw new Error(`create system=${created.status}: ${await created.text()}`)
        const identity = await fetch(`/api/v1/business-systems/${key}/browser-identity`, {
          method: 'POST', headers,
          body: JSON.stringify({
            clientCommandId: `e2e-t21-identity-${suffix}-${ordinal}`,
            name: `T21 readonly ${ordinal}`,
            startUrl: 'http://t20-auth-fixture:8081/login',
            authenticationProbe: { journeyId: 'authentication.url-prefix.v1', journeyVersion: 1, params: { authenticatedUrlPrefix: 'http://t20-auth-fixture:8081/authenticated' } },
          }),
        })
        if (identity.status !== 202) throw new Error(`configure identity=${identity.status}: ${await identity.text()}`)
        const configured = await identity.json() as { identity: { rowVersion: number } }
        return { key, identity: configured.identity }
      }
      const first = await createIdentity(1)
      const second = await createIdentity(2)
      const start = async (item: { key: string, identity: { rowVersion: number } }, ordinal: number) => {
        const response = await fetch(`/api/v1/browser-login/${item.key}/operations`, {
          method: 'POST', headers,
          body: JSON.stringify({ clientCommandId: `e2e-t21-start-${suffix}-${ordinal}`, expectedRowVersion: item.identity.rowVersion }),
        })
        if (!response.ok) throw new Error(`start ${ordinal}=${response.status}: ${await response.text()}`)
        return { key: item.key, operation: await response.json() as { id: string } }
      }
      return { first: await start(first, 1), second: await start(second, 2), suffix }
    })

    const operation = async (item: { key: string, operation: { id: string } }) => await page.evaluate(async ({ key, id }) => await (await fetch(`/api/v1/browser-login/${key}/operations/${id}`)).json(), { key: item.key, id: item.operation.id }) as { state: string, rowVersion: number }
    await expect.poll(() => operation(result.first), { timeout: 45_000 }).toMatchObject({ state: 'Running' })
    await expect.poll(() => operation(result.second), { timeout: 15_000 }).toMatchObject({ state: 'WaitingForCapacity' })

    const firstRunning = await operation(result.first)
    await page.evaluate(async ({ item, rowVersion, suffix }) => {
      const response = await fetch(`/api/v1/browser-login/${item.key}/operations/${item.operation.id}/cancel`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `e2e-t21-cancel-first-${suffix}`, expectedOperationRowVersion: rowVersion }),
      })
      if (!response.ok) throw new Error(`cancel first=${response.status}: ${await response.text()}`)
    }, { item: result.first, rowVersion: firstRunning.rowVersion, suffix: result.suffix })
    await expect.poll(() => operation(result.second), { timeout: 45_000 }).toMatchObject({ state: 'Running' })

    const secondRunning = await operation(result.second)
    await page.evaluate(async ({ item, rowVersion, suffix }) => {
      const response = await fetch(`/api/v1/browser-login/${item.key}/operations/${item.operation.id}/cancel`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `e2e-t21-cancel-second-${suffix}`, expectedOperationRowVersion: rowVersion }),
      })
      if (!response.ok) throw new Error(`cancel second=${response.status}: ${await response.text()}`)
    }, { item: result.second, rowVersion: secondRunning.rowVersion, suffix: result.suffix })
    await expect.poll(() => operation(result.second), { timeout: 45_000 }).toMatchObject({ state: 'Cancelled' })

    const evidenceDir = process.env.QUOIN_EVIDENCE_DIR
    if (evidenceDir) {
      writeFileSync(join(evidenceDir, 't21-lifecycle-observations.json'), `${JSON.stringify({
        observedAt: new Date().toISOString(),
        fixture: 'real T20 Compose Runtime/Lintel Chromium stack',
        capacitySlots: 1,
        first: { systemKey: result.first.key, operationID: result.first.operation.id, observed: ['Running', 'Cancelled'] },
        second: { systemKey: result.second.key, operationID: result.second.operation.id, observed: ['WaitingForCapacity', 'Running', 'Cancelled'] },
        assertion: 'The second real browser process was not started until the first operation received physical cleanup confirmation.',
      }, null, 2)}\n`)
    }
  })
})
