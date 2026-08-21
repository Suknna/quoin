import { useEffect, useMemo, useRef, useState } from 'react'
import {
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePartPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
	useLocalRuntime,
	type AssistantRuntime,
  type ChatModelAdapter,
  type ThreadMessageLike,
} from '@assistant-ui/react'
import { api, type InvestigationMessage } from './api'
import { streamInvestigationMessage } from './stream'
import './investigation.css'

// ChatThread owns the assistant-ui thread and the frozen two-step command
// protocol: the composer send persists the message through
// sendInvestigationMessage, then the stream attaches to the created
// attempt (HTTP-STREAM-004: the adapter's client message view is never an
// input authority). The head fence is tracked locally from the server's
// responses and reconciles after every turn.

interface ChatThreadProps {
  investigationId: string
  messages: InvestigationMessage[]
  headMessageId: string | null
  // attachMessageId is set when the head user message still has an active
  // attempt on load: the runtime re-attaches without sending
  // (HTTP-STREAM-006 transport detach never cancels the task).
  attachMessageId?: string
  onTurnFinished: () => void
}

export function ChatThread({ investigationId, messages, headMessageId, attachMessageId, onTurnFinished }: ChatThreadProps) {
  const headRef = useRef<string | null>(headMessageId)
  useEffect(() => {
    // The parent reconciles the committed head after every turn; the next
    // send fences on the authoritative value.
    headRef.current = headMessageId
  }, [headMessageId])

  const initialMessages = useMemo<ThreadMessageLike[]>(
    () =>
      messages
        .filter((message) => message.status === 'active')
        .map((message) => ({
          id: `quoin-m${message.id}`,
          role: message.role,
          content: message.content,
        })),
    [messages],
  )

  const adapter = useMemo<ChatModelAdapter>(
    () => ({
      run: async function* ({ messages: threadMessages, runConfig, abortSignal }) {
        // Re-attach path: the run was triggered for the persisted head
        // user message, not a new composer send.
        const attachId = (runConfig?.custom?.quoinAttach as string | undefined) ?? null
        let streamMessageId: string
        if (attachId === null) {
          const last = [...threadMessages].reverse().find((message) => message.role === 'user')
          const text = last?.content
            .filter((part): part is { type: 'text'; text: string } => part.type === 'text')
            .map((part) => part.text)
            .join('')
			if (!text || text.trim() === '') throw new Error('消息不能为空。')
			const sent = await api.sendMessage(investigationId, text, headRef.current)
			// The head fence stays on the durable projection: the commit
			// moves it to the assistant message and the parent reconciles
			// it through headMessageId — the user message id is never a
			// valid fence for the next send.
			streamMessageId = sent.id
        } else {
          streamMessageId = attachId
        }
        yield* streamInvestigationMessage(investigationId, streamMessageId, abortSignal)
      },
    }),
    [investigationId],
  )

  const runtime = useLocalRuntime(adapter, { initialMessages })

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <ChatSurface
        runtime={runtime}
        attachMessageId={attachMessageId}
        onTurnFinished={onTurnFinished}
      />
    </AssistantRuntimeProvider>
  )
}

function ChatSurface({ runtime, attachMessageId, onTurnFinished }: { runtime: AssistantRuntime; attachMessageId?: string; onTurnFinished: () => void }) {
  const [showNewReply, setShowNewReply] = useState(false)
  const [streaming, setStreaming] = useState(false)
  const scrollerRef = useRef<HTMLDivElement | null>(null)
  const followRef = useRef(true)

	// Re-attach on mount: the persisted head user message still owns an
	// active attempt, so the stream continues exactly where the last
	// observer detached (the run is keyed once per mount).
	const attached = useRef(false)
	useEffect(() => {
		if (attachMessageId && !attached.current) {
			attached.current = true
			// startRun rewinds to the parent: the parent is the thread's
			// last message (a root parent would wipe the restored history
			// before the run appends).
			const threadState = runtime.thread.getState()
			const parentId = threadState.messages.at(-1)?.id ?? null
			runtime.thread.startRun({ parentId, runConfig: { custom: { quoinAttach: attachMessageId } } })
		}
	}, [attachMessageId, runtime])

  // A finished turn reconciles the durable messages and the head fence
  // (HTTP-STREAM-003: the committed message is the authority); the
  // callback fires only on the running -> stopped transition.
  useEffect(() => {
    let wasRunning = false
    const unsubscribe = runtime.thread.subscribe(() => {
      const next = runtime.thread.getState()
      setStreaming(next.isRunning)
      if (wasRunning && !next.isRunning) onTurnFinished()
      wasRunning = next.isRunning
    })
    return unsubscribe
  }, [runtime, onTurnFinished])

		// Each new run starts in follow mode unless the reader detaches.
		useEffect(() => {
			if (!streaming) return
			setShowNewReply(!followRef.current)
		}, [streaming])

  return (
		<div
			className="chat-scroll-capture"
			onScrollCapture={(event) => {
				// The wrapper contains nothing but the thread: any scroll
				// event captured here is the viewport's own (scroll events
				// do not bubble, capture-phase only).
				const target = event.target as HTMLDivElement
				scrollerRef.current = target
				const distance = target.scrollHeight - target.scrollTop - target.clientHeight
				followRef.current = distance < 80
				// A detach during a stream surfaces the entry; it stays until
				// the reader scrolls back down (UI-CHAT-003).
				if (streaming && !followRef.current) setShowNewReply(true)
			}}
		>
      <ThreadPrimitive.Root className="chat-thread">
        <ThreadPrimitive.Viewport className="chat-viewport">
          <ThreadPrimitive.Empty>
            <div className="chat-empty">
              <p>发送第一条消息，开始生成回复。</p>
            </div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages
            components={{
              UserMessage: UserMessageShell,
              AssistantMessage: AssistantMessageShell,
            }}
          />
          {showNewReply && (
			<button
				className="chat-new-reply"
				onClick={() => {
					const scroller = scrollerRef.current
					scroller?.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' })
					followRef.current = true
					setShowNewReply(false)
				}}
			>
				查看新回复
			</button>
          )}
        </ThreadPrimitive.Viewport>
        <ComposerPrimitive.Root className="chat-composer">
          <ComposerPrimitive.Input
            className="chat-composer-input"
            placeholder="输入消息…（Enter 发送）"
            autoFocus
          />
          <ComposerPrimitive.Send className="chat-send">发送</ComposerPrimitive.Send>
        </ComposerPrimitive.Root>
      </ThreadPrimitive.Root>
    </div>
  )
}

function UserMessageShell() {
  return (
    <MessagePrimitive.Root className="chat-message chat-message-user">
      <MessagePrimitive.Content components={{ Text: TextPart }} />
    </MessagePrimitive.Root>
  )
}

function AssistantMessageShell() {
  return (
    <MessagePrimitive.Root className="chat-message chat-message-assistant">
      <MessagePrimitive.Content components={{ Text: TextPart }} />
    </MessagePrimitive.Root>
  )
}

function TextPart() {
	// smooth defaults to true on MessagePartPrimitive.Text and its segment
	// animation drops the head of a replaced whole-content text; the
	// streamed arrival itself is the progressive feedback, so disable it
	// explicitly.
	return <MessagePartPrimitive.Text component="p" className="chat-text" smooth={false} />
}
