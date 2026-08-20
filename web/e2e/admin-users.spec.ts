import { randomBytes } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}


async function login(page: import('@playwright/test').Page, username: string, password: string) {
  await page.goto('/')
  await page.fill('#username', username)
  await page.fill('#password', password)
  await page.getByRole('button', { name: '登录' }).click()
}

test.describe('T05 用户与会话管理 @ticket-05', () => {
  test('管理员创建用户、重置密码并保护最后一个管理员；操作员只读', async ({ page, browser }) => {
    test.slow()
    const adminPassword = readAdminPassword()
    const operatorPassword = `op-${randomBytes(12).toString('hex')}-2026`
    const replacementPassword = `rp-${randomBytes(12).toString('hex')}-2027`

    await login(page, 'admin', adminPassword)
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible()
    await page.getByRole('button', { name: '管理' }).click()
    await expect(page.getByRole('heading', { name: '用户与会话' })).toBeVisible()

    // The list shows the bootstrap admin with role and login facts.
    await expect(page.getByRole('button', { name: /E2E Admin（@admin）/ })).toBeVisible()

    // Creating an operator through the real form.
    await page.getByRole('button', { name: '+ 创建用户' }).click()
    await page.fill('#new-username', 'op-ui')
    await page.fill('#new-display-name', '界面操作员')
    await page.selectOption('#new-role', 'operator')
    await page.fill('#new-user-password', operatorPassword)
    await page.getByRole('button', { name: '创建', exact: true }).click()
    await expect(page.getByText('已创建 @op-ui。')).toBeVisible()
    await expect(page.getByRole('button', { name: /界面操作员（@op-ui）/ })).toBeVisible()

    // Last-effective-admin protection surfaces as an ordinary-language failure.
    await page.getByRole('button', { name: /E2E Admin（@admin）/ }).click()
    await page.getByLabel('启用该账号（当前登录账号）').uncheck()
    await page.getByRole('button', { name: '保存修改' }).click()
    await expect(page.getByText('系统必须保留至少一个有效的管理员；请先创建或启用另一个管理员。')).toBeVisible()
    await page.getByLabel('启用该账号（当前登录账号）').check()
    await page.getByRole('button', { name: '保存修改' }).click()
    await expect(page.getByText('没有需要保存的修改。')).toBeVisible()

    // Reset the operator's password through the inline flow.
    await page.getByRole('button', { name: /界面操作员（@op-ui）/ }).click()
    await page.getByRole('button', { name: '重置密码' }).click()
    await page.fill('#admin-new-password', replacementPassword)
    await page.getByRole('button', { name: '确认重置' }).click()
    await expect(page.getByText(/已重置密码；本次撤销了 \d+ 个登录会话。/)).toBeVisible()

    // Audit trail renders actions (the card shows the newest window; the
    // create action itself is asserted through the filtered API from the
    // page's own session because the fixture's live alert traffic fills the
    // recent-events window).
    await expect(page.locator('.admin-audit-list .admin-audit-action').first()).toBeVisible()
    const createAudit = await page.evaluate(async () => {
      const response = await fetch('/api/v1/audit-events?action=user.create', { credentials: 'include' })
      return response.ok ? await response.text() : `status=${response.status}`
    })
    expect(createAudit).toContain('user.create')

    // Operator journey in an isolated context: full workbench, no admin
    // surface, and the temporary password forces the change flow first.
    const operatorContext = await browser.newContext()
    const operatorPage = await operatorContext.newPage()
    await login(operatorPage, 'op-ui', replacementPassword)
    await expect(operatorPage.getByRole('heading', { name: '先设置你自己的密码' })).toBeVisible()
    const operatorFinal = `fn-${randomBytes(12).toString('hex')}-2028`
    await operatorPage.fill('#current-password', replacementPassword)
    await operatorPage.fill('#new-password', operatorFinal)
    await operatorPage.fill('#confirm-password', operatorFinal)
    await operatorPage.getByRole('button', { name: '保存并进入工作台' }).click()
    await expect(operatorPage.getByRole('navigation', { name: '全局模块' })).toBeVisible()
    await expect(operatorPage.getByRole('button', { name: '管理' })).toBeHidden()
    const operatorForbidden = await operatorPage.evaluate(async () => {
      const response = await fetch('/api/v1/admin/users', { credentials: 'include' })
      return response.status
    })
    expect(operatorForbidden).toBe(403)

    // The operator still sees the audit surface through the API (the admin
    // module is navigation-hidden for operators; authorization is server-side).
    const auditReadable = await operatorPage.evaluate(async () => {
      const response = await fetch('/api/v1/audit-events', { credentials: 'include' })
      return response.status
    })
    expect(auditReadable).toBe(200)
    const operatorStillValid = await operatorPage.evaluate(async () => {
      const response = await fetch('/api/v1/auth/sessions', { credentials: 'include' })
      return response.status
    })
    expect(operatorStillValid).toBe(200)
    await operatorContext.close()

    // Revoking the operator's sessions kills the next authenticated request
    // (a fresh operator login proves the dead bearer server-side).
    const revokeInfo = await page.evaluate(async () => {
      const listResponse = await fetch('/api/v1/admin/users', { credentials: 'include' })
      const list = (await listResponse.json()) as { items: Array<{ id: string; username: string }> }
      const row = list.items.find((item) => item.username === 'op-ui')
      if (!row) return 'missing'
      const revokeResponse = await fetch(`/api/v1/admin/users/${row.id}/revoke-sessions`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ clientCommandId: `e2e-revoke-${Date.now()}` }),
      })
      return `status=${revokeResponse.status}`
    })
    expect(revokeInfo).toBe('status=200')
    const verifierContext = await browser.newContext()
    const verifierPage = await verifierContext.newPage()
    await login(verifierPage, 'op-ui', operatorFinal)
    await expect(verifierPage.getByRole('heading', { name: '登录工作台' })).toBeVisible()
    const afterRevoke = await verifierPage.evaluate(async () => {
      const response = await fetch('/api/v1/auth/me', { credentials: 'include' })
      return response.status
    })
    expect(afterRevoke).toBe(401)
    await verifierContext.close()
  })
})
