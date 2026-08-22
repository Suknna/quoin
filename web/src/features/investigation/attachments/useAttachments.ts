import { useCallback, useMemo, useRef, useState } from 'react'
import { uploadAttachment, attachmentCommandId, type TextAttachmentSummary } from './api'

// Pending attachment state for one composer (UI-CHAT-006/007): files stage
// immediately on selection/paste so the send command only references
// durable ids; per-item errors stay in place and never block other items.

export interface PendingAttachment {
  key: string
  filename: string
  sizeBytes: number
  attachmentId?: string
  error?: string
}

export interface AttachmentDraft {
  items: PendingAttachment[]
  addFiles: (files: File[] | FileList) => void
  addPastedText: (text: string) => void
  remove: (key: string) => void
  readyIds: () => string[]
  hasBlocking: boolean
  anyContent: boolean
  clear: () => void
}

// The frozen paste threshold (UI-CHAT-007): one paste of 16 KiB or 200
// lines becomes a previewable/removable temporary .txt attachment; typed
// input never converts mid-flight.
export const PASTE_THRESHOLD_BYTES = 16 * 1024
export const PASTE_THRESHOLD_LINES = 200

export function pasteExceedsThreshold(text: string): boolean {
  if (text.length >= PASTE_THRESHOLD_BYTES) return true
  let lines = 1
  for (const character of text) {
    if (character === '\n') {
      lines++
      if (lines >= PASTE_THRESHOLD_LINES) return true
    }
  }
  return false
}

let counter = 0
function nextKey(): string {
  counter += 1
  return `attach-${Date.now().toString(36)}-${counter}`
}

export function useAttachments(): AttachmentDraft {
  const [items, setItems] = useState<PendingAttachment[]>([])
  const commands = useRef(new Map<string, string>())

  const addFiles = useCallback((files: File[] | FileList) => {
    const list = Array.from(files)
    if (list.length === 0) return
    const added: PendingAttachment[] = list.map((file) => ({
      key: nextKey(),
      filename: file.name,
      sizeBytes: file.size,
    }))
    setItems((current) => [...current, ...added])
    list.forEach((file, index) => {
      const entry = added[index]
      const commandId = attachmentCommandId()
      commands.current.set(entry.key, commandId)
      uploadAttachment(file, commandId)
        .then((summary: TextAttachmentSummary) => {
          setItems((current) =>
            current.map((item) =>
              item.key === entry.key
                ? { ...item, attachmentId: summary.id, sizeBytes: summary.sizeBytes, filename: summary.originalFilename }
                : item,
            ),
          )
        })
        .catch((reason: unknown) => {
          setItems((current) =>
            current.map((item) =>
              item.key === entry.key
                ? { ...item, error: reason instanceof Error ? reason.message : '附件上传失败，请重试。' }
                : item,
            ),
          )
        })
    })
  }, [])

  const addPastedText = useCallback((text: string) => {
    const stamp = new Date()
    const pad = (value: number) => value.toString().padStart(2, '0')
    const filename = `粘贴-${stamp.getFullYear()}${pad(stamp.getMonth() + 1)}${pad(stamp.getDate())}-${pad(stamp.getHours())}${pad(stamp.getMinutes())}${pad(stamp.getSeconds())}.txt`
    addFiles([new File([text], filename, { type: 'text/plain' })])
  }, [addFiles])

  const remove = useCallback((key: string) => {
    setItems((current) => current.filter((item) => item.key !== key))
  }, [])

  const readyIds = useCallback(
    () => items.filter((item) => item.attachmentId !== undefined).map((item) => item.attachmentId as string),
    [items],
  )

  const hasBlocking = useMemo(() => items.some((item) => item.attachmentId === undefined), [items])
  const anyContent = items.length > 0

  const clear = useCallback(() => {
    setItems([])
  }, [])

  return { items, addFiles, addPastedText, remove, readyIds, hasBlocking, anyContent, clear }
}
