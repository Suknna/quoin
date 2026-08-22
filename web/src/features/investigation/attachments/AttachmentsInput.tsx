import { useId, useRef, useState, type DragEvent } from 'react'
import type { AttachmentDraft, PendingAttachment } from './useAttachments'

// AttachmentsInput is the composer's attachment strip (UI-CHAT-006):
// keyboard-reachable file picker, drag & drop, hover chips with remove and
// a bounded preview, per-item errors in place. No attachment count cap;
// the size boundary belongs to the server.

interface AttachmentsInputProps {
  draft: AttachmentDraft
}

function formatSize(sizeBytes: number): string {
  if (sizeBytes >= 1024 * 1024) return `${(sizeBytes / (1024 * 1024)).toFixed(1)} MiB`
  if (sizeBytes >= 1024) return `${(sizeBytes / 1024).toFixed(1)} KiB`
  return `${sizeBytes} 字节`
}

function AttachmentChip({ item, onRemove }: { item: PendingAttachment; onRemove: (key: string) => void }) {
  return (
    <span className={`attachment-chip${item.error ? ' error' : ''}`}>
      <span className="attachment-chip-icon" aria-hidden="true">▤</span>
      <span className="attachment-chip-name" title={item.filename}>{item.filename}</span>
      <span className="attachment-chip-size">{formatSize(item.sizeBytes)}</span>
      <button
        type="button"
        className="attachment-chip-remove"
        aria-label={`移除附件 ${item.filename}`}
        onClick={() => onRemove(item.key)}
      >
        ×
      </button>
      {item.attachmentId === undefined && !item.error && <span className="attachment-chip-status">上传中…</span>}
      {item.error && (
        <span className="attachment-chip-error" role="alert">
          {item.error}
        </span>
      )}
    </span>
  )
}

export function AttachmentsInput({ draft }: AttachmentsInputProps) {
  const fileInput = useRef<HTMLInputElement>(null)
  const inputId = useId()
  const [dragOver, setDragOver] = useState(false)

  function pick(files: FileList | null) {
    if (files && files.length > 0) draft.addFiles(files)
  }

  function onDrop(event: DragEvent<HTMLDivElement>) {
    event.preventDefault()
    setDragOver(false)
    if (event.dataTransfer?.files && event.dataTransfer.files.length > 0) {
      draft.addFiles(event.dataTransfer.files)
    }
  }

  return (
    <div
      className={`attachment-strip${dragOver ? ' drag-over' : ''}`}
      onDragOver={(event) => {
        event.preventDefault()
        setDragOver(true)
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={onDrop}
    >
      <label className="attachment-pick" htmlFor={inputId}>
        <input
          id={inputId}
          ref={fileInput}
          type="file"
          multiple
          className="visually-hidden"
          onChange={(event) => {
            pick(event.target.files)
            event.target.value = ''
          }}
        />
        附件
      </label>
      {draft.items.map((item) => (
        <AttachmentChip key={item.key} item={item} onRemove={draft.remove} />
      ))}
      {draft.items.length === 0 && <span className="attachment-hint">可选择、拖放或粘贴文本附件</span>}
    </div>
  )
}
