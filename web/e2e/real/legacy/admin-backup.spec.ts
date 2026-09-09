import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Browser, type Page } from '@playwright/test'

const stackDir = join(import.meta.dirname, '..', '..', '.artifacts', 'e2e-stack')
const evidenceDir = process.env.QUOIN_EVIDENCE_DIR

function password() {
  const path = join(stackDir, 'admin-new-password')
  if (!existsSync(path)) throw new Error(`admin password fixture missing: ${path}`)
  return readFileSync(path, 'utf-8').trim()
}

function writeObservations(value: unknown) {
  if (!evidenceDir) return
  mkdirSync(evidenceDir, { recursive: true })
  writeFileSync(join(evidenceDir, 't32-backup-observations.json'), JSON.stringify(value, null, 2))
}

async function signIn(page: Page) {
  await page.goto('/')
  await page.fill('#username', 'admin')
  await page.fill('#password', password())
  await page.getByRole('button', { name: '登录' }).click()
  await expect(page.getByRole('navigation', { name: '全局模块' })).toBeVisible({ timeout: 30_000 })
  await page.getByRole('button', { name: '管理' }).click()
  await page.getByRole('button', { name: '备份与保留' }).click()
  await expect(page.getByRole('heading', { name: '备份与保留' })).toBeVisible()
}

async function waitForTerminal(page: Page, status: 'succeeded' | 'failed') {
  const run = page.locator('.admin-user-list li').filter({ hasText: status }).first()
  await expect(run).toBeVisible({ timeout: 90_000 })
  return run
}

