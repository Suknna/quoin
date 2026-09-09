import { existsSync, readFileSync } from 'node:fs'
import { execSync } from 'node:child_process'
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

test.describe('T04 实时告警生命周期 @ticket-04', () => {
  const amBase = 'http://127.0.0.1:19093'

  // The real Alertmanager v2 API drives the lifecycle from the host; the
  // browser must observe every transition live over SSE without a reload.
  function postAlert(startsAt: string, endsAt: string | null, extra: Record<string, string> = {}) {
    const labels: Record<string, string> = { alertname: 'T04Live', severity: 'warning', ...extra }
    const body = [{ labels, startsAt, ...(endsAt ? { endsAt } : {}), generatorURL: '' }]
    const result = execSync(
      `curl -sf -X POST -H 'Content-Type: application/json' -d '${JSON.stringify(body).replace(/'/g, `'\\''`)}' ${amBase}/api/v2/alerts`,
    )
    return result.toString()
  }

  test('列表与详情实时跟随 firing 与 resolved，接入问题可标记已处理', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readNewPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // Fire through the real Alertmanager; the list must update live.
    const now = new Date().toISOString()
    postAlert(now, null)
    const row = page.locator('.object-row', { hasText: 'T04Live' }).first()
    await expect(row).toBeVisible({ timeout: 30_000 })
    await row.click()
    await expect(page.getByRole('heading', { name: 'T04Live' })).toBeVisible()
    await expect(page.getByText('Firing', { exact: true }).first()).toBeVisible()

    // Resolve through the same Alertmanager; the detail must switch to
    // 已恢复 live, and the current list must drop the row without reload.
    postAlert(now, new Date().toISOString())
    await expect(page.getByText('已恢复').first()).toBeVisible({ timeout: 30_000 })
    await page.goBack()
    await expect(page.locator('.object-row', { hasText: 'T04Live' })).toHaveCount(0, { timeout: 30_000 })

    // The history segment shows the resolved occurrence.
    await page.getByRole('button', { name: '历史' }).click()
    const historyRow = page.locator('.object-row', { hasText: 'T04Live' }).first()
    await expect(historyRow).toBeVisible({ timeout: 30_000 })
    await historyRow.click()
    await expect(page.getByText('已恢复').first()).toBeVisible()

    // A fingerprint-mismatch body posted through the forwarder (the same
    // bearer-authenticated webhook path Alertmanager uses) lands in 接入问题.
    const mismatch = JSON.stringify({
      status: 'firing',
      alerts: [{ status: 'firing', labels: { alertname: 'T04Mismatch' }, startsAt: new Date().toISOString(), fingerprint: '0123456789abcdef' }],
      truncatedAlerts: 0,
    })
    execSync(`curl -sf -X POST -H 'Content-Type: application/json' -d '${mismatch.replace(/'/g, `'\\''`)}' http://127.0.0.1:18082/`)
    await page.getByRole('button', { name: '接入问题' }).click()
    await expect(page.getByText('指纹不符').first()).toBeVisible({ timeout: 30_000 })

    // Admin acknowledges in place without a second confirmation dialog.
    await page.getByRole('button', { name: '标记已处理' }).click()
    await expect(page.getByText('没有未处理的接入问题')).toBeVisible({ timeout: 30_000 })
  })
})
