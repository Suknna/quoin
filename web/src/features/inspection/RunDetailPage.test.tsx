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
  analysisActive: false,
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
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    await waitFor(() => expect(screen.getByText('query-latency')).toBeInTheDocument())
    expect(screen.getByText('login').closest('tr')!.textContent).toContain('需要人工登录')
    await waitFor(() => expect(screen.getByText('报告 v1')).toBeInTheDocument())
    expect(screen.getByText('查询已通过。', { exact: false })).toBeInTheDocument()
  })
  it('offers distinct evidence-reuse and recollection actions for a terminal run', async () => {
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    await waitFor(() => expect(screen.getByText('已完成（有缺口）')).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: '取消巡检' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新分析' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '重新采证' })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: '返回巡检记录' }))
  })
  it('submits reanalysis without navigating away from the immutable Run', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/analyze')) return response({ id: '77', type: 'inspection_analysis', state: 'Queued', rowVersion: 1, createdAt: '2026-08-28T10:02:00Z' }, 202)
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response(detail)
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    await waitFor(() => expect(screen.getByRole('button', { name: '重新分析' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: '重新分析' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/v1/inspections/runs/42/analyze', expect.objectContaining({ method: 'POST' })))
    expect(screen.getByText('已受理重新分析，正在复用本 Run 已收集的 Evidence 生成新报告版本。')).toBeInTheDocument()
  })
  it('keeps reanalysis disabled until its authoritative detail refresh completes', async () => {
    let reanalysisAccepted = false
    let releaseRefresh: ((value: Response) => void) | undefined
    const refreshed = new Promise<Response>((resolve) => { releaseRefresh = resolve })
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/analyze')) {
        reanalysisAccepted = true
        return response({ id: '77', type: 'inspection_analysis', state: 'Queued' }, 202)
      }
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      if (path.endsWith('/runs/42')) return reanalysisAccepted ? refreshed : response(detail)
      return response(detail)
    }))
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    const reanalyze = await screen.findByRole('button', { name: '重新分析' })
    fireEvent.click(reanalyze)
    await waitFor(() => expect(screen.getByRole('button', { name: '正在重新分析…' })).toBeDisabled())
    releaseRefresh?.(response({ ...detail, analysisActive: true, latestAnalysis: { id: '77', state: 'Assigned' } }))
    await waitFor(() => expect(screen.getByRole('button', { name: '重新分析' })).toBeDisabled())
  })
  it('shows the server-authoritative failed analysis outcome and recovery path', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response({ ...detail, latestAnalysis: { id: '77', state: 'Failed', terminationReason: 'provider_unavailable' } })
    }))
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    expect(await screen.findByText('最近一次分析未完成（provider_unavailable）。可稍后重新分析。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新分析' })).toBeEnabled()
  })
  it.each(['Failed', 'Cancelled', 'Interrupted'] as const)('offers recollection but not analysis for a %s Run', async (state) => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response({ ...detail, state })
    }))
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    expect(await screen.findByRole('button', { name: '重新采证' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: '重新分析' })).not.toBeInTheDocument()
  })
  it('shows and clears cancellation from the server-authoritative active analysis state', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/cancel')) return response({ ...detail, analysisActive: false })
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response({ ...detail, analysisActive: true })
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    const cancel = await screen.findByRole('button', { name: '取消进行中的分析' })
    expect(screen.getByRole('button', { name: '重新分析' })).toBeDisabled()
    fireEvent.click(cancel)
    await waitFor(() => expect(screen.queryByRole('button', { name: '取消进行中的分析' })).not.toBeInTheDocument())
  })
  it('keeps a server-authoritative cancelling analysis visibly non-repeatable', async () => {
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response({ ...detail, analysisActive: true, latestAnalysis: { id: '77', state: 'Cancelling' } })
    }))
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={() => undefined} />)
    const cancel = await screen.findByRole('button', { name: '正在取消…' })
    expect(cancel).toBeDisabled()
  })
  it('opens the newly created Run after recollection is accepted', async () => {
    const open = vi.fn()
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/rerun')) return response({ ...detail, id: '43', state: 'Running', rowVersion: 1, reportCount: 0, analysisActive: false }, 202)
      if (path.includes('/reports/1')) return response(reportDetail)
      if (path.includes('/reports')) return response(reports)
      return response(detail)
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<RunDetailPage runId="42" onBack={() => undefined} onOpenRun={open} />)
    await waitFor(() => expect(screen.getByRole('button', { name: '重新采证' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: '重新采证' }))
    await waitFor(() => expect(open).toHaveBeenCalledWith('43'))
  })
})
