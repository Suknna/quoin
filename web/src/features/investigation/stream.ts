import { AssistantMessageAccumulator, UIMessageStreamDecoder } from 'assistant-stream'
import type { ChatModelRunResult } from '@assistant-ui/react'
import { commandId } from './api'

// The frozen ui-message-stream consumption path (HTTP-STREAM-001/002):
// assistant-stream 0.3.37 decodes the SSE framing (`data: [DONE]` is the
// only success terminator; EOF without it decodes as an abrupt error) and
// accumulates the chunks into thread-message updates.

export async function* streamInvestigationMessage(
  investigationId: string,
  messageId: string,
  signal: AbortSignal,
): AsyncGenerator<ChatModelRunResult, void, unknown> {
	const response = await fetch(`/api/v1/investigations/${investigationId}/messages/${messageId}/stream`, {
		method: 'POST',
		credentials: 'include',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ clientCommandId: commandId(), protocol: 'ui-message-stream' }),
		signal,
	})
  if (!response.ok) {
    let message = '暂时无法打开回复流，请重试。'
    try {
      const problem = (await response.json()) as { message?: string }
      if (problem.message) message = problem.message
    } catch {
      // The ordinary-language fallback stays authoritative.
    }
    throw new Error(message)
  }
  if (!response.body) throw new Error('回复流不可用，请重试。')
  const chunks = response.body
    .pipeThrough(new UIMessageStreamDecoder())
    .pipeThrough(new AssistantMessageAccumulator())
	const reader = chunks.getReader()
	while (true) {
		const { done, value } = await reader.read()
		if (done) break
		yield value as unknown as ChatModelRunResult
	}
}
