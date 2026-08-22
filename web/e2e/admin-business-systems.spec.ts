import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

// T16 — the Admin upload/publish browser path over the real compose stack:
// field-error recovery that keeps the file, the valid upload landing on the
// immutable version detail, the explicit publish confirm switching the
// current pointer, and the deep-linked version detail surviving a reload.

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

const contractYAML = 'label_contract:\n  business_system_label: business_system\n'

const badSystemYAML = `system_key: browser-sys
display_name: 浏览器验收系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
unknown_top_field: 1
resource_discoveries: []
inspection_plans: []
`

const goodSystemYAML = `system_key: browser-sys
display_name: 浏览器验收系统
enabled: false
timezone: Asia/Shanghai
resource_refresh_interval_seconds: 300
resource_discoveries:
  - key: web-pods
    display_name: Web Pods
    selector: 'up{business_system="browser-sys", job="web"}'
    identity_labels: [job, instance]
inspection_plans:
  - key: daily
    display_name: Daily
    cron: "30 8 * * *"
    checks:
      - key: up-instant
        display_name: Up Instant
        analysis_question: 当前可用吗？
        kind: promql
        query:
          mode: instant
          expression: 'up{business_system="browser-sys"}'
`

test.describe('T16 业务系统配置上传与发布 @ticket-16', () => {
  test('上传错误恢复、合法上传与发布的真实浏览器路径', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // Fixture through the real production endpoints from inside the page, so
    // the authenticated session cookie and same-origin metadata ride along.
    const statuses = await page.evaluate(async (yaml) => {
      const commandSuffix = Date.now()
      const form = new FormData()
      form.append('file', new File([yaml], 'contract.yaml', { type: 'application/yaml' }))
      form.append('clientCommandId', `e2e-t16-contract-${commandSuffix}`)
      const create = await fetch('/api/v1/label-contracts', { method: 'POST', body: form })
      const activate = await fetch('/api/v1/label-contracts/1/activate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          clientCommandId: `e2e-t16-activate-${commandSuffix}`,
          expectedStateRowVersion: 1,
          expectedCurrentContractVersionId: null,
          expectedTargetRowVersion: 1,
          compatibleVersions: [],
        }),
      })
      return { create: create.status, activate: activate.status }
    }, contractYAML)
    expect(statuses.create).toBe(201)
    expect(statuses.activate).toBe(200)

    // The module opens on the first-upload empty state.
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '业务系统' }).click()
    await expect(page.getByText('还没有业务系统。')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText(/上传第一份配置 YAML 会创建一个 Disabled 业务系统/)).toBeVisible()

    // Upload layer: provenance is visible before any file is chosen.
    await page.getByRole('button', { name: '上传配置' }).click()
    const overlay = page.getByRole('dialog', { name: '上传业务系统配置' })
    await expect(overlay).toBeVisible()
    await expect(overlay.getByText(/Journey Catalog：/)).toBeVisible({ timeout: 15_000 })
    await expect(overlay.getByText(/v1-empty/)).toBeVisible()
    await expect(overlay.locator('#upload-target-contract')).toContainText('v1 · 当前激活')

    // The invalid file reports the exact unknown-field path and keeps the file.
    await overlay.locator('input[type=file]').setInputFiles({
      name: 'bad.yaml',
      mimeType: 'application/yaml',
      buffer: Buffer.from(badSystemYAML),
    })
    await expect(overlay.getByText(/unknown_top_field/)).toBeVisible()
    await expect(overlay.getByText('bad.yaml')).toBeVisible()

    // Correcting the file in place succeeds and lands on the version detail.
    await overlay.locator('input[type=file]').setInputFiles({
      name: 'good.yaml',
      mimeType: 'application/yaml',
      buffer: Buffer.from(goodSystemYAML),
    })
    await overlay.getByRole('button', { name: '上传并校验' }).click()
    await expect(page.getByRole('heading', { name: /配置版本 v1/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('草稿')).toBeVisible()
    await expect(page.getByText(/up\{business_system="browser-sys"\}/)).toBeVisible()

    // The explicit publish confirm switches the current pointer.
    await page.getByRole('button', { name: '发布此版本' }).click()
    await page.getByRole('button', { name: '确认发布' }).click()
    await expect(page.getByRole('heading', { name: '浏览器验收系统' })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已发布').first()).toBeVisible({ timeout: 15_000 })
    await expect(page.getByText('Disabled')).toBeVisible()

    // The system list row reflects the published state.
    await page.getByRole('button', { name: '返回列表' }).click()
    await expect(page.getByRole('button', { name: /浏览器验收系统/ })).toBeVisible()
    await expect(page.getByText('已发布配置 · 时区 Asia/Shanghai')).toBeVisible()

    // Deep link: the version detail survives a full reload.
    await page.goto('/business-systems/browser-sys/configs/1')
    await expect(page.getByRole('heading', { name: /配置版本 v1/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('已发布').first()).toBeVisible()

    // Back returns to the system detail through browser history.
    await page.goBack()
    await expect(page.getByRole('button', { name: /浏览器验收系统/ })).toBeVisible({ timeout: 15_000 })
  })
})
