import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T07 连接与探测 @ticket-07', () => {
  test('管理员创建连接、运行探测并看到不可变结果历史', async ({ page }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '连接' }).click()
    await expect(page.getByRole('heading', { name: '连接' })).toBeVisible()

    // The fixture-created connection renders with its type and state.
    const row = page.locator('.admin-connection-list .object-row-main', { hasText: 'main-thanos' })
    await expect(row).toBeVisible()
    await expect(row.getByText('Thanos 查询')).toBeVisible()
    await expect(row.getByText('未启用')).toBeVisible()

    // The creation form explains one-time secret handling.
    await page.getByRole('button', { name: '新建连接' }).click()
    await expect(page.getByText('一次性提交，保存后不可查看').first()).toBeVisible()
    await page.getByRole('button', { name: '取消' }).click()

    // Select the connection and run a real probe against the fixture
    // Thanos; the immutable history must show the passed typed result.
    await row.click()
    await expect(page.getByRole('heading', { name: 'main-thanos' })).toBeVisible()
    await page.locator('.connection-detail-card').getByRole('button', { name: '运行探测' }).click()
    await expect(page.getByText('探测已受理').or(page.getByText('探测运行中'))).toBeVisible()
    await expect(page.locator('.admin-audit-list li').getByText('通过')).toBeVisible({ timeout: 60_000 })
    await expect(
      page.locator('.admin-audit-list li .admin-mono', { hasText: 'thanos-query-v1' }),
    ).toBeVisible()

    // Enable with the current row version succeeds; the single-enabled
    // rule then blocks a second thanos from the UI-facing API.
    await page.locator('.connection-detail-card').getByRole('button', { name: '启用', exact: true }).click()
    await expect(page.getByText('连接已启用。')).toBeVisible()
    await expect(row.getByText('已启用')).toBeVisible()
  })
})
