import '@testing-library/jest-dom/vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { BackupPanel } from './BackupPanel'
import { formatBackupBytes, formatBackupTimestamp } from './backupFormat'

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const settings = { enabled: true, scheduleCron: '0 1 * * *', timezone: 'UTC', backupTarget: '/var/lib/quoin/backups', retentionCount: 2, rowVersion: 1 }
const retention = { generatedRetentionDays: 90, rowVersion: 1 }
const run = (id: string) => ({ id, status: 'succeeded', stage: 'completed', triggerKind: 'manual', executionMode: 'online', createdAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:01:00Z', dbSha256: null, manifestSha256: null, artifactCount: 0, sizeBytes: 1536, errorCode: null, errorDetail: null })

function baseResponse(path: string) {
  if (path === '/api/v1/backups/settings') return response(settings)
  if (path === '/api/v1/artifacts/retention-settings') return response(retention)
  return undefined
}

describe('BackupPanel', () => {
  beforeEach(() => { vi.stubGlobal('fetch', vi.fn()) })
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals() })

  it('automatically retries a transient trigger with its original command ID', async () => {
    const commands: string[] = []
    let attempts = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      const base = baseResponse(path)
      if (base) return base
      if (path.startsWith('/api/v1/backups?')) return response({ items: [] })
      if (path === '/api/v1/backups') {
        commands.push(JSON.parse(String(init?.body)).clientCommandId)
        attempts++
        return attempts === 1 ? response({ message: 'temporary failure' }, 503) : response(run('1'), 202)
      }
      return response({})
    })
    vi.useFakeTimers()
    render(<BackupPanel />)
    await act(async () => { await vi.advanceTimersByTimeAsync(0) })
    fireEvent.click(screen.getByRole('button', { name: '立即备份' }))
    await act(async () => {
      await Promise.resolve()
      await vi.advanceTimersByTimeAsync(1000)
    })
    expect(commands).toHaveLength(2)
    expect(commands[0]).toBe(commands[1])
  })

  it('keeps a dirty settings draft and its original row version when polling sees a newer remote version', async () => {
    let settingsReads = 0
    const writes: Array<Record<string, unknown>> = []
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      if (path === '/api/v1/backups/settings' && init?.method === 'PUT') {
        writes.push(JSON.parse(String(init.body)))
        return response({ ...settings, timezone: 'Asia/Shanghai', rowVersion: 2 })
      }
      if (path === '/api/v1/backups/settings') {
        settingsReads++
        return response(settingsReads === 1 ? settings : { ...settings, timezone: 'America/New_York', rowVersion: 2 })
      }
      const base = baseResponse(path)
      if (base) return base
      if (path.startsWith('/api/v1/backups?')) return response({ items: [] })
      return response({})
    })
    vi.useFakeTimers()
    render(<BackupPanel />)
    await act(async () => { await vi.advanceTimersByTimeAsync(0) })
    const timezone = screen.getByLabelText('时区')
    fireEvent.change(timezone, { target: { value: 'Asia/Shanghai' } })
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(screen.getByLabelText('时区')).toHaveValue('Asia/Shanghai')
    fireEvent.submit(screen.getByRole('button', { name: '保存备份设置' }).closest('form')!)
    await act(async () => { await Promise.resolve() })
    expect(writes).toHaveLength(1)
    expect(writes[0]).toMatchObject({ timezone: 'Asia/Shanghai', expectedRowVersion: 1 })
  })

  it('keeps the loaded tail cursor after a poll and loads the next page without gaps or duplicates', async () => {
    let firstPageReads = 0
    vi.mocked(fetch).mockImplementation(async (input: RequestInfo | URL) => {
      const path = String(input)
      const base = baseResponse(path)
      if (base) return base
      if (path.includes('cursor=cursor-2')) return response({ items: [run('oldest')], nextCursor: undefined })
      if (path.includes('cursor=cursor-1')) return response({ items: [run('older')], nextCursor: 'cursor-2' })
      if (path.startsWith('/api/v1/backups?')) {
        firstPageReads++
        return response({ items: [run(firstPageReads === 1 ? 'newest' : 'newest-refreshed')], nextCursor: 'cursor-1' })
      }
      return response({})
    })
    render(<BackupPanel />)
    expect(await screen.findByText(/#newest/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '加载更多备份记录' }))
    expect(await screen.findByText(/#older/)).toBeInTheDocument()
    await new Promise(resolve => setTimeout(resolve, 5100))
    await waitFor(() => expect(screen.getByText(/#newest-refreshed/)).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: '加载更多备份记录' }))
    expect(await screen.findByText(/#oldest/)).toBeInTheDocument()
    const ids = screen.getAllByRole('listitem').map(item => item.textContent?.match(/#([^\s]+)/)?.[1])
    expect(ids).toEqual(['newest-refreshed', 'newest', 'older', 'oldest'])
    expect(new Set(ids).size).toBe(ids.length)
  }, 10_000)

  it('formats persisted archive-set bytes for the run projection', () => {
    expect(formatBackupBytes(1536)).toBe('1.5 KiB')
  })

  it('formats timestamps in local absolute time with a timezone', () => {
    const formatted = formatBackupTimestamp('2026-01-01T00:00:00Z')
    expect(formatted).not.toBe('2026-01-01T00:00:00Z')
    expect(formatted).toMatch(/GMT|UTC|[+−-]\d{2}/)
  })
})
