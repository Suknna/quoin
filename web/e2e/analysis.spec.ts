import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T10 初步分析 @ticket-10', () => {
  test('告警详情发起初步分析、观察到运行中与取消入口、最终阅读密封结论并可返回', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // The T10Probe alert seeded by the acceptance stack must be listed.
    await expect(page.getByText('T10Probe', { exact: false }).first()).toBeVisible({ timeout: 30_000 })
    await page.getByText('T10Probe', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T10Probe/ })).toBeVisible()
    await expect(page.getByRole('heading', { name: '初步分析' })).toBeVisible()

    // The primary action starts the analysis; the受理 state flips
    // immediately and the live stage exposes the cancellation fence.
    await page.getByRole('button', { name: '初步分析', exact: true }).click()
    await expect(page.getByText(/排队中|分析中/).first()).toBeVisible({ timeout: 30_000 })
    await expect(page.getByRole('button', { name: '取消' })).toBeVisible()

    // The deterministic fixture drives one bash tool call then the text
    // diagnosis; the sealed output appears with its reading entry.
    await expect(page.getByText('已完成', { exact: true }).first()).toBeVisible({ timeout: 180_000 })
    await expect(page.getByText('最新结论').first()).toBeVisible()
    await expect(page.getByText('agent-fixture-proof', { exact: false }).first()).toBeVisible()

    // Open the full reading layer (URL-backed), read the seal, close back
    // to the alert detail.
    await page.getByRole('button', { name: '查看完整分析' }).click()
    await expect(page.getByRole('document', { name: '初步分析详情' })).toBeVisible()
    await expect(page.getByRole('heading', { name: '告警初步分析结论' })).toBeVisible()
    await expect(page.getByText('初步诊断', { exact: false }).first()).toBeVisible()
    await page.getByRole('button', { name: '← 返回告警详情' }).click()
    await expect(page.getByRole('heading', { name: /T10Probe/ })).toBeVisible()

    // The history row keeps the terminal record visible after return.
    await expect(page.locator('.analysis-history')).toContainText('已完成')
  })
})
