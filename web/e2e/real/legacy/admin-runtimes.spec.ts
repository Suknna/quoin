import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

test.describe('T06 运行组件注册 @ticket-06', () => {
  test('管理员看到 Plinth/Lintel 注册与连接状态，运行组件视图可用', async ({ page }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    await page.getByRole('button', { name: '管理' }).click()
    // Both runtime cards render their persistent state (empty install: both
    // unregistered or registered; the assertion stays on the frozen labels).
    await page.getByRole('button', { name: '运行组件' }).click()
    await expect(page.getByRole('heading', { name: '运行组件注册' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Plinth（分析执行组件）' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Lintel（浏览器执行组件）' })).toBeVisible()
    // Each card shows the generation fact and a state pill.
    await expect(page.locator('.runtime-card .status-pill').first()).toBeVisible()
    await expect(page.getByText('当前凭据 generation：').first()).toBeVisible()

    // The prepare flow asks for confirmation only on an existing slot; the
    // unregistered slot exposes a direct prepare button. Whichever control
    // is present, the frozen state machine stays observable.
    const plinthCard = page.locator('section.runtime-card').first()
    await expect(
      plinthCard.getByRole('button', { name: /准备注册|替换注册（吊销旧凭据）/ }),
    ).toBeVisible()

    // Server-side authorization for the runtime surface.
    const status = await page.evaluate(async () => {
      const response = await fetch('/api/v1/runtime', { credentials: 'include' })
      return response.status
    })
    expect(status).toBe(200)
  })
})
