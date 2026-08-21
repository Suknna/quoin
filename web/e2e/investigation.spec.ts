import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

test.describe('T13 调查对话 @ticket-13', () => {
  test('新建空白调查首条消息流式回复、深链重载恢复、从告警带来源发起、滚动跟随', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // --- 空白调查：首条消息被受理才创建（UI-CHAT-002） ----------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '调查' }).click()
    await expect(page.getByText('还没有调查。')).toBeVisible()
    await page.getByRole('main').getByRole('button', { name: '新建调查' }).click()
    await expect(page.getByRole('heading', { name: '新调查' })).toBeVisible()
    await page.fill('#first-message', '请分析 T10Probe 告警的排查思路')
    await page.getByRole('button', { name: '发送', exact: true }).click()

    // 调查页以派生标题打开，流式回复在真实 ui-message-stream 上到达。
    await expect(page.getByRole('heading', { name: /请分析 T10Probe/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('fixture-proof-t13')).toBeVisible({ timeout: 120_000 })
    await expect(page.getByText('调查结论：')).toBeVisible()

    // --- 深链/重载恢复：持久 head 与消息在真实浏览器刷新后还原 -------
    await page.reload()
    await expect(page.getByRole('heading', { name: /请分析 T10Probe/ })).toBeVisible({ timeout: 30_000 })
    await expect(page.getByText('fixture-proof-t13')).toBeVisible({ timeout: 30_000 })

    // --- 从告警发起：自动携带不可变来源（UI-ALERT-005） ----------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '告警' }).click()
    await page.getByText('T10Probe', { exact: false }).first().click()
    await expect(page.getByRole('heading', { name: /T10Probe/ })).toBeVisible()
    await page.getByRole('button', { name: '发起调查' }).click()
    await expect(page.getByRole('heading', { name: '新调查' })).toBeVisible()
    await expect(page.getByText(/告警 #\d+/)).toBeVisible()
    // The long first message gives the thread real scroll distance for the
    // follow scenario below.
    await page.fill('#first-message', `第二条消息：请继续排查并给出结论，${'请覆盖容量与依赖的完整证据链，'.repeat(60)}`)
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByText('fixture-proof-t13').first()).toBeVisible({ timeout: 120_000 })

    // --- 滚动跟随：向上阅读停止跟随并显示入口（UI-CHAT-003） ----------
    const composer = page.locator('.chat-composer-input')
    // The long message keeps the thread overflowing; the fixture streams
    // this turn slowly (容量/依赖 branch) so the reader detach lands
    // mid-stream deterministically.
    await composer.fill(`第三条消息：请围绕容量、依赖与恢复顺序给出完整结论，${'并补充现场处置建议，'.repeat(120)}`)
    await page.getByRole('button', { name: '发送', exact: true }).click()
    // The first delta of the slow turn arrives quickly; the wheel then
    // detaches while the stream is still running. The pointer must sit
    // over the thread content — hovering the viewport's bounding box can
    // land on the composer textarea instead.
    await expect(page.locator('.chat-message-assistant').last()).toContainText('调查结论：', { timeout: 60_000 })
    await page.locator('.chat-message-assistant').last().hover()
    await page.mouse.wheel(0, -1200)
    await expect(page.getByRole('button', { name: '查看新回复' })).toBeVisible({ timeout: 60_000 })
    await page.getByRole('button', { name: '查看新回复' }).click()
    await expect(page.getByText('fixture-proof-t13').last()).toBeVisible({ timeout: 120_000 })

    // --- 列表投影：两条调查按机械派生标题呈现（UI-CHAT-001） ----------
    await page.getByRole('button', { name: '返回列表' }).click()
    await expect(page.getByRole('button', { name: /第二条消息：请继续排查/ })).toBeVisible()
    await expect(page.getByRole('button', { name: /请分析 T10Probe/ })).toBeVisible()
  })
})
