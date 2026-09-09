import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T15 调查并发控制 @ticket-15', () => {
  test('停止、重试、撤回与返回重连的真实浏览器路径', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '调查' }).click()
    await page.getByRole('main').getByRole('button', { name: '新建调查' }).click()
    await page.fill('#first-message', 'T15 并发控制首轮：请给出结论')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByRole('heading', { name: /T15 并发控制首轮/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('fixture-proof-t13')).toBeVisible({ timeout: 120_000 })

    // --- 撤回：整轮只读、正文回到输入区、空 head 重发恢复 -------------
    await page.getByRole('button', { name: '撤回这一轮' }).click()
    await expect(page.getByText('该回合已撤回（保留审计）').first()).toBeVisible({ timeout: 15_000 })
    // The withdrawn turn returns to the composer (UI-CHAT-005).
    await expect(page.locator('.chat-composer-input')).toHaveValue(/T15 并发控制首轮/, { timeout: 15_000 })
    // The withdrawn branch is read-only: no further undo/retry controls on it.
    await expect(page.getByRole('button', { name: '撤回这一轮' })).toHaveCount(0)
    // Resend from the restored text resumes the branch through the null head.
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByText('fixture-proof-t13').last()).toBeVisible({ timeout: 120_000 })

    // --- 停止：方形停止按钮提交 fence，终态为已停止且不卡死 -------------
    const composer = page.locator('.chat-composer-input')
    await composer.fill('T15 停止验证：请围绕容量、依赖与恢复顺序给出完整结论，并补充现场处置建议。')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByRole('button', { name: '停止' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: '停止' }).click()
    await expect(page.getByRole('button', { name: '正在停止…' })).toBeVisible({ timeout: 15_000 })
    // The fence closes the stream with the cancelled terminal frame; the
    // turn ends without a reply and the composer returns.
    await expect(page.getByRole('button', { name: '停止' })).toHaveCount(0, { timeout: 60_000 })
    await expect(page.getByRole('button', { name: '发送', exact: true })).toBeVisible({ timeout: 60_000 })
    await expect(composer).toBeEnabled({ timeout: 60_000 })

    // --- 返回重连：离开页面不取消任务，返回后状态恢复 -------------------
    // (Runs before the T13Broken turn: the deterministic fixture branches
    // on the WHOLE conversation prompt, so a T13Broken message in the
    // history would hang up every later turn of this investigation.)
    await composer.fill('T15 返回重连：请围绕容量、依赖与恢复顺序给出完整结论。')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByRole('button', { name: '停止' })).toBeVisible({ timeout: 30_000 })
    // Back-button: leave mid-stream, the domain turn keeps running.
    await page.getByRole('button', { name: '返回列表' }).click()
    await expect(page.getByRole('button', { name: '新建调查' })).toBeVisible({ timeout: 30_000 })
    await page.getByRole('button', { name: /T15 并发控制首轮/ }).click()
    await expect(page.getByText('回复正在生成中，页面会持续更新。')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('fixture-proof-t13').last()).toBeVisible({ timeout: 120_000 })
    await expect(page.getByText('回复正在生成中，页面会持续更新。')).toHaveCount(0, { timeout: 60_000 })

    // --- 重试：失败回合的环形重试按钮用同一条消息重新生成 ---------------
    await composer.fill('T13Broken 请触发失败路径')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByRole('button', { name: '重试这一轮' })).toBeVisible({ timeout: 120_000 })
    await page.getByRole('button', { name: '重试这一轮' }).click()
    // The deterministic broken provider fails the retried attempt the same
    // way; the retry control returns once the new attempt settles.
    await expect(page.getByRole('button', { name: '重试这一轮' })).toBeVisible({ timeout: 120_000 })

    // --- 撤回态在离开/返回后保持只读 ------------------------------------
    await page.getByRole('button', { name: '撤回这一轮' }).click()
    await expect(page.getByText('该回合已撤回（保留审计）').first()).toBeVisible({ timeout: 15_000 })
    await page.getByRole('button', { name: '返回列表' }).click()
    await page.getByRole('button', { name: /T15 并发控制首轮/ }).click()
    await expect(page.getByText('该回合已撤回（保留审计）').first()).toBeVisible({ timeout: 30_000 })
    // The branch history keeps the withdrawn audit record: the first turn
    // (u1/a1) and the failed T13Broken turn (no assistant existed).
    await expect(page.locator('.chat-message.withdrawn')).toHaveCount(3, { timeout: 30_000 })
  })
})
