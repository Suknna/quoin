import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

// T38 部署验收 UI：开发栈由源码构建、没有 release manifest 绑定。面板必须真实
// 呈现发起入口，服务端以诚实的错误说明该部署无可验收的 release subject，
// 且路由刷新后保持。完整 invocation 生命周期由 TestTicket38 以真实
// HTTP/SQLite/二进制路径证明。
test.describe('T38 部署验收 @ticket-38', () => {
  test('管理员打开部署验收面板，无绑定开发部署返回不可用说明且路由可恢复', async ({ page }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', adminPassword)
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()

    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '部署验收' }).click()
    await expect(page.getByRole('heading', { name: '部署验收' })).toBeVisible()
    await expect(page.getByText('尚无部署验收记录。')).toBeVisible()
    await expect(page.getByRole('button', { name: '发起部署验收' })).toBeVisible()

    // 服务端拒绝：该部署没有冻结的 release subject（诚实错误，非客户端伪装）。
    await page.getByRole('button', { name: '发起部署验收' }).click()
    await expect(page.getByText(/没有冻结的部署绑定|release-manifest deployment|无法发起部署验收/)).toBeVisible({ timeout: 15_000 })

    // 直接调用 API 复核服务端裁决与鉴权。
    const status = await page.evaluate(async () => {
      const response = await fetch('/api/v1/deployment-verifications', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: 'ui-ticket-38-probe' }),
      })
      return { code: response.status, body: await response.json() }
    })
    expect([503].includes(status.code)).toBe(true)
    expect(status.body.code).toBe('deployment_acceptance_unavailable')

    // 匿名读取被拒绝。
    const anonymous = await page.evaluate(async () => {
      const response = await fetch('/api/v1/deployment-verifications', { credentials: 'omit' })
      return response.status
    })
    expect(anonymous).toBe(401)

    // 路由刷新后仍停留在部署验收页（会话 Cookie 存续，无需重新登录）。
    await page.reload()
    await expect(page.getByRole('heading', { name: '部署验收' })).toBeVisible()
  })
})
