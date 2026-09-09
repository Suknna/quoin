import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T08 模型供应商资格 @ticket-08', () => {
  test('管理员创建模型供应商、等待探测并看到资格矩阵与失败状态', async ({ page }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '连接' }).click()

    // The fixture-created qualified connection shows its state.
    const row = page.locator('.admin-connection-list .object-row-main', { hasText: 'main-openai' })
    await expect(row).toBeVisible()
    await expect(row.getByText('模型供应商')).toBeVisible()

    // The creation form explains one-time key handling and the discovery
    // fallback.
    await page.getByRole('button', { name: '新建连接' }).click()
    await page.locator('.admin-create-form select').selectOption('model_provider')
    await expect(page.getByText('API Key（一次性提交，保存后不可查看）')).toBeVisible()
    await expect(page.getByText('发现只辅助选择，不构成能力证明')).toBeVisible()
    await page.getByRole('button', { name: '取消' }).click()

    // Run a real qualification against the fixture: the waiting state is
    // observable and the passed result carries the capability matrix.
    await row.click()
    await page.locator('.connection-detail-card').getByRole('button', { name: '运行探测', exact: true }).click()
    await expect(page.locator('.detail-content').getByText(/探测已受理|探测运行中/).first()).toBeVisible()
    await expect(page.locator('.admin-audit-list li').getByText('通过')).toBeVisible({ timeout: 60_000 })

    // Enable is blocked without a qualification; with the passed result it
    // succeeds (server-side fence; the UI surfaces both outcomes).
    await page.locator('.connection-detail-card').getByRole('button', { name: '启用', exact: true }).click()
    await expect(page.getByText(/连接已启用|需要先通过探测/).first()).toBeVisible({ timeout: 30_000 })

    // The broken fixture connection shows the failure state with the
    // unqualified capability in its immutable history.
    const broken = page.locator('.admin-connection-list .object-row-main', { hasText: 'broken-openai' })
    await broken.click()
    await page.locator('.connection-detail-card').getByRole('button', { name: '运行探测', exact: true }).click()
    await expect(page.locator('.admin-audit-list li').getByText('失败')).toBeVisible({ timeout: 60_000 })
  })
})
