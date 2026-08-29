import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CandidateEditor } from './CandidateEditor'
import type { CandidateDetail } from './api'

// CandidateEditor tests (UI-KNOWLEDGE-003): the original suggestion stays
// read-only, draft saves carry the expected revision, and a conflict keeps
// the local input while surfacing the authoritative revision.

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const detail: CandidateDetail = {
  id: '5',
  sourceType: 'initial_analysis_output',
  sourceId: '9',
  state: 'AwaitingConfirmation',
  rowVersion: 1,
  generation: 1,
  draftRevision: 0,
  draftTitle: '连接池处置',
  draftBody: '先检查连接池上限。',
  originalSuggestion: {
    v: 1,
    source: { type: 'initial_analysis_output', id: '9', modelId: 'fixture-chat', locator: { analysisId: 3 } },
    title: '数据库连接池耗尽导致超时。',
    body: '数据库连接池耗尽导致超时。\n建议检查最大连接数。',
  },
}

afterEach(cleanup)

describe('CandidateEditor', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
  })

  it('renders the immutable suggestion and prefilled draft', async () => {
    vi.mocked(fetch).mockResolvedValue(response(detail))
    render(<CandidateEditor candidateId="5" onClose={() => undefined} onConfirmed={() => undefined} />)
    expect(await screen.findByDisplayValue('连接池处置')).toBeInTheDocument()
    expect(screen.getByText('数据库连接池耗尽导致超时。')).toBeInTheDocument()
    expect((screen.getByLabelText('正文') as HTMLTextAreaElement).value).toBe('先检查连接池上限。')
    expect(screen.getByText(/模型原始建议（不可修改）/)).toBeInTheDocument()
  })

  it('reports revision conflicts without losing local input', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response(detail))
    render(<CandidateEditor candidateId="5" onClose={() => undefined} onConfirmed={() => undefined} />)
    await screen.findByDisplayValue('连接池处置')
    const body = screen.getByLabelText('正文') as HTMLTextAreaElement
    fireEvent.change(body, { target: { value: '扩容后重启实例。' } })
    vi.mocked(fetch).mockResolvedValue(response({ message: '草稿已被更新，请基于最新版本修改。', conflict: { code: 'row_version_conflict', currentRevision: 3 } }, 409))
    fireEvent.click(screen.getByRole('button', { name: '保存草稿' }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('草稿已被其他页面更新'))
    expect((screen.getByLabelText('正文') as HTMLTextAreaElement).value).toBe('扩容后重启实例。')
    expect(screen.getByText(/r3/)).toBeInTheDocument()
  })

  it('edits the scope rows and saves them with the draft', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response(detail))
    render(<CandidateEditor candidateId="5" onClose={() => undefined} onConfirmed={() => undefined} />)
    await screen.findByDisplayValue('连接池处置')
    fireEvent.click(screen.getByRole('button', { name: '添加范围' }))
    fireEvent.change(screen.getByLabelText('范围键 1'), { target: { value: '业务系统' } })
    fireEvent.change(screen.getByLabelText('范围值 1'), { target: { value: 'payments' } })
    vi.mocked(fetch).mockResolvedValue(response({ ...detail, draftRevision: 1, draftScope: { 业务系统: 'payments' } }))
    fireEvent.click(screen.getByRole('button', { name: '保存草稿' }))
    await waitFor(() => expect(screen.getByText('草稿已保存。')).toBeInTheDocument())
    const editCall = vi.mocked(fetch).mock.calls.find((call) => String(call[0]).endsWith('/knowledge/candidates/5') && (call[1] as RequestInit | undefined)?.method === 'PATCH')
    expect(editCall).toBeDefined()
    const body = JSON.parse(String(editCall?.[1]?.body)) as { scope: Record<string, string> }
    expect(body.scope).toEqual({ 业务系统: 'payments' })
  })

  it('confirms through the human boundary and hands the knowledge id over', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(response(detail))
    const confirmed = vi.fn()
    render(<CandidateEditor candidateId="5" onClose={() => undefined} onConfirmed={confirmed} />)
    await screen.findByDisplayValue('连接池处置')
    vi.mocked(fetch).mockResolvedValue(response({ ...detail, state: 'Confirmed', confirmedKnowledgeId: '12', draftRevision: 0 }))
    fireEvent.click(screen.getByRole('button', { name: '确认并创建知识' }))
    await waitFor(() => expect(confirmed).toHaveBeenCalledWith('12'))
    const confirmCall = vi.mocked(fetch).mock.calls.find((call) => String(call[0]).endsWith('/confirm'))
    expect(confirmCall).toBeDefined()
    const body = JSON.parse(String(confirmCall?.[1]?.body)) as { expectedRevision: number }
    expect(body.expectedRevision).toBe(0)
  })
})
