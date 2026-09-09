import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

// T12 recovery UI: the slow provider stream keeps the analysis running long
// enough to exercise the browser-side recovery story — start, observe the
// running stage with the cancel entry, leave the page and return (transport
// detach must not cancel; DATA-ATTEMPT-004), then read the sealed terminal
// outcome with its history row intact.
test.describe('T12 恢复与取消 @ticket-12', () => {
  test('离开页面后分析继续、取消栏可见、终态与历史记录保留', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // The T12SlowPage alert (slow fixture stream) is the subject.
    await expect(page.getByText('T12SlowPage', { exact: false }).first()).toBeVisible({ timeout: 30_000 })
    await page.getByText('T12SlowPage', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T12SlowPage/ })).toBeVisible()

    await page.getByRole('button', { name: '初步分析', exact: true }).click()
    await expect(page.getByText(/排队中|分析中/).first()).toBeVisible({ timeout: 30_000 })
    await expect(page.getByRole('button', { name: '取消' })).toBeVisible()

    // Leaving the detail and coming back must not disturb the running
    // task: the analysis continues server-side and the panel restores the
    // live stage from the authoritative detail.
    await page.getByRole('button', { name: '← 返回列表' }).click()
    await expect(page.getByText('T12SlowPage', { exact: false }).first()).toBeVisible({ timeout: 30_000 })
    await page.getByText('T12SlowPage', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: '初步分析' })).toBeVisible()
    await expect(page.getByText(/排队中|分析中/).first()).toBeVisible({ timeout: 30_000 })

    // The slow stream finishes; the sealed outcome stays visible with its
    // history row.
    await expect(page.getByText(/已完成|失败|已中断|已取消/).first()).toBeVisible({ timeout: 240_000 })
    await expect(page.locator('.analysis-history')).toContainText(/已完成|失败|已中断|已取消/)
  })
})
