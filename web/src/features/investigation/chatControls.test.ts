import { describe, expect, it } from 'vitest'
import type { InvestigationMessage } from './api'
import {
  canOfferRetry,
  canOfferUndo,
  latestActiveUserMessage,
  mergeAttachmentIds,
  withdrawnRevision,
} from './chatControls'

function userMessage(id: string, seq: number, overrides: Partial<InvestigationMessage> = {}): InvestigationMessage {
  return {
    id, seq, role: 'user', status: 'active', content: `消息 ${id}`, attachments: [],
    evidenceIds: [], createdAt: '2026-01-01T00:00:00Z', ...overrides,
  }
}

function assistantMessage(id: string, seq: number, overrides: Partial<InvestigationMessage> = {}): InvestigationMessage {
  return {
    id, seq, role: 'assistant', status: 'active', content: `回复 ${id}`, attachments: [],
    evidenceIds: [], createdAt: '2026-01-01T00:00:00Z', ...overrides,
  }
}

describe('latestActiveUserMessage', () => {
  it('returns the newest active user message past assistants', () => {
    const messages = [
      userMessage('1', 1),
      assistantMessage('2', 2),
      userMessage('3', 3),
      assistantMessage('4', 4),
    ]
    expect(latestActiveUserMessage(messages)?.id).toBe('3')
  })

  it('skips withdrawn turns entirely', () => {
    const messages = [userMessage('1', 1, { status: 'withdrawn' }), assistantMessage('2', 2, { status: 'withdrawn' })]
    expect(latestActiveUserMessage(messages)).toBeNull()
  })
})

describe('canOfferUndo', () => {
  const turn = [userMessage('1', 1), assistantMessage('2', 2)]

  it('offers undo under the latest user turn while nothing runs', () => {
    expect(canOfferUndo(turn, turn[0])).toBe(true)
  })

  it('refuses while an attempt is running and on non-latest turns', () => {
    expect(canOfferUndo(turn, turn[0], 'attempt-9')).toBe(false)
    const older = [userMessage('0', 0), userMessage('1', 1)]
    expect(canOfferUndo(older, older[0])).toBe(false)
    expect(canOfferUndo(turn, turn[1])).toBe(false)
  })

  it('refuses withdrawn candidates', () => {
    const withdrawn = userMessage('1', 1, { status: 'withdrawn' })
    expect(canOfferUndo([withdrawn], withdrawn)).toBe(false)
  })
})

describe('canOfferRetry', () => {
  it('offers retry only for failed attempts of active user turns', () => {
    const states = { a1: { state: 'Failed' as const, rowVersion: 2 }, a2: { state: 'Succeeded' as const, rowVersion: 3 } }
    const failedTurn = userMessage('1', 1, { attemptId: 'a1' })
    const doneTurn = userMessage('2', 3, { attemptId: 'a2' })
    expect(canOfferRetry(states, failedTurn)).toBe(true)
    expect(canOfferRetry(states, doneTurn)).toBe(false)
    expect(canOfferRetry(states, failedTurn, 'attempt-live')).toBe(false)
    expect(canOfferRetry(states, userMessage('3', 5))).toBe(false)
  })
})

describe('mergeAttachmentIds', () => {
  it('puts restored references first and collapses duplicates', () => {
    const restored = [
      { id: 'att-2', artifactId: '7', originalFilename: 'b.txt', mediaType: 'text/plain', sizeBytes: 2, digest: 'd', bodyExpired: false, createdAt: 't' },
      { id: 'att-1', artifactId: '6', originalFilename: 'a.txt', mediaType: 'text/plain', sizeBytes: 1, digest: 'd', bodyExpired: false, createdAt: 't' },
    ]
    expect(mergeAttachmentIds(restored, ['att-3', 'att-1'])).toEqual(['att-2', 'att-1', 'att-3'])
  })
})

describe('withdrawnRevision', () => {
  it('counts withdrawn rows so the rebuild key moves only on undo', () => {
    const before = [userMessage('1', 1), assistantMessage('2', 2)]
    const after = [
      userMessage('1', 1, { status: 'withdrawn' }),
      assistantMessage('2', 2, { status: 'withdrawn' }),
    ]
    expect(withdrawnRevision(before)).toBe(0)
    expect(withdrawnRevision(after)).toBe(2)
  })
})
