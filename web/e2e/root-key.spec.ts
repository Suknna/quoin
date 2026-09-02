import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stack = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function adminPassword(): string {
  const path = join(stack, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`T34 administrator fixture is missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

test.describe('T34 根密钥重绑定 @ticket-34', () => {
  test('管理员只能在全工作台维护页重新输入凭据或明确保持连接停用', async ({ page }) => {
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword())
    await page.getByRole('button', { name: '登录' }).click()

    await expect(page.getByRole('heading', { name: '根密钥更换' })).toBeVisible()
    await expect(page.getByText('普通业务入口已暂停。系统会在退出维护时重新核验清单，不能跳过。')).toBeVisible()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toHaveCount(0)
    await expect(page.getByRole('heading', { name: '连接' })).toBeVisible()

    const row = page.locator('.admin-connection-list .object-row-main', { hasText: 'main-thanos' })
    await expect(row).toBeVisible()
    await row.click()
    await expect(page.getByRole('button', { name: '确认保持停用' })).toBeVisible()
    for (const connection of ['main-thanos', 'main-openai', 'broken-openai']) {
      await page.locator('.admin-connection-list .object-row-main', { hasText: connection }).click()
      await page.getByRole('button', { name: '确认保持停用' }).click()
      await expect(page.getByText(`已确认 ${connection} 保持停用。`)).toBeVisible()
    }
    await expect(page.getByRole('button', { name: '退出维护' })).toBeVisible()
    await page.getByRole('button', { name: '退出维护' }).click()
    await expect(page.getByText('维护状态已退出。请重启 Quoin 以恢复普通服务。')).toBeVisible()
  })
})
