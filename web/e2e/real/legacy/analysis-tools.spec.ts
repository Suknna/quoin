import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T11 分析工具与证据 @ticket-11', () => {
  test('分析完成后阅读层展示 Thanos 工具证据、产物下载入口并可返回', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // The T11Thanosa alert seeded by the acceptance stack must be listed.
    await expect(page.getByText('T11Thanosa', { exact: false }).first()).toBeVisible({ timeout: 30_000 })
    await page.getByText('T11Thanosa', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T11Thanosa/ })).toBeVisible()
    await expect(page.getByRole('heading', { name: '初步分析' })).toBeVisible()

    // The deterministic fixture drives thanos_query -> artifact_read ->
    // the text diagnosis; the sealed output appears with its evidence.
    await page.getByRole('button', { name: '初步分析', exact: true }).click()
    await expect(page.getByText('已完成', { exact: true }).first()).toBeVisible({ timeout: 240_000 })
    await expect(page.getByText('thanos-proof', { exact: false }).first()).toBeVisible()

    // Open the full reading layer: the tool-details section shows the
    // sealed Thanos evidence with the artifact download entry.
    await page.getByRole('button', { name: '查看完整分析' }).click()
    await expect(page.getByRole('document', { name: '初步分析详情' })).toBeVisible()
    await expect(page.getByRole('heading', { name: '采集证据' })).toBeVisible()
    await expect(page.getByText('Thanos 查询')).toBeVisible()
    await expect(page.getByText('已封存').first()).toBeVisible()
    await expect(page.getByText('完整响应共', { exact: false }).first()).toBeVisible()
    await expect(page.getByRole('link', { name: /下载完整产物/ })).toBeVisible()
    await expect(page.getByRole('button', { name: '← 返回告警详情' })).toBeVisible()
    await page.getByRole('button', { name: '← 返回告警详情' }).click()
    await expect(page.getByRole('heading', { name: /T11Thanosa/ })).toBeVisible()
  })
})
