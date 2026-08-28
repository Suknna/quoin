import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RunDetailPage } from './RunDetailPage'

function response(body: unknown, status = 200): Response { return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }) }
const detail = {
  id: '42', businessSystemKey: 'payments', planKey: 'critical-path', state: 'CompletedWithGaps' as const,
  rowVersion: 3, triggerKind: 'manual' as const, evidenceAt: '2026-08-28T10:00:00Z', createdAt: '2026-08-28T09:59:00Z',
  checks: [
    { checkKey: 'query-latency', status: 'ok' as const, evidenceId: '88' },
    { checkKey: 'login', status: 'gap' as const, gapReason: 'authentication_required' },
  ],
  reportCount: 1,
}
const reports = { items: [{ version: 1, modelId: 'ops-model', createdAt: '2026-08-28T10:01:00Z' }] }
const reportDetail = {
  runId: '42', version: 1, evidenceDigest: 'a'.repeat(64), evidenceIds: ['88'],
  modelId: 'ops-model', content: '查询已通过。\n浏览器身份需要重新登录。', createdAt: '2026-08-28T10:01:00Z',
}

describe('RunDetailPage', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const path = String(input)
    if (path.includes('/reports/1')) return response(reportDetail)
    if (path.includes('/reports')) return response(reports)
    return response(detail)
  })))
  afterEach(() => { cleanup(); vi.unstubAllGlobals() })
  it('renders mixed check outcomes and the immutable report', async () => {
    render(<RunDetailPage runId="42" onBack={() => undefined} />)
    await waitFor(() => expect(screen.getByText('query-latency')).toBeInTheDocument())
    expect(screen.getByText('login').closest('tr')!.textContent).toContain('需要人工登录')
    await waitFor(() => expect(screen.getByText('报告 v1')).toBeInTheDocument())
    expect(screen.getByText('查询已通过。', { exact: false })).toBeInTheDocument()
  })
  it('does not render cancellation for a terminal run', async () => {
    render(<RunDetailPage runId="42" onBack={() => undefined} />)
    await waitFor(() => expect(screen.getByText('已完成（有缺口）')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '取消巡检' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '返回巡检记录' }))
  })
})
