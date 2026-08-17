import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readNewPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`new password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

async function login(page: import('@playwright/test').Page): Promise<void> {
  await page.goto('/')
  await page.fill('#username', 'admin')
  await page.fill('#password', readNewPassword())
  await page.getByRole('button', { name: '登录' }).click()
  await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()
}

test.describe('T01 工作台外壳 @ticket-01', () => {
  test('登录后可键盘导航六个模块，管理页投影运行组件状态', async ({ page }) => {
    test.slow()
    await login(page)

    for (const label of ['告警', '调查', '巡检', '业务系统', '知识', '管理']) {
      await expect(page.getByRole('button', { name: label, exact: true })).toBeVisible()
    }

    await page.getByRole('button', { name: '管理', exact: true }).click()
    await expect(page.getByRole('heading', { name: '运行组件' })).toBeVisible()
    await expect(page.getByRole('button', { name: '管理', exact: true })).toBeFocused()

    await page.getByRole('button', { name: '告警', exact: true }).focus()
    await expect(page.getByRole('button', { name: '告警', exact: true })).toBeFocused()
    await page.keyboard.press('Enter')
    await expect(page.getByRole('heading', { name: '告警', level: 1 })).toBeVisible()
  })

  test('窄屏下导航折叠为抽屉，320px 无页面级横向滚动', async ({ page }) => {
    test.slow()
    await login(page)

    await page.setViewportSize({ width: 375, height: 800 })
    await expect(page.getByRole('button', { name: '打开模块导航' })).toBeVisible()
    await page.getByRole('button', { name: '打开模块导航' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()
    await page.getByRole('button', { name: '巡检' }).click()
    await expect(page.getByRole('heading', { name: '巡检', level: 1 })).toBeVisible()

    await page.setViewportSize({ width: 320, height: 720 })
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(0)
    await expect(page.getByRole('button', { name: '返回列表' })).toHaveCount(0)
  })
})
