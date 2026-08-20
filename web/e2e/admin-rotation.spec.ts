import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T09 凭据轮换与吊销 @ticket-09', () => {
  test('管理员轮换连接凭据并看到重新验证要求；运行组件替换需通过栅栏', async ({ page }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '连接' }).click()

    const row = page.locator('.admin-connection-list .object-row-main', { hasText: 'main-thanos' })
    await expect(row).toBeVisible()
    await row.click()

    // Open the rotation form: it explains immediate invalidation and
    // mandatory re-probing.
    await page.locator('.connection-detail-card').getByRole('button', { name: '轮换凭据' }).click()
    await expect(page.getByText('提交新秘密后旧秘密立即停用；探测与启用都针对新的凭据。')).toBeVisible()

    // A real rotation against the live Thanos fixture.
    await page.fill('.admin-create-form label:has-text("查询入口 URL") input', 'http://quoin-t07-thanos:9090')
    await page.getByRole('button', { name: '提交轮换' }).click()
    await expect(page.getByText('凭据已轮换：旧秘密立即停用；需要重新通过探测才能启用。')).toBeVisible({ timeout: 30_000 })

    // The card reflects the revalidation requirement and the fresh probe
    // closes over the new pair.
    await expect(page.locator('.connection-detail-card')).toContainText('未启用')
    await page.locator('.connection-detail-card').getByRole('button', { name: '运行探测', exact: true }).click()
    await expect(page.locator('.admin-audit-list li').getByText('通过')).toBeVisible({ timeout: 60_000 })

    // The runtimes view still shows the registered slot with its replace
    // control behind the row-version fence.
    await page.getByRole('button', { name: '运行组件' }).click()
    await expect(page.getByRole('heading', { name: '运行组件注册' })).toBeVisible()
    await expect(
      page.locator('section.runtime-card').first().getByRole('button', { name: /准备注册|替换注册（吊销旧凭据）/ }),
    ).toBeVisible()
  })
})