function runID(text: string | null) {
  const match = text?.match(/#(\d+)/)
  if (!match) throw new Error(`Backup Run ID missing from row: ${text}`)
  return Number(match[1])
}

async function waitForSucceededAfter(page: Page, failedID: number) {
  await expect.poll(async () => {
    const rows = await page.locator('.admin-user-list li').allTextContents()
    return rows.some(row => row.includes('succeeded') && runID(row) > failedID)
  }, { timeout: 90_000 }).toBe(true)
  const rows = page.locator('.admin-user-list li').filter({ hasText: 'succeeded' })
  for (let index = 0; index < await rows.count(); index++) {
    const row = rows.nth(index)
    if (runID(await row.textContent()) > failedID) return row
  }
  throw new Error('Succeeded recovery Run disappeared after poll')
}

function backupMount() {
  const raw = execFileSync('docker', ['inspect', '--format', '{{json .Mounts}}', 'quoin-quoin-1'], { encoding: 'utf8' })
  const mounts = JSON.parse(raw) as Array<{ Source: string; Destination: string }>
  const mount = mounts.find(value => value.Destination === '/var/lib/quoin/backups')
  if (!mount) throw new Error(`Quoin backup mount was not found: ${raw}`)
  return mount.Source
}

function verifyDownloadedArchive(path: string) {
  const members = execFileSync('tar', ['-tf', path], { encoding: 'utf8' }).trim().split('\n').filter(Boolean)
  expect(members).toContain('manifest.json')
  const extracted = `${path}.contents`
  rmSync(extracted, { recursive: true, force: true })
  mkdirSync(extracted, { recursive: true })
  execFileSync('tar', ['-xf', path, '-C', extracted])
  const manifestBody = readFileSync(join(extracted, 'manifest.json'))
  const manifest = JSON.parse(manifestBody.toString()) as {
    version: number
    database: { path: string; sha256: string; sizeBytes: number }
    artifacts: Array<{ path: string; sha256: string; sizeBytes: number }>
  }
  expect(manifest.version).toBe(1)
  let archiveSetBytes = statSync(join(extracted, 'manifest.json')).size
  for (const entry of [manifest.database, ...manifest.artifacts]) {
    const path = join(extracted, entry.path)
    const body = readFileSync(path)
    expect(createHash('sha256').update(body).digest('hex')).toBe(entry.sha256)
    expect(statSync(path).size).toBe(entry.sizeBytes)
    archiveSetBytes += entry.sizeBytes
  }
  rmSync(extracted, { recursive: true, force: true })
  return { members, database: manifest.database.path, artifactCount: manifest.artifacts.length, archiveSetBytes }
}

async function simultaneousTrigger(browser: Browser, first: Page) {
  const second = await browser.newPage()
  try {
    await signIn(second)
    const firstClick = first.getByRole('button', { name: '立即备份' }).click()
    const secondClick = second.getByRole('button', { name: '立即备份' }).click()
    await Promise.all([firstClick, secondClick])
    await Promise.race([
      expect(second.getByRole('alert')).toContainText('已有正在等待或执行的备份。', { timeout: 30_000 }),
      expect(first.getByRole('alert')).toContainText('已有正在等待或执行的备份。', { timeout: 30_000 }),
    ])
  } finally {
    await second.close()
  }
}

test.describe('T32 backup administration @ticket-32', () => {
  test('Admin triggers, leaves and returns, handles the active conflict, survives a real backup-volume fault, and verifies the downloaded archive', async ({ page, browser }) => {
    test.slow()
    test.setTimeout(300_000)
    const observed: Record<string, unknown> = { startedAt: new Date().toISOString() }
    await signIn(page)
    await expect(page.getByText(/备份目标：\/var\/lib\/quoin\/backups。/)).toBeVisible()
    observed.backupTarget = '/var/lib/quoin/backups'

    await simultaneousTrigger(browser, page)
    observed.activeConflict = 'one concurrent Admin request received the visible active-run conflict'
    await page.getByRole('button', { name: '告警' }).click()
    await expect(page.getByRole('heading', { name: '告警', exact: true })).toBeVisible()
    await page.getByRole('button', { name: '管理' }).click()
    await page.getByRole('button', { name: '备份与保留' }).click()
    await expect(page.getByRole('heading', { name: '备份与保留' })).toBeVisible()
    await waitForTerminal(page, 'succeeded')
    observed.leaveAndReturn = 'terminal backup remains projected after route leave and return'

    const target = backupMount()
    const mode = statSync(target).mode
    chmodSync(target, mode & ~0o222)
    try {
      await page.getByRole('button', { name: '立即备份' }).click()
      const failedRun = await waitForTerminal(page, 'failed')
      await expect(failedRun).toContainText('storage_unavailable')
      const failedRunID = runID(await failedRun.textContent())
      observed.physicalFault = { target, failedRunID, injected: 'removed write permission from actual Compose backup volume', observed: 'failed/storage_unavailable' }
    } finally {
      chmodSync(target, mode)
    }

    const failedRunID = (observed.physicalFault as { failedRunID: number }).failedRunID
    await page.getByRole('button', { name: '立即备份' }).click()
    const recoveredRun = await waitForSucceededAfter(page, failedRunID)
    const recoveredRunID = runID(await recoveredRun.textContent())
    expect(recoveredRunID).toBeGreaterThan(failedRunID)
    observed.recovery = { failedRunID, recoveredRunID }
    await expect(recoveredRun).toContainText(/大小\s+\d/)
    const detail = await page.evaluate(async (id) => {
      const response = await fetch(`/api/v1/backups/${id}`, { credentials: 'include' })
      if (!response.ok) throw new Error(`backup detail status ${response.status}`)
      return response.json() as Promise<{ sizeBytes: number }>
    }, recoveredRunID)
    const link = recoveredRun.getByRole('link', { name: '下载归档' })
    const download = await Promise.all([page.waitForEvent('download'), link.click()]).then(([value]) => value)
    const archive = join(stackDir, `t32-${Date.now()}.tar`)
    try {
      await download.saveAs(archive)
      const verified = verifyDownloadedArchive(archive)
      expect(detail.sizeBytes).toBe(verified.archiveSetBytes)
      observed.download = { ...verified, sizeBytes: detail.sizeBytes }
    } finally {
      rmSync(archive, { force: true })
      rmSync(`${archive}.contents`, { recursive: true, force: true })
    }
    observed.finishedAt = new Date().toISOString()
    writeObservations(observed)
  })
})
