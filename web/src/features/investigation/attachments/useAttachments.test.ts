import { describe, expect, it } from 'vitest'
import { PASTE_THRESHOLD_BYTES, PASTE_THRESHOLD_LINES, pasteExceedsThreshold } from './useAttachments'

// UI-CHAT-007: one paste of 16 KiB or 200 lines (either boundary) converts
// to a temporary .txt attachment; smaller pastes stay as plain input.
describe('pasteExceedsThreshold', () => {
  it('keeps small pastes as plain input', () => {
    expect(pasteExceedsThreshold('短文本')).toBe(false)
    expect(pasteExceedsThreshold('line\nline\nline')).toBe(false)
  })

  it('trips the byte boundary exactly at 16 KiB', () => {
    expect(pasteExceedsThreshold('x'.repeat(PASTE_THRESHOLD_BYTES - 1))).toBe(false)
    expect(pasteExceedsThreshold('x'.repeat(PASTE_THRESHOLD_BYTES))).toBe(true)
  })

  it('trips the line boundary at 200 lines regardless of bytes', () => {
    const text = Array.from({ length: PASTE_THRESHOLD_LINES }, () => '行').join('\n')
    expect(text.length).toBeLessThan(PASTE_THRESHOLD_BYTES)
    expect(pasteExceedsThreshold(text)).toBe(true)
    expect(pasteExceedsThreshold(Array.from({ length: PASTE_THRESHOLD_LINES - 1 }, () => '行').join('\n'))).toBe(false)
  })

  it('handles multi-byte characters on the boundary', () => {
    // CJK runes count one character each: 16384 of them trip the boundary
    // without hitting the line cap.
    const text = '查'.repeat(PASTE_THRESHOLD_BYTES)
    expect(pasteExceedsThreshold(text)).toBe(true)
    expect(pasteExceedsThreshold('查'.repeat(PASTE_THRESHOLD_BYTES - 1))).toBe(false)
  })
})
