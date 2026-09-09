import { randomBytes } from 'node:crypto'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readTempPassword(): string {
  const path = join(stackDir, 'admin-temp-password')
  if (!existsSync(path)) throw new Error(`temporary password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

function rememberNewPassword(password: string): void {
  writeFileSync(join(stackDir, 'admin-new-password'), password, { mode: 0o600 })
}

function newLoginPassword(): string {
  return `e2e-${randomBytes(12).toString('hex')}-2026`
}

test.describe('T01 认证旅程 @ticket-01', () => {
  test('临时密码强制改密后进入工作台，登出后旧密码失效', async ({ page }) => {
    test.slow()
    const tempPassword = readTempPassword()
    const newPassword = newLoginPassword()

    await page.goto('/')
    await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible()
    await expect(page.locator('#username')).toBeFocused()

    await page.fill('#username', 'admin')
    await page.fill('#password', tempPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('heading', { name: '先设置你自己的密码' })).toBeVisible()

    await page.fill('#current-password', tempPassword)
    await page.fill('#new-password', newPassword)
    await page.fill('#confirm-password', newPassword)
    await page.getByRole('button', { name: '保存并进入工作台' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()
    await expect(page.getByRole('button', { name: '告警' })).toBeVisible()

    await page.getByRole('button', { name: /E2E Admin/ }).click()
    await page.getByRole('menuitem', { name: '退出登录' }).click()
    await expect(page.getByRole('heading', { name: '登录工作台' })).toBeVisible()

    await page.fill('#username', 'admin')
    await page.fill('#password', tempPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.locator('.error-summary')).toContainText('没有登录成功')

    await page.setViewportSize({ width: 320, height: 720 })
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(0)

    rememberNewPassword(newPassword)
  })
})
