import { describe, expect, test } from 'vitest'
import { AssistantMessageAccumulator, UIMessageStreamDecoder } from 'assistant-stream'

// The frozen ui-message-stream framing contract on the consumption side
// (HTTP-STREAM-001/002): `data: [DONE]` is the only success terminator,
// EOF without it decodes as an abrupt error, and a multi-byte UTF-8 rune
// split across transport byte boundaries survives exactly.

const encoder = new TextEncoder()

function sse(payload: string): Uint8Array {
  return encoder.encode(`data: ${payload}\n\n`)
}

async function decode(chunks: Uint8Array[]): Promise<Array<Record<string, unknown>>> {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk)
      controller.close()
    },
  })
  const messages: Array<Record<string, unknown>> = []
  const readable = stream
    .pipeThrough(new UIMessageStreamDecoder())
    .pipeThrough(new AssistantMessageAccumulator())
  const reader = readable.getReader()
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    messages.push(value as unknown as Record<string, unknown>)
  }
  return messages
}

function textOf(messages: Array<Record<string, unknown>>): string {
  const last = messages[messages.length - 1] as { content?: Array<{ text?: string }> }
  return (last.content ?? []).map((part) => part.text ?? '').join('')
}

describe('ui-message-stream consumption', () => {
  test('完整帧序列解码为最终消息（text-delta 连接 + finish + [DONE]）', async () => {
    const messages = await decode([
      sse('{"type":"text-start","id":"t1"}'),
      sse('{"type":"text-delta","textDelta":"调查"}'),
      sse('{"type":"text-delta","textDelta":"结论"}'),
      sse('{"type":"text-end"}'),
      sse('{"type":"finish","finishReason":"stop","usage":{"inputTokens":12,"outputTokens":4}}'),
      sse('[DONE]'),
    ])
    expect(messages.length).toBeGreaterThan(0)
    expect(textOf(messages)).toBe('调查结论')
    const last = messages[messages.length - 1] as { status?: { type?: string } }
    expect(last.status?.type).toBe('complete')
  })

  test('多字节 rune 跨字节边界切分仍完整解码（UTF-8 split）', async () => {
    const wire = sse('{"type":"text-delta","textDelta":"中"}')
    // 中 is a 3-byte rune; cut inside it so the transport splits the rune.
    let runeStart = -1
    for (let index = 0; index < wire.length; index++) {
      if (wire[index] === 0xe4) {
        runeStart = index
        break
      }
    }
		expect(runeStart).toBeGreaterThanOrEqual(0)
		const messages = await decode([wire.slice(0, runeStart + 2), wire.slice(runeStart + 2), sse('[DONE]')])
		expect(textOf(messages)).toBe('中')
  })

  test('缺失 [DONE] 的 EOF 解码为 abrupt error（不得视为成功）', async () => {
    await expect(decode([sse('{"type":"text-delta","textDelta":"半截"}')])).rejects.toThrow(/\[DONE\]/)
  })
})
