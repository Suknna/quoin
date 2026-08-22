import '@testing-library/jest-dom/vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LabelContractsPanel } from './LabelContractsPanel'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const contracts = [
  { id: '1', version: 1, state: 'active' as const, rowVersion: 3, parserVersion: 'p', schemaVersion: 's', createdAt: '2026-09-01T00:00:00Z', activatedAt: '2026-09-01T00:00:05Z' },
  { id: '2', version: 2, state: 'draft' as const, rowVersion: 1, parserVersion: 'p', schemaVersion: 's', createdAt: '2026-09-02T00:00:00Z' },
]

function readinessFor(systems: Array<{ key: string; candidates: Array<[string, string]>; blockers: string[] }>) {
  return {
    targetContractVersion: 2,
    stateRowVersion: 3,
    targetRowVersion: 1,
    currentContractVersionId: '1',
    systems: systems.map((system) => ({
      businessSystemKey: system.key,
      currentConfigVersionId: null,
      businessSystemRowVersion: 1,
      activationCandidates: system.candidates.map(([configVersionId, passedVerificationRunId]) => ({ configVersionId, passedVerificationRunId })),
      blockers: system.blockers,
    })),
  }
}

describe('LabelContractsPanel readiness + activation', () => {
  beforeEach(() => vi.stubGlobal('fetch', vi.fn()))
  afterEach(cleanup)

  it('focuses the panel on open and restores the invoking control on close', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockResolvedValue(jsonResponse({ items: [] }))
    const opener = document.createElement('button')
    document.body.append(opener)
    opener.focus()
    const { unmount } = render(<LabelContractsPanel isAdmin onClose={() => undefined} onOpenSystem={() => undefined} />)
    expect(screen.getByRole('button', { name: '关闭' })).toHaveFocus()
    unmount()
    expect(opener).toHaveFocus()
    opener.remove()
  })

  it('renders per-system candidates and blockers from the readiness view', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input) => {
      const url = String(input)
      if (url.includes('/readiness')) {
        return jsonResponse(readinessFor([
          { key: 'payments', candidates: [['7', '3']], blockers: [] },
          { key: 'billing', candidates: [], blockers: ['verification_run_missing'] },
        ]))
      }
      return jsonResponse({ items: contracts })
    })
    render(<LabelContractsPanel isAdmin onClose={() => undefined} onOpenSystem={() => undefined} />)
    expect(await screen.findByText('逐系统就绪')).toBeInTheDocument()
    expect(screen.getByText(/配置版本 #7 · 验证 Run #3/)).toBeInTheDocument()
    expect(screen.getByText(/草稿还没有运行过 Config Verification Run/)).toBeInTheDocument()
    // Blocked systems keep the atomic activate entry disabled (no partial activation).
    expect(screen.getByRole('button', { name: '原子激活此契约' })).toBeDisabled()
  })

  it('allows the first contract to activate with zero enabled systems', async () => {
    const fetchMock = vi.mocked(fetch)
    let activationBody: unknown
    fetchMock.mockImplementation(async (input, init) => {
      const url = String(input)
      if (url.includes('/activate')) {
        activationBody = JSON.parse(String(init?.body ?? '{}'))
        return jsonResponse({ id: '2', version: 2, state: 'active' })
      }
      if (url.includes('/readiness')) return jsonResponse(readinessFor([]))
      return jsonResponse({ items: contracts })
    })
    const close = vi.fn()
    render(<LabelContractsPanel isAdmin onClose={close} onOpenSystem={() => undefined} />)
    expect(await screen.findByText(/当前没有启用中的业务系统/)).toBeInTheDocument()
    const activate = screen.getByRole('button', { name: '原子激活此契约' })
    expect(activate).toBeEnabled()
    fireEvent.click(activate)
    fireEvent.click(screen.getByRole('button', { name: '确认原子激活' }))
    await waitFor(() => expect(close).toHaveBeenCalledOnce())
    expect(activationBody).toMatchObject({
      expectedStateRowVersion: 3,
      expectedCurrentContractVersionId: '1',
      expectedTargetRowVersion: 1,
      compatibleVersions: [],
    })
  })

  it('traps confirmation focus and returns it to the activation action on Escape', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input) => {
      if (String(input).includes('/readiness')) return jsonResponse(readinessFor([]))
      return jsonResponse({ items: contracts })
    })
    render(<LabelContractsPanel isAdmin onClose={() => undefined} onOpenSystem={() => undefined} />)
    await screen.findByText(/当前没有启用中的业务系统/)
    const activate = screen.getByRole('button', { name: '原子激活此契约' })
    fireEvent.click(activate)
    const confirmation = screen.getByRole('dialog', { name: '确认原子激活' })
    const cancel = screen.getByRole('button', { name: '取消' })
    // aria-modal's background must be genuinely inert, not merely visually
    // covered, so pointer/focus interaction cannot change activation choices.
    const background = screen.getByRole('button', { name: '关闭', hidden: true }).closest('[inert]')
    expect(background).toHaveAttribute('inert')
    expect(background).toHaveAttribute('aria-hidden', 'true')
    expect(cancel).toHaveFocus()
    fireEvent.keyDown(cancel, { key: 'Tab' })
    expect(screen.getByRole('button', { name: '确认原子激活' })).toHaveFocus()
    fireEvent.keyDown(confirmation, { key: 'Escape' })
    expect(screen.queryByRole('dialog', { name: '确认原子激活' })).not.toBeInTheDocument()
    await waitFor(() => expect(screen.getByRole('button', { name: '原子激活此契约' })).toHaveFocus())
  })

  it('requires an explicit choice when a system has multiple legal candidates', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input) => {
      const url = String(input)
      if (url.includes('/readiness')) {
        return jsonResponse(readinessFor([
          { key: 'payments', candidates: [['7', '3'], ['8', '4']], blockers: [] },
        ]))
      }
      return jsonResponse({ items: contracts })
    })
    render(<LabelContractsPanel isAdmin onClose={() => undefined} onOpenSystem={() => undefined} />)
    await screen.findByText('逐系统就绪')
    const activate = screen.getByRole('button', { name: '原子激活此契约' })
    expect(activate).toBeDisabled()
    // Choosing one candidate enables the confirmation path.
    fireEvent.click(screen.getByLabelText(/配置版本 #8/))
    expect(activate).toBeEnabled()
    fireEvent.click(activate)
    expect(screen.getByRole('dialog', { name: '确认原子激活' })).toBeInTheDocument()
  })

  it('submits the exact selected candidate pair when a config version has multiple Passed runs', async () => {
    const fetchMock = vi.mocked(fetch)
    let activationBody: { compatibleVersions?: Array<{ configVersionId: string; verificationRunId: string }> } | undefined
    fetchMock.mockImplementation(async (input, init) => {
      const url = String(input)
      if (url.includes('/activate')) {
        activationBody = JSON.parse(String(init?.body ?? '{}'))
        return jsonResponse({ id: '2', version: 2, state: 'active' })
      }
      if (url.includes('/readiness')) {
        return jsonResponse(readinessFor([{ key: 'payments', candidates: [['7', '3'], ['7', '4']], blockers: [] }]))
      }
      return jsonResponse({ items: contracts })
    })
    const close = vi.fn()
    render(<LabelContractsPanel isAdmin onClose={close} onOpenSystem={() => undefined} />)
    await screen.findByText('逐系统就绪')
    fireEvent.click(screen.getByLabelText(/配置版本 #7 · 验证 Run #4/))
    fireEvent.click(screen.getByRole('button', { name: '原子激活此契约' }))
    fireEvent.click(screen.getByRole('button', { name: '确认原子激活' }))
    await waitFor(() => expect(close).toHaveBeenCalledOnce())
    expect(activationBody?.compatibleVersions).toEqual([
      expect.objectContaining({ configVersionId: '7', verificationRunId: '4' }),
    ])
  })

  it('submits one atomic activation carrying the readiness preconditions and re-reads on conflict', async () => {
    const fetchMock = vi.mocked(fetch)
    let activateCalled = false
    fetchMock.mockImplementation(async (input, init) => {
      const url = String(input)
      if (url.includes('/activate')) {
        activateCalled = true
        const body = JSON.parse(String(init?.body ?? '{}'))
        expect(body.expectedStateRowVersion).toBe(3)
        expect(body.expectedCurrentContractVersionId).toBe('1')
        expect(body.compatibleVersions).toEqual([
          { businessSystemKey: 'payments', configVersionId: '7', verificationRunId: '3', expectedCurrentConfigVersionId: null, expectedBusinessSystemRowVersion: 1 },
        ])
        return jsonResponse({ code: 'current_pointer_conflict', message: '契约当前指针已变化' }, 409)
      }
      if (url.includes('/readiness')) {
        return jsonResponse(readinessFor([{ key: 'payments', candidates: [['7', '3']], blockers: [] }]))
      }
      return jsonResponse({ items: contracts })
    })
    const close = vi.fn()
    render(<LabelContractsPanel isAdmin onClose={close} onOpenSystem={() => undefined} />)
    await screen.findByText('逐系统就绪')
    // The single candidate is preselected (only legal option), so confirm + submit.
    fireEvent.click(screen.getByRole('button', { name: '原子激活此契约' }))
    fireEvent.click(screen.getByRole('button', { name: '确认原子激活' }))
    await waitFor(() => expect(activateCalled).toBe(true))
    // A 409 surfaces the message and re-reads readiness instead of closing.
    expect(await screen.findByText(/契约当前指针已变化/)).toBeInTheDocument()
    expect(close).not.toHaveBeenCalled()
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('/readiness')).length).toBeGreaterThanOrEqual(2))
  })

  it('does not let a superseded readiness request replace the newly selected contract', async () => {
    let resolveDraft: ((response: Response) => void) | undefined
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation((input) => {
      const url = String(input)
      if (url.includes('/label-contracts/2/readiness')) return new Promise<Response>((resolve) => { resolveDraft = resolve })
      if (url.includes('/label-contracts/1/readiness')) {
        return Promise.resolve(jsonResponse({
          ...readinessFor([{ key: 'active-system', candidates: [], blockers: ['verification_run_missing'] }]),
          targetContractVersion: 1,
        }))
      }
      return Promise.resolve(jsonResponse({ items: contracts }))
    })
    render(<LabelContractsPanel isAdmin onClose={() => undefined} onOpenSystem={() => undefined} />)
    await screen.findByRole('radio', { name: /v1/ })
    fireEvent.click(screen.getByRole('radio', { name: /v1/ }))
    expect(await screen.findByText('active-system')).toBeInTheDocument()
    resolveDraft?.(jsonResponse(readinessFor([{ key: 'stale-draft-system', candidates: [], blockers: ['verification_run_missing'] }])))
    await Promise.resolve()
    await Promise.resolve()
    expect(screen.queryByText('stale-draft-system')).not.toBeInTheDocument()
    expect(screen.getByText('active-system')).toBeInTheDocument()
  })

  it('keeps activation controls away from operators', async () => {
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockImplementation(async (input) => {
      const url = String(input)
      if (url.includes('/readiness')) {
        return jsonResponse(readinessFor([{ key: 'payments', candidates: [['7', '3']], blockers: [] }]))
      }
      return jsonResponse({ items: contracts })
    })
    render(<LabelContractsPanel isAdmin={false} onClose={() => undefined} onOpenSystem={() => undefined} />)
    await screen.findByText('逐系统就绪')
    expect(screen.queryByRole('button', { name: '原子激活此契约' })).not.toBeInTheDocument()
    expect(screen.getByLabelText(/配置版本 #7/)).toBeDisabled()
  })
})
