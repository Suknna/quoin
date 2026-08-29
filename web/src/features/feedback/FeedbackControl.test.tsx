import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FeedbackControl } from './FeedbackControl'

// FeedbackControl tests: latest-value projection, append-only history and
// the rejected confirmation boundary (UI-FEEDBACK-001/002).

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

afterEach(cleanup)

describe('FeedbackControl', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  it('shows the latest value and the full history', async () => {
    const timeline = {
      latestValue: 'verified_effective',
      items: [
        { id: '2', targetType: 'initial_analysis_output', targetId: '9', value: 'verified_effective', createdAt: '2026-01-02T00:00:00Z' },
        { id: '1', targetType: 'initial_analysis_output', targetId: '9', value: 'adopted', note: '对症', createdAt: '2026-01-01T00:00:00Z' },
      ],
    }
    vi.mocked(fetch).mockResolvedValue(response(timeline))
    render(<FeedbackControl target={{ type: 'initial_analysis_output', id: '9' }} />)
    expect(await screen.findByText('验证有效')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '查看反馈历史（2 条）' }))
    expect(screen.getByText('对症')).toBeInTheDocument()
    expect(screen.getAllByText('已采纳').length).toBe(2)
  })

  it('appends a chosen value with the note', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ latestValue: undefined, items: [] }))
    render(<FeedbackControl target={{ type: 'initial_analysis_output', id: '9' }} />)
    await screen.findByText('还没有记录过这条诊断的实际结果。')
    vi.mocked(fetch)
      .mockResolvedValueOnce(response({ id: '7', targetType: 'initial_analysis_output', targetId: '9', value: 'executed', createdAt: '2026-01-02T00:00:00Z' }))
      .mockResolvedValueOnce(response({ latestValue: 'executed', items: [{ id: '7', targetType: 'initial_analysis_output', targetId: '9', value: 'executed', note: '已重启', createdAt: '2026-01-02T00:00:00Z' }] }))
    fireEvent.change(screen.getByLabelText('反馈说明'), { target: { value: '已重启' } })
    fireEvent.click(screen.getByRole('button', { name: '已执行' }))
    await waitFor(() => expect(screen.getAllByText('已执行').length).toBeGreaterThan(0))
    const appendCall = vi.mocked(fetch).mock.calls.find((call) => String(call[0]) === '/api/v1/knowledge/feedback' && (call[1] as RequestInit | undefined)?.method === 'POST')
    expect(appendCall).toBeDefined()
    const body = JSON.parse(String(appendCall?.[1]?.body)) as { value: string; note: string }
    expect(body.value).toBe('executed')
    expect(body.note).toBe('已重启')
  })

  it('confirms before recording a rejection', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ latestValue: undefined, items: [] }))
    render(<FeedbackControl target={{ type: 'investigation_message', id: '4' }} />)
    await screen.findByText('还没有记录过这条诊断的实际结果。')
    fireEvent.click(screen.getByRole('button', { name: '不采纳' }))
    expect(await screen.findByRole('dialog', { name: '确认标记为“不采纳”？' })).toBeInTheDocument()
    expect(screen.getByText(/永久退出检索/)).toBeInTheDocument()
    expect(vi.mocked(fetch).mock.calls.filter((call) => String(call[0]) === '/api/v1/knowledge/feedback' && (call[1] as RequestInit | undefined)?.method === 'POST')).toHaveLength(0)
    vi.mocked(fetch)
      .mockResolvedValueOnce(response({ id: '8', targetType: 'investigation_message', targetId: '4', value: 'rejected', createdAt: '2026-01-02T00:00:00Z' }))
      .mockResolvedValueOnce(response({ latestValue: 'rejected', items: [{ id: '8', targetType: 'investigation_message', targetId: '4', value: 'rejected', createdAt: '2026-01-02T00:00:00Z' }] }))
    fireEvent.click(screen.getByRole('button', { name: '确认不采纳' }))
    await waitFor(() => expect(screen.getAllByText('不采纳').length).toBeGreaterThan(0))
  })
})
