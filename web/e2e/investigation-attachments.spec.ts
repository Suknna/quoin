import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Locator } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')

function readAdminPassword(): string {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf8').trim()
}

const pasteText = (blocks: number) => Array.from({ length: blocks }, (_, index) => `第${index + 1}行 粘贴样本日志`).join('\n')

test.describe('T14 调查附件与工具 @ticket-14', () => {
  test('键盘附件流、粘贴阈值转换、附件-only 发送、多附件折叠与工具卡证据', async ({ page }) => {
    test.slow()
    await page.goto('/')
    await page.fill('#username', 'admin')
    await page.fill('#password', readAdminPassword())
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })

    // --- 新建调查：键盘到达附件入口并选择文件 --------------------------
    await page.getByRole('navigation', { name: '全局模块' }).getByRole('button', { name: '调查' }).click()
    await page.getByRole('main').getByRole('button', { name: '新建调查' }).click()
    await expect(page.getByRole('heading', { name: '新调查' })).toBeVisible()

    // The first-message entry's own file input is keyboard-reachable via
    // its label; attach two files without touching the mouse.
    await page.focus('#first-message')
    const fileInput = page.locator('.new-investigation input[type="file"]')
    await fileInput.setInputFiles([
      { name: 't14-keyboard.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-KEYBOARD 内容：键盘附件流。') },
      { name: 't14-second.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-SECOND 内容：第二份。') },
    ])
    await expect(page.locator('.attachment-chip', { hasText: 't14-keyboard.txt' })).toBeVisible()
    await expect(page.locator('.attachment-chip', { hasText: 't14-second.txt' })).toBeVisible()

    // --- 粘贴阈值：一次大粘贴转换为可移除的 .txt 附件 ------------------
    const composer = page.locator('#first-message')
    await composer.evaluate((element, text) => {
      const transfer = new DataTransfer()
      transfer.setData('text/plain', text)
      element.dispatchEvent(new ClipboardEvent('paste', { clipboardData: transfer, bubbles: true, cancelable: true }))
    }, pasteText(220))
    await expect(page.locator('.attachment-chip', { hasText: /粘贴-\d{8}-\d{6}\.txt/ })).toBeVisible()
    // The pasted text did NOT flood the textarea (typed input only).
    await expect(composer).toHaveValue('')

    // --- 键盘移除一份附件 ----------------------------------------------
    const secondChipRemove = page.locator('.attachment-chip', { hasText: 't14-second.txt' }).getByRole('button', { name: '移除附件 t14-second.txt' })
    await secondChipRemove.focus()
    await page.keyboard.press('Enter')
    await expect(page.locator('.attachment-chip', { hasText: 't14-second.txt' })).toHaveCount(0)

    // Ctrl+Enter submits from the keyboard (attachment present, empty text
    // allowed: content or attachment, never both empty). The displayTitle
    // falls back for attachment-only first turns (UI-CHAT-001). Focus
    // returns to the textarea first — the remove button held it.
    await page.focus('#first-message')
    await page.keyboard.press('Control+Enter')
    await expect(page.getByRole('heading', { name: /新调查/ })).toBeVisible({ timeout: 30_000 })

    // The fixture reads the attachments through artifact_read and answers
    // with a bounded slice of the pasted body.
    await expect(page.getByText('attachment-proof-t14')).toBeVisible({ timeout: 120_000 })

    // --- 附件-only 回合 + 多附件折叠 + 工具卡 ---------------------------
    const chatComposer = page.locator('.chat-composer-input')
    await expect(chatComposer).toBeVisible()

    // Attachment-only turn via the native composer attachment button
    // (keyboard-operable; Playwright intercepts the file chooser).
    await page.getByRole('button', { name: '附件', exact: true }).focus()
    const chooser = page.waitForEvent('filechooser')
    await page.keyboard.press('Enter')
    const files = await chooser
    await files.setFiles([
      { name: 't14-chat-a.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-CHAT-A 聊天附件 A。') },
      { name: 't14-chat-b.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-CHAT-B 聊天附件 B。') },
      { name: 't14-chat-c.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-CHAT-C 聊天附件 C。') },
      { name: 't14-chat-d.txt', mimeType: 'text/plain', buffer: Buffer.from('T14-CHAT-D 聊天附件 D。') },
    ])
    await expect(page.locator('.chat-composer .attachment-chip', { hasText: 't14-chat-a.txt' })).toBeVisible({ timeout: 30_000 })
    // The composer send stays clickable with empty text when attachments
    // are staged (attachment-only turns are legal).
    await page.getByRole('button', { name: '发送', exact: true }).click()

    // The durable view folds more than three attachments under the sent
    // message and expands in place.
    await expect(page.getByText('共 4 份附件')).toBeVisible({ timeout: 120_000 })
    await page.getByText('共 4 份附件').click()
    await expect(page.locator('.message-attachment', { hasText: 't14-chat-d.txt' })).toBeVisible()

    // --- 工具链回合：工具卡、参数折叠、产物下载入口 ---------------------
    await chatComposer.fill('T14Tools 请执行完整工具链')
    await page.getByRole('button', { name: '发送', exact: true }).click()
    await expect(page.getByText('t14-tools-proof')).toBeVisible({ timeout: 180_000 })

    const toolCard = (name: string): Locator => page.locator('.tool-call-card', { hasText: name })
    await expect(toolCard('工作区命令')).toContainText('已完成')
    await expect(toolCard('Thanos 查询')).toContainText('已完成')
    // Raw arguments fold in place behind the summary row (the card
    // carries separate 参数/结果 blocks).
    await toolCard('Thanos 查询').locator('.tool-call-summary').click()
    await expect(toolCard('Thanos 查询').locator('.tool-call-json').first()).toContainText('"query"')
    // The spilled long output exposes its artifact download entry inside
    // its expanded details.
    await toolCard('工作区命令').locator('.tool-call-summary').first().click()
    await expect(toolCard('工作区命令').locator('.tool-call-details .message-attachment').first()).toContainText('完整输出产物')

    // The assistant turn carries the sealed evidence link.
    const evidenceToggle = page.locator('.tool-evidence-toggle').first()
    await expect(evidenceToggle).toBeVisible()
    await evidenceToggle.click()
    await expect(page.locator('.tool-evidence-detail')).toContainText('观测时间')

    // --- 列表投影回到调查（附件-only 首条消息回退到创建时间标题） --------
    await page.getByRole('button', { name: '返回列表' }).click()
    await expect(page.getByRole('complementary', { name: '调查列表' }).getByRole('button', { name: /2026/ })).toBeVisible()
  })
})
