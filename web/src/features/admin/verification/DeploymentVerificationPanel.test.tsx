import '@testing-library/jest-dom/vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { DeploymentVerificationDetail } from './api'
import { DeploymentVerificationPanel } from './DeploymentVerificationPanel'

const digest = 'a'.repeat(64)
const now = '2026-03-01T00:00:00.000Z'

function detail(overrides: Partial<DeploymentVerificationDetail> = {}): DeploymentVerificationDetail {
  return {
    id: '42', releaseSubjectDigest: digest, catalogDigest: digest, resultProfileDigest: digest,
    deploymentConfigDigest: digest, publicOriginDigest: digest, applicableSetDigest: digest,
    itemCount: 1, itemSetDigest: digest, manifestDigest: digest, startedAt: now,
    deadlineAt: '2026-03-01T08:00:00.000Z', progress: { completed: 0, total: 1 },
    items: [], results: [], conflicts: [], subjectDrifts: [], ...overrides,
  }
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

describe('DeploymentVerificationPanel', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn())
    vi.stubGlobal('crypto', { randomUUID: () => 'command-id', getRandomValues: (value: Uint8Array) => value })
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  })
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals() })

  it('polls an unfinalized detail every five seconds while visible', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-01T00:00:00.000Z'))
    vi.mocked(fetch).mockImplementation(async () => json(detail()))
    render(<DeploymentVerificationPanel invocationId="42" onOpen={() => undefined} onBack={() => undefined} />)
    await act(async () => { await vi.advanceTimersByTimeAsync(0) })
    expect(fetch).toHaveBeenCalledTimes(1)
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  it('shows the final receipt and stops periodic polling', async () => {
    vi.useFakeTimers()
    const receipt = { id: '7', manifestDigest: digest, applicableSetDigest: digest, itemSetDigest: digest, resultSetDigest: digest, helperImportSetDigest: digest, typedObservationSetDigest: digest, conflictSetDigest: digest, subjectDriftDigest: digest, overallOutcome: 'passed' as const, finalResultDigest: digest, canonicalArtifactId: '9', snapshotAt: now, finalizedAt: now }
    vi.mocked(fetch).mockImplementation(async () => json(detail({ receipt })))
    render(<DeploymentVerificationPanel invocationId="42" onOpen={() => undefined} onBack={() => undefined} />)
    await act(async () => { await vi.advanceTimersByTimeAsync(0) })
    expect(screen.getByText('最终 receipt')).toBeInTheDocument()
    const callsAfterLoad = vi.mocked(fetch).mock.calls.length
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(fetch).toHaveBeenCalledTimes(callsAfterLoad)
  })

  it('marks an expired unfinalized invocation as requiring a new one and stops polling', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-02T00:00:00.000Z'))
    vi.mocked(fetch).mockImplementation(async () => json(detail({ deadlineAt: '2026-03-01T08:00:00.000Z' })))
    render(<DeploymentVerificationPanel invocationId="42" onOpen={() => undefined} onBack={() => undefined} />)
    await act(async () => { await vi.advanceTimersByTimeAsync(0) })
    expect(screen.getByText('错过截止，需新建 invocation。该验收不会在 8 小时窗口外补写最终结论。')).toBeInTheDocument()
    const callsAfterLoad = vi.mocked(fetch).mock.calls.length
    await act(async () => { await vi.advanceTimersByTimeAsync(5000) })
    expect(fetch).toHaveBeenCalledTimes(callsAfterLoad)
  })

  it('requires all three typed observation choices before submitting', async () => {
    const observationItem = { id: '11', itemSeq: 1, scenarioId: 'acceptance', cellId: 'linux-amd64', objectKind: 'ui_observation' as const, inputDigest: digest, locator: { page: '/admin' } }
    vi.mocked(fetch).mockImplementation(async () => json(detail({ items: [observationItem] })))
    render(<DeploymentVerificationPanel invocationId="42" onOpen={() => undefined} onBack={() => undefined} />)
    expect(await screen.findByText('需要人工观察')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '提交观察' }))
    expect(screen.getByText(/请完成三个观察项/)).toBeInTheDocument()
    expect(vi.mocked(fetch).mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
  })
})
