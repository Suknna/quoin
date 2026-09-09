import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useInvestigationsModule } from './index'

const { api } = vi.hoisted(() => ({ api: {
  list: vi.fn(), get: vi.fn(), listMessages: vi.fn(), listAttempts: vi.fn(), create: vi.fn(), sendMessage: vi.fn(), cancelAttempt: vi.fn(), retryAttempt: vi.fn(), undo: vi.fn(),
} }))
vi.mock('@/features/investigation/api', () => ({ api, sourceLabel: (type: string) => type }))
vi.mock('@/features/investigation/attachments/api', () => ({ attachmentCommandId: () => 'upload-command', uploadAttachment: vi.fn() }))
vi.mock('@/features/investigation/tools/api', () => ({ listToolCalls: vi.fn() }))
vi.mock('@/features/feedback/api', () => ({ appendFeedback: vi.fn(), fetchFeedback: vi.fn(), feedbackValueLabels: {} }))
vi.mock('@/features/knowledge/api', () => ({ api: { createMessageCandidate: vi.fn() } }))
const user = { id: 'u', username: 'operator', displayName: 'Operator', role: 'operator' as const, passwordChangeRequired: false, authRevision: 1, enabled: true, lastLoginAt: null, rowVersion: 1 }
function View({ route, suspended = false }: { route: string; suspended?: boolean }) { const view = useInvestigationsModule({ user, route, suspended, navigate: vi.fn(), openEvidence: vi.fn() }); return <>{view.list}{view.content}</> }
const detail = { id: 'i1', displayTitle: 'CPU 排查', lastActivityAt: '2026-01-01T00:00:00Z', createdAt: '2026-01-01T00:00:00Z', createdBy: 'u', headMessageId: 'm1', activeAttemptId: 'a1', messageCount: 1, attemptCount: 1, sources: [] }
afterEach(() => { cleanup(); vi.clearAllMocks() })
describe('investigations module', () => {
  it('creates the first message atomically with supplied occurrence sources', async () => {
    api.list.mockResolvedValue({ items: [] }); api.create.mockResolvedValue({ ...detail, id: 'i2' })
    render(<View route="/investigations/new?occurrence=o1&initialAnalysis=a1" />)
    fireEvent.change(screen.getByPlaceholderText('描述需要调查的问题…'), { target: { value: '排查 CPU' } })
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    await waitFor(() => expect(api.create).toHaveBeenCalledWith('排查 CPU', [{ type: 'occurrence', sourceId: 'o1' }, { type: 'initial_analysis', sourceId: 'a1' }], []))
  })
  it('does not create an empty investigation', async () => {
    api.list.mockResolvedValue({ items: [] }); render(<View route="/investigations/new" />)
    fireEvent.click(screen.getByRole('button', { name: '创建并发送' }))
    expect(api.create).not.toHaveBeenCalled(); expect(await screen.findByText('请输入第一条消息或添加附件。')).toBeInTheDocument()
  })
  it('cancels the active attempt using its authoritative row version', async () => {
    api.list.mockResolvedValue({ items: [] }); api.get.mockResolvedValue(detail); api.listMessages.mockResolvedValue({ items: [] }); api.listAttempts.mockResolvedValue({ items: [{ id: 'a1', type: 'chat', state: 'Running', rowVersion: 7, createdAt: '2026-01-01T00:00:00Z' }] }); api.cancelAttempt.mockResolvedValue({})
    render(<View route="/investigations/i1" />)
    fireEvent.click(await screen.findByRole('button', { name: '停止 Running' }))
    await waitFor(() => expect(api.cancelAttempt).toHaveBeenCalledWith('i1', 'a1', 7))
  })
  it('shows a failed list request instead of an empty healthy state', async () => {
    api.list.mockRejectedValue(new Error('调查服务不可用'))
    render(<View route="/investigations" />)
    expect(await screen.findByText('调查服务不可用')).toBeInTheDocument()
  })
})
