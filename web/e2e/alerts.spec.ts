import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readNewPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`new password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

test.describe('T03 告警模块 @ticket-03', () => {
  test('登录后告警列表展示真实 Firing 告警并可查看详情', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readNewPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // The T03Probe alert fired by the acceptance stack must be listed.
    await expect(page.getByText('T03Probe', { exact: false }).first()).toBeVisible({ timeout: 30_000 })
    await page.getByText('T03Probe', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T03Probe/ })).toBeVisible()
    await expect(page.getByRole('heading', { name: '标签' })).toBeVisible()

    // 接入问题分段可见且为空。
    await page.getByRole('button', { name: '接入问题' }).click()
    await expect(page.getByText('没有未处理的接入问题')).toBeVisible()
  })
})
